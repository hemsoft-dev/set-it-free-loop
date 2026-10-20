//go:build linux

package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
)

func TestTelemetryCannotCountAsAllowedMetadata(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		b := &codexBackend{logHTTP: func(any) {}}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest(method, "/backend-api/codex/analytics-events/events", nil))
		if w.Code != 400 || b.telemetry != 1 || b.failed != 1 || b.metadata != 0 || b.calls != 0 {
			t.Fatal("telemetry accepted or hidden in metadata", method, w.Code, b)
		}
	}
}

// Sanitized output fragments produced by pinned Codex against the fake backend.
func TestCapturedNativeCodeModeSourceResults(t *testing.T) {
	data, err := os.ReadFile("testdata/native-source-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured []struct {
		ID     string `json:"id"`
		Output any    `json:"output"`
	}
	if json.Unmarshal(data, &captured) != nil || len(captured) != 2 {
		t.Fatal("invalid native fixture")
	}
	for _, item := range captured {
		if !verifiedSourceOutput(item.Output, item.ID) {
			t.Fatal("actual source output rejected", item.ID)
		}
	}
	for _, kind := range []string{"swapped", "error", "duplicate", "wrong-revision"} {
		t.Run(kind, func(t *testing.T) {
			var values []struct {
				ID     string `json:"id"`
				Output any    `json:"output"`
			}
			_ = json.Unmarshal(data, &values)
			output := values[1].Output.([]any)
			if kind == "swapped" {
				output = values[0].Output.([]any)
			} else {
				index := len(output) - 1
				part := output[index].(map[string]any)
				var wrapper map[string]any
				_ = json.Unmarshal([]byte(part["text"].(string)), &wrapper)
				switch kind {
				case "error":
					wrapper["isError"] = true
				case "duplicate":
					output = append(output, part)
				case "wrong-revision":
					wrapper["content"].([]any)[0].(map[string]any)["text"] = `[{"path":"source.go","line":1,"quote":"immutable source evidence","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`
				}
				encoded, _ := json.Marshal(wrapper)
				part["text"] = string(encoded)
			}
			if verifiedSourceOutput(output, "fixture_search") {
				t.Fatal("mismatched result accepted", kind)
			}
		})
	}
}
func TestSourceOutputIDMustBeOneTopLevelNativeItem(t *testing.T) {
	item := map[string]any{"type": "custom_tool_call_output", "call_id": "fixture_read", "output": "fixture"}
	if _, ok := outputFor(map[string]any{"input": []any{item}}, "fixture_read"); !ok {
		t.Fatal("native output not found")
	}
	if _, ok := outputFor(map[string]any{"input": []any{item, item}}, "fixture_read"); ok {
		t.Fatal("duplicate ID accepted")
	}
	if _, ok := outputFor(map[string]any{"input": []any{map[string]any{"type": "message", "content": []any{item}}}}, "fixture_read"); ok {
		t.Fatal("nested imitation accepted")
	}
}
