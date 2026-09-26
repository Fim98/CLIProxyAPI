package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// zedCompletionEvent is one line of the /completions NDJSON stream:
// CompletionEvent<T> in crates/cloud_llm_client/src/cloud_llm_client.rs, i.e.
// {"event": {…native provider event…}} or {"status": …}. Unit status variants
// serialize as bare strings ("queued", "started", "stream_ended") while
// payload variants serialize as objects ({"failed": {...}}).
type zedCompletionEvent struct {
	Event  json.RawMessage `json:"event,omitempty"`
	Status json.RawMessage `json:"status,omitempty"`
}

// statusFailed carries the failed status payload.
type statusFailed struct {
	Code       string  `json:"code"`
	Message    string  `json:"message"`
	RequestID  string  `json:"request_id"`
	RetryAfter float64 `json:"retry_after"`
}

// readNDJSONLines splits the host's stream chunks into NDJSON lines and feeds
// each to handleLine until the stream ends, the context is cancelled, or
// handleLine returns false.
func readNDJSONLines(ctx context.Context, input <-chan pluginapi.HTTPStreamChunk, maxLineBytes int, handleLine func(line []byte) bool) error {
	var pending []byte
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-input:
			if !ok {
				if len(pending) > 0 {
					handleLine(pending)
				}
				return nil
			}
			if chunk.Err != nil {
				return chunk.Err
			}
			pending = append(pending, chunk.Payload...)
			if bytes.IndexByte(pending, '\n') < 0 && len(pending) > maxLineBytes {
				return fmt.Errorf("Zed stream line exceeds %d bytes", maxLineBytes)
			}
			for {
				index := bytes.IndexByte(pending, '\n')
				if index < 0 {
					break
				}
				line := bytes.TrimSuffix(pending[:index], []byte("\r"))
				pending = pending[index+1:]
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				if !handleLine(line) {
					return nil
				}
			}
		}
	}
}

// classifyZedLine parses one NDJSON line. It returns:
//   - ok=false for a line that is not a valid completion event (caller decides),
//   - event payload when the line carries one,
//   - a status error when the line reports a failed status,
//   - ended=true for stream_ended.
func classifyZedLine(line []byte) (event json.RawMessage, failed *statusFailed, ended bool, ok bool) {
	var parsed zedCompletionEvent
	if errUnmarshal := json.Unmarshal(line, &parsed); errUnmarshal != nil {
		return nil, nil, false, false
	}
	if len(parsed.Event) > 0 {
		return parsed.Event, nil, false, true
	}
	if len(parsed.Status) > 0 {
		status := bytes.TrimSpace(parsed.Status)
		if status[0] == '"' {
			var name string
			if errUnmarshal := json.Unmarshal(status, &name); errUnmarshal == nil {
				switch name {
				case "stream_ended":
					return nil, nil, true, true
				case "queued", "started":
					return nil, nil, false, true
				}
			}
			return nil, nil, false, false
		}
		if status[0] == '{' {
			var fields map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(status, &fields); errUnmarshal == nil {
				if rawFailed, hasFailed := fields["failed"]; hasFailed {
					var failure statusFailed
					if errUnmarshal := json.Unmarshal(rawFailed, &failure); errUnmarshal == nil && failure.Message != "" {
						return nil, &failure, false, true
					}
					return nil, &statusFailed{Code: "failed", Message: string(rawFailed)}, false, true
				}
				if _, hasEnded := fields["stream_ended"]; hasEnded {
					return nil, nil, true, true
				}
				if _, hasQueued := fields["queued"]; hasQueued {
					return nil, nil, false, true
				}
				if _, hasStarted := fields["started"]; hasStarted {
					return nil, nil, false, true
				}
			}
		}
		return nil, nil, false, false
	}
	return nil, nil, false, false
}
