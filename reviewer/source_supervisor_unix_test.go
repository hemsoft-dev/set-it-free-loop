//go:build linux || darwin

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These reader roles are fake processes with real private pipe framing and a
// real source broker. Native provider roles use the existing fake executable.
// Calls into the fake reader model IPC; no OS identity isolation is claimed.
type managedReaderFixture struct {
	mode       string
	startErr   error
	startAfter bool
	stopErr    error
	configs    []SourceReaderConfig
	processes  []*managedReaderProcess
	current    *managedReaderProcess
}

type managedReaderProcess struct {
	config  SourceReaderConfig
	input   *io.PipeWriter
	audit   *io.PipeReader
	broker  *sourceBroker
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	stopErr error
	waitErr error
	stops   atomic.Int32
}

func (f *managedReaderFixture) StartReader(ctx context.Context, config SourceReaderConfig) (SourceReaderProcess, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	verifier, err := brokerReader(config.Session.Snapshot)
	if err != nil {
		return nil, err
	}
	broker, err := newSourceBroker(ctx, config.Session, verifier)
	if err != nil {
		return nil, err
	}
	in, input := io.Pipe()
	audit, out := io.Pipe()
	p := &managedReaderProcess{config: config, input: input, audit: audit, broker: broker, stop: make(chan struct{}), done: make(chan struct{}), stopErr: f.stopErr}
	f.configs, f.processes, f.current = append(f.configs, config), append(f.processes, p), p
	go func() {
		defer close(p.done)
		defer in.Close()
		defer out.Close()
		raw, err := io.ReadAll(in)
		expected, _ := json.Marshal(config)
		if err != nil || !bytes.Equal(raw, expected) {
			p.waitErr = errors.New("fake reader manifest mismatch")
			return
		}
		ready := SourceReaderEvent{Version: 1, Kind: "ready", SessionHash: Digest(config.Session), ManifestHash: Digest(config), LaunchID: config.LaunchID,
			Socket: filepath.Join(config.Root, Digest(config.Session), "source.sock"), ReaderUID: config.ReaderUID}
		if f.mode == "wrong-ready" {
			ready.LaunchID = strings.Repeat("f", 32)
		}
		if _, err := out.Write(encodeReaderAudit(ready)); err != nil {
			p.waitErr = err
			return
		}
		<-p.stop
		stopped := SourceReaderEvent{Version: 1, Kind: "stopped", SessionHash: Digest(config.Session), ManifestHash: Digest(config), LaunchID: config.LaunchID, Access: broker.seal()}
		switch f.mode {
		case "wrong-manifest":
			stopped.ManifestHash = strings.Repeat("f", 64)
		case "failed":
			stopped.Failed = true
			p.waitErr = errors.New("private-reader-canary")
		case "exit-error":
			p.waitErr = errors.New("private-reader-canary")
		}
		result := encodeReaderAudit(stopped)
		if f.mode == "truncated" {
			result = result[:len(result)-1]
		}
		_, _ = out.Write(result)
	}()
	if f.startAfter {
		return p, errors.New("private-started-reader-canary")
	}
	return p, nil
}

func (p *managedReaderProcess) Input() io.WriteCloser { return p.input }
func (p *managedReaderProcess) Audit() io.ReadCloser  { return p.audit }
func (p *managedReaderProcess) Wait() error {
	<-p.done
	return p.waitErr
}
func (p *managedReaderProcess) Stop(ctx context.Context) error {
	p.stops.Add(1)
	p.once.Do(func() { close(p.stop) })
	select {
	case <-p.done:
		return p.stopErr
	case <-ctx.Done():
		_ = p.input.Close()
		_ = p.audit.Close()
		return ctx.Err()
	}
}

type managedClientFixture struct {
	*nativeFixtureLauncher
	readers *managedReaderFixture
	starts  int
	omit    bool
}

func (f *managedClientFixture) Start(ctx context.Context, inv Invocation) (Worker, error) {
	f.starts++
	reader := f.readers.current
	if reader == nil || !readerInvocationMatches(reader.config, inv) || inv.Source != nil {
		return nil, errors.New("wrong private reader binding")
	}
	if _, err := reader.broker.Read(ctx, reader.config.Session.Snapshot.Head, "worker.go"); err != nil {
		return nil, err
	}
	if _, err := reader.broker.Search(ctx, reader.config.Session.Snapshot.Base, "source", 2); err != nil {
		return nil, err
	}
	if f.omit {
		_, _ = reader.broker.Read(ctx, reader.config.Session.Snapshot.Head, "missing.go")
	}
	return f.nativeFixtureLauncher.Start(ctx, inv)
}

