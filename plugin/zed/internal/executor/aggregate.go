package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// aggregator consumes native provider stream events (one wire format) and
// produces the equivalent non-stream response body. Zed's /completions always
// streams, so a non-streaming downstream request needs this aggregation before
// the builtin non-stream response translators run.
type aggregator func(events []json.RawMessage) (json.RawMessage, error)

func aggregatorForFormat(wire string) aggregator {
	switch wire {
	case "claude":
		return aggregateClaude
	case "openai-response":
		return aggregateOpenAIResponses
	case "openai":
		return aggregateOpenAIChat
	case "gemini":
		return aggregateGemini
	}
	return nil
}

// aggregateClaude rebuilds an Anthropic Messages response from its SSE events
// (message_start, content_block_start/delta/stop, message_delta, message_stop).
type claudeBlock struct {
	body   []byte
	text   strings.Builder
	think  strings.Builder
	sig    strings.Builder
	args   strings.Builder
	isJSON bool
}

func aggregateClaude(events []json.RawMessage) (json.RawMessage, error) {
	var message gjson.Result
	blocks := make(map[int64]*claudeBlock)
	var stopReason, stopSequence string
	var outputTokens int64
	var sawStop bool

	for _, event := range events {
		root := gjson.ParseBytes(event)
		switch root.Get("type").String() {
		case "message_start":
			message = root.Get("message")
		case "content_block_start":
			index := root.Get("index").Int()
			block := &claudeBlock{}
			contentBlock := root.Get("content_block")
			block.body = []byte(contentBlock.Raw)
			block.isJSON = contentBlock.Get("type").String() == "tool_use"
			blocks[index] = block
		case "content_block_delta":
			index := root.Get("index").Int()
			block, ok := blocks[index]
			if !ok {
				continue
			}
			delta := root.Get("delta")
			switch delta.Get("type").String() {
			case "text_delta":
				block.text.WriteString(delta.Get("text").String())
			case "thinking_delta":
				block.think.WriteString(delta.Get("thinking").String())
			case "signature_delta":
				block.sig.WriteString(delta.Get("signature").String())
			case "input_json_delta":
				block.args.WriteString(delta.Get("partial_json").String())
			}
		case "message_delta":
			stopReason = root.Get("delta.stop_reason").String()
			stopSequence = root.Get("delta.stop_sequence").String()
			outputTokens = root.Get("usage.output_tokens").Int()
		case "message_stop":
			sawStop = true
		case "error":
			return nil, fmt.Errorf("Zed upstream error: %s", root.Get("error.message").String())
		}
	}
	if !message.Exists() && !sawStop {
		return nil, fmt.Errorf("Zed upstream produced no message events")
	}

	response := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	if message.Exists() {
		for _, field := range []string{"id", "type", "role", "model"} {
			if value := message.Get(field); value.Exists() {
				response, _ = sjson.SetBytes(response, field, value.Value())
			}
		}
		if usage := message.Get("usage"); usage.Exists() {
			response, _ = sjson.SetBytes(response, "usage", usage.Value())
		}
	}
	maxIndex := int64(-1)
	for index := range blocks {
		if index > maxIndex {
			maxIndex = index
		}
	}
	for index := int64(0); index <= maxIndex; index++ {
		block, ok := blocks[index]
		if !ok {
			continue
		}
		blockType := gjson.GetBytes(block.body, "type").String()
		switch blockType {
		case "text":
			response, _ = sjson.SetBytes(response, "content.-1", map[string]any{
				"type": "text",
				"text": block.text.String(),
			})
		case "thinking":
			response, _ = sjson.SetBytes(response, "content.-1", map[string]any{
				"type":      "thinking",
				"thinking":  block.think.String(),
				"signature": block.sig.String(),
			})
		case "redacted_thinking":
			response, _ = sjson.SetBytes(response, "content.-1", json.RawMessage(block.body))
		case "tool_use":
			input := map[string]any{}
			if args := block.args.String(); args != "" {
				if errUnmarshal := json.Unmarshal([]byte(args), &input); errUnmarshal != nil {
					return nil, fmt.Errorf("decode Zed tool_use arguments: %w", errUnmarshal)
				}
			}
			response, _ = sjson.SetBytes(response, "content.-1", map[string]any{
				"type":  "tool_use",
				"id":    gjson.GetBytes(block.body, "id").String(),
				"name":  gjson.GetBytes(block.body, "name").String(),
				"input": input,
			})
		default:
			response, _ = sjson.SetBytes(response, "content.-1", json.RawMessage(block.body))
		}
	}
	if stopReason != "" {
		response, _ = sjson.SetBytes(response, "stop_reason", stopReason)
	}
	if stopSequence != "" {
		response, _ = sjson.SetBytes(response, "stop_sequence", stopSequence)
	}
	if outputTokens > 0 {
		response, _ = sjson.SetBytes(response, "usage.output_tokens", outputTokens)
	}
	return response, nil
}

