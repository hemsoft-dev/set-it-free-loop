//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func controlRequestFixture(t *testing.T, p Provider) nativeVMRequest {
	t.Helper()
	q := nativeRequestFixture(t, p)
	input := controlInputFixture(p)
	capability, err := ControlCapabilitySession(input)
	if err != nil {
		t.Fatal(err)
	}
	q.Version = 2
	q.Task = "evaluation-control-v1"
	q.Control = &input
	q.Session = capability
	q.Binding.SessionHash = Digest(capability)
	if !q.valid() {
		t.Fatal("invalid control request")
	}
	return q
}
func TestNativeControlRequestBindsExactTaskAndPreservesReviewWire(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		q := controlRequestFixture(t, p)
		client, server := vmSocketPair(t)
		done := make(chan error, 1)
		go func() {
			got, err := readVMRecord[nativeVMRequest](server, 1<<20)
			if err != nil || !got.valid() || Digest(got) != Digest(q) {
				done <- ErrVMBoot
				return
			}
			done <- writeVMRecord(server, nativeVMReady{1, got.Binding, Digest(got), got.Session.Identity, "native-vm-profile-1"}, 4096)
		}()
		if err := writeVMRecord(client, q, 1<<20); err != nil {
			t.Fatal(err)
		}
		ready, err := readNativeVMReady(client, q, nil)
		if err != nil || !ready.valid(q) {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		inv, err := nativeGuestInvocation(q, "http://127.0.0.1:49152")
		if err != nil || inv.Control == nil || !readerInvocationMatches(q.readerConfig(), inv) || !bytes.Equal(q.reportSchema(), ControlReportSchema()) {
			t.Fatal("guest lost typed source-bound task", err)
		}
		raw, _ := json.Marshal(fakeControl(*q.Control))
		if q.validateReport(raw) != nil {
			t.Fatal("control output refused")
		}
		raw, _ = json.Marshal(fakeReport(q.Session))
		if q.validateReport(raw) == nil {
			t.Fatal("control accepted peer report")
		}
		for _, mutate := range []func(*nativeVMRequest){func(q *nativeVMRequest) { q.Version = 1 }, func(q *nativeVMRequest) { q.Task = "review" }, func(q *nativeVMRequest) { q.Control = nil }, func(q *nativeVMRequest) { q.Session.JobID = "foreign" }, func(q *nativeVMRequest) { q.Control.Step = 2 }} {
			wire, _ := json.Marshal(q)
			var bad nativeVMRequest
			json.Unmarshal(wire, &bad)
			mutate(&bad)
			if bad.valid() || ready.valid(bad) {
				t.Fatal("substituted task admitted")
			}
		}
		legacy := nativeRequestFixture(t, p)
		old := struct {
			Version      int
			Binding      AuthBinding
			Session      Session
			Paths        []string
			Policy       EgressPolicy
			ProfileHints NativeVMProfileHints
		}{legacy.Version, legacy.Binding, legacy.Session, legacy.Paths, legacy.Policy, legacy.ProfileHints}
		a, _ := json.Marshal(legacy)
		b, _ := json.Marshal(old)
		if !bytes.Equal(a, b) {
			t.Fatal("old review request hash changed")
		}
	}
}
func TestNativeControlGatesAndInvalidHistoryPrecedeAssetsAuthAndTransport(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		input := controlInputFixture(provider)
		reader, err := testEngine(t, nil).Reader(input.Job.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		policy := egressTestPolicy(provider)
		policy.ResponsePolicy = CodexSourceResponses
		withCodexRequestPolicy(&policy)
		if provider == Claude {
			policy.ResponsePolicy = ClaudeSourceMessages
		}
		calls, checks := 0, 0
		options := NativeVMOptions{Provider: provider, Root: "/unopened/native-root", Stub: "/unopened/stub", StubHash: strings.Repeat("a", 64), Auth: &AuthStore{provider: provider, directory: "/unopened/auth"}, Reader: reader, Policy: policy, Transport: egressRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, ErrLiveBlocked }), Check: func(context.Context, Session) error { checks++; return ErrLiveBlocked }}
		worker, err := NewNativeVMControlReviewer(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err = worker.CritiqueAudited(context.Background(), input); !errors.Is(err, ErrLiveBlocked) || checks != 1 || calls != 0 {
			t.Fatal("closed gate reached resources", checks, calls, err)
		}
		bad := nextControlInput(input)
		bad.History[0].InputHash = Digest("forged")
		if _, _, _, err = worker.CritiqueAudited(context.Background(), bad); !errors.Is(err, ErrLiveBlocked) || checks != 1 || calls != 0 {
			t.Fatal("invalid history reached eligibility", checks, calls, err)
		}
		worker.reviewer.options.Check = func(context.Context, Session) error { checks++; return nil }
		poisoned := nextControlInput(input)
		poisoned.History[0].Findings[0].Anchor.Quote = "absent source quote"
		_, _, _, err = worker.CritiqueAudited(context.Background(), poisoned)
		var failure PhaseError
		if !errors.As(err, &failure) || !failure.BeforeWork || failure.State != "invalid_output" || checks != 2 || calls != 0 {
			t.Fatal("forged prior source reached assets/auth", checks, calls, err)
		}
	}
}
func TestNativeControlStoppedSourceValidationRejectsFalseEvidence(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		q := controlRequestFixture(t, p)
		reader, err := testEngine(t, nil).Reader(q.Session.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		r := fakeControl(*q.Control)
		access := &SourceAccess{Coverage: []Coverage{}}
		got, err := validateNativeVMTaskSource(context.Background(), q, nativeVMTaskReport{control: &r}, access, reader)
		if err != nil || got.control == nil || got.control.InputHash != Digest(*q.Control) {
			t.Fatal("valid control source refused", err)
		}
		r.Findings[0].Anchor.Quote = "absent source quote"
		if _, err = validateNativeVMTaskSource(context.Background(), q, nativeVMTaskReport{control: &r}, access, reader); err == nil {
			t.Fatal("control accepted unverified source")
		}
		if _, err = validateNativeVMTaskSource(context.Background(), q, nativeVMTaskReport{review: fakeReport(q.Session)}, access, reader); err == nil {
			t.Fatal("control accepted wrong typed result")
		}
	}
}

