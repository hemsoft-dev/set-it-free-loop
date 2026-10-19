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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This fake wrapper is intentionally NOT an official CLI, OS sandbox or usable
// production launcher. Only its protocol and process lifecycle are measured.
type fixtureWorker struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     io.ReadCloser
	once    sync.Once
	waitErr error
	stopped atomic.Bool
	stopErr error
}

func (w *fixtureWorker) Input() io.WriteCloser { return w.in }
func (w *fixtureWorker) Output() io.ReadCloser { return w.out }
func (w *fixtureWorker) Wait() error {
	w.once.Do(func() { w.waitErr = w.cmd.Wait() })
	return w.waitErr
}
func (w *fixtureWorker) Stop(context.Context) error {
	_ = w.in.Close()
	_ = w.out.Close()
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	_ = w.Wait()
	w.stopped.Store(true)
	return w.stopErr
}

type fixtureLauncher struct {
	identity     Identity
	mode         string
	workers      []*fixtureWorker
	inspectCalls int
	postIdentity *Identity
	startErr     error
	stopErr      error
}

func (l *fixtureLauncher) Inspect(context.Context, Invocation) (Identity, error) {
	l.inspectCalls++
	if l.inspectCalls%2 == 0 && l.postIdentity != nil {
		return *l.postIdentity, nil
	}
	return l.identity, nil
}
func (l *fixtureLauncher) Start(ctx context.Context, inv Invocation) (Worker, error) {
	if l.startErr != nil {
		return nil, l.startErr
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, "-test.run=^TestWorkerWrapperExecutable$")
	cmd.Env = []string{"LANG=C.UTF-8", "GORACE=atexit_sleep_ms=0", "SFL_WORKER_FIXTURE=" + l.mode}
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
	if err = ctx.Err(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, err
	}
	w := &fixtureWorker{cmd: cmd, in: input, out: output, stopErr: l.stopErr}
	l.workers = append(l.workers, w)
	return w, nil
}

func TestWorkerWrapperExecutable(t *testing.T) {
	mode := os.Getenv("SFL_WORKER_FIXTURE")
	if mode == "" {
		return
	}
	code := func() int {
		for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "HOME", "SSH_AUTH_SOCK"} {
			if os.Getenv(key) != "" {
				return 20
			}
		}
		if mode == "no-input" {
			time.Sleep(time.Minute)
			return 21
		}
		dec := json.NewDecoder(os.Stdin)
		var inv Invocation
		if dec.Decode(&inv) != nil {
			return 22
		}
		var s Session
		if json.Unmarshal(inv.Input[bytes.IndexByte(inv.Input, '\n')+1:], &s) != nil {
			return 23
		}
		switch mode {
		case "hang":
			time.Sleep(time.Minute)
			return 24
		case "oversized":
			fmt.Fprintln(os.Stdout, strings.Repeat("x", inv.OutputLimit+8192))
			return 0
		case "duplicate":
			fmt.Fprintln(os.Stdout, `{"version":1,"kind":"report","kind":"failure","state":"quota_wait"}`)
			return 0
		case "unknown":
			fmt.Fprintln(os.Stdout, `{"version":1,"kind":"shell","path":"canary"}`)
			return 0
		case "malformed":
			fmt.Fprintln(os.Stdout, `{"version":1`)
			return 0
		case "quota_wait", "auth_failed", "provider_unavailable":
			_ = json.NewEncoder(os.Stdout).Encode(WorkerMessage{Version: 1, Kind: "failure", State: mode})
			return 0
		case "flood":
			for i := 0; i < inv.Limits.ReaderCalls+2; i++ {
				_ = json.NewEncoder(os.Stdout).Encode(WorkerMessage{Version: 1, Kind: "read", Revision: s.Snapshot.Head, Path: "worker.go"})
				var r WorkerReply
				if dec.Decode(&r) != nil {
					return 0
				}
			}
			return 0
		}
		enc := json.NewEncoder(os.Stdout)
		if mode == "source" || mode == "denied" {
			path := "worker.go"
			if mode == "denied" {
				path = "../private-canary"
			}
			if enc.Encode(WorkerMessage{Version: 1, Kind: "read", Revision: s.Snapshot.Head, Path: path}) != nil {
				return 25
			}
			var reply WorkerReply
			if dec.Decode(&reply) != nil || reply.Version != 1 {
				return 26
			}
			if mode == "source" && (reply.Denied || reply.Text != "worker-only evidence") {
				return 27
			}
			if mode == "denied" && (!reply.Denied || reply.Text != "") {
				return 28
			}
			if mode == "source" {
				if enc.Encode(WorkerMessage{Version: 1, Kind: "search", Revision: s.Snapshot.Head, Needle: "worker-only", MaxResults: 1}) != nil {
					return 29
				}
				if dec.Decode(&reply) != nil || reply.Denied || len(reply.Matches) != 1 {
					return 30
				}
			}
		}
		raw, _ := json.Marshal(protocolReport(s))
		if enc.Encode(WorkerMessage{Version: 1, Kind: "report", Report: raw}) != nil {
			return 31
		}
		switch mode {
		case "trailing":
			fmt.Fprintln(os.Stdout, `{"version":1,"kind":"failure","state":"quota_wait"}`)
		case "nonzero":
			return 32
		case "after-report-hang":
			time.Sleep(time.Minute)
		}
		return 0
	}()
	os.Exit(code)
}

