package zed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
)

const (
	userAgentHeader = "User-Agent"
	// versionHeader carries the app semver on every Bearer-authed LLM call.
	versionHeader = "x-zed-version"
	// systemIDHeader optionally identifies the machine on /client endpoints.
	systemIDHeader = "x-zed-system-id"
	// expiredTokenHeader and outdatedTokenHeader tell the client its cached LLM
	// token must be re-minted without burning a 401 round trip.
	expiredTokenHeader  = "x-zed-expired-token"
	outdatedTokenHeader = "x-zed-outdated-token"
	// supportsXAIHeader opts the account's model list into xAI models.
	supportsXAIHeader = "x-zed-client-supports-x-ai"
)

// Organization is one Zed organization an account belongs to.
type Organization struct {
	ID         string
	Name       string
	IsPersonal bool
}

// UserProfile is the subset of GET /client/users/me the plugin consumes.
type UserProfile struct {
	Username              string
	Email                 string
	Organizations         []Organization
	DefaultOrganizationID string
	// PlansByOrganization maps organization id to plan id (best effort; unknown
	// shapes are skipped).
	PlansByOrganization map[string]string
}

// ModelDefinition is one entry of GET /models.
type ModelDefinition struct {
	Provider         string `json:"provider"`
	ID               string `json:"id"`
	DisplayName      string `json:"display_name"`
	MaxTokenCount    int64  `json:"max_token_count"`
	MaxOutputTokens  int64  `json:"max_output_tokens"`
	SupportsThinking bool   `json:"supports_thinking"`
	IsDisabled       bool   `json:"is_disabled"`
	DisabledReason   string `json:"disabled_reason"`
}

// ListModelsResponse is the GET /models payload.
type ListModelsResponse struct {
	Models       []ModelDefinition `json:"models"`
	DefaultModel string            `json:"default_model"`
}

// StatusError carries an upstream HTTP status so the ABI error envelope can
// propagate it to the client instead of collapsing everything into a 500.
type StatusError struct {
	statusCode int
	Body       []byte
}

func (e *StatusError) Error() string {
	snippet := string(e.Body)
	if len(snippet) > 512 {
		snippet = snippet[:512]
	}
	snippet = strings.TrimSpace(snippet)
	if snippet == "" {
		return fmt.Sprintf("Zed upstream returned HTTP %d", e.statusCode)
	}
	return fmt.Sprintf("Zed upstream returned HTTP %d: %s", e.statusCode, snippet)
}

// StatusCode makes the error consumable by the ABI envelope builder.
func (e *StatusError) StatusCode() int { return e.statusCode }

func NewStatusError(statusCode int, body []byte) *StatusError {
	return &StatusError{statusCode: statusCode, Body: bytes.Clone(body)}
}

// UserAgent mirrors crates/zed/src/main.rs: Zed/{version} ({os}; {arch}).
func UserAgent(settings zedconfig.Settings) string {
	return fmt.Sprintf("Zed/%s (%s; %s)", settings.ClientVersion, osName(), archName())
}

func osName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return runtime.GOOS
	}
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// authorizationHeader builds Zed's user-credential header. It is a
// space-separated "{user_id} {access_token}" pair, not a Bearer token.
func authorizationHeader(userID, accessToken string) string {
	return userID + " " + accessToken
}

func doJSON(ctx context.Context, httpClient pluginapi.HostHTTPClient, method, url string, headers http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	request := pluginapi.HTTPRequest{Method: method, URL: url, Headers: headers, Body: body}
	if request.Headers == nil {
		request.Headers = make(http.Header)
	}
	response, errDo := httpClient.Do(ctx, request)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("Zed upstream request to %s failed: %w", url, errDo)
	}
	return response, nil
}

func decodeJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if errDecode := decoder.Decode(target); errDecode != nil && errDecode != io.EOF {
		return fmt.Errorf("decode Zed response: %w", errDecode)
	}
	return nil
}