func nativeControlHostPhaseFixture(t *testing.T) (*nativeVMHostPhase, *fakeContainmentManager) {
	t.Helper()
	q := controlRequestFixture(t, Codex)
	q.Binding.LaunchID = strings.Repeat("a", 32)
	s, manager, _ := systemdSupervisorFixture(t)
	s.payloadKind = "native-vm"
	supervisor := &NativeVMSupervisor{s}
	manager.observation.MemoryMax = 3 << 30
	manager.observation.TasksMax = 128
	manager.kernel.group.Path = "/user.slice/user-1000.slice/sfl-alpha-" + q.Binding.LaunchID + ".service"
	manager.observation.ControlGroup = manager.kernel.group.Path
	spec := vmSpec(t)
	spec.Binding = q.Binding
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { images.Close() })
	manifest, err := images.NativeManifest(q.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Prepare(manifest); err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Start(context.Background(), q.Binding); err != nil {
		t.Fatal(err)
	}
	channels, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { channels.Close() })
	reader, err := NewReader(&fixtureStore{files: map[string]Object{}}, q.Session.Snapshot, q.Paths, q.Session.Limits)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeVMHostPhase{request: q, manifest: manifest, supervisor: supervisor, channels: channels, reader: reader, started: true}, manager
}

func TestNativeControlInvalidOutputStillRequiresStoppedAuthReturn(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "stopped"
		if uncertain {
			name = "uncertain"
		}
		t.Run(name, func(t *testing.T) {
			phase, manager := nativeControlHostPhaseFixture(t)
			store := authTestStore(t, Codex)
			profile, err := store.LeaseProfile(context.Background(), phase.request.Binding)
			if err != nil {
				t.Fatal(err)
			}
			phase.profile = profile
			seed, err := profile.ExportAuth(context.Background(), authExportCheck(func(context.Context, AuthBinding) error { return nil }))
			if err != nil {
				t.Fatal(err)
			}
			defer seed.Close()
			phase.exported = true
			info, _ := seed.Info()
			returned, err := NewAuthReturn(info, []byte("synthetic-control-return"))
			if err != nil {
				t.Fatal(err)
			}
			phase.finished = &nativeVMFinished{Version: 1, Binding: phase.request.Binding, RequestHash: Digest(phase.request), State: "invalid_output", AuthReturn: authWire(t, returned), SourceAudit: nativeHostAuditFixture(t, phase.request)}
			attachIdleNativeProviderBridge(t, phase)
			if uncertain {
				manager.kernel.changed = true
			}
			report, _, _, err := phase.finishTask(context.Background(), nativeVMTaskReport{}, PhaseError{State: "invalid_output"})
			if err == nil || report.control != nil || report.review.JobID != "" {
				t.Fatal("failed control exposed a report", err)
			}
			raw, readErr := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if uncertain {
				if !errors.Is(err, ErrWorkerStopPending) || string(raw) == "synthetic-control-return" {
					t.Fatal("uncertain stop committed auth", err)
				}
				manager.kernel.changed = false
				if err = phase.supervisor.AbortAndWait(context.Background(), phase.request.Binding); err != nil {
					t.Fatal(err)
				}
				phase.supervisor.CleanupRuntime(context.Background(), phase.request.Binding)
				if store.Refresh(context.Background(), func(old []byte) ([]byte, error) { return old, nil }) == nil {
					t.Fatal("uncertain exported lease was discarded")
				}
			} else if string(raw) != "synthetic-control-return" || !phase.authReturned || manager.stops != 1 {
				t.Fatal("stopped invalid output lost matching auth", manager.stops, phase.authReturned)
			}
		})
	}
}

