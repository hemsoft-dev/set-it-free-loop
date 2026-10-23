package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responseEvent(kind string, value map[string]any, explicit bool) string {
	value["type"] = kind
	raw, _ := json.Marshal(value)
	prefix := ""
	if explicit {
		prefix = "event: " + kind + "\n"
	}
	return prefix + "data: " + string(raw) + "\n\n"
}
func codexToolStream(name string) string {
	item := map[string]any{"type": "custom_tool_call", "namespace": "functions", "name": name, "call_id": "fixture_call", "input": "text(await tools.mcp__sfl_source__read({revision: \"synthetic\",path: \"source.go\"}));"}
	return responseEvent("response.output_item.done", map[string]any{"item": item}, false) + responseEvent("response.completed", map[string]any{"response": map[string]any{"id": "response_fixture"}}, false)
}
func claudeToolStream(name string) string {
	return responseEvent("message_start", map[string]any{"message": map[string]any{"type": "message", "role": "assistant", "content": []any{}}}, true) +
		responseEvent("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call_fixture", "name": name, "input": map[string]any{}}}, true) +
		responseEvent("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"source.go"}`}}, true) +
		responseEvent("content_block_stop", map[string]any{"index": 0}, true) + responseEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "tool_use"}}, true) + responseEvent("message_stop", map[string]any{}, true)
}
func TestProviderResponseAllowsCapturedToolsAndOrdinaryText(t *testing.T) {
	for _, s := range []string{codexToolStream("exec"), strings.ReplaceAll(codexToolStream("exec"), "\n", "\r\n"), codexToolStream("exec") + "data: [DONE]\n\n"} {
		if err := validateProviderResponse(Codex, CodexSourceResponses, "text/event-stream", 200, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"mcp__sfl-source__read", "mcp__sfl-source__search", "StructuredOutput", "EndConversation"} {
		if err := validateProviderResponse(Claude, ClaudeSourceMessages, "text/event-stream; charset=utf-8", 200, []byte(claudeToolStream(name))); err != nil {
			t.Fatal(name, err)
		}
	}
	message := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": `Ordinary quoted source: {"type":"custom_tool_call","name":"apply_patch"}`}}}
	stream := responseEvent("response.output_item.done", map[string]any{"item": message}, false) + responseEvent("response.completed", map[string]any{"response": map[string]any{}}, false)
	if err := validateProviderResponse(Codex, CodexSourceResponses, "text/event-stream", 200, []byte(stream)); err != nil {
		t.Fatal("ordinary source text misclassified as a command", err)
	}
}
func TestProviderResponseRejectsHiddenNativeHandlersAndAmbiguousStreams(t *testing.T) {
	positive := codexToolStream("exec")
	for name, stream := range map[string]string{
		"hidden-apply-patch":   codexToolStream("apply_patch"),
		"namespace":            strings.Replace(positive, `"namespace":"functions"`, `"namespace":"clock"`, 1),
		"duplicate-key":        strings.Replace(positive, `"name":"exec"`, `"name":"exec","name":"apply_patch"`, 1),
		"event-mismatch":       "event: response.completed\n" + positive,
		"missing-terminal":     strings.Split(positive, "data: {\"response\"")[0],
		"truncated-frame":      strings.TrimSuffix(positive, "\n"),
		"after-terminal":       positive + codexToolStream("apply_patch"),
		"unknown-event":        responseEvent("response.unknown_tool_event", map[string]any{}, false) + positive,
		"native-function-call": strings.Replace(positive, `"type":"custom_tool_call"`, `"type":"function_call"`, 1),
		"nested-hidden":        responseEvent("response.completed", map[string]any{"response": map[string]any{"output": []any{map[string]any{"type": "custom_tool_call", "namespace": "functions", "name": "apply_patch", "call_id": "nested", "input": "forbidden"}}}}, false),
		"bare-cr-comment":      ": ignored\r" + codexToolStream("apply_patch") + positive,
		"retry-field":          "retry: 10\n" + positive,
		"duplicate-event":      "event: response.output_item.done\nevent: response.output_item.done\n" + positive,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateProviderResponse(Codex, CodexSourceResponses, "text/event-stream", 200, []byte(stream)); err == nil {
				t.Fatal("hostile stream accepted")
			}
		})
	}
	for _, name := range []string{"Bash", "Read", "WebFetch", "mcp__other__read"} {
		if err := validateProviderResponse(Claude, ClaudeSourceMessages, "text/event-stream", 200, []byte(claudeToolStream(name))); err == nil {
			t.Fatal("hidden Claude handler accepted", name)
		}
	}
}
func TestProviderResponseClaudeRejectsBlockAndArgumentAmbiguities(t *testing.T) {
	valid := claudeToolStream("mcp__sfl-source__read")
	for name, stream := range map[string]string{
		"unfinished-block":    strings.ReplaceAll(valid, "event: content_block_stop\ndata: {\"index\":0,\"type\":\"content_block_stop\"}\n\n", ""),
		"wrong-index":         strings.Replace(valid, `"index":0,"type":"content_block_stop"`, `"index":1,"type":"content_block_stop"`, 1),
		"duplicate-arguments": strings.Replace(valid, `{\"path\":\"source.go\"}`, `{\"path\":\"source.go\",\"path\":\"other\"}`, 1),
		"malformed-arguments": strings.Replace(valid, `{\"path\":\"source.go\"}`, `{`, 1),
		"server-tool":         strings.Replace(valid, `"type":"tool_use"`, `"type":"server_tool_use"`, 1),
		"after-stop":          valid + responseEvent("content_block_start", map[string]any{"index": 1, "content_block": map[string]any{"type": "tool_use", "id": "late", "name": "Bash", "input": map[string]any{}}}, true),
	} {
		t.Run(name, func(t *testing.T) {
			if stream == valid {
				t.Fatal("mutation did not change stream")
			}
			if err := validateProviderResponse(Claude, ClaudeSourceMessages, "text/event-stream", 200, []byte(stream)); err == nil {
				t.Fatal("hostile block accepted")
			}
		})
	}
}
func TestProviderResponseNonstreamedJSONHasNoToolEscape(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		policy := CodexSourceResponses
		good := `{"object":"response","output":[]}`
		bad := `{"object":"response","output":[{"type":"custom_tool_call","namespace":"functions","name":"apply_patch","call_id":"bad","input":"bad"}]}`
		if provider == Claude {
			policy = ClaudeSourceMessages
			good = `{"type":"message","role":"assistant","content":[]}`
			bad = `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"bad","name":"Bash","input":{}}]}`
		}
		if err := validateProviderResponse(provider, policy, "application/json", 200, []byte(good)); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderResponse(provider, policy, "application/json", 200, []byte(bad)); err == nil {
			t.Fatal("nonstreamed tool escape")
		}
		if err := validateProviderResponse(provider, policy, "application/json", 429, []byte(`{"error":{"type":"rate_limit_error","message":"synthetic quota"}}`)); err != nil {
			t.Fatal("ordinary HTTP error lost", err)
		}
	}
}
func TestProviderEgressPolicyDenialExposesNoBufferedToolBytes(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			session := testSession(provider)
			policy := egressTestPolicy(provider)
			output := codexToolStream("exec") + codexToolStream("apply_patch")
			policy.ResponsePolicy = CodexSourceResponses
			if provider == Claude {
				policy.ResponsePolicy = ClaudeSourceMessages
				output = claudeToolStream("Bash")
			}
			guard, err := NewProviderEgress(context.Background(), session, policy, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(output)), ContentLength: int64(len(output))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			q := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
			q.Host = "provider.example"
			guard.ServeHTTP(recorder, q)
			if recorder.Code != 400 || strings.Contains(recorder.Body.String(), "custom_tool_call") || strings.Contains(recorder.Body.String(), "Bash") || strings.Contains(recorder.Body.String(), "exec") {
				t.Fatal("tool bytes leaked before late denial", recorder.Code, recorder.Body.String())
			}
			audit, err := guard.Close(context.Background())
			if err != nil || !audit.Stopped || !audit.Failed || audit.Requests != 1 || audit.Forwarded != 1 || audit.Responses != 0 || audit.ResponseBytes != len(output) || audit.ResponseDenials != 1 {
				t.Fatal("denied work became free/complete or lost bytes", audit, err)
			}
			if audit.ValidateCompleted(session, policy) == nil {
				t.Fatal("denied response qualified completed receipt")
			}
			changed := policy
			changed.ResponsePolicy = ""
			if Digest(changed) == audit.PolicyHash {
				t.Fatal("tool restriction missing from private policy binding")
			}
		})
	}
}

