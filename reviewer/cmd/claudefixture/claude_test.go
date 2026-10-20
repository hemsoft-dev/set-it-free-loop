//go:build linux

package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestNativeSourceResultsRemainDistinctAndNonError(t *testing.T) {
	data, err := os.ReadFile("testdata/native-tool-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured []map[string]any
	if json.Unmarshal(data, &captured) != nil || len(captured) != 2 {
		t.Fatal("invalid captured fixture")
	}
	wrap := func(result map[string]any) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{result}}}}
	}
	for _, result := range captured {
		if !claudeResult(wrap(result), result["tool_use_id"].(string)) {
			t.Fatal("actual native source result rejected")
		}
	}
	for _, kind := range []string{"read-as-search", "search-as-read", "error", "wrong-error-type", "wrong-revision", "duplicate", "wrong-role"} {
		t.Run(kind, func(t *testing.T) {
			var values []map[string]any
			_ = json.Unmarshal(data, &values)
			result := values[1]
			switch kind {
			case "read-as-search":
				result["content"] = values[0]["content"]
			case "search-as-read":
				result["tool_use_id"] = "sfl_fixture_read_1"
			case "error":
				result["is_error"] = true
			case "wrong-error-type":
				result["is_error"] = "false"
			case "wrong-revision":
				blocks := result["content"].([]any)
				block := blocks[0].(map[string]any)
				block["text"] = `[{"path":"source.go","line":1,"quote":"immutable source evidence","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`
			}
			body := wrap(result)
			if kind == "duplicate" {
				body["messages"].([]any)[0].(map[string]any)["content"] = []any{result, result}
			}
			if kind == "wrong-role" {
				body["messages"].([]any)[0].(map[string]any)["role"] = "assistant"
			}
			if claudeResult(body, result["tool_use_id"].(string)) {
				t.Fatal("mismatched native result accepted")
			}
		})
	}
}
