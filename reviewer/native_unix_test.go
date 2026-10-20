//go:build linux || darwin

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This executable fixture speaks native CLI envelopes. It is deliberately not
// an official client, provider backend, credential store or isolation launcher.
func TestNativeCLIExecutable(t *testing.T) {
	mode := os.Getenv("SFL_NATIVE_FIXTURE")
	if mode == "" {
		return
	}
	code := func() int {
		for _, key := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK"} {
			if os.Getenv(key) != "" {
				return 20
			}
		}
		if mode == "no-input" {
			time.Sleep(time.Minute)
			return 21
		}
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, int64(DefaultLimits().InputBytes)+1))
		_, data, ok := bytes.Cut(raw, []byte{'\n'})
		var s Session
		if err != nil || !ok || json.Unmarshal(data, &s) != nil || s.Validate() != nil {
			return 22
		}
		switch mode {
		case "hang":
			time.Sleep(time.Minute)
			return 23
		case "oversized":
			fmt.Print(strings.Repeat("x", s.Limits.OutputBytes+1))
			return 0
		case "transport-oversized":
			fmt.Print(strings.Repeat("x", NativeTransportLimit(s)+1))
			return 0
		case "malformed":
			fmt.Print("native-private-payload-canary")
			return 0
		case "wrapper":
			_ = json.NewEncoder(os.Stdout).Encode(WorkerMessage{Version: 1, Kind: "report", Report: json.RawMessage(`{}`)})
			return 0
		case "quota", "auth", "unavailable":
			if s.Identity.Provider == Claude {
				status := map[string]int{"quota": 429, "auth": 401, "unavailable": 503}[mode]
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "subtype": "success", "is_error": true, "api_error_status": status, "result": "native-private-payload-canary"})
			} else {
				fmt.Println(`{"type":"turn.started"}`)
				fmt.Println(`{"type":"turn.failed","error":{"message":"quota authentication native-private-payload-canary"}}`)
			}
			return 1
		}
		r := fakeReport(s)
		r.Usage = Usage{999, 999, true, 999}
		if mode == "wrong-session" {
			r.JobID = "forged-job"
		}
		if s.Identity.Provider == Codex {
			if mode == "incomplete" {
				fmt.Println(`{"type":"turn.failed"}`)
				return 0
			}
			fmt.Println("{\"type\":\"thread.started\"}\n{\"type\":\"turn.started\"}")
			if mode == "large-source" {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "item.completed", "item": map[string]any{"type": "mcp_tool_call", "server": "sfl-source", "tool": "read", "result": map[string]any{"content": []any{map[string]string{"type": "text", "text": strings.Repeat("s", s.Limits.OutputBytes+1)}}}}})
			}
			report, _ := json.Marshal(r)
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(report)}})
			fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":4}}`)
		} else {
			subtype, failed := "success", false
			if mode == "incomplete" {
				subtype, failed = "error_max_turns", true
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "subtype": subtype, "is_error": failed, "structured_output": r, "num_turns": 1, "usage": map[string]int{"input_tokens": 3, "output_tokens": 4}})
		}
		switch mode {
		case "valid-then-exit":
			return 24
		case "valid-then-hang":
			time.Sleep(time.Minute)
		case "trailing":
			fmt.Println("trailing-private-canary")
		}
		return 0
	}()
	os.Exit(code)
}

type nativeFixtureLauncher struct {
	fixtureLauncher
	fixtureTest string
	auditPath   string
}

func (l *nativeFixtureLauncher) Start(ctx context.Context, inv Invocation) (Worker, error) {
	if l.startErr != nil && !l.startThenError {
		return nil, l.startErr
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	test := l.fixtureTest
	if test == "" {
		test = "TestNativeCLIExecutable"
	}
	cmd := exec.Command(binary, "-test.run=^"+test+"$")
	cmd.Env = []string{"LANG=C.UTF-8", "GORACE=atexit_sleep_ms=0", "SFL_NATIVE_FIXTURE=" + l.mode}
	if l.auditPath != "" {
		cmd.Env = append(cmd.Env, "SFL_NATIVE_FIXTURE_AUDIT="+l.auditPath)
		config, err := capturedClaudeSource(inv)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(config)
		cmd.Env = append(cmd.Env, "SFL_NATIVE_FIXTURE_MCP="+string(data))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, err
	}
	if err = ctx.Err(); err == nil {
		err = cmd.Start()
	}
	if err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, err
	}
	w := &fixtureWorker{cmd: cmd, in: input, out: output, stopErr: l.stopErr}
	l.workers = append(l.workers, w)
	return w, l.startErr
}