// Only a completely read response refused by the response policy counts as a
// policy denial. A failed, truncated, oversized or unavailable upstream fails
// the session without that signal, so a guard fixture cannot mistake one for
// the other.
func TestProviderEgressResponseDenialExcludesTransportAndReadFailures(t *testing.T) {
	tool := codexToolStream("apply_patch")
	for _, kind := range []string{"transport-error", "read-error", "truncated", "oversized", "status", "policy"} {
		t.Run(kind, func(t *testing.T) {
			session := testSession(Codex)
			policy := egressTestPolicy(Codex)
			policy.ResponsePolicy = CodexSourceResponses
			guard, err := NewProviderEgress(context.Background(), session, policy, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
				r := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tool)), ContentLength: int64(len(tool))}
				switch kind {
				case "transport-error":
					return nil, errors.New("synthetic upstream failure")
				case "read-error":
					r.Body = io.NopCloser(io.MultiReader(strings.NewReader(tool[:len(tool)/2]), egressFailingReader{}))
					r.ContentLength = -1
				case "truncated":
					// A real HTTP/1.1 peer declares the whole body, sends half
					// and closes; the client transport reports the short read.
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						conn, buffered, err := http.NewResponseController(w).Hijack()
						if err != nil {
							return
						}
						defer conn.Close()
						fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(tool), tool[:len(tool)/2])
						_ = buffered.Flush()
					}))
					t.Cleanup(upstream.Close)
					forward, _ := http.NewRequestWithContext(q.Context(), "POST", upstream.URL, nil)
					return upstream.Client().Transport.RoundTrip(forward)
				case "oversized":
					r.ContentLength = int64(policy.Limits.ResponseBytes) + 1
				case "status":
					r.StatusCode = 302
				}
				return r, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			q := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
			q.Host = "provider.example"
			guard.ServeHTTP(recorder, q)
			audit, err := guard.Close(context.Background())
			want := 0
			if kind == "policy" {
				want = 1
			}
			if err != nil || !audit.Failed || audit.Responses != 0 || audit.ResponseDenials != want || strings.Contains(recorder.Body.String(), "apply_patch") {
				t.Fatal("response denial signal differs", kind, audit, err)
			}
			raw, _ := json.Marshal(audit)
			if strings.Contains(string(raw), "response_denials") != (want != 0) {
				t.Fatal("zero response denial changed receipt bytes", string(raw))
			}
		})
	}
}
func TestProviderResponsePolicyProviderAndBounds(t *testing.T) {
	s := testSession(Codex)
	policy := egressTestPolicy(Codex)
	policy.ResponsePolicy = ClaudeSourceMessages
	if _, err := NewProviderEgress(context.Background(), s, policy, egressRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("not called") })); err == nil {
		t.Fatal("cross-provider response policy accepted")
	}
	if err := validateProviderResponse(Codex, CodexSourceResponses, "text/event-stream", 200, []byte("data: "+strings.Repeat("x", (4<<20)+1)+"\n\n")); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestProviderResponseClaudeTerminalErrorsRemainVisibleToClient(t *testing.T) {
	failure := responseEvent("error", map[string]any{"error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}}, true)
	start := responseEvent("message_start", map[string]any{"message": map[string]any{"type": "message", "role": "assistant", "content": []any{}}}, true)
	for _, prefix := range []string{"", start} {
		stream := prefix + failure
		policy := egressTestPolicy(Claude)
		policy.ResponsePolicy = ClaudeSourceMessages
		guard, err := NewProviderEgress(context.Background(), testSession(Claude), policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), ContentLength: int64(len(stream))}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		q := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
		q.Host = "provider.example"
		guard.ServeHTTP(recorder, q)
		audit, err := guard.Close(context.Background())
		if recorder.Code != 200 || recorder.Body.String() != stream || err != nil || audit.Failed || !audit.Stopped || audit.Requests != 1 || audit.Responses != 1 {
			t.Fatal("provider error replaced or audit poisoned", recorder.Code, audit, err)
		}
	}
	for name, stream := range map[string]string{
		"missing-error":    responseEvent("error", map[string]any{}, true),
		"malformed-detail": responseEvent("error", map[string]any{"error": "Overloaded"}, true),
		"missing-message":  responseEvent("error", map[string]any{"error": map[string]any{"type": "overloaded_error"}}, true),
		"extra-tool":       responseEvent("error", map[string]any{"error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}, "content_block": map[string]any{"type": "tool_use", "id": "hidden", "name": "Bash", "input": map[string]any{}}}, true),
		"after-error":      failure + claudeToolStream("EndConversation"),
		"duplicate-error":  failure + failure,
	} {
		t.Run(name, func(t *testing.T) {
			if validateProviderResponse(Claude, ClaudeSourceMessages, "text/event-stream", 200, []byte(stream)) == nil {
				t.Fatal("unsafe error envelope accepted")
			}
		})
	}
}
func TestProviderResponseClaudeEndConversationJSONMatchesNativeControlPolicy(t *testing.T) {
	raw := []byte(`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"finish","name":"EndConversation","input":{}}]}`)
	if err := validateProviderResponse(Claude, ClaudeSourceMessages, "application/json", 200, raw); err != nil {
		t.Fatal(err)
	}
}

func TestProviderResponseCodexFailureTerminalsRemainVisibleToClient(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete"} {
		t.Run(kind, func(t *testing.T) {
			response := map[string]any{"id": "resp_fixture", "object": "response", "status": strings.TrimPrefix(kind, "response."), "output": []any{}}
			if kind == "response.failed" {
				response["error"] = map[string]any{"code": "server_error", "message": "Synthetic provider failure"}
			} else {
				response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			stream := responseEvent(kind, map[string]any{"response": response, "sequence_number": 1}, false)
			policy := egressTestPolicy(Codex)
			policy.ResponsePolicy = CodexSourceResponses
			guard, err := NewProviderEgress(context.Background(), testSession(Codex), policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), ContentLength: int64(len(stream))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			q := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
			q.Host = "provider.example"
			guard.ServeHTTP(recorder, q)
			audit, err := guard.Close(context.Background())
			if recorder.Code != 200 || recorder.Body.String() != stream || err != nil || audit.Failed || !audit.Stopped || audit.Requests != 1 || audit.Responses != 1 || audit.ResponseBytes != len(stream) {
				t.Fatal("provider terminal replaced or audit poisoned", recorder.Code, audit, err)
			}
			for _, unsafe := range []string{
				responseEvent(kind, map[string]any{"response": map[string]any{"output": []any{map[string]any{"type": "custom_tool_call", "namespace": "functions", "name": "apply_patch", "call_id": "hidden", "input": "forbidden"}}}}, false),
				stream + codexToolStream("exec"),
				responseEvent(kind, map[string]any{"response": nil}, false),
				stream + stream,
			} {
				if validateProviderResponse(Codex, CodexSourceResponses, "text/event-stream", 200, []byte(unsafe)) == nil {
					t.Fatal("unsafe or post-terminal output accepted", unsafe)
				}
			}
		})
	}
}
