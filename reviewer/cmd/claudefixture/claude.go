//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

func seedClaude() {
	_ = os.MkdirAll("/profile/claude", 0700)
	creds := map[string]any{"claudeAiOauth": map[string]any{"accessToken": "sfl-fake-access", "refreshToken": "sfl-fake-refresh", "expiresAt": int64(4102444800000), "scopes": []string{"user:profile", "user:inference", "user:sessions:claude_code"}, "subscriptionType": "max", "rateLimitTier": "default_claude_max_5x"}}
	account := map[string]any{"hasCompletedOnboarding": true, "lastOnboardingVersion": "2.1.303", "oauthAccount": map[string]string{"accountUuid": "11111111-1111-4111-8111-111111111111", "emailAddress": "fixture@example.invalid", "organizationUuid": "22222222-2222-4222-8222-222222222222", "organizationName": "SFL fixture"}}
	for name, v := range map[string]any{".credentials.json": creds, ".claude.json": account} {
		b, _ := json.Marshal(v)
		if e := os.WriteFile("/profile/claude/"+name, b, 0600); e != nil {
			panic(e)
		}
	}
}

type claudeBackend struct {
	mode     string
	mu       sync.Mutex
	s        r.Session
	calls    int
	failures int
	requests int
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
	record("http.jsonl", map[string]any{"method": q.Method, "path": q.URL.Path, "query": q.URL.RawQuery, "headers": headers, "body": string(raw)})
	if readErr != nil || len(raw) > 2<<20 {
		b.failures++
		http.Error(w, "request cap", 413)
		return
	}
	if q.Method != "POST" || q.URL.Path != "/v1/messages" || q.URL.RawQuery != "" && q.URL.RawQuery != "beta=true" || q.Header.Get("Authorization") != "Bearer sfl-fake-access" || q.Header.Get("X-Api-Key") != "" {
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
		write("missing-tools.json", body)
		http.Error(w, "source/structured tools missing", 500)
		return
	}
	if b.calls == 2 && !claudeResult(body, "sfl_fixture_read_1") || b.calls == 3 && !claudeResult(body, "sfl_fixture_search_1") {
		b.failures++
		write("missing-result.json", body)
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
		input = r.Report{JobID: b.s.JobID, PolicyVersion: b.s.PolicyVersion, PacketHash: b.s.PacketHash, Provider: r.Claude, Round: 1, SnapshotHash: b.s.SnapshotHash, ConfigHash: b.s.ConfigHash, Findings: []r.Finding{{ID: "claude:fixture", Title: "Synthetic finding", Body: "Protocol fixture only.", Severity: 2, Anchor: r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}}}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}}
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

// This is a protocol fixture launcher. Its identity and stop checks are fake
// qualification inputs, not a production WorkerLauncher implementation. The
// outer fixture destroys the entire private PID namespace when this helper exits.
type protocolLauncher struct {
	s    r.Session
	base string
}

func (l protocolLauncher) Inspect(context.Context, r.Invocation) (r.Identity, error) {
	return l.s.Identity, nil
}
func (l protocolLauncher) Start(ctx context.Context, inv r.Invocation) (r.Worker, error) {
	cmd := exec.CommandContext(ctx, "/client", inv.Args...)
	cmd.Dir = "/work"
	cmd.Env = append([]string{"HOME=/profile", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "ANTHROPIC_BASE_URL=" + l.base, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "GOMAXPROCS=1"}, inv.Environment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		in.Close()
		return nil, e
	}
	stderr := &limited{max: 2 << 20}
	cmd.Stderr = stderr
	if e = cmd.Start(); e != nil {
		in.Close()
		out.Close()
		return nil, e
	}
	w := &protocolWorker{cmd: cmd, in: in, out: out, stderr: stderr, done: make(chan struct{}), raw: &limited{max: 2 << 20}}
	w.reader = &traceReader{ReadCloser: out, capture: w.raw}
	return w, nil
}

type traceReader struct {
	io.ReadCloser
	capture io.Writer
}

func (t *traceReader) Read(p []byte) (int, error) {
	n, e := t.ReadCloser.Read(p)
	if n > 0 {
		if _, err := t.capture.Write(p[:n]); err != nil {
			return n, err
		}
	}
	return n, e
}

type protocolWorker struct {
	cmd         *exec.Cmd
	in          io.WriteCloser
	out         io.ReadCloser
	reader      io.ReadCloser
	stderr, raw *limited
	once        sync.Once
	done        chan struct{}
	err         error
}