// aggregateOpenAIResponses prefers the terminal response.completed payload,
// which carries the full response object, and falls back to collecting
// output_item.done items.
func aggregateOpenAIResponses(events []json.RawMessage) (json.RawMessage, error) {
	var completed gjson.Result
	items := make(map[int64]gjson.Result)
	maxIndex := int64(-1)
	var responseID, model string
	usage := gjson.Result{}

	for _, event := range events {
		root := gjson.ParseBytes(event)
		switch root.Get("type").String() {
		case "response.completed":
			completed = root.Get("response")
		case "response.created":
			created := root.Get("response")
			if created.Exists() {
				responseID = created.Get("id").String()
				model = created.Get("model").String()
			}
		case "response.output_item.done":
			index := root.Get("output_index").Int()
			if item := root.Get("item"); item.Exists() {
				items[index] = item
				if index > maxIndex {
					maxIndex = index
				}
			}
		case "response.failed":
			return nil, fmt.Errorf("Zed upstream error: %s", root.Get("response.error.message").String())
		case "error":
			return nil, fmt.Errorf("Zed upstream error: %s", root.Get("message").String())
		}
	}
	if completed.Exists() {
		return json.RawMessage(completed.Raw), nil
	}
	if responseID == "" && maxIndex < 0 {
		return nil, fmt.Errorf("Zed upstream produced no response events")
	}
	response := []byte(`{"id":"","object":"response","created_at":0,"status":"completed","output":[]}`)
	if responseID != "" {
		response, _ = sjson.SetBytes(response, "id", responseID)
	}
	if model != "" {
		response, _ = sjson.SetBytes(response, "model", model)
	}
	if usage.Exists() {
		response, _ = sjson.SetBytes(response, "usage", usage.Value())
	}
	for index := int64(0); index <= maxIndex; index++ {
		if item, ok := items[index]; ok {
			response, _ = sjson.SetBytes(response, "output.-1", json.RawMessage(item.Raw))
		}
	}
	return response, nil
}