func nativeFixtureAdapter(t *testing.T, p Provider, mode string) (Adapter, *nativeFixtureLauncher) {
	t.Helper()
	a, previous := workerFixtureAdapter(t, p, mode)
	l := &nativeFixtureLauncher{fixtureLauncher: *previous}
	a.Driver = NativeDriver{Launcher: l}
	t.Cleanup(func() {
		for _, w := range l.workers {
			if !w.stopped.Load() {
				_ = w.Stop(context.Background())
			}
		}
	})
	return a, l
}

func TestNativeDriverBothProvidersCompleteDurably(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK"} {
		t.Setenv(key, "native-parent-secret-canary")
	}
	store, job, remote := admittedFixture(t)
	engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{}}
	var launchers []*nativeFixtureLauncher
	for _, p := range []Provider{Codex, Claude} {
		a, l := nativeFixtureAdapter(t, p, "success")
		engine.Reviewers[p] = a
		launchers = append(launchers, l)
	}
	now := controlTime.Add(4 * time.Minute)
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
	if err != nil || out.State != "dual_complete" || len(out.Artifacts) != 4 {
		t.Fatal(out, err)
	}
	for _, artifact := range out.Artifacts {
		if artifact.Report.Usage != (Usage{3, 4, true, 1}) || artifact.SourceAccess == nil || artifact.SourceAccess.ToolCalls != 0 {
			t.Fatal("native usage/empty source receipt differs", artifact)
		}
	}
	for _, l := range launchers {
		assertFixtureStopped(t, &l.fixtureLauncher)
		if len(l.workers) != 2 || l.inspectCalls != 4 {
			t.Fatal("missing native sessions or identity recheck")
		}
	}
	publisher := Publisher{Store: store, Remote: remote, Now: func() time.Time { return now }}
	intent, err := publisher.Prepare(context.Background(), out, map[string]map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.Dispatch(context.Background(), intent.ID); err != nil || remote.posts != 1 {
		t.Fatal("native fake publication failed", err)
	}
}

func TestNativeDriverRejectsIncompleteMalformedAndFailedProcesses(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		for _, mode := range []string{"oversized", "malformed", "wrapper", "incomplete", "wrong-session", "valid-then-exit", "trailing"} {
			t.Run(string(p)+"/"+mode, func(t *testing.T) {
				a, l := nativeFixtureAdapter(t, p, mode)
				_, err := a.Review(context.Background(), testSession(p))
				state := "invalid_output"
				if mode == "incomplete" {
					state = "provider_unavailable"
					if p == Claude {
						state = "incomplete_expired"
					}
				}
				if err == nil || strings.Contains(err.Error(), "canary") || phaseState(err) != state {
					t.Fatal("accepted/disclosed failed native output", err)
				}
				assertFixtureStopped(t, &l.fixtureLauncher)
			})
		}
	}
}

func TestNativeDriverTimeoutReapsPromptAndCompletionHangs(t *testing.T) {
	for _, mode := range []string{"no-input", "hang", "valid-then-hang"} {
		t.Run(mode, func(t *testing.T) {
			a, l := nativeFixtureAdapter(t, Codex, mode)
			s := testSession(Codex)
			s.Limits.Timeout = 150 * time.Millisecond
			_, err := a.Review(context.Background(), s)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			assertFixtureStopped(t, &l.fixtureLauncher)
		})
	}
}