func workerFixtureAdapter(t *testing.T, p Provider, mode string) (Adapter, *fixtureLauncher) {
	t.Helper()
	identity, _ := PinnedIdentity(p)
	l := &fixtureLauncher{identity: identity, mode: mode}
	t.Cleanup(func() {
		for _, w := range l.workers {
			if !w.stopped.Load() {
				_ = w.Stop(context.Background())
			}
		}
	})
	reader, err := brokerReader(testSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	return Adapter{Provider: p, Driver: WorkerDriver{Launcher: l}, AuthDirectory: t.TempDir(), Reader: reader}, l
}
func assertFixtureStopped(t *testing.T, l *fixtureLauncher) {
	t.Helper()
	for _, w := range l.workers {
		// ProcessState exists only after Wait reaped the process. Exited can be
		// false for signal termination, so it is not the shutdown assertion.
		if !w.stopped.Load() || w.cmd.ProcessState == nil {
			t.Fatal("worker returned without reaping")
		}
	}
}
func TestWorkerDriverBothProvidersCompleteDurably(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK"} {
		t.Setenv(key, "worker-secret-canary")
	}
	store, job, remote := admittedFixture(t)
	engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{}}
	var launchers []*fixtureLauncher
	for _, p := range []Provider{Codex, Claude} {
		a, l := workerFixtureAdapter(t, p, "source")
		engine.Reviewers[p] = a
		launchers = append(launchers, l)
	}
	now := controlTime.Add(4 * time.Minute)
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
	if err != nil || out.State != "dual_complete" || len(out.Artifacts) != 4 {
		t.Fatal(out, err)
	}
	for _, a := range out.Artifacts {
		if a.SourceAccess == nil || a.SourceAccess.ToolCalls != 2 || len(a.SourceAccess.Reads) != 3 {
			t.Fatal("actual process source evidence missing", a.SourceAccess)
		}
	}
	for _, l := range launchers {
		assertFixtureStopped(t, l)
		if len(l.workers) != 2 || l.inspectCalls != 4 {
			t.Fatal("wrong sessions/pre-post inspection", len(l.workers), l.inspectCalls)
		}
	}
	saved, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Jobs[job.ID].LeaseToken != "" || len(saved.Jobs[job.ID].SourceAttempts) != 4 {
		t.Fatal("durable worker completion missing")
	}
	publisher := Publisher{Store: store, Remote: remote, Now: func() time.Time { return now }}
	intent, err := publisher.Prepare(context.Background(), out, map[string]map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.Dispatch(context.Background(), intent.ID); err != nil || remote.posts != 1 {
		t.Fatal("fake publication failed", err)
	}
}
func TestWorkerDriverRejectsMalformedAndIncompleteProcesses(t *testing.T) {
	for _, mode := range []string{"oversized", "duplicate", "unknown", "malformed", "trailing", "nonzero", "flood"} {
		t.Run(mode, func(t *testing.T) {
			a, l := workerFixtureAdapter(t, Codex, mode)
			s := testSession(Codex)
			s.Limits.ReaderCalls = 2
			_, access, err := a.ReviewSource(context.Background(), s)
			if err == nil || phaseState(err) != "invalid_output" {
				t.Fatal("invalid worker accepted", err)
			}
			if access == nil {
				t.Fatal("missing attempt receipt")
			}
			assertFixtureStopped(t, l)
		})
	}
}
func TestWorkerDriverDenialsPreserveCoverage(t *testing.T) {
	a, l := workerFixtureAdapter(t, Claude, "denied")
	r, access, err := a.ReviewSource(context.Background(), testSession(Claude))
	if err != nil || access.ToolCalls != 1 || len(access.Reads) != 0 || len(r.Coverage) == 0 {
		t.Fatal(r, access, err)
	}
	assertFixtureStopped(t, l)
}
func TestWorkerDriverTimeoutStopsBlockedIOAndTerminalHang(t *testing.T) {
	for _, mode := range []string{"hang", "no-input", "after-report-hang"} {
		t.Run(mode, func(t *testing.T) {
			a, l := workerFixtureAdapter(t, Codex, mode)
			s := testSession(Codex)
			if mode == "no-input" {
				s.Packet = strings.Repeat("x", 64<<10)
				s.PacketHash = Digest(s.Packet)
			}
			s.Limits.Timeout = 150 * time.Millisecond
			start := time.Now()
			_, _, err := a.ReviewSource(context.Background(), s)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
				t.Fatal("phase deadline did not stop process", err, time.Since(start))
			}
			assertFixtureStopped(t, l)
		})
	}
}
func TestWorkerDriverFailureStatesRemainCharged(t *testing.T) {
	for _, mode := range []string{"quota_wait", "auth_failed", "provider_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			a, l := workerFixtureAdapter(t, Claude, mode)
			_, _, err := a.ReviewSource(context.Background(), testSession(Claude))
			var pe PhaseError
			if !errors.As(err, &pe) || pe.State != mode || pe.BeforeWork {
				t.Fatal("failure relabeled or refunded", err)
			}
			assertFixtureStopped(t, l)
		})
	}
}
func TestWorkerStopFailureRetainsLeaseAndAttemptUntilRecovery(t *testing.T) {
	store, job, _ := admittedFixture(t)
	a, l := workerFixtureAdapter(t, Codex, "source")
	l.stopErr = errors.New("supervisor-stop-uncertain-secret-canary")
	engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{Codex: a}}
	now := controlTime.Add(4 * time.Minute)
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
	if !errors.Is(err, ErrWorkerStopPending) || strings.Contains(err.Error(), "secret-canary") || out.FailedSourceAccess == nil {
		t.Fatal(out, err)
	}
	d, _ := store.Snapshot(context.Background())
	j := d.Jobs[job.ID]
	if j.LeaseToken == "" || !j.Outstanding || j.Checkpoint.ConsumedSessions != 1 || len(j.SourceAttempts) != 0 || len(j.Checkpoint.Artifacts) != 0 {
		t.Fatal("uncertain worker released or accepted", j)
	}
	if _, err = store.Claim(context.Background(), job.ID, now.Add(time.Hour)); err == nil {
		t.Fatal("uncertain stopped worker lease stolen")
	}
	// The fixture actually reaped its process, but simulated a missing supervisor
	// attestation. A trusted operator recovers that fact and the retained audit.
	if err = store.RecordStoppedSourceAccess(context.Background(), job.ID, j.LeaseToken, j.AttemptID, out.FailedSourceAccess); err != nil {
		t.Fatal(err)
	}
	if err = store.ReleaseStopped(context.Background(), job.ID, j.LeaseToken, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	d, _ = store.Snapshot(context.Background())
	if d.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 || d.Jobs[job.ID].LeaseToken != "" {
		t.Fatal("recovery refunded uncertain work")
	}
	assertFixtureStopped(t, l)
}
func TestWorkerDriverRechecksTrustedIdentityAndNeverUsesWorkerIdentity(t *testing.T) {
	a, l := workerFixtureAdapter(t, Codex, "source")
	other := l.identity
	other.AuthMode = "api_key"
	l.postIdentity = &other
	_, _, err := a.ReviewSource(context.Background(), testSession(Codex))
	if phaseState(err) != "auth_failed" {
		t.Fatal("auth substitution accepted", err)
	}
	assertFixtureStopped(t, l)
}

