package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// codexRequestVariant re-encodes one measured request after edit. The edit
// receives the decoded request; nested values are fresh copies.
func codexRequestVariant(t *testing.T, turn int, edit func(map[string]any)) []byte {
	t.Helper()
	request := codexMeasuredRequests(t)[turn]
	if edit != nil {
		edit(request)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func codexInventoryItem(r map[string]any) map[string]any {
	return r["input"].([]any)[0].(map[string]any)
}
func codexNamespace(r map[string]any) map[string]any {
	return codexInventoryItem(r)["tools"].([]any)[0].(map[string]any)
}
func codexTool(r map[string]any, name string) map[string]any {
	for _, v := range codexNamespace(r)["tools"].([]any) {
		if tool := v.(map[string]any); tool["name"] == name {
			return tool
		}
	}
	return nil
}
func codexCopy(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestProviderRequestAcceptsMeasuredCodeModeRequests(t *testing.T) {
	for turn := range 2 {
		body := codexRequestVariant(t, turn, nil)
		if err := checkProviderRequest(Codex, codexRequestPolicy, body); err != nil {
			t.Fatal("measured code-mode request refused", turn, err)
		}
	}
	// The capture's exact bytes, not only their re-encoding, are accepted.
	raw, err := os.ReadFile("testdata/codex-0.163.0-code-mode-requests.json")
	if err != nil {
		t.Fatal(err)
	}
	var requests []json.RawMessage
	if json.Unmarshal(raw, &requests) != nil {
		t.Fatal("measured requests unreadable")
	}
	for _, request := range requests {
		if err := checkProviderRequest(Codex, codexRequestPolicy, bytes.TrimSpace(request)); err != nil {
			t.Fatal("measured bytes refused", err)
		}
	}
	// The third turn carries history: two exec calls and their outputs.
	history := codexMeasuredRequests(t)[1]["input"].([]any)
	if len(history) != 11 || history[7].(map[string]any)["type"] != "custom_tool_call" || history[10].(map[string]any)["type"] != "custom_tool_call_output" {
		t.Fatal("measured history shape changed", len(history))
	}
	// An empty policy is the legacy unenforced path.
	if err := checkProviderRequest(Codex, "", []byte("not json")); err != nil {
		t.Fatal("empty request policy enforced", err)
	}
}

func TestProviderRequestRejectsEveryOtherInventory(t *testing.T) {
	widened, err := os.ReadFile("testdata/codex-0.163.0-widened-code-mode-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var widenedItem any
	if json.Unmarshal(widened, &widenedItem) != nil {
		t.Fatal("widened capture unreadable")
	}
	// The direct-mode inventory the codex-metadata case recorded: apply_patch,
	// MCP resource tools and direct source handles in a top-level tools list.
	direct := []any{
		map[string]any{"type": "custom", "name": "apply_patch", "description": "patch", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: x"}},
		map[string]any{"type": "function", "name": "list_mcp_resources", "description": "", "strict": false, "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "list_mcp_resource_templates", "description": "", "strict": false, "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "read_mcp_resource", "description": "", "strict": false, "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "namespace", "name": "mcp__sfl_source", "description": "", "tools": []any{map[string]any{"type": "function", "name": "read"}, map[string]any{"type": "function", "name": "search"}}},
	}
	tools := func(r map[string]any) []any { return codexNamespace(r)["tools"].([]any) }
	setTools := func(r map[string]any, v []any) { codexNamespace(r)["tools"] = v }
	heading := func(r map[string]any, old, new string) {
		exec := codexTool(r, "exec")
		exec["description"] = strings.Replace(exec["description"].(string), old, new, 1)
	}
	cases := map[string]func(map[string]any){
		// Widened inventories.
		"direct-mode-top-level-tools":        func(r map[string]any) { r["tools"] = direct },
		"direct-mode-without-code-mode-item": func(r map[string]any) { r["tools"] = direct; r["input"] = r["input"].([]any)[1:] },
		"empty-top-level-tools":              func(r map[string]any) { r["tools"] = []any{} },
		"null-top-level-tools":               func(r map[string]any) { r["tools"] = nil },
		"measured-widened-code-mode":         func(r map[string]any) { r["input"].([]any)[0] = codexCopy(widenedItem) },
		"extra-namespace": func(r map[string]any) {
			item := codexInventoryItem(r)
			item["tools"] = append(item["tools"].([]any), codexCopy(widenedItem.(map[string]any)["tools"].([]any)[1]))
		},
		"third-tool": func(r map[string]any) { setTools(r, append(tools(r), codexCopy(direct[0]))) },
		"nested-exec-apply-patch": func(r map[string]any) {
			heading(r, "### `mcp__sfl_source__search`", "### `mcp__sfl_source__search`\n\n### `apply_patch`")
		},
		"nested-exec-read-mcp-resource": func(r map[string]any) {
			heading(r, "### `mcp__sfl_source__read`", "### `read_mcp_resource`\n\n### `mcp__sfl_source__read`")
		},
		"nested-exec-replaced":       func(r map[string]any) { heading(r, "### `mcp__sfl_source__search`", "### `exec_command`") },
		"nested-exec-missing":        func(r map[string]any) { heading(r, "### `mcp__sfl_source__search`", "") },
		"nested-exec-duplicated":     func(r map[string]any) { heading(r, "### `mcp__sfl_source__search`", "### `mcp__sfl_source__read`") },
		"nested-exec-deeper-heading": func(r map[string]any) { heading(r, "### `mcp__sfl_source__search`", "#### `mcp__sfl_source__search`") },
		"nested-namespace":           func(r map[string]any) { setTools(r, append(tools(r), codexCopy(direct[4]))) },
		"namespace-in-namespace": func(r map[string]any) {
			setTools(r, []any{codexTool(r, "exec"), map[string]any{"type": "namespace", "name": "functions", "description": "", "tools": []any{codexTool(r, "wait")}}})
		},
		"later-additional-tools":         func(r map[string]any) { r["input"] = append(r["input"].([]any), codexCopy(r["input"].([]any)[0])) },
		"later-widened-additional-tools": func(r map[string]any) { r["input"] = append(r["input"].([]any), codexCopy(widenedItem)) },
		"tools-in-history-item":          func(r map[string]any) { r["input"].([]any)[1].(map[string]any)["tools"] = codexCopy(direct) },
		"namespace-in-tool-output": func(r map[string]any) {
			r["input"].([]any)[2].(map[string]any)["content"] = []any{codexCopy(direct[4])}
		},
		"tools-in-client-metadata": func(r map[string]any) { r["client_metadata"] = map[string]any{"tools": "apply_patch"} },
		"additional-tools-in-text": func(r map[string]any) { r["text"].(map[string]any)["extra"] = codexCopy(r["input"].([]any)[0]) },
		// Missing, misplaced or duplicate inventories.
		"missing-inventory":         func(r map[string]any) { r["input"] = r["input"].([]any)[1:] },
		"inventory-not-first":       func(r map[string]any) { in := r["input"].([]any); in[0], in[1] = in[1], in[0] },
		"missing-input":             func(r map[string]any) { delete(r, "input") },
		"empty-input":               func(r map[string]any) { r["input"] = []any{} },
		"input-string":              func(r map[string]any) { r["input"] = "synthetic" },
		"inventory-not-object":      func(r map[string]any) { r["input"].([]any)[0] = "additional_tools" },
		"no-namespaces":             func(r map[string]any) { codexInventoryItem(r)["tools"] = []any{} },
		"namespace-tools-missing":   func(r map[string]any) { delete(codexNamespace(r), "tools") },
		"namespace-tools-object":    func(r map[string]any) { codexNamespace(r)["tools"] = map[string]any{"exec": true} },
		"only-exec":                 func(r map[string]any) { setTools(r, []any{codexTool(r, "exec")}) },
		"only-wait":                 func(r map[string]any) { setTools(r, []any{codexTool(r, "wait")}) },
		"duplicate-exec":            func(r map[string]any) { setTools(r, []any{codexTool(r, "exec"), codexCopy(codexTool(r, "exec"))}) },
		"duplicate-wait":            func(r map[string]any) { setTools(r, []any{codexTool(r, "wait"), codexCopy(codexTool(r, "wait"))}) },
		"tool-not-object":           func(r map[string]any) { setTools(r, []any{codexTool(r, "exec"), "wait"}) },
		"tool-without-name":         func(r map[string]any) { delete(codexTool(r, "wait"), "name") },
		"renamed-namespace":         func(r map[string]any) { codexNamespace(r)["name"] = "mcp__sfl_source" },
		"namespace-wrong-type":      func(r map[string]any) { codexNamespace(r)["type"] = "function" },
		"namespace-extra-key":       func(r map[string]any) { codexNamespace(r)["strict"] = false },
		"namespace-description-num": func(r map[string]any) { codexNamespace(r)["description"] = 1 },
		// Unknown tool types and altered tool shapes.
		"exec-function-type":       func(r map[string]any) { codexTool(r, "exec")["type"] = "function" },
		"exec-type-missing":        func(r map[string]any) { delete(codexTool(r, "exec"), "type") },
		"exec-local-shell-type":    func(r map[string]any) { codexTool(r, "exec")["type"] = "local_shell" },
		"wait-custom-type":         func(r map[string]any) { codexTool(r, "wait")["type"] = "custom" },
		"wait-web-search-type":     func(r map[string]any) { codexTool(r, "wait")["type"] = "web_search" },
		"exec-extra-key":           func(r map[string]any) { codexTool(r, "exec")["strict"] = false },
		"exec-format-missing":      func(r map[string]any) { delete(codexTool(r, "exec"), "format") },
		"exec-format-text":         func(r map[string]any) { codexTool(r, "exec")["format"].(map[string]any)["type"] = "text" },
		"exec-format-regex":        func(r map[string]any) { codexTool(r, "exec")["format"].(map[string]any)["syntax"] = "regex" },
		"exec-format-extra-key":    func(r map[string]any) { codexTool(r, "exec")["format"].(map[string]any)["tools"] = []any{} },
		"exec-description-missing": func(r map[string]any) { delete(codexTool(r, "exec"), "description") },
		"wait-strict":              func(r map[string]any) { codexTool(r, "wait")["strict"] = true },
		"wait-strict-missing":      func(r map[string]any) { delete(codexTool(r, "wait"), "strict") },
		"wait-extra-key":           func(r map[string]any) { codexTool(r, "wait")["format"] = map[string]any{} },
		"wait-parameters-extra": func(r map[string]any) {
			codexTool(r, "wait")["parameters"].(map[string]any)["properties"].(map[string]any)["command"] = map[string]any{"type": "string"}
		},
		"wait-parameters-open": func(r map[string]any) {
			codexTool(r, "wait")["parameters"].(map[string]any)["additionalProperties"] = true
		},
		"wait-parameters-tools": func(r map[string]any) {
			codexTool(r, "wait")["parameters"].(map[string]any)["tools"] = codexCopy(direct)
		},
		"inventory-extra-key":   func(r map[string]any) { codexInventoryItem(r)["tool_choice"] = "auto" },
		"inventory-role-user":   func(r map[string]any) { codexInventoryItem(r)["role"] = "user" },
		"inventory-id-missing":  func(r map[string]any) { delete(codexInventoryItem(r), "id") },
		"inventory-id-empty":    func(r map[string]any) { codexInventoryItem(r)["id"] = "" },
		"inventory-id-long":     func(r map[string]any) { codexInventoryItem(r)["id"] = strings.Repeat("a", 257) },
		"unknown-top-level-key": func(r map[string]any) { r["prompt"] = map[string]any{"id": "pmpt_synthetic"} },
		"instructions-key":      func(r map[string]any) { r["instructions"] = "synthetic" },
		"tool-choice-named":     func(r map[string]any) { r["tool_choice"] = map[string]any{"type": "function", "name": "apply_patch"} },
		"tool-choice-required":  func(r map[string]any) { r["tool_choice"] = "required" },
		"tool-choice-allowed": func(r map[string]any) {
			r["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": direct}
		},
		"model-missing":    func(r map[string]any) { delete(r, "model") },
		"model-empty":      func(r map[string]any) { r["model"] = "" },
		"model-not-string": func(r map[string]any) { r["model"] = 6 },
	}
	for name, edit := range cases {
		for turn := range 2 {
			if err := checkProviderRequest(Codex, codexRequestPolicy, codexRequestVariant(t, turn, edit)); err != errProviderToolRequest {
				t.Fatal("inventory variant accepted", name, turn, err)
			}
		}
	}
	good := string(codexRequestVariant(t, 0, nil))
	deep := strings.Repeat(`{"a":`, 40) + `1` + strings.Repeat(`}`, 40)
	malformed := map[string][]byte{
		"empty":             nil,
		"whitespace":        []byte(" "),
		"not-json":          []byte("not json"),
		"truncated":         []byte(good[:len(good)/2]),
		"trailing-value":    []byte(good + ` {}`),
		"two-values":        []byte(good + good),
		"array":             []byte(`[` + good + `]`),
		"null":              []byte(`null`),
		"string":            []byte(`"tools"`),
		"invalid-utf8":      append([]byte(good[:len(good)-1]+`,"x":"`), 0xff, '"', '}'),
		"duplicate-model":   []byte(strings.Replace(good, `"model":`, `"model":"other","model":`, 1)),
		"duplicate-tools":   []byte(strings.Replace(good, `"tools":[{"description":""`, `"tools":[],"tools":[{"description":""`, 1)),
		"duplicate-input":   []byte(strings.Replace(good, `"input":[`, `"input":[],"input":[`, 1)),
		"too-deep":          []byte(strings.Replace(good, `"include":`, `"reasoning_deep":`+deep+`,"include":`, 1)),
		"oversized":         []byte(strings.Replace(good, `"prompt_cache_key":"`, `"prompt_cache_key":"`+strings.Repeat("x", requestBytesBound), 1)),
		"oversized-by-one":  append([]byte(good), bytes.Repeat([]byte(" "), requestBytesBound-len(good)+1)...),
		"nul-before-object": append([]byte{0}, good...),
	}
	if strings.Count(string(malformed["duplicate-tools"]), `"tools":[]`) != 1 || len(malformed["oversized-by-one"]) != requestBytesBound+1 {
		t.Fatal("malformed fixture did not apply")
	}
	for name, body := range malformed {
		if err := checkProviderRequest(Codex, codexRequestPolicy, body); err != errProviderToolRequest {
			t.Fatal("malformed request accepted", name, err)
		}
	}
	// Exactly at the bound, trailing whitespace is still one JSON value.
	exact := append([]byte(good), bytes.Repeat([]byte(" "), requestBytesBound-len(good))...)
	if err := checkProviderRequest(Codex, codexRequestPolicy, exact); err != nil {
		t.Fatal("request at the byte bound refused", err)
	}
	// The policy is Codex-only and has exactly one name.
	for _, mismatch := range []struct {
		provider Provider
		policy   string
	}{{Claude, codexRequestPolicy}, {Codex, "codex-source-requests-v2"}, {Codex, CodexSourceResponses}, {Provider("other"), codexRequestPolicy}} {
		if err := checkProviderRequest(mismatch.provider, mismatch.policy, []byte(good)); err != errProviderToolRequest {
			t.Fatal("request policy accepted for another provider or name", mismatch, err)
		}
	}
}

// The canonical wait schema constant must be the measured schema, so a change
// to the encoder cannot silently refuse or admit a different schema.
func TestProviderRequestWaitSchemaIsTheMeasuredOne(t *testing.T) {
	for turn := range 2 {
		raw, _ := json.Marshal(codexTool(codexMeasuredRequests(t)[turn], "wait")["parameters"])
		if string(raw) != measuredWaitParameters {
			t.Fatal("measured wait schema differs from the accepted one", string(raw))
		}
	}
}

// requestPolicyEgress builds a Codex guard with both source policies whose
// upstream counts every forwarded request and answers with output.
func requestPolicyEgress(t *testing.T, output string) (*ProviderEgress, Session, EgressPolicy, *atomic.Int32) {
	t.Helper()
	session := testSession(Codex)
	policy := egressTestPolicy(Codex)
	policy.ResponsePolicy = CodexSourceResponses
	withCodexRequestPolicy(&policy)
	var calls atomic.Int32
	guard, err := NewProviderEgress(context.Background(), session, policy, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(output)), ContentLength: int64(len(output))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return guard, session, policy, &calls
}

func requestPolicyCall(guard *ProviderEgress, body []byte) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	q := httptest.NewRequest("POST", "/infer", bytes.NewReader(body))
	q.Host = "provider.example"
	guard.ServeHTTP(recorder, q)
	return recorder
}

// A refused inventory is read and charged but never forwarded. The guard
// latches failure, records the denial, refuses replays without forwarding them
// and cannot produce a completed receipt.
func TestProviderEgressRefusesWidenedCodexRequestBeforeForwarding(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"direct-mode-tools": func(r map[string]any) {
			r["tools"] = []any{map[string]any{"type": "custom", "name": "apply_patch"}, map[string]any{"type": "function", "name": "read_mcp_resource"}}
		},
		"nested-apply-patch": func(r map[string]any) {
			exec := codexTool(r, "exec")
			exec["description"] = exec["description"].(string) + "\n### `apply_patch`\n"
		},
		"missing-inventory": func(r map[string]any) { r["input"] = r["input"].([]any)[1:] },
	} {
		t.Run(name, func(t *testing.T) {
			guard, session, policy, calls := requestPolicyEgress(t, codexToolStream("exec"))
			body := codexRequestVariant(t, 0, edit)
			recorder := requestPolicyCall(guard, body)
			if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != "provider tool request denied" || calls.Load() != 0 {
				t.Fatal("widened request reached the provider", recorder.Code, recorder.Body.String(), calls.Load())
			}
			// The client may retry. Nothing more reaches the provider and the
			// retry is not charged as another request.
			if replay := requestPolicyCall(guard, codexRequestVariant(t, 0, nil)); replay.Code == http.StatusOK || calls.Load() != 0 {
				t.Fatal("latched refusal forwarded a later request", replay.Code, calls.Load())
			}
			audit, err := guard.Close(context.Background())
			if err != nil || !audit.Stopped || !audit.Failed || audit.Exhausted || audit.Requests != 1 || audit.InferenceRequests != 1 || audit.Forwarded != 0 || audit.Responses != 0 || audit.RequestBytes != len(body) || audit.ResponseBytes != 0 || requestDenials(audit) != 1 || audit.ResponseDenials != 0 || audit.RouteDenials != 0 {
				t.Fatal("request refusal receipt differs", audit, err)
			}
			if audit.ValidateCompleted(session, policy) == nil {
				t.Fatal("refused request qualified a completed receipt")
			}
			evidence := &ProviderEvidence{Policy: policy, Access: audit}
			if err = evidence.validateStoppedCaptured(Codex, session.Limits, Digest(session)); err != nil {
				t.Fatal("stopped refusal receipt is not recoverable evidence", err)
			}
			if !nativeProviderRefused(evidence) {
				t.Fatal("request refusal could be reclassified as shutdown")
			}
			raw, _ := json.Marshal(audit)
			if !strings.Contains(string(raw), `"request_denials":1`) || strings.Contains(string(raw), "apply_patch") || strings.Contains(string(raw), "mcp__sfl_source") {
				t.Fatal("receipt lacks the denial or carries request content", string(raw))
			}
			changed := policy
			setRequestPolicy(&changed, "")
			if Digest(changed) == audit.PolicyHash {
				t.Fatal("request policy missing from the private policy binding")
			}
		})
	}
}

// The measured inventory is forwarded, and the response policy behind it still
// refuses a forbidden tool and admits an allowed one.
func TestProviderEgressRequestPolicyKeepsTheResponsePolicy(t *testing.T) {
	for _, tool := range []string{"exec", "apply_patch"} {
		t.Run(tool, func(t *testing.T) {
			output := codexToolStream(tool)
			guard, session, policy, calls := requestPolicyEgress(t, output)
			for turn := range 2 {
				recorder := requestPolicyCall(guard, codexRequestVariant(t, turn, nil))
				if tool == "exec" && (recorder.Code != 200 || recorder.Body.String() != output) || tool == "apply_patch" && (recorder.Code != 400 || strings.Contains(recorder.Body.String(), "apply_patch")) {
					t.Fatal("response policy changed behind the request policy", tool, recorder.Code, recorder.Body.String())
				}
				if tool == "apply_patch" {
					break
				}
			}
			audit, err := guard.Close(context.Background())
			if err != nil || requestDenials(audit) != 0 {
				t.Fatal("measured request was refused", audit, err)
			}
			if tool == "exec" && (audit.ValidateCompleted(session, policy) != nil || calls.Load() != 2 || audit.Forwarded != 2) {
				t.Fatal("measured requests did not complete", audit, calls.Load())
			}
			if tool == "apply_patch" && (audit.ResponseDenials != 1 || audit.Forwarded != 1 || calls.Load() != 1 || !audit.Failed) {
				t.Fatal("response policy denial lost", audit, calls.Load())
			}
			raw, _ := json.Marshal(audit)
			if strings.Contains(string(raw), "request_denials") {
				t.Fatal("zero request denials changed receipt bytes", string(raw))
			}
		})
	}
}

// The request policy applies only to inference. Refresh and metadata routes
// carry no tool inventory: a non-JSON refresh body and a metadata GET are
// forwarded and answered as before.
func TestProviderEgressRequestPolicyLeavesOtherRoutes(t *testing.T) {
	guard, _, _, calls := requestPolicyEgress(t, `{}`)
	for _, q := range []*http.Request{httptest.NewRequest("POST", "/refresh", strings.NewReader("grant_type=refresh_token")), httptest.NewRequest("GET", "/metadata", nil)} {
		q.Host = "provider.example"
		if q.URL.Path == "/refresh" {
			q.Host = "auth.provider.example"
		}
		recorder := httptest.NewRecorder()
		guard.ServeHTTP(recorder, q)
		if recorder.Code != 200 {
			t.Fatal("non-inference route refused", q.URL.Path, recorder.Code, recorder.Body.String())
		}
	}
	audit, err := guard.Close(context.Background())
	if err != nil || audit.Failed || requestDenials(audit) != 0 || calls.Load() != 2 || audit.Forwarded != 2 || audit.Responses != 2 {
		t.Fatal("request policy changed a non-inference route", audit, err)
	}
}

// Construction accepts the request policy only for Codex and only by name.
func TestProviderRequestPolicyIsCapturedAndProviderBound(t *testing.T) {
	transport := egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("constructor contacted upstream")
		return nil, nil
	})
	for _, c := range []struct {
		provider Provider
		policy   string
	}{{Claude, codexRequestPolicy}, {Codex, "codex-source-requests-v2"}, {Codex, CodexSourceResponses}} {
		policy := egressTestPolicy(c.provider)
		setRequestPolicy(&policy, c.policy)
		if _, err := NewProviderEgress(context.Background(), testSession(c.provider), policy, transport); err == nil {
			t.Fatal("foreign request policy accepted", c)
		}
	}
	policy := egressTestPolicy(Codex)
	withCodexRequestPolicy(&policy)
	if requestPolicyOf(policy) != codexRequestPolicy {
		t.Fatal("request policy not set")
	}
	if _, err := NewProviderEgress(context.Background(), testSession(Codex), policy, transport); err != nil {
		t.Fatal("Codex request policy refused", err)
	}
	raw, _ := json.Marshal(egressTestPolicy(Codex))
	if strings.Contains(string(raw), "request_policy") {
		t.Fatal("empty request policy changed policy bytes", string(raw))
	}
}

// Request denial counters must agree with the recorded outcome: a completed
// receipt carries none, and each stopped denial is a reserved inference that
// was never forwarded, under a failed receipt with a request policy.
func TestProviderEvidenceValidatesRequestDenials(t *testing.T) {
	guard, session, policy, _ := requestPolicyEgress(t, codexToolStream("exec"))
	requestPolicyCall(guard, codexRequestVariant(t, 0, nil))
	requestPolicyCall(guard, codexRequestVariant(t, 1, nil))
	audit, err := guard.Close(context.Background())
	if err != nil || audit.ValidateCompleted(session, policy) != nil || audit.Forwarded != 2 {
		t.Fatal("clean fixture receipt", audit, err)
	}
	clean := &ProviderEvidence{Policy: policy, Access: audit}
	valid := func(e *ProviderEvidence) bool {
		return e.validateStoppedCaptured(Codex, session.Limits, Digest(session)) == nil
	}
	// One of the two inferences refused: reserved and charged, never forwarded.
	refused := cloneProviderEvidence(clean)
	refused.Access.Failed = true
	refused.Access.Forwarded, refused.Access.Responses = 1, 1
	setRequestDenials(&refused.Access, 1)
	if !valid(refused) || refused.Access.ValidateCompleted(session, policy) == nil {
		t.Fatal("consistent request denial receipt rejected or completed", refused.Access)
	}
	for name, mutate := range map[string]func(*ProviderEvidence){
		"not-failed":       func(e *ProviderEvidence) { e.Access.Failed = false },
		"negative":         func(e *ProviderEvidence) { setRequestDenials(&e.Access, -1) },
		"more-than-unsent": func(e *ProviderEvidence) { setRequestDenials(&e.Access, 2) },
		"forwarded-denial": func(e *ProviderEvidence) { e.Access.Forwarded, e.Access.Responses = 2, 2 },
		"beyond-inferences": func(e *ProviderEvidence) {
			e.Access.Forwarded, e.Access.Responses = 0, 0
			setRequestDenials(&e.Access, 3)
		},
		"without-the-policy": func(e *ProviderEvidence) { setRequestPolicy(&e.Policy, ""); e.Access.PolicyHash = Digest(e.Policy) },
	} {
		changed := cloneProviderEvidence(refused)
		mutate(changed)
		if valid(changed) {
			t.Fatal("inconsistent request denial accepted", name, changed.Access)
		}
	}
	completed := cloneProviderEvidence(clean)
	setRequestDenials(&completed.Access, 1)
	if completed.Access.ValidateCompleted(session, policy) == nil || valid(completed) {
		t.Fatal("a request denial with every request forwarded accepted", completed.Access)
	}
}