func TestNativeDriverUncertainStopRetainsDurableLease(t *testing.T) {
	store, job, _ := admittedFixture(t)
	a, l := nativeFixtureAdapter(t, Codex, "success")
	l.stopErr = errors.New("supervisor failure")
	engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{Codex: a}}
	now := controlTime.Add(4 * time.Minute)
	_, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
	if !errors.Is(err, ErrWorkerStopPending) {
		t.Fatal("lost physical stop uncertainty", err)
	}
	saved, err := store.Snapshot(context.Background())
	if err != nil || saved.Jobs[job.ID].LeaseToken == "" {
		t.Fatal("released uncertain native worker", err)
	}
}

func TestNativeDriverRejectsInvalidInvocationBeforeLaunch(t *testing.T) {
	a, l := nativeFixtureAdapter(t, Codex, "success")
	inv, err := a.Invocation(testSession(Codex), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Invocation){
		func(v *Invocation) { v.Input = []byte("missing session") },
		func(v *Invocation) { v.Provider = Claude },
		func(v *Invocation) { v.OutputLimit++ },
		func(v *Invocation) { v.Limits.MaxTurns-- },
		func(v *Invocation) { v.Input = bytes.Repeat([]byte("x"), v.Limits.InputBytes+1) },
	} {
		bad := inv
		mutate(&bad)
		_, _, err := a.Driver.Execute(context.Background(), bad)
		if err == nil || len(l.workers) != 0 {
			t.Fatal("invalid native session launched", err)
		}
	}
}

func TestNativeDriverClassifiesStructuredFailuresWithoutTextOrRefund(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		for _, mode := range []string{"quota", "auth", "unavailable"} {
			t.Run(string(p)+"/"+mode, func(t *testing.T) {
				a, l := nativeFixtureAdapter(t, p, mode)
				_, err := a.Review(context.Background(), testSession(p))
				state := "provider_unavailable"
				if p == Claude {
					state = map[string]string{"quota": "quota_wait", "auth": "auth_failed", "unavailable": "provider_unavailable"}[mode]
				}
				var failure PhaseError
				if !errors.As(err, &failure) || failure.BeforeWork || failure.State != state || strings.Contains(err.Error(), "canary") {
					t.Fatal("native failure misclassified or refunded", err)
				}
				assertFixtureStopped(t, &l.fixtureLauncher)
			})
		}
	}
}

func TestNativeFailureDurabilityPreservesConsumedWork(t *testing.T) {
	for _, mode := range []string{"quota", "auth", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			store, job, _ := admittedFixture(t)
			codex, cl := nativeFixtureAdapter(t, Codex, "success")
			claude, al := nativeFixtureAdapter(t, Claude, mode)
			engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{Codex: codex, Claude: claude}}
			now := controlTime.Add(4 * time.Minute)
			out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
			state := map[string]string{"quota": "quota_wait", "auth": "auth_failed", "unavailable": "provider_unavailable"}[mode]
			if err == nil || out.State != state {
				t.Fatal(out, err)
			}
			saved, err := store.Snapshot(context.Background())
			j := saved.Jobs[job.ID]
			if err != nil || j.State != state || j.LeaseToken != "" || j.Checkpoint.ConsumedSessions != 2 || len(j.Checkpoint.Artifacts) != 1 || len(j.SourceAttempts) != 2 {
				t.Fatal("native failure lost charge, receipt or valid phase", j, err)
			}
			assertFixtureStopped(t, &cl.fixtureLauncher)
			assertFixtureStopped(t, &al.fixtureLauncher)
		})
	}
}

func TestNativeDriverTransportAndReportLimitsRemainSeparate(t *testing.T) {
	a, l := nativeFixtureAdapter(t, Codex, "large-source")
	_, access, err := a.ReviewSource(context.Background(), testSession(Codex))
	if err != nil || access == nil || access.ToolCalls != 0 {
		t.Fatal("transport cap rejected source traffic or fabricated reads", err, access)
	}
	assertFixtureStopped(t, &l.fixtureLauncher)
	a, l = nativeFixtureAdapter(t, Codex, "transport-oversized")
	s := testSession(Codex)
	s.Limits.ReaderBytes, s.Limits.ReaderCalls, s.Limits.OutputBytes = 64, 1, 4096
	if _, err := a.Review(context.Background(), s); err == nil {
		t.Fatal("native process exceeded captured transport bound")
	}
	assertFixtureStopped(t, &l.fixtureLauncher)
}