type controlCountingStore struct {
	store ObjectStore
	reads int
}

func (s *controlCountingStore) Read(ctx context.Context, revision, path string, limit int) (Object, error) {
	s.reads++
	return s.store.Read(ctx, revision, path, limit)
}

type slowControlHistoryStore struct {
	reads int
}

func (s *slowControlHistoryStore) Read(ctx context.Context, revision, path string, limit int) (Object, error) {
	s.reads++
	select {
	case <-time.After(100 * time.Millisecond):
		return Object{Mode: "100644", Data: []byte("x\n")}, nil
	case <-ctx.Done():
		return Object{}, ctx.Err()
	}
}

func TestNativeControlHistoryDoesNotRepeatSlowImmutableReads(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(strconv.FormatBool(forged), func(t *testing.T) {
			input := controlInputFixture(Codex)
			input.Limits.Timeout = 250 * time.Millisecond
			input.Job.Config.Limits = input.Limits
			anchor := Anchor{Path: "source.go", Line: 1, Quote: "x", Revision: input.Job.Snapshot.Head}
			for step := 1; step <= 3; step++ {
				report := ControlReport{InputHash: Digest(input), JobHash: Digest(input.Job), Step: step, Findings: []Finding{}, Judgments: []Judgment{}, Retractions: []string{}, Coverage: []Coverage{}, Usage: Usage{Turns: 1}}
				if step == 1 {
					report.Findings = append(report.Findings, Finding{ID: "codex:slow", Title: "Synthetic", Body: "Synthetic source evidence", Severity: 2, Anchor: anchor})
				} else {
					report.Judgments = append(report.Judgments, Judgment{FindingID: "codex:slow", Verdict: "confirm", Reason: "same frozen source", Evidence: []Anchor{anchor}})
				}
				if step == 3 && forged {
					report.Judgments[0].Evidence[0].Quote = "forged"
				}
				if err := ValidateControlReport(report, input); err != nil {
					t.Fatal("invalid slow history fixture", step, err)
				}
				input.History = append(input.History, report)
				input.Step++
			}
			store := &slowControlHistoryStore{}
			reader, err := NewReader(store, input.Job.Snapshot, []string{"source.go"}, input.Limits)
			if err != nil {
				t.Fatal(err)
			}
			policy := egressTestPolicy(Codex)
			policy.ResponsePolicy = CodexSourceResponses
			withCodexRequestPolicy(&policy)
			worker, err := NewNativeVMControlReviewer(NativeVMOptions{Provider: Codex, Root: "/unopened/native-root", Stub: "/unopened/stub", StubHash: strings.Repeat("a", 64), Auth: &AuthStore{provider: Codex, directory: "/unopened/auth"}, Reader: reader, Policy: policy, Transport: egressRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("history check reached HTTP")
				return nil, ErrLiveBlocked
			}), Check: func(context.Context, Session) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = worker.CritiqueAudited(context.Background(), input)
			// Three independent frozen quote checks need one immutable object. Without
			// deduplication the third read consumes the deadline before boot validation.
			// Missing boot assets must stop this fixture after successful source checks.
			if err == nil || errors.Is(err, ErrSourceService) != forged || store.reads != 1 {
				t.Fatal("repeated history I/O consumed the phase deadline", store.reads, err)
			}
			if reads, _ := reader.Evidence(); len(reads) != 0 {
				t.Fatal("history replay consumed the current reader")
			}
		})
	}
}