func TestWorkerDriverRejectsInvalidInputBeforeLaunching(t *testing.T) {
	a, l := workerFixtureAdapter(t, Codex, "source")
	inv, err := a.Invocation(testSession(Codex), nil)
	if err != nil {
		t.Fatal(err)
	}
	inv.Input = make([]byte, inv.Limits.InputBytes+1)
	_, _, err = (WorkerDriver{Launcher: l}).Execute(context.Background(), inv)
	var pe PhaseError
	if !errors.As(err, &pe) || !pe.BeforeWork || len(l.workers) != 0 {
		t.Fatal("invalid input launched worker", err)
	}
	inv.Input = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = (WorkerDriver{Launcher: l}).Execute(ctx, inv)
	if !errors.Is(err, context.Canceled) || len(l.workers) != 0 {
		t.Fatal("cancelled phase launched worker", err)
	}
	_, _, err = (WorkerDriver{}).Execute(context.Background(), inv)
	if !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("missing launcher did not block", err)
	}
}
func TestWorkerDriverPrecheckAndStartFailureNeverExecuteProvider(t *testing.T) {
	a, l := workerFixtureAdapter(t, Codex, "source")
	l.identity.AuthMode = "api_key"
	_, _, err := a.ReviewSource(context.Background(), testSession(Codex))
	var pe PhaseError
	if phaseState(err) != "auth_failed" || !errors.As(err, &pe) || !pe.BeforeWork || len(l.workers) != 0 {
		t.Fatal("invalid subscription identity launched", err)
	}
	l.identity, _ = PinnedIdentity(Codex)
	l.startErr = errors.New("launch-private-path-secret-canary")
	_, _, err = a.ReviewSource(context.Background(), testSession(Codex))
	if !errors.As(err, &pe) || !pe.BeforeWork || strings.Contains(err.Error(), "secret-canary") || len(l.workers) != 0 {
		t.Fatal("before-start failure was not safely classified", err)
	}
}
