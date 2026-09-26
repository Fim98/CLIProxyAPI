package executor

import (
	"encoding/json"
	"testing"
)

func TestClassifyZedLineEvent(t *testing.T) {
	line := []byte(`{"event":{"type":"message_start","message":{"id":"msg_1"}}}`)
	event, failed, ended, ok := classifyZedLine(line)
	if !ok || failed != nil || ended {
		t.Fatalf("classifyZedLine() ok=%v failed=%v ended=%v, want event", ok, failed, ended)
	}
	var payload struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(event, &payload); errUnmarshal != nil || payload.Type != "message_start" {
		t.Fatalf("event payload = %s, want message_start", event)
	}
}

func TestClassifyZedLineStatuses(t *testing.T) {
	if _, failed, ended, ok := classifyZedLine([]byte(`{"status":"queued"}`)); !ok || failed != nil || ended {
		t.Fatalf("queued: ok=%v failed=%v ended=%v", ok, failed, ended)
	}
	if _, failed, ended, ok := classifyZedLine([]byte(`{"status":"started"}`)); !ok || failed != nil || ended {
		t.Fatalf("started: ok=%v failed=%v ended=%v", ok, failed, ended)
	}
	if _, failed, ended, ok := classifyZedLine([]byte(`{"status":"stream_ended"}`)); !ok || failed != nil || !ended {
		t.Fatalf("stream_ended: ok=%v failed=%v ended=%v", ok, failed, ended)
	}
	_, failed, _, ok := classifyZedLine([]byte(`{"status":{"failed":{"code":"upstream_http_503","message":"overloaded","request_id":"abc"}}}`))
	if !ok || failed == nil || failed.Message != "overloaded" {
		t.Fatalf("failed status: ok=%v failed=%+v", ok, failed)
	}
}

func TestClassifyZedLineRejectsGarbage(t *testing.T) {
	if _, _, _, ok := classifyZedLine([]byte(`not json`)); ok {
		t.Fatal("classifyZedLine() accepted invalid JSON")
	}
}

func TestAggregateClaude(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`),
		json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`),
		json.RawMessage(`{"type":"content_block_stop","index":0}`),
		json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}`),
		json.RawMessage(`{"type":"message_stop"}`),
	}
	payload, errAggregate := aggregateClaude(events)
	if errAggregate != nil {
		t.Fatalf("aggregateClaude() error = %v", errAggregate)
	}
	root := jsonUnmarshal(t, payload)
	if root["id"] != "msg_1" || root["model"] != "claude-haiku-4-5" {
		t.Fatalf("message metadata = %v", root)
	}
	if root["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", root["stop_reason"])
	}
	usage, _ := root["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 7 {
		t.Fatalf("usage = %v", usage)
	}
	content, _ := root["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content blocks = %d", len(content))
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "Hello world" {
		t.Fatalf("text block = %v", first)
	}
}

func TestAggregateClaudeToolUse(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-4-5","usage":{"input_tokens":5,"output_tokens":1}}}`),
		json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`),
		json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Tokyo\"}"}}`),
		json.RawMessage(`{"type":"content_block_stop","index":0}`),
		json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`),
		json.RawMessage(`{"type":"message_stop"}`),
	}
	payload, errAggregate := aggregateClaude(events)
	if errAggregate != nil {
		t.Fatalf("aggregateClaude() error = %v", errAggregate)
	}
	root := jsonUnmarshal(t, payload)
	content, _ := root["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content blocks = %d", len(content))
	}
	tool, _ := content[0].(map[string]any)
	input, _ := tool["input"].(map[string]any)
	if tool["name"] != "get_weather" || input["city"] != "Tokyo" {
		t.Fatalf("tool block = %v", tool)
	}
}

func TestAggregateOpenAIChat(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"grok-4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`),
		json.RawMessage(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"grok-4","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`),
		json.RawMessage(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"grok-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`),
	}
	payload, errAggregate := aggregateOpenAIChat(events)
	if errAggregate != nil {
		t.Fatalf("aggregateOpenAIChat() error = %v", errAggregate)
	}
	root := jsonUnmarshal(t, payload)
	if root["object"] != "chat.completion" || root["model"] != "grok-4" {
		t.Fatalf("response metadata = %v", root)
	}
	choice, _ := root["choices"].([]any)
	first, _ := choice[0].(map[string]any)
	message, _ := first["message"].(map[string]any)
	if message["content"] != "Hi" || first["finish_reason"] != "stop" {
		t.Fatalf("choice = %v", first)
	}
}

func TestAggregateOpenAIResponses(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-luna"}}`),
		json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6-luna","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`),
	}
	payload, errAggregate := aggregateOpenAIResponses(events)
	if errAggregate != nil {
		t.Fatalf("aggregateOpenAIResponses() error = %v", errAggregate)
	}
	root := jsonUnmarshal(t, payload)
	if root["id"] != "resp_1" || root["status"] != "completed" {
		t.Fatalf("completed response = %v", root)
	}
}

func TestAggregateGemini(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"candidates":[{"content":{"parts":[{"text":"he"},{"text":"llo"}],"role":"model"},"index":0}]}`),
		json.RawMessage(`{"candidates":[{"content":{"parts":[],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2},"modelVersion":"gemini-3-pro"}`),
	}
	payload, errAggregate := aggregateGemini(events)
	if errAggregate != nil {
		t.Fatalf("aggregateGemini() error = %v", errAggregate)
	}
	root := jsonUnmarshal(t, payload)
	candidates, _ := root["candidates"].([]any)
	candidate, _ := candidates[0].(map[string]any)
	if candidate["finishReason"] != "STOP" {
		t.Fatalf("candidate = %v", candidate)
	}
}

func jsonUnmarshal(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if errUnmarshal := json.Unmarshal(payload, &root); errUnmarshal != nil {
		t.Fatalf("unmarshal %s: %v", payload, errUnmarshal)
	}
	return root
}