func TestNativeControlHistoryHasPerReportVerificationBudget(t *testing.T) {
	input := controlInputFixture(Codex)
	files := map[string]Object{}
	paths := []string{}
	active := []Finding{}
	for step := 1; step <= 3; step++ {
		report := ControlReport{InputHash: Digest(input), JobHash: Digest(input.Job), Step: step, Findings: []Finding{}, Judgments: []Judgment{}, Retractions: []string{}, Coverage: []Coverage{}, Usage: Usage{Turns: 1}}
		for _, prior := range active {
			report.Judgments = append(report.Judgments, Judgment{FindingID: prior.ID, Verdict: "confirm", Reason: "fixed evidence", Evidence: []Anchor{prior.Anchor}})
		}
		for n := 0; n < 30; n++ {
			id := strconv.Itoa(len(active) + n)
			path := "object" + id + ".go"
			finding := Finding{ID: "codex:f" + id, Title: "Synthetic", Body: "Synthetic evidence only", Severity: 2, Anchor: Anchor{Path: path, Line: 1, Quote: "x", Revision: input.Job.Snapshot.Head}}
			report.Findings = append(report.Findings, finding)
			files[path] = Object{Mode: "100644", Data: []byte("x\n")}
			paths = append(paths, path)
		}
		if err := ValidateControlReport(report, input); err != nil {
			t.Fatal("invalid bounded history fixture", step, err)
		}
		active = append(active, report.Findings...)
		input.History = append(input.History, report)
		input.Step++
	}
	store := &controlCountingStore{store: &fixtureStore{files: files}}
	reader, err := NewReader(store, input.Job.Snapshot, paths, input.Limits)
	if err != nil {
		t.Fatal(err)
	}
	policy := egressTestPolicy(Codex)
	policy.ResponsePolicy = CodexSourceResponses
	withCodexRequestPolicy(&policy)
	checks := 0
	worker, err := NewNativeVMControlReviewer(NativeVMOptions{Provider: Codex, Root: "/unopened/native-root", Stub: "/unopened/stub", StubHash: strings.Repeat("a", 64), Auth: &AuthStore{provider: Codex, directory: "/unopened/auth"}, Reader: reader, Policy: policy, Transport: egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("history check reached HTTP")
		return nil, ErrLiveBlocked
	}), Check: func(context.Context, Session) error { checks++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = worker.CritiqueAudited(context.Background(), input)
	// Missing boot assets stop this no-auth test after source preflight. The
	// Per-report Readers still account for 30+60+90 reads, within their own128.
	// The immutable store sees only90 unique objects across those verifications.
	if err == nil || checks != 1 || store.reads != 90 {
		t.Fatal("valid history exhausted a shared reader budget", checks, store.reads, err)
	}
	reads, _ := reader.Evidence()
	if len(reads) != 0 {
		t.Fatal("preflight consumed the caller's reader")
	}
}

func TestNativeRejectedTaskRetainsOriginalPrivateReportHash(t *testing.T) {
	for _, control := range []bool{false, true} {
		name := "review"
		if control {
			name = "control"
		}
		t.Run(name, func(t *testing.T) {
			var phase *nativeVMHostPhase
			if control {
				phase, _ = nativeControlHostPhaseFixture(t)
			} else {
				phase, _ = nativeHostPhaseFixture(t)
			}
			guest, host := vmSocketPair(t)
			var err error
			phase.bridge, err = startNativeProviderBridge(context.Background(), phase.request.Session, phase.request.Policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(codexToolStream("exec")))}, nil
			}), host)
			if err != nil {
				t.Fatal(err)
			}
			transport := nativeProviderTransport(guest)
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			status, _ := nativeProviderCall(t, client, phase.request.Session.Identity.Model)
			if status != 200 {
				t.Fatal("private provider precondition", status)
			}
			report := nativeVMTaskReport{review: protocolReport(phase.request.Session)}
			if control {
				r := fakeControl(*phase.request.Control)
				report = nativeVMTaskReport{control: &r}
			}
			raw, _ := json.Marshal(report.value())
			phase.finished = &nativeVMFinished{Version: 1, Binding: phase.request.Binding, RequestHash: Digest(phase.request), State: "complete", Report: raw, SourceAudit: nativeHostAuditFixture(t, phase.request), AuthReturn: []byte("synthetic structural return"), CodeHost: &nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(phase.request.Session), UID: nativeVMCodeHostUID, InputHash: Digest("input"), OutputHash: Digest("output"), Stopped: true, RelayComplete: true}}
			if !phase.finished.valid(phase.request) {
				t.Fatal("invalid private completed envelope")
			}
			// The host fixture's immutable store is empty: schema-valid allegations
			// must fail source verification after completed source/provider receipts.
			result, _, _, err := phase.finishTask(context.Background(), report, nil)
			if err == nil || result.control != nil || len(result.review.Findings) != 0 {
				t.Fatal("rejected task escaped", err)
			}
			data, err := os.ReadFile(filepath.Join(phase.supervisor.supervisor.root, phase.request.Binding.LaunchID, "native-audit.json"))
			if err != nil {
				t.Fatal(err)
			}
			var audit struct {
				ReportHash, Failure string
				Complete            bool
			}
			if json.Unmarshal(data, &audit) != nil || audit.Complete || audit.Failure != "source-service" {
				t.Fatal("did not exercise source rejection", string(data))
			}
			if audit.ReportHash != Digest(report.value()) {
				t.Fatal("private rejected-report identity was erased", audit.ReportHash, Digest(report.value()))
			}
		})
	}
}
