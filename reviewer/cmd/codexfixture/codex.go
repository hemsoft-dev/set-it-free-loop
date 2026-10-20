//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func encoded(value any) string {
	b, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(b)
}
func seedCodex() {
	if err := os.MkdirAll("/profile/codex", 0700); err != nil {
		panic(err)
	}
	jwt := encoded(map[string]string{"alg": "none", "typ": "JWT"}) + "." + encoded(map[string]any{"email": "fixture@example.invalid", "https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": "plus", "chatgpt_user_id": "sfl-fake-user", "chatgpt_account_id": "sfl-fake-account"}}) + "." + encoded("fake-signature")
	value := map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"id_token": jwt, "access_token": "sfl-fake-access", "refresh_token": "sfl-fake-refresh", "account_id": "sfl-fake-account"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)}
	data, _ := json.Marshal(value)
	if err := os.WriteFile("/profile/codex/auth.json", data, 0600); err != nil {
		panic(err)
	}
}

type codexBackend struct {
	mu                                    sync.Mutex
	session                               r.Session
	mode                                  string
	calls, failed, negotiations, metadata int
	negativeOutput                        string
	runtimeInventory                      []string
}

func outputFor(value any, id string) (any, bool) {
	body, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	items, ok := body["input"].([]any)
	if !ok {
		return nil, false
	}
	var output any
	count := 0
	for _, item := range items {
		if v, ok := item.(map[string]any); ok && v["type"] == "custom_tool_call_output" && v["call_id"] == id {
			count++
			output = v["output"]
		}
	}
	return output, count == 1
}
func verifiedSourceOutput(value any, id string) bool {
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	count := 0
	for _, part := range parts {
		v, ok := part.(map[string]any)
		if !ok || v["type"] != "input_text" {
			return false
		}
		text, ok := v["text"].(string)
		if !ok {
			return false
		}
		if !strings.HasPrefix(strings.TrimSpace(text), "{") {
			continue
		}
		result, err := r.DecodeStrict[struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}]([]byte(text), 1<<16)
		if err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" {
			return false
		}
		count++
		switch id {
		case "fixture_read":
			if result.Content[0].Text != source {
				return false
			}
		case "fixture_search":
			matches, err := r.DecodeStrict[[]r.Anchor]([]byte(result.Content[0].Text), 1<<16)
			if err != nil || len(matches) != 1 || matches[0] != (r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}) {
				return false
			}
		default:
			return false
		}
	}
	return count == 1
}