// FetchUserProfile calls GET /client/users/me with the account credential.
// A 401 means the long-lived access token is no longer valid and the only
// remedy is a fresh browser sign-in.
func FetchUserProfile(ctx context.Context, httpClient pluginapi.HostHTTPClient, settings zedconfig.Settings, userID, accessToken, systemID string) (*UserProfile, error) {
	headers := make(http.Header)
	headers.Set("Authorization", authorizationHeader(userID, accessToken))
	headers.Set("Accept", "application/json")
	if systemID != "" {
		headers.Set(systemIDHeader, systemID)
	}
	response, errDo := doJSON(ctx, httpClient, http.MethodGet, settings.CloudURL+"/client/users/me", headers, nil)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode != http.StatusOK {
		return nil, NewStatusError(response.StatusCode, response.Body)
	}
	var payload struct {
		User struct {
			Username string `json:"username"`
			Email    string `json:"email"`
		} `json:"user"`
		Organizations []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			IsPersonal bool   `json:"is_personal"`
		} `json:"organizations"`
		DefaultOrganizationID string         `json:"default_organization_id"`
		PlansByOrganization   map[string]any `json:"plans_by_organization"`
	}
	if errDecode := decodeJSON(response.Body, &payload); errDecode != nil {
		return nil, errDecode
	}
	profile := &UserProfile{
		Username:              strings.TrimSpace(payload.User.Username),
		Email:                 strings.TrimSpace(payload.User.Email),
		DefaultOrganizationID: strings.TrimSpace(payload.DefaultOrganizationID),
		PlansByOrganization:   make(map[string]string, len(payload.PlansByOrganization)),
	}
	for _, organization := range payload.Organizations {
		profile.Organizations = append(profile.Organizations, Organization{
			ID:         strings.TrimSpace(organization.ID),
			Name:       strings.TrimSpace(organization.Name),
			IsPersonal: organization.IsPersonal,
		})
	}
	for organizationID, value := range payload.PlansByOrganization {
		if plan := planIDFromValue(value); plan != "" {
			profile.PlansByOrganization[organizationID] = plan
		}
	}
	return profile, nil
}

// planIDFromValue reads KnownOrUnknown<Plan, String>, which serializes either
// as a bare plan id string or as an object wrapping one.
func planIDFromValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, key := range []string{"known", "plan", "plan_v3"} {
			if text, ok := typed[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

// TokenPool caches per-credential, per-organization LLM tokens in memory. The
// token carries the organization claim, so a credential and an organization id
// always mint the same logical token.
type TokenPool struct {
	mu     sync.Mutex
	tokens map[string]string
}

func NewTokenPool() *TokenPool {
	return &TokenPool{tokens: make(map[string]string)}
}

func (p *TokenPool) cached(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokens[key]
}

func (p *TokenPool) store(key, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens[key] = token
}

// Invalidate drops the cached LLM token after a 401 or an expiry header.
func (p *TokenPool) Invalidate(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tokens, key)
}

// LLMToken returns a cached LLM token or mints one via
// POST /client/llm_tokens with the account credential and the organization id.
func (p *TokenPool) LLMToken(ctx context.Context, httpClient pluginapi.HostHTTPClient, settings zedconfig.Settings, userID, accessToken, systemID, organizationID, cacheKey string) (string, error) {
	if token := p.cached(cacheKey); token != "" {
		return token, nil
	}
	body, errMarshal := json.Marshal(map[string]string{"organization_id": organizationID})
	if errMarshal != nil {
		return "", fmt.Errorf("encode Zed llm token request: %w", errMarshal)
	}
	headers := make(http.Header)
	headers.Set("Authorization", authorizationHeader(userID, accessToken))
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	if systemID != "" {
		headers.Set(systemIDHeader, systemID)
	}
	response, errDo := doJSON(ctx, httpClient, http.MethodPost, settings.CloudURL+"/client/llm_tokens", headers, body)
	if errDo != nil {
		return "", errDo
	}
	if response.StatusCode != http.StatusOK {
		return "", NewStatusError(response.StatusCode, response.Body)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if errDecode := decodeJSON(response.Body, &payload); errDecode != nil {
		return "", errDecode
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		return "", fmt.Errorf("Zed llm token response carried no token")
	}
	p.store(cacheKey, token)
	return token, nil
}

// CompletionHeaders builds the header set of an authenticated LLM call,
// mirroring authenticated_llm_request in language_models_cloud.rs.
func CompletionHeaders(settings zedconfig.Settings, llmToken string) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+llmToken)
	headers.Set("Content-Type", "application/json")
	headers.Set(versionHeader, settings.ClientVersion)
	headers.Set(userAgentHeader, UserAgent(settings))
	headers.Set("x-zed-client-supports-status-messages", "true")
	headers.Set("x-zed-client-supports-stream-ended-request-completion-status", "true")
	return headers
}

// TokenNeedsRefresh reports whether the response tells the client its LLM
// token is stale, matching needs_llm_token_refresh in language_models_cloud.rs.
func TokenNeedsRefresh(headers http.Header, statusCode int) bool {
	if statusCode == http.StatusUnauthorized {
		return true
	}
	return headers.Get(expiredTokenHeader) != "" || headers.Get(outdatedTokenHeader) != ""
}

// FetchModels calls GET /models with the LLM token.
func FetchModels(ctx context.Context, httpClient pluginapi.HostHTTPClient, settings zedconfig.Settings, llmToken string) (*ListModelsResponse, error) {
	headers := CompletionHeaders(settings, llmToken)
	headers.Set(supportsXAIHeader, "true")
	response, errDo := doJSON(ctx, httpClient, http.MethodGet, settings.CloudURL+"/models", headers, nil)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode != http.StatusOK {
		return nil, NewStatusError(response.StatusCode, response.Body)
	}
	var payload ListModelsResponse
	if errDecode := decodeJSON(response.Body, &payload); errDecode != nil {
		return nil, errDecode
	}
	return &payload, nil
}