func managedFixture(t *testing.T, provider Provider) (ManagedSourceReviewer, *managedReaderFixture, *managedClientFixture) {
	t.Helper()
	var a Adapter
	var client *nativeFixtureLauncher
	if provider == Claude {
		a, client, _ = claudeSourceFixture(t, "source-success")
	} else {
		a, client = nativeFixtureAdapter(t, provider, "success")
		a.SourceBridge = &SourceBridge{Program: "/opt/sfl/sourceproxy", Directory: "/run/sfl", ReaderUID: 1201}
	}
	reader, err := brokerReader(testSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	a.Reader = reader
	readers := &managedReaderFixture{}
	clients := &managedClientFixture{nativeFixtureLauncher: client, readers: readers}
	a.Driver = NativeDriver{Launcher: clients}
	return ManagedSourceReviewer{Adapter: a, Readers: readers, WorkerUID: 1202, WorkerGID: 1301}, readers, clients
}

func assertManagedStopped(t *testing.T, readers *managedReaderFixture, clients *managedClientFixture) {
	t.Helper()
	assertFixtureStopped(t, &clients.fixtureLauncher)
	for _, process := range readers.processes {
		select {
		case <-process.done:
		default:
			t.Fatal("reader role was not joined")
		}
		if process.stops.Load() != 1 {
			t.Fatal("reader did not receive exactly one physical-stop request")
		}
	}
}

func TestManagedSourceUsesPrivateBrokerReceiptForBothNativeProviders(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			r, readers, clients := managedFixture(t, provider)
			clients.omit = true
			s := testSession(provider)
			report, access, err := r.ReviewSource(context.Background(), s)
			if err != nil || access == nil || access.ToolCalls != 3 || access.ReaderCalls != 4 || len(access.Reads) != 3 || access.SessionHash != Digest(s) || len(access.Coverage) == 0 || len(report.Coverage) == 0 {
				t.Fatal("external broker access/omission lost", report, access, err)
			}
			reads, _ := r.Adapter.Reader.Evidence()
			if len(reads) != 0 || access.Reads[0].Path != "worker.go" || access.Reads[0].Hash != Digest([]byte("worker-only evidence")) {
				t.Fatal("worker access was replaced by citation reads", reads, access)
			}
			if clients.starts != 1 || len(readers.configs) != 1 || readers.configs[0].Session.Identity.Provider != provider || readers.configs[0].LaunchID == "" {
				t.Fatal("missing provider/session launch binding")
			}
			assertManagedStopped(t, readers, clients)
		})
	}
}

func TestManagedSourceTimeoutAndClientStartFailureStopBothRoles(t *testing.T) {
	for _, mode := range []string{"timeout", "client-start"} {
		t.Run(mode, func(t *testing.T) {
			r, readers, clients := managedFixture(t, Codex)
			s := testSession(Codex)
			if mode == "timeout" {
				s.Limits.Timeout = 100 * time.Millisecond
				clients.mode = "hang"
			} else {
				clients.startErr = errors.New("private-client-start-canary")
			}
			_, access, err := r.ReviewSource(context.Background(), s)
			if err == nil || access == nil || access.ToolCalls != 2 || strings.Contains(err.Error(), "canary") || errors.Is(err, ErrWorkerStopPending) || errors.Is(err, ErrSourceAuditPending) {
				t.Fatal("stopped failure lost actual reader audit", access, err)
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("timeout cause lost", err)
			}
			assertManagedStopped(t, readers, clients)
		})
	}
}

func TestManagedSourceStartedReaderFailureClosesManifestAndJoins(t *testing.T) {
	r, readers, clients := managedFixture(t, Codex)
	readers.startAfter = true
	start := time.Now()
	_, access, err := r.ReviewSource(context.Background(), testSession(Codex))
	if err == nil || access != nil || !errors.Is(err, ErrSourceAuditPending) || errors.Is(err, ErrWorkerStopPending) || clients.starts != 0 || time.Since(start) > time.Second || strings.Contains(err.Error(), "canary") {
		t.Fatal("partially launched reader was not closed/joined", access, err)
	}
	assertManagedStopped(t, readers, clients)
}

func TestManagedSourceRejectsBadPrivateAuditWithoutZeroReadFallback(t *testing.T) {
	for _, mode := range []string{"wrong-ready", "wrong-manifest", "truncated", "exit-error", "failed"} {
		t.Run(mode, func(t *testing.T) {
			r, readers, clients := managedFixture(t, Codex)
			readers.mode = mode
			_, access, err := r.ReviewSource(context.Background(), testSession(Codex))
			if err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatal("bad private audit accepted/disclosed", access, err)
			}
			if mode == "failed" {
				if access == nil || access.ToolCalls != 2 || errors.Is(err, ErrSourceAuditPending) {
					t.Fatal("valid failed-reader evidence was discarded", access, err)
				}
			} else if access != nil || !errors.Is(err, ErrSourceAuditPending) {
				t.Fatal("missing audit became a zero-read receipt", access, err)
			}
			if mode == "wrong-ready" && clients.starts != 0 {
				t.Fatal("client launched before matching private readiness")
			}
			assertManagedStopped(t, readers, clients)
		})
	}
}

