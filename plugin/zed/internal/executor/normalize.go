package executor

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Zed's cloud parses the OpenAI Responses provider request with its own serde
// structs (crates/open_ai/src/responses.rs), where every input item and every
// content part is an internally-tagged enum that REQUIRES a `type` field. The
// real OpenAI API is more forgiving: "easy input" message items omit `type`,
// content parts may arrive as plain strings, and tool calls infer their kind.
// Clients that speak the lax dialect fail Zed's parse with
// "missing field `type`", so the executor normalizes the request to the strict
// shape before forwarding.
func normalizeZedResponsesRequest(requestBody []byte) []byte {
	root := gjson.ParseBytes(requestBody)
	if !root.IsObject() {
		return requestBody
	}
	updated := string(requestBody)

	// Input items: add the missing item tag and normalize message content
	// parts. "type" "text" is the output-side spelling; Zed's input enum only
	// knows "input_text".
	if input := root.Get("input"); input.IsArray() {
		items := make([]json.RawMessage, 0, len(input.Array()))
		for _, item := range input.Array() {
			items = append(items, normalizeResponsesItem(item))
		}
		encoded, errMarshal := json.Marshal(items)
		if errMarshal == nil {
			updated = sjsonSetRaw(updated, "input", string(encoded))
		}
	}

	// Tool definitions are tagged too; a chat-style nested tool
	// {"type":"function","function":{...}} is flattened to the Responses shape.
	if tools := root.Get("tools"); tools.IsArray() {
		normalized := make([]json.RawMessage, 0, len(tools.Array()))
		for _, tool := range tools.Array() {
			normalized = append(normalized, normalizeResponsesTool(tool))
		}
		encoded, errMarshal := json.Marshal(normalized)
		if errMarshal == nil {
			updated = sjsonSetRaw(updated, "tools", string(encoded))
		}
	}
	return []byte(updated)
}

func normalizeResponsesTool(tool gjson.Result) json.RawMessage {
	if !tool.IsObject() {
		return json.RawMessage(tool.Raw)
	}
	if nested := tool.Get("function"); nested.IsObject() {
		flat := map[string]any{"type": "function"}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value := nested.Get(key); value.Exists() {
				flat[key] = json.RawMessage(value.Raw)
			}
		}
		encoded, errMarshal := json.Marshal(flat)
		if errMarshal == nil {
			return encoded
		}
	}
	if !tool.Get("type").Exists() && tool.Get("parameters").Exists() {
		return json.RawMessage(sjsonSet(tool.Raw, "type", "function"))
	}
	return json.RawMessage(tool.Raw)
}

func normalizeResponsesItem(item gjson.Result) json.RawMessage {
	if !item.IsObject() {
		return json.RawMessage(item.Raw)
	}
	if itemType := item.Get("type"); itemType.Exists() {
		switch itemType.String() {
		case "message":
			return json.RawMessage(normalizeResponsesMessageContent(item.Raw))
		case "text":
			// Output-style content part sent as an item; treat as a message.
			fixed := sjsonSet(item.Raw, "type", "message")
			return json.RawMessage(normalizeResponsesMessageContent(fixed))
		}
		return json.RawMessage(item.Raw)
	}

	switch {
	case item.Get("role").Exists():
		normalized := sjsonSet(item.Raw, "type", "message")
		return json.RawMessage(normalizeResponsesMessageContent(normalized))
	case item.Get("arguments").Exists() && item.Get("name").Exists():
		return json.RawMessage(sjsonSet(item.Raw, "type", "function_call"))
	case item.Get("call_id").Exists() && item.Get("output").Exists():
		return json.RawMessage(sjsonSet(item.Raw, "type", "function_call_output"))
	case item.Get("id").Exists() && (item.Get("summary").Exists() || item.Get("encrypted_content").Exists()):
		return json.RawMessage(sjsonSet(item.Raw, "type", "reasoning"))
	default:
		// Unrecognized shape; leave it for Zed to reject with a precise error.
		return json.RawMessage(item.Raw)
	}
}

// normalizeResponsesMessageContent rewrites a message item's content into the
// typed array Zed's ResponseInputContent enum expects.
func normalizeResponsesMessageContent(raw string) string {
	content := gjson.Get(raw, "content")
	switch {
	case content.Type == gjson.String:
		part := []map[string]any{{"type": "input_text", "text": content.String()}}
		encoded, errMarshal := json.Marshal(part)
		if errMarshal != nil {
			return raw
		}
		return sjsonSetRaw(raw, "content", string(encoded))
	case content.IsArray():
		parts := make([]json.RawMessage, 0, len(content.Array()))
		for _, part := range content.Array() {
			parts = append(parts, normalizeContentPart(part))
		}
		encoded, errMarshal := json.Marshal(parts)
		if errMarshal != nil {
			return raw
		}
		return sjsonSetRaw(raw, "content", string(encoded))
	}
	return raw
}

func normalizeContentPart(part gjson.Result) json.RawMessage {
	if !part.IsObject() {
		encoded, errMarshal := json.Marshal(map[string]any{"type": "input_text", "text": part.String()})
		if errMarshal == nil {
			return encoded
		}
		return json.RawMessage(part.Raw)
	}
	switch part.Get("type").String() {
	case "", "text":
		text := part.Get("text").String()
		encoded, errMarshal := json.Marshal(map[string]any{"type": "input_text", "text": text})
		if errMarshal == nil {
			return encoded
		}
	case "input_text", "input_image", "output_text", "refusal":
		return json.RawMessage(part.Raw)
	case "image_url":
		encoded, errMarshal := json.Marshal(map[string]any{
			"type": "input_image", "image_url": imageURLRef(part),
		})
		if errMarshal == nil {
			return encoded
		}
	}
	return json.RawMessage(part.Raw)
}

// imageURLRef extracts the URL string from either a bare string or the
// chat-style {"image_url": {"url": ...}} wrapper.
func imageURLRef(part gjson.Result) string {
	value := part.Get("image_url")
	if value.Type == gjson.String {
		return value.String()
	}
	return value.Get("url").String()
}

func sjsonSet(raw, path string, value any) string {
	updated, errSet := sjson.Set(raw, path, value)
	if errSet != nil {
		return raw
	}
	return updated
}

func sjsonSetRaw(raw, path, value string) string {
	updated, errSet := sjson.SetRaw(raw, path, value)
	if errSet != nil {
		return raw
	}
	return updated
}
