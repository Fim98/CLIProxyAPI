package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/zed"
)

// InputFormats are the client protocol formats the executor accepts; the host
// translates the incoming request into one of them before calling the plugin.
var InputFormats = []string{
	sdktranslator.FormatOpenAI.String(),
	sdktranslator.FormatOpenAIResponse.String(),
	sdktranslator.FormatClaude.String(),
	sdktranslator.FormatGemini.String(),
	sdktranslator.FormatCodex.String(),
}

// OutputFormats are the response formats the executor emits. Every protocol
// CPA serves can be produced, so the host never needs to translate plugin
// output on the caller's behalf.
var OutputFormats = []string{
	sdktranslator.FormatOpenAI.String(),
	sdktranslator.FormatOpenAIResponse.String(),
	sdktranslator.FormatClaude.String(),
	sdktranslator.FormatGemini.String(),
	sdktranslator.FormatCodex.String(),
}

const maxStreamLineBytes = 16 << 20

// Executor implements pluginapi.ProviderExecutor for Zed's cloud completions.
type Executor struct {
	settings zedconfig.Settings
	tokens   *zed.TokenPool
	catalog  *catalogCache
}

func New(settings zedconfig.Settings, tokens *zed.TokenPool) *Executor {
	return &Executor{settings: settings, tokens: tokens, catalog: newCatalogCache()}
}

func (e *Executor) Identifier() string { return credentials.Provider }