func TestManagedSourcePreflightAndReaderStartFailureAvoidClientLaunch(t *testing.T) {
	for _, mode := range []string{"uid", "driver", "snapshot", "start"} {
		t.Run(mode, func(t *testing.T) {
			r, readers, clients := managedFixture(t, Codex)
			s := testSession(Codex)
			switch mode {
			case "uid":
				r.WorkerUID = r.Adapter.SourceBridge.ReaderUID
			case "driver":
				r.Adapter.Driver = WorkerDriver{Launcher: clients}
			case "snapshot":
				s.Snapshot.PR++
			case "start":
				readers.startErr = errors.New("private-start-canary")
			}
			_, access, err := r.ReviewSource(context.Background(), s)
			var phase PhaseError
			if err == nil || access != nil || !errors.As(err, &phase) || !phase.BeforeWork || clients.starts != 0 || strings.Contains(err.Error(), "canary") || len(readers.processes) != 0 {
				t.Fatal("invalid managed launch started work/disclosed backend error", access, err)
			}
		})
	}
}

func TestManagedSourceAuditLossAndEitherUncertainStopRetainDurableLease(t *testing.T) {
	for _, mode := range []string{"truncated", "reader-stop", "client-stop"} {
		t.Run(mode, func(t *testing.T) {
			store, job, _ := admittedFixture(t)
			r, readers, clients := managedFixture(t, Codex)
			switch mode {
			case "truncated":
				readers.mode = mode
			case "reader-stop":
				readers.stopErr = errors.New("private-reader-stop-canary")
			case "client-stop":
				clients.stopErr = errors.New("private-client-stop-canary")
			}
			engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{Codex: r}}
			now := controlTime.Add(4 * time.Minute)
			_, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
			if err == nil || (mode == "truncated" && !errors.Is(err, ErrSourceAuditPending)) || (mode != "truncated" && !errors.Is(err, ErrWorkerStopPending)) || strings.Contains(err.Error(), "canary") {
				t.Fatal("uncertainty lost/disclosed", err)
			}
			saved, err := store.Snapshot(context.Background())
			if err != nil || saved.Jobs[job.ID].LeaseToken == "" || !saved.Jobs[job.ID].Outstanding || saved.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 || len(saved.Jobs[job.ID].SourceAttempts) != 0 {
				t.Fatal("uncertain/auditless attempt released or uncharged", saved.Jobs[job.ID], err)
			}
			assertManagedStopped(t, readers, clients)
		})
	}
}

func TestManagedSourceReceiptsPersistThroughFourSessionsAndFakePublication(t *testing.T) {
	store, job, remote := admittedFixture(t)
	engine := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{}}
	var readers []*managedReaderFixture
	var clients []*managedClientFixture
	for _, p := range []Provider{Codex, Claude} {
		r, reader, client := managedFixture(t, p)
		engine.Reviewers[p] = r
		readers, clients = append(readers, reader), append(clients, client)
	}
	now := controlTime.Add(4 * time.Minute)
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return now })
	if err != nil || out.State != "dual_complete" || len(out.Artifacts) != 4 {
		t.Fatal(out, err)
	}
	nonces := map[string]bool{}
	for i, reader := range readers {
		assertManagedStopped(t, reader, clients[i])
		if len(reader.configs) != 2 {
			t.Fatal("wrong phase count")
		}
		for _, config := range reader.configs {
			if nonces[config.LaunchID] {
				t.Fatal("launch identity reused")
			}
			nonces[config.LaunchID] = true
		}
	}
	for _, artifact := range out.Artifacts {
		if artifact.SourceAccess == nil || artifact.SourceAccess.ToolCalls != 2 || artifact.SourceAccess.ReaderCalls != 3 || len(artifact.ValidationReads) != 0 || artifact.SourceAccess.SessionHash != artifact.InputHash {
			t.Fatal("actual external reads not captured", artifact)
		}
	}
	saved, err := store.Snapshot(context.Background())
	if err != nil || len(saved.Jobs[job.ID].SourceAttempts) != 4 || saved.Jobs[job.ID].LeaseToken != "" || saved.Jobs[job.ID].Outstanding {
		t.Fatal("private receipts not durably recorded before lease release", saved.Jobs[job.ID], err)
	}
	for _, attempt := range saved.Jobs[job.ID].SourceAttempts {
		if attempt.ID == "" || attempt.Access == nil || attempt.Access.ToolCalls != 2 {
			t.Fatal("missing attempt-bound read audit", attempt)
		}
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