func (w *protocolWorker) Input() io.WriteCloser { return w.in }
func (w *protocolWorker) Output() io.ReadCloser { return w.reader }
func (w *protocolWorker) Wait() error {
	w.once.Do(func() { w.err = w.cmd.Wait(); close(w.done) })
	return w.err
}
func (w *protocolWorker) Stop(ctx context.Context) error {
	_ = w.in.Close()
	_ = w.out.Close()
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- w.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	_ = os.WriteFile("/artifacts/client-stdout.jsonl", w.raw.buf.Bytes(), 0600)
	_ = os.WriteFile("/artifacts/client-stderr.txt", w.stderr.buf.Bytes(), 0600)
	return nil
}
func claudeMain(mode string) {
	seedClaude()
	limits := r.DefaultLimits()
	limits.Timeout = 35 * time.Second
	snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, e := r.NewSession("native-fake-backend", snap, r.Digest("synthetic-only"), r.Claude, 1, "immutable source evidence", nil, nil, limits)
	if e != nil {
		panic(e)
	}
	reader, e := r.NewReader(store{}, snap, []string{"source.go"}, limits)
	if e != nil {
		panic(e)
	}
	inv, e := (r.Adapter{Provider: r.Claude, AuthDirectory: "/profile/claude", Reader: reader, SourceBridge: &r.SourceBridge{Program: "/helper", Directory: "/run/sfl", ReaderUID: 1201}}).Invocation(s, nil)
	if e != nil {
		panic(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		panic(e)
	}
	b := &claudeBackend{s: s, mode: mode}
	guard, policy, intake, closeUpstream, e := fixtureEgress(s, b)
	if e != nil {
		panic(e)
	}
	defer closeUpstream()
	server := &http.Server{Handler: intake, ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10}
	go server.Serve(listener)
	defer server.Close()
	base := "http://" + listener.Addr().String()
	write("invocation.json", map[string]any{"args": inv.Args, "environment": inv.Environment, "backend": base, "fake_only": true})
	driver := r.NativeDriver{Launcher: protocolLauncher{s: s, base: base}}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	data, _, err := driver.Execute(ctx, inv)
	var report r.Report
	decodeErr := json.Unmarshal(data, &report)
	var validation error
	if decodeErr == nil {
		validation = r.ValidateReport(report, s)
	} else {
		validation = decodeErr
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	stopErr := server.Shutdown(stopCtx)
	stopCancel()
	if stopErr != nil {
		_ = server.Close()
	}
	guardCtx, guardCancel := context.WithTimeout(context.Background(), time.Second)
	access, guardStopErr := guard.Close(guardCtx)
	guardCancel()
	guardErr := access.ValidateCompleted(s, policy)
	write("provider-access.json", access)
	write("provider-policy.json", policy)
	b.mu.Lock()
	calls := b.calls
	failures := b.failures
	requests := b.requests
	b.mu.Unlock()
	str := func(e error) string {
		if e == nil {
			return ""
		}
		return e.Error()
	}
	_, canaryErr := os.Stat("/work/undeclared-canary.txt")
	preDispatchDenied := mode == "guarded-bash" && err != nil && validation != nil && calls == 1 && requests == 1 && access.Failed && access.Stopped && access.InferenceRequests == 1 && guardErr != nil && os.IsNotExist(canaryErr)
	write("result.json", map[string]any{"provider": "claude", "case": mode, "provider_response_guarded": policy.ResponsePolicy == r.ClaudeSourceMessages, "forbidden_tool_pre_dispatch_denied": preDispatchDenied, "canary_exists": canaryErr == nil, "run_error": str(err), "report_error": str(validation), "report": report, "backend_stop_error": str(stopErr), "provider_stop_error": str(guardStopErr), "provider_access_error": str(guardErr), "backend_calls": calls, "backend_failures": failures, "backend_requests": requests, "fake_only": true, "identity_and_stop_inputs": "protocol fixture fakes; outer PID namespace only", "actual_account_qualified": false, "production_boundary_qualified": false})
	fmt.Printf("calls=%d run=%v report=%v\n", calls, err, validation)
	if mode == "guarded-bash" {
		if !preDispatchDenied || stopErr != nil || guardStopErr != nil || failures != 0 {
			os.Exit(1)
		}
		return
	}
	if err != nil || validation != nil || stopErr != nil || guardStopErr != nil || guardErr != nil || failures != 0 || requests != 3 {
		os.Exit(1)
	}
}
