package executor

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// The exact failure reported by Zed: pi sends easy-input message items without
// "type", which OpenAI accepts but Zed's tagged enums reject.
func TestNormalizeZedResponsesRequestEasyInput(t *testing.T) {
	request := []byte(`{
		"model":"gpt-5.6","stream":true,
		"input":[
			{"role":"user","content":"你好"},
			{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"text","text":" there"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"kept as-is"}]},
			{"name":"get_weather","call_id":"call_1","arguments":"{\"city\":\"Tokyo\"}"},
			{"call_id":"call_1","output":"22C"},
			{"id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"enc"}
		],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{}}},{"name":"g","parameters":{}}]
	}`)
	normalized := normalizeZedResponsesRequest(request)
	root := gjson.ParseBytes(normalized)

	items := root.Get("input").Array()
	if len(items) != 6 {
		t.Fatalf("input items = %d", len(items))
	}
	first := items[0]
	if first.Get("type").String() != "message" {
		t.Fatalf("item 0 type = %s, want message", first.Get("type"))
	}
	content := first.Get("content")
	if !content.IsArray() || content.Array()[0].Get("type").String() != "input_text" || content.Array()[0].Get("text").String() != "你好" {
		t.Fatalf("item 0 content = %s", content.Raw)
	}
	second := items[1]
	if second.Get("type").String() != "message" {
		t.Fatalf("item 1 type = %s", second.Get("type"))
	}
	for index, part := range second.Get("content").Array() {
		if part.Get("type").String() != "input_text" {
			t.Fatalf("item 1 part %d type = %s", index, part.Get("type"))
		}
	}
	third := items[2]
	if third.Get("type").String() != "message" || third.Get("content.0.type").String() != "input_text" {
		t.Fatalf("item 2 (already typed) changed: %s", third.Raw)
	}
	if items[3].Get("type").String() != "function_call" {
		t.Fatalf("item 3 type = %s", items[3].Get("type"))
	}
	if items[4].Get("type").String() != "function_call_output" {
		t.Fatalf("item 4 type = %s", items[4].Get("type"))
	}
	if items[5].Get("type").String() != "reasoning" {
		t.Fatalf("item 5 type = %s", items[5].Get("type"))
	}

	tools := root.Get("tools").Array()
	if tools[0].Get("name").String() != "f" || tools[0].Get("type").String() != "function" {
		t.Fatalf("tool 0 = %s", tools[0].Raw)
	}
	if tools[1].Get("type").String() != "function" || tools[1].Get("name").String() != "g" {
		t.Fatalf("tool 1 = %s", tools[1].Raw)
	}
	if gjson.GetBytes(normalized, "input.0.content.0.text").String() != "你好" {
		t.Fatalf("unicode handling broken: %s", normalized)
	}
}

func TestNormalizeZedResponsesRequestImageWrapper(t *testing.T) {
	request := []byte(`{"input":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	normalized := normalizeZedResponsesRequest(request)
	part := gjson.GetBytes(normalized, "input.0.content.0")
	if part.Get("type").String() != "input_image" || part.Get("image_url").String() != "https://example.com/a.png" {
		t.Fatalf("image part = %s", part.Raw)
	}
}

func TestNormalizeZedResponsesRequestLeavesTypedRequestUntouched(t *testing.T) {
	request := []byte(`{"input":[{"type":"function_call","name":"f","call_id":"c","arguments":"{}"}],"tools":[{"type":"custom","name":"x","format":{"type":"text"}}]}`)
	normalized := normalizeZedResponsesRequest(request)
	if gjson.GetBytes(normalized, "input.0.type").String() != "function_call" {
		t.Fatalf("function_call item changed: %s", normalized)
	}
	if gjson.GetBytes(normalized, "tools.0.type").String() != "custom" {
		t.Fatalf("custom tool changed: %s", normalized)
	}
	var before, after map[string]any
	_ = json.Unmarshal(request, &before)
	_ = json.Unmarshal(normalized, &after)
}
