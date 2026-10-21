//go:build linux

// Derived from the pinned native protocol fixture backend. No transcript logging.
package nativefixture

import (
	"encoding/json"
	"fmt"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"io"
	"net/http"
	"strings"
	"sync"
)

type claudeBackend struct {
	output      any
	mode        string
	mu          sync.Mutex
	s           r.Session
	calls       int
	failures    int
	requests    int
	accessToken string
}

func (b *claudeBackend) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests++
	raw, readErr := io.ReadAll(io.LimitReader(q.Body, (2<<20)+1))
	_ = q.Body.Close()
	headers := []string{}
	for h := range q.Header {
		headers = append(headers, h)
	}

	if readErr != nil || len(raw) > 2<<20 {
		b.failures++
		http.Error(w, "request cap", 413)
		return
	}
	accessToken := b.accessToken
	if accessToken == "" {
		accessToken = "sfl-fake-access"
	}
	if q.Method != "POST" || q.URL.Path != "/v1/messages" || q.URL.RawQuery != "" && q.URL.RawQuery != "beta=true" || q.Header.Get("Authorization") != "Bearer "+accessToken || q.Header.Get("X-Api-Key") != "" {
		b.failures++
		http.Error(w, "unexpected fixture route", 404)
		return
	}
	b.calls++
	if b.calls > 6 {
		b.failures++
		http.Error(w, "fixture request cap", 429)
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || body["model"] != b.s.Identity.Model {
		b.failures++
		http.Error(w, "invalid fixture request", 400)
		return
	}
	name := ""
	structured := false
	search := false
	inventory := 0
	if list, ok := body["tools"].([]any); ok {
		for _, v := range list {
			if t, ok := v.(map[string]any); ok {
				n, _ := t["name"].(string)
				inventory++
				if n == "mcp__sfl-source__search" {
					search = true
				}
				if n == "mcp__sfl-source__read" {
					name = n
				}
				if n == "StructuredOutput" {
					structured = true
				}
			}
		}
	}
	if name == "" || !structured || !search || inventory != 3 {
		b.failures++

		http.Error(w, "source/structured tools missing", 500)
		return
	}
	if b.calls == 2 && !claudeResult(body, "sfl_fixture_read_1") || b.calls == 3 && !claudeResult(body, "sfl_fixture_search_1") {
		b.failures++

		http.Error(w, "missing actual source result", 500)
		return
	}
	toolID := "sfl_fixture_read_1"
	var input any = map[string]string{"revision": head, "path": "source.go"}
	if b.calls == 2 {
		name = "mcp__sfl-source__search"
		toolID = "sfl_fixture_search_1"
		input = map[string]any{"revision": head, "needle": "immutable", "max_results": 5}
	}
	if b.calls > 2 {
		name = "StructuredOutput"
		toolID = "sfl_fixture_report_1"
		input = sourceReport(b.s)
		if b.output != nil {
			input = b.output
		}
	}
	if b.mode == "guarded-bash" {
		name = "Bash"
		toolID = "sfl_fixture_forbidden_1"
		input = map[string]string{"command": "printf forbidden > /work/undeclared-canary.txt"}
	}
	encoded, _ := json.Marshal(input)
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(kind string, v map[string]any) {
		v["type"] = kind
		data, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	send("message_start", map[string]any{"message": map[string]any{"id": fmt.Sprintf("msg_fixture_%d", b.calls), "type": "message", "role": "assistant", "model": b.s.Identity.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
	send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": toolID, "name": name, "input": map[string]any{}}})
	send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}})
	send("content_block_stop", map[string]any{"index": 0})
	send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	send("message_stop", map[string]any{})
}
func claudeResult(value any, id string) bool {
	body, ok := value.(map[string]any)
	if !ok {
		return false
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return false
	}
	count := 0
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, value := range content {
			result, ok := value.(map[string]any)
			if !ok || result["type"] != "tool_result" || result["tool_use_id"] != id {
				continue
			}
			count++
			if failed, present := result["is_error"]; present && failed != false {
				return false
			}
			blocks, ok := result["content"].([]any)
			if !ok || len(blocks) != 1 {
				return false
			}
			text, ok := blocks[0].(map[string]any)
			if !ok || text["type"] != "text" {
				return false
			}
			data, ok := text["text"].(string)
			if !ok {
				return false
			}
			switch id {
			case "sfl_fixture_read_1":
				if data != source {
					return false
				}
			case "sfl_fixture_search_1":
				matches, err := r.DecodeStrict[[]r.Anchor]([]byte(data), 1<<16)
				if err != nil || len(matches) != 1 || matches[0] != (r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}) {
					return false
				}
			default:
				return false
			}
		}
	}
	return count == 1
}