// aggregateOpenAIChat folds chat.completion.chunk events into one
// chat.completion response.
func aggregateOpenAIChat(events []json.RawMessage) (json.RawMessage, error) {
	var id, model string
	var created int64
	var role, content, reasoning strings.Builder
	type toolCall struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolCalls := make(map[int64]*toolCall)
	var finishReason string
	var usage gjson.Result

	for _, event := range events {
		root := gjson.ParseBytes(event)
		if root.Get("error").Exists() {
			return nil, fmt.Errorf("Zed upstream error: %s", root.Get("error.message").String())
		}
		if value := root.Get("id"); value.Exists() && value.String() != "" {
			id = value.String()
		}
		if value := root.Get("model"); value.Exists() && value.String() != "" {
			model = value.String()
		}
		if value := root.Get("created"); value.Exists() {
			created = value.Int()
		}
		if value := root.Get("usage"); value.Exists() && value.IsObject() {
			usage = value
		}
		choice := root.Get("choices.0")
		if !choice.Exists() {
			continue
		}
		if value := choice.Get("finish_reason"); value.Exists() && value.String() != "" {
			finishReason = value.String()
		}
		delta := choice.Get("delta")
		if !delta.Exists() {
			continue
		}
		if value := delta.Get("role"); value.Exists() && value.String() != "" {
			role.WriteString(value.String())
		}
		if value := delta.Get("content"); value.Exists() {
			content.WriteString(value.String())
		}
		if value := delta.Get("reasoning_content"); value.Exists() {
			reasoning.WriteString(value.String())
		}
		for _, call := range delta.Get("tool_calls").Array() {
			index := call.Get("index").Int()
			entry, ok := toolCalls[index]
			if !ok {
				entry = &toolCall{}
				toolCalls[index] = entry
			}
			if value := call.Get("id"); value.Exists() && value.String() != "" {
				entry.id = value.String()
			}
			if value := call.Get("function.name"); value.Exists() && value.String() != "" {
				entry.name = value.String()
			}
			if value := call.Get("function.arguments"); value.Exists() {
				entry.arguments.WriteString(value.String())
			}
		}
	}
	if id == "" && finishReason == "" && content.Len() == 0 && len(toolCalls) == 0 {
		return nil, fmt.Errorf("Zed upstream produced no chat completion events")
	}

	response := []byte(`{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":null}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	if id != "" {
		response, _ = sjson.SetBytes(response, "id", id)
	}
	if created > 0 {
		response, _ = sjson.SetBytes(response, "created", created)
	}
	if model != "" {
		response, _ = sjson.SetBytes(response, "model", model)
	}
	if role.Len() > 0 {
		response, _ = sjson.SetBytes(response, "choices.0.message.role", role.String())
	}
	if content.Len() > 0 {
		response, _ = sjson.SetBytes(response, "choices.0.message.content", content.String())
	}
	if reasoning.Len() > 0 {
		response, _ = sjson.SetBytes(response, "choices.0.message.reasoning_content", reasoning.String())
	}
	maxToolIndex := int64(-1)
	for index := range toolCalls {
		if index > maxToolIndex {
			maxToolIndex = index
		}
	}
	for index := int64(0); index <= maxToolIndex; index++ {
		call, ok := toolCalls[index]
		if !ok {
			continue
		}
		entry := map[string]any{
			"id":   call.id,
			"type": "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.arguments.String(),
			},
		}
		response, _ = sjson.SetBytes(response, fmt.Sprintf("choices.0.message.tool_calls.%d", index), entry)
	}
	if finishReason != "" {
		response, _ = sjson.SetBytes(response, "choices.0.finish_reason", finishReason)
	}
	if usage.Exists() {
		response, _ = sjson.SetBytes(response, "usage", usage.Value())
	}
	return response, nil
}

// aggregateGemini folds streamGenerateContent chunks into one
// GenerateContentResponse.
func aggregateGemini(events []json.RawMessage) (json.RawMessage, error) {
	var parts []any
	var role string
	var finishReason string
	var usage gjson.Result
	var modelVersion string

	for _, event := range events {
		root := gjson.ParseBytes(event)
		if value := root.Get("modelVersion"); value.Exists() && value.String() != "" {
			modelVersion = value.String()
		}
		if value := root.Get("usageMetadata"); value.Exists() {
			usage = value
		}
		candidate := root.Get("candidates.0")
		if !candidate.Exists() {
			continue
		}
		if value := candidate.Get("role"); value.Exists() && value.String() != "" {
			role = value.String()
		}
		if value := candidate.Get("finishReason"); value.Exists() && value.String() != "" {
			finishReason = value.String()
		}
		for _, part := range candidate.Get("content.parts").Array() {
			parts = append(parts, part.Value())
		}
	}
	if len(parts) == 0 && finishReason == "" {
		return nil, fmt.Errorf("Zed upstream produced no generateContent events")
	}

	response := []byte(`{"candidates":[{"content":{"parts":[],"role":"user"},"index":0}]}`)
	if role != "" {
		response, _ = sjson.SetBytes(response, "candidates.0.content.role", role)
	}
	if len(parts) > 0 {
		response, _ = sjson.SetBytes(response, "candidates.0.content.parts", parts)
	}
	if finishReason != "" {
		response, _ = sjson.SetBytes(response, "candidates.0.finishReason", finishReason)
	}
	if modelVersion != "" {
		response, _ = sjson.SetBytes(response, "modelVersion", modelVersion)
	}
	if usage.Exists() {
		response, _ = sjson.SetBytes(response, "usageMetadata", usage.Value())
	}
	return response, nil
}