func inventory(value any) ([]string, bool) {
	switch x := value.(type) {
	case []any:
		for _, v := range x {
			if out, ok := inventory(v); ok {
				return out, true
			}
		}
	case map[string]any:
		if text, ok := x["text"].(string); ok {
			var names []string
			if json.Unmarshal([]byte(text), &names) == nil && len(names) > 0 {
				return names, true
			}
		}
		for _, v := range x {
			if out, ok := inventory(v); ok {
				return out, true
			}
		}
	}
	return nil, false
}
func advertised(body map[string]any) bool {
	items, ok := body["input"].([]any)
	if !ok {
		return false
	}
	count := 0
	seen := map[string]bool{}
	for _, v := range items {
		item, ok := v.(map[string]any)
		if !ok || item["type"] != "additional_tools" {
			continue
		}
		list, ok := item["tools"].([]any)
		if !ok {
			return false
		}
		for _, v := range list {
			ns, ok := v.(map[string]any)
			if !ok || ns["type"] != "namespace" || ns["name"] != "functions" {
				return false
			}
			children, ok := ns["tools"].([]any)
			if !ok {
				return false
			}
			for _, v := range children {
				tool, ok := v.(map[string]any)
				if !ok {
					return false
				}
				name, _ := tool["name"].(string)
				if name != "exec" && name != "wait" || seen[name] {
					return false
				}
				count++
				seen[name] = true
				if name == "exec" {
					desc, _ := tool["description"].(string)
					if !strings.Contains(desc, "mcp__sfl_source__read") || !strings.Contains(desc, "mcp__sfl_source__search") {
						return false
					}
				}
			}
		}
	}
	return count == 2 && seen["exec"] && seen["wait"]
}
func sse(w http.ResponseWriter, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}
func (b *codexBackend) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := io.ReadAll(io.LimitReader(q.Body, (2<<20)+1))
	_ = q.Body.Close()
	headers := []string{}
	for h := range q.Header {
		headers = append(headers, h)
	}
	record("http.jsonl", map[string]any{"method": q.Method, "path": q.URL.Path, "query": q.URL.RawQuery, "headers": headers, "body": string(raw), "upgrade": q.Header.Get("Upgrade")})
	deny := func(message string) { b.failed++; http.Error(w, message, 400) }
	if err != nil || len(raw) > 2<<20 {
		deny("input cap")
		return
	}
	if strings.EqualFold(q.Header.Get("Upgrade"), "websocket") {
		b.negotiations++
		if q.Method != "GET" || q.URL.Path != "/v1/responses" || b.negotiations > 1 {
			deny("unexpected negotiation")
			return
		}
		w.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	if q.Method == "GET" && q.URL.Path != "/v1/responses" || q.Method == "POST" && q.URL.Path == "/backend-api/codex/analytics-events/events" {
		b.metadata++
		if b.metadata > 12 {
			deny("metadata cap")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch q.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/backend-api/wham/accounts/check":
			_, _ = io.WriteString(w, `{"accounts":{},"account_ordering":[]}`)
		case "/backend-api/wham/config/bundle":
			_, _ = io.WriteString(w, `{}`)
		case "/backend-api/wham/settings/user":
			_, _ = io.WriteString(w, `{}`)
		case "/backend-api/codex/analytics-events/events":
			_, _ = io.WriteString(w, `{}`)
		default:
			deny("unexpected metadata route")
		}
		return
	}
	if q.Method != "POST" || q.URL.Path != "/v1/responses" || q.URL.RawQuery != "" || q.Header.Get("Authorization") != "Bearer sfl-fake-access" || q.Header.Get("X-Api-Key") != "" {
		deny("unexpected inference route or auth")
		return
	}
	b.calls++
	if b.calls > 4 {
		deny("inference cap")
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || body["model"] != b.session.Identity.Model || !advertised(body) {
		deny("model or advertised tools differ")
		return
	}
	previousID := "fixture_read"
	if b.mode == "undeclared-patch" {
		previousID = "fixture_patch"
	} else if b.calls == 3 {
		previousID = "fixture_search"
	}
	if b.calls > 1 {
		value, ok := outputFor(body, previousID)
		if !ok {
			deny("missing actual prior tool output")
			return
		}
		data, _ := json.Marshal(value)
		if b.mode == "undeclared-patch" {
			b.negativeOutput = string(data)
		} else {
			if !verifiedSourceOutput(value, previousID) {
				deny("missing source result")
				return
			}
			if b.calls == 2 {
				names, ok := inventory(value)
				if !ok || len(names) != 2 || names[0] != "mcp__sfl_source__read" || names[1] != "mcp__sfl_source__search" {
					deny("runtime handles differ")
					return
				}
				b.runtimeInventory = names
			}
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if b.calls == 1 && b.mode == "undeclared-patch" {
		patch := "*** Begin Patch\n*** Add File: /work/undeclared-canary.txt\n+synthetic forbidden write\n*** End Patch\n"
		sse(w, map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "custom_tool_call", "call_id": "fixture_patch", "namespace": "functions", "name": "apply_patch", "input": patch}})
	} else if b.mode == "source" && b.calls <= 2 {
		id, program := "fixture_read", `text(ALL_TOOLS.map(t=>t.name));text(await tools.mcp__sfl_source__read({revision:"`+head+`",path:"source.go"}));`
		if b.calls == 2 {
			id = "fixture_search"
			program = `text(await tools.mcp__sfl_source__search({revision:"` + head + `",needle:"immutable",max_results:5}));`
		}
		sse(w, map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "custom_tool_call", "call_id": id, "namespace": "functions", "name": "exec", "input": program}})
	} else {
		report := r.Report{JobID: b.session.JobID, PolicyVersion: b.session.PolicyVersion, PacketHash: b.session.PacketHash, Provider: r.Codex, Round: 1, SnapshotHash: b.session.SnapshotHash, ConfigHash: b.session.ConfigHash, Findings: []r.Finding{}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}}
		if b.mode == "source" {
			report.Findings = append(report.Findings, r.Finding{ID: "codex:fixture", Title: "Synthetic finding", Body: "Protocol fixture only.", Severity: 2, Anchor: r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}})
		}
		data, _ := json.Marshal(report)
		sse(w, map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "msg_fixture", "content": []map[string]string{{"type": "output_text", "text": string(data)}}}})
	}
	sse(w, map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("fixture_%d", b.calls), "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "input_tokens_details": nil, "output_tokens_details": nil}}})
}
func codexMain(mode string) {
	seedCodex()
	limits := r.DefaultLimits()
	limits.Timeout = 35 * time.Second
	snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("native-fake-backend", snap, r.Digest("synthetic-only"), r.Codex, 1, "immutable source evidence", nil, nil, limits)
	if err != nil {
		panic(err)
	}
	reader, err := r.NewReader(store{}, snap, []string{"source.go"}, limits)
	if err != nil {
		panic(err)
	}
	inv, err := (r.Adapter{Provider: r.Codex, AuthDirectory: "/profile/codex", Reader: reader, SourceBridge: &r.SourceBridge{Program: "/helper", Directory: "/run/sfl", ReaderUID: 1201}}).Invocation(s, nil)
	if err != nil {
		panic(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	b := &codexBackend{session: s, mode: mode}
	server := &http.Server{Handler: b, ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10}
	go server.Serve(listener)
	defer server.Close()
	base := "http://" + listener.Addr().String()
	extra := []string{"-c", `cli_auth_credentials_store="file"`, "-c", `openai_base_url="` + base + `/v1"`, "-c", `chatgpt_base_url="` + base + `/backend-api/"`, "-c", `features.enable_request_compression=false`}
	args := append(append(inv.Args[:len(inv.Args)-1:len(inv.Args)-1], extra...), "-")
	env := append([]string{"HOME=/profile", "PATH=/usr/bin:/bin", "GOMAXPROCS=1"}, inv.Environment...)
	write("invocation.json", map[string]any{"args": args, "environment": env, "fake_only": true})
	for name, data := range inv.ReadOnlyFiles {
		if err := os.WriteFile(filepath.Join("/work", name), data, 0444); err != nil {
			panic(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/client", args...)
	cmd.Env = env
	cmd.Dir = "/work"
	cmd.Stdin = bytes.NewReader(inv.Input)
	stdout, stderr := &limited{max: 2 << 20}, &limited{max: 2 << 20}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	stopErr := server.Shutdown(stopCtx)
	stopCancel()
	if stopErr != nil {
		_ = server.Close()
	}
	_ = os.WriteFile("/artifacts/client-stdout.jsonl", stdout.buf.Bytes(), 0600)
	_ = os.WriteFile("/artifacts/client-stderr.txt", stderr.buf.Bytes(), 0600)
	report, normErr := r.NormalizeCLIOutput(s, stdout.buf.Bytes())
	shapeErr := r.ValidateReport(report, s)
	_, canaryErr := os.Stat("/work/undeclared-canary.txt")
	b.mu.Lock()
	calls, failed, negotiations, metadata := b.calls, b.failed, b.negotiations, b.metadata
	negative := b.negativeOutput
	names := append([]string{}, b.runtimeInventory...)
	b.mu.Unlock()
	denied := mode == "undeclared-patch" && os.IsNotExist(canaryErr) && strings.Contains(negative, "patch rejected: writing is blocked by read-only sandbox")
	text := func(e error) string {
		if e == nil {
			return ""
		}
		return e.Error()
	}
	write("result.json", map[string]any{"provider": "codex", "case": mode, "run_error": text(runErr), "normalize_error": text(normErr), "shape_error": text(shapeErr), "report": report, "backend_calls": calls, "backend_failures": failed, "backend_stop_error": text(stopErr), "negotiations": negotiations, "metadata_requests": metadata, "runtime_inventory": names, "undeclared_patch_denied": denied, "undeclared_patch_output": negative, "canary_exists": canaryErr == nil, "fake_only": true, "capability_allowlist_qualified": false, "actual_account_qualified": false, "production_boundary_qualified": false})
	fmt.Printf("case=%s calls=%d run=%v normalize=%v shape=%v negative_denied=%t\n", mode, calls, runErr, normErr, shapeErr, denied)
	expected := 3
	if mode == "undeclared-patch" {
		expected = 2
	}
	if runErr != nil || normErr != nil || shapeErr != nil || stopErr != nil || failed != 0 || calls != expected || negotiations != 1 || mode == "undeclared-patch" && !denied {
		os.Exit(1)
	}
}