type completionBody struct {
	ThreadID       string          `json:"thread_id,omitempty"`
	PromptID       string          `json:"prompt_id,omitempty"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	ProviderRequest json.RawMessage `json:"provider_request"`
}

func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	if req.Alt != "" {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("Zed executor does not support alternate route %q", req.Alt)
	}
	storage, errStorage := e.storage(req.StorageJSON)
	if errStorage != nil {
		return pluginapi.ExecutorResponse{}, errStorage
	}
	model := cleanModelName(req.Model)
	requestBody, provider, errBuild := e.buildCompletionBody(ctx, storage, req, model, true)
	if errBuild != nil {
		return pluginapi.ExecutorResponse{}, errBuild
	}
	wire := wireFormatForProvider(provider)
	outputFormat := responseFormat(req)

	events, errEvents := e.collectCompletionEvents(ctx, storage, req, requestBody, model)
	if errEvents != nil {
		return pluginapi.ExecutorResponse{}, errEvents
	}
	aggregate := aggregatorForFormat(wire.String())
	if aggregate == nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("Zed executor has no aggregator for wire format %s", wire)
	}
	upstreamPayload, errAggregate := aggregate(events)
	if errAggregate != nil {
		return pluginapi.ExecutorResponse{}, errAggregate
	}
	payload, errTranslate := translateNonStream(ctx, wire, outputFormat, model, req.OriginalRequest, requestBody, upstreamPayload)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, errTranslate
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return pluginapi.ExecutorResponse{Payload: payload, Headers: headers}, nil
}

func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	if req.Alt != "" {
		return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("Zed executor does not support alternate route %q", req.Alt)
	}
	storage, errStorage := e.storage(req.StorageJSON)
	if errStorage != nil {
		return pluginapi.ExecutorStreamResponse{}, errStorage
	}
	model := cleanModelName(req.Model)
	requestBody, provider, errBuild := e.buildCompletionBody(ctx, storage, req, model, true)
	if errBuild != nil {
		return pluginapi.ExecutorStreamResponse{}, errBuild
	}
	wire := wireFormatForProvider(provider)
	outputFormat := responseFormat(req)

	response, errDo := e.postCompletions(ctx, storage, req.HTTPClient, requestBody)
	if errDo != nil {
		return pluginapi.ExecutorStreamResponse{}, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return pluginapi.ExecutorStreamResponse{}, zed.NewStatusError(response.StatusCode, readAllChunks(ctx, response.Chunks))
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "text/event-stream")
	return pluginapi.ExecutorStreamResponse{
		Headers: headers,
		Chunks:  e.translateCompletionStream(ctx, req, model, wire, outputFormat, requestBody, response.Chunks),
	}, nil
}

// translateCompletionStream converts the Zed NDJSON stream into chunks in the
// requested output format. Zed always streams, so this is the single response
// path for both streaming and non-streaming downstream requests.
func (e *Executor) translateCompletionStream(ctx context.Context, req pluginapi.ExecutorRequest, model string, wire, outputFormat sdktranslator.Format, requestBody []byte, input <-chan pluginapi.HTTPStreamChunk) <-chan pluginapi.ExecutorStreamChunk {
	output := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(output)
		var translatedState any
		var pending []json.RawMessage
		var failed *zed.StatusError

		emitTranslated := func(event json.RawMessage) bool {
			pending = append(pending, event)
			if wire == outputFormat {
				return sendChunk(ctx, output, pluginapi.ExecutorStreamChunk{Payload: frameWireEvent(wire, event)})
			}
			line := append([]byte("data: "), bytes.TrimSpace(event)...)
			frames := builtin.Registry().TranslateStream(ctx, wire, outputFormat, model, req.OriginalRequest, requestBody, line, &translatedState)
			for _, frame := range frames {
				if len(frame) == 0 {
					continue
				}
				if !sendChunk(ctx, output, pluginapi.ExecutorStreamChunk{Payload: append([]byte(nil), frame...)}) {
					return false
				}
			}
			return true
		}

		errRead := readNDJSONLines(ctx, input, maxStreamLineBytes, func(line []byte) bool {
			event, failedStatus, ended, ok := classifyZedLine(line)
			if !ok {
				return true
			}
			if failedStatus != nil {
				failed = zed.NewStatusError(http.StatusBadGateway, []byte(failedStatus.Message))
				return false
			}
			if ended {
				return false
			}
			return emitTranslated(event)
		})
		if errRead != nil && failed == nil {
			sendChunk(ctx, output, pluginapi.ExecutorStreamChunk{Err: errRead})
			return
		}
		if failed != nil {
			sendChunk(ctx, output, pluginapi.ExecutorStreamChunk{Err: failed})
			return
		}
		// A stream that ended without events still needs a terminal frame for
		// protocols whose clients block on one.
		if len(pending) == 0 {
			switch outputFormat {
			case sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
				sendChunk(ctx, output, pluginapi.ExecutorStreamChunk{Err: fmt.Errorf("Zed upstream produced no completion events")})
			}
		}
		// The openai chat and gemini handlers append their own terminators;
		// responses-style protocols end with their terminal event.
	}()
	return output
}

// frameWireEvent renders one native provider event as a chunk in its own wire
// style. The chunk style matches what CPA's protocol handlers expect when they
// write plugin chunks straight to the client.
func frameWireEvent(wire sdktranslator.Format, event json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(event)
	switch wire {
	case sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(trimmed, &probe)
		frame := make([]byte, 0, len(trimmed)+len(probe.Type)+32)
		if probe.Type != "" {
			frame = append(frame, "event: "...)
			frame = append(frame, probe.Type...)
			frame = append(frame, '\n')
		}
		frame = append(frame, "data: "...)
		frame = append(frame, trimmed...)
		frame = append(frame, '\n', '\n')
		return frame
	default:
		// openai chat and gemini handlers wrap bare payloads in SSE frames.
		return append([]byte(nil), trimmed...)
	}
}

func (e *Executor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	storage, errStorage := e.storage(req.StorageJSON)
	if errStorage != nil {
		return pluginapi.ExecutorResponse{}, errStorage
	}
	model := cleanModelName(req.Model)
	provider, errProvider := e.providerForModel(ctx, storage, model, req.HTTPClient)
	if errProvider != nil {
		return pluginapi.ExecutorResponse{}, errProvider
	}
	wire := wireFormatForProvider(provider)
	if wire != sdktranslator.FormatClaude && wire != sdktranslator.FormatOpenAIResponse {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("Zed does not support token counting for provider %s", provider)
	}
	source := sourceFormat(req)
	requestBody, errTranslate := translateRequest(source, wire, model, req.Payload, false)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, errTranslate
	}
	requestBody, errNormalize := normalizeProviderRequest(requestBody, wire, false)
	if errNormalize != nil {
		return pluginapi.ExecutorResponse{}, errNormalize
	}
	body, errMarshal := json.Marshal(completionBody{
		Provider:        provider,
		Model:           model,
		ProviderRequest: requestBody,
	})
	if errMarshal != nil {
		return pluginapi.ExecutorResponse{}, errMarshal
	}
	response, errDo := e.postWithTokenRetry(ctx, storage, req.HTTPClient, "/count_tokens", body)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, zed.NewStatusError(response.StatusCode, response.Body)
	}
	var count struct {
		Tokens int64 `json:"tokens"`
	}
	if errDecode := json.Unmarshal(response.Body, &count); errDecode != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("decode Zed token count: %w", errDecode)
	}
	output := responseFormat(req)
	payload := response.Body
	if output != wire {
		payload = builtin.Registry().TranslateTokenCount(ctx, wire, output, count.Tokens, response.Body)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return pluginapi.ExecutorResponse{Payload: payload, Headers: headers}, nil
}

// HttpRequest forwards a caller-issued request to Zed's cloud with the right
// credential: account authentication on /client paths, an LLM token elsewhere.
func (e *Executor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	storage, errStorage := e.storage(req.StorageJSON)
	if errStorage != nil {
		return pluginapi.ExecutorHTTPResponse{}, errStorage
	}
	parsed, errParse := url.Parse(strings.TrimSpace(req.URL))
	if errParse != nil {
		return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("parse Zed HTTP request URL: %w", errParse)
	}
	target := e.settings.CloudURL + parsed.Path
	if parsed.RawQuery != "" {
		target += "?" + parsed.RawQuery
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodPost
	}
	headers := req.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	if strings.HasPrefix(parsed.Path, "/client/") {
		headers.Set("Authorization", fmt.Sprintf("%s %s", storage.UserID, storage.AccessToken))
	} else {
		llmToken, errToken := e.tokens.LLMToken(ctx, req.HTTPClient, e.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
		if errToken != nil {
			return pluginapi.ExecutorHTTPResponse{}, errToken
		}
		headers.Set("Authorization", "Bearer "+llmToken)
		headers.Set("x-zed-version", e.settings.ClientVersion)
	}
	headers.Set("User-Agent", zed.UserAgent(e.settings))
	request := pluginapi.HTTPRequest{Method: method, URL: target, Headers: headers, Body: append([]byte(nil), req.Body...)}
	response, errDo := req.HTTPClient.Do(ctx, request)
	if errDo != nil {
		return pluginapi.ExecutorHTTPResponse{}, errDo
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: response.Body}, nil
}

// buildCompletionBody translates the incoming payload into the native provider
// request and wraps it in Zed's CompletionBody.
func (e *Executor) buildCompletionBody(ctx context.Context, storage credentials.Storage, req pluginapi.ExecutorRequest, model string, stream bool) ([]byte, string, error) {
	provider, errProvider := e.providerForModel(ctx, storage, model, req.HTTPClient)
	if errProvider != nil {
		return nil, "", errProvider
	}
	wire := wireFormatForProvider(provider)
	if wire == "" {
		return nil, "", fmt.Errorf("Zed provider %s has no supported wire format", provider)
	}
	source := sourceFormat(req)
	requestBody, errTranslate := translateRequest(source, wire, model, req.Payload, stream)
	if errTranslate != nil {
		return nil, "", errTranslate
	}
	requestBody, errNormalize := normalizeProviderRequest(requestBody, wire, stream)
	if errNormalize != nil {
		return nil, "", errNormalize
	}
	body, errMarshal := json.Marshal(completionBody{
		ThreadID:       uuid.NewString(),
		PromptID:       uuid.NewString(),
		Provider:       provider,
		Model:          model,
		ProviderRequest: requestBody,
	})
	if errMarshal != nil {
		return nil, "", errMarshal
	}
	return body, provider, nil
}

// normalizeProviderRequest adjusts the translated request to what Zed's client
// actually sends: Anthropic and Gemini requests carry no stream flag (Zed
// always streams server-side), while OpenAI Requests set stream explicitly.
func normalizeProviderRequest(requestBody []byte, wire sdktranslator.Format, stream bool) ([]byte, error) {
	var payload map[string]any
	if errDecode := json.Unmarshal(requestBody, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode translated Zed provider request: %w", errDecode)
	}
	if payload["model"] != nil {
		// The translator may embed the model; Zed's CompletionBody names it
		// authoritatively, and provider_request.model must match.
		if model, ok := payload["model"].(string); ok && strings.TrimSpace(model) != "" {
			payload["model"] = cleanModelName(model)
		}
	}
	switch wire {
	case sdktranslator.FormatClaude, sdktranslator.FormatGemini:
		delete(payload, "stream")
	default:
		payload["stream"] = stream
	}
	updated, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode Zed provider request: %w", errMarshal)
	}
	return updated, nil
}

// collectCompletionEvents posts /completions and gathers every native event of
// the NDJSON stream, for non-streaming downstream requests.
func (e *Executor) collectCompletionEvents(ctx context.Context, storage credentials.Storage, req pluginapi.ExecutorRequest, requestBody []byte, model string) ([]json.RawMessage, error) {
	response, errDo := e.postCompletions(ctx, storage, req.HTTPClient, requestBody)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, zed.NewStatusError(response.StatusCode, readAllChunks(ctx, response.Chunks))
	}
	var events []json.RawMessage
	var failed *zed.StatusError
	errRead := readNDJSONLines(ctx, response.Chunks, maxStreamLineBytes, func(line []byte) bool {
		event, failedStatus, ended, ok := classifyZedLine(line)
		if !ok {
			return true
		}
		if failedStatus != nil {
			failed = zed.NewStatusError(http.StatusBadGateway, []byte(failedStatus.Message))
			return false
		}
		if ended {
			return false
		}
		events = append(events, event)
		return true
	})
	if failed != nil {
		return nil, failed
	}
	if errRead != nil {
		return nil, errRead
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("Zed upstream produced no completion events")
	}
	return events, nil
}

// postCompletions posts to /completions with a freshly minted LLM token,
// retrying once when Zed signals the cached token is stale.
func (e *Executor) postCompletions(ctx context.Context, storage credentials.Storage, httpClient pluginapi.HostHTTPClient, requestBody []byte) (pluginapi.HTTPStreamResponse, error) {
	llmToken, errToken := e.tokens.LLMToken(ctx, httpClient, e.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
	if errToken != nil {
		return pluginapi.HTTPStreamResponse{}, fmt.Errorf("mint Zed LLM token: %w", errToken)
	}
	headers := zed.CompletionHeaders(e.settings, llmToken)
	request := pluginapi.HTTPRequest{Method: http.MethodPost, URL: e.settings.CloudURL + "/completions", Headers: headers, Body: requestBody}
	response, errDo := httpClient.DoStream(ctx, request)
	if errDo != nil {
		return pluginapi.HTTPStreamResponse{}, fmt.Errorf("Zed completions request failed: %w", errDo)
	}
	if zed.TokenNeedsRefresh(response.Headers, response.StatusCode) {
		e.tokens.Invalidate(storage.Key())
		llmToken, errToken = e.tokens.LLMToken(ctx, httpClient, e.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
		if errToken != nil {
			return pluginapi.HTTPStreamResponse{}, fmt.Errorf("re-mint Zed LLM token: %w", errToken)
		}
		response, errDo = httpClient.DoStream(ctx, pluginapi.HTTPRequest{
			Method: http.MethodPost, URL: e.settings.CloudURL + "/completions",
			Headers: zed.CompletionHeaders(e.settings, llmToken), Body: requestBody,
		})
		if errDo != nil {
			return pluginapi.HTTPStreamResponse{}, fmt.Errorf("Zed completions request failed: %w", errDo)
		}
	}
	return response, nil
}

// postWithTokenRetry is postCompletions for non-streaming endpoints.
func (e *Executor) postWithTokenRetry(ctx context.Context, storage credentials.Storage, httpClient pluginapi.HostHTTPClient, path string, requestBody []byte) (pluginapi.HTTPResponse, error) {
	sendOnce := func(llmToken string) (pluginapi.HTTPResponse, error) {
		headers := zed.CompletionHeaders(e.settings, llmToken)
		request := pluginapi.HTTPRequest{Method: http.MethodPost, URL: e.settings.CloudURL + path, Headers: headers, Body: requestBody}
		return httpClient.Do(ctx, request)
	}
	llmToken, errToken := e.tokens.LLMToken(ctx, httpClient, e.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
	if errToken != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("mint Zed LLM token: %w", errToken)
	}
	response, errDo := sendOnce(llmToken)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("Zed request to %s failed: %w", path, errDo)
	}
	if zed.TokenNeedsRefresh(response.Headers, response.StatusCode) {
		e.tokens.Invalidate(storage.Key())
		llmToken, errToken = e.tokens.LLMToken(ctx, httpClient, e.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
		if errToken != nil {
			return pluginapi.HTTPResponse{}, fmt.Errorf("re-mint Zed LLM token: %w", errToken)
		}
		response, errDo = sendOnce(llmToken)
		if errDo != nil {
			return pluginapi.HTTPResponse{}, fmt.Errorf("Zed request to %s failed: %w", path, errDo)
		}
	}
	return response, nil
}

func (e *Executor) storage(raw []byte) (credentials.Storage, error) {
	storage, errParse := credentials.Parse(raw, e.settings)
	if errParse != nil {
		return credentials.Storage{}, errParse
	}
	if storage == nil {
		return credentials.Storage{}, fmt.Errorf("Zed auth storage is missing")
	}
	return *storage, nil
}

func sourceFormat(req pluginapi.ExecutorRequest) sdktranslator.Format {
	value := strings.TrimSpace(req.SourceFormat)
	if value == "" {
		value = strings.TrimSpace(req.Format)
	}
	return sdktranslator.FromString(value)
}

func responseFormat(req pluginapi.ExecutorRequest) sdktranslator.Format {
	value := strings.TrimSpace(req.Format)
	if value == "" {
		return sourceFormat(req)
	}
	return sdktranslator.FromString(value)
}

func translateRequest(from, to sdktranslator.Format, model string, body []byte, stream bool) ([]byte, error) {
	if from == "" {
		return nil, fmt.Errorf("Zed executor request format is missing")
	}
	if from == to {
		return append([]byte(nil), body...), nil
	}
	registry := builtin.Registry()
	if !registry.HasRequestTransformer(from, to) {
		return nil, fmt.Errorf("Zed executor cannot translate request %s -> %s", from, to)
	}
	return registry.TranslateRequest(from, to, model, body, stream), nil
}

func translateNonStream(ctx context.Context, from, to sdktranslator.Format, model string, originalRequest, translatedRequest, body []byte) ([]byte, error) {
	if to == "" || from == to {
		return append([]byte(nil), body...), nil
	}
	registry := builtin.Registry()
	if !registry.HasNonStreamResponseTransformer(to, from) {
		return nil, fmt.Errorf("Zed executor cannot translate response %s -> %s", from, to)
	}
	var state any
	return registry.TranslateNonStream(ctx, from, to, model, originalRequest, translatedRequest, body, &state), nil
}

func cleanModelName(model string) string {
	// CPA model selectors may carry a parenthesized suffix such as
	// "claude-sonnet-4-5(high)"; Zed only knows the bare model id.
	model = strings.TrimSpace(model)
	if index := strings.Index(model, "("); index > 0 && strings.HasSuffix(model, ")") {
		model = model[:index]
	}
	return strings.TrimSpace(model)
}

func sendChunk(ctx context.Context, output chan<- pluginapi.ExecutorStreamChunk, chunk pluginapi.ExecutorStreamChunk) bool {
	select {
	case output <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func readAllChunks(ctx context.Context, input <-chan pluginapi.HTTPStreamChunk) []byte {
	var body []byte
	for len(body) < 1<<20 {
		select {
		case <-ctx.Done():
			return body
		case chunk, ok := <-input:
			if !ok {
				return body
			}
			remaining := (1 << 20) - len(body)
			if len(chunk.Payload) > remaining {
				body = append(body, chunk.Payload[:remaining]...)
				return body
			}
			body = append(body, chunk.Payload...)
			if chunk.Err != nil {
				return body
			}
		}
	}
	return body
}
