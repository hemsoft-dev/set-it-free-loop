package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var controlTime = time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)

func alphaEnrollment() Enrollment { return Enrollment{RepositoryID, 9001, 9002, 9003, OwnerID} }
func alphaScope() Scope           { return Scope{RepositoryID, 9001, 1} }
func openFixtureStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	s, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}
func native(id int64, at time.Time) NativeRequest {
	return NativeRequest{ID: id, RequesterID: OwnerID, BotID: 9003, CreatedAt: at, Snapshot: testSnapshot()}
}
func commandEvent(id int64, body string, at time.Time) CommandEvent {
	return CommandEvent{ID: id, AuthorID: OwnerID, Body: body, CreatedAt: at, UpdatedAt: at}
}
func mustCapture(t *testing.T, s *Store, events []CommandEvent, through time.Time) {
	t.Helper()
	if err := s.CaptureCommands(context.Background(), alphaScope(), events, through, true); err != nil {
		t.Fatal(err)
	}
}
func writes(t *testing.T, s *Store, kind string) []WriteIntent {
	t.Helper()
	d, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []WriteIntent
	for _, w := range d.Writes {
		if w.Kind == kind {
			out = append(out, *w)
		}
	}
	return out
}

type fakeRemote struct {
	state                                 RemoteState
	receipts                              []RemoteReceipt
	complete                              bool
	posts, annotations                    int
	post                                  func(WriteIntent) (RemoteReceipt, error)
	history                               []NativeRequest
	currentErr, historyErr, annotationErr error
}

func (f *fakeRemote) Current(context.Context, Scope) (RemoteState, error) {
	return f.state, f.currentErr
}
func (f *fakeRemote) Post(_ context.Context, w WriteIntent) (RemoteReceipt, error) {
	f.posts++
	if f.post != nil {
		return f.post(w)
	}
	r := RemoteReceipt{int64(f.posts), 9003, w.Marker, w.BodyHash, controlTime.Add(5 * time.Minute)}
	f.receipts = append(f.receipts, r)
	return r, nil
}
func (f *fakeRemote) Scan(context.Context, Scope, string) ([]RemoteReceipt, bool, error) {
	return f.receipts, f.complete, nil
}
func (f *fakeRemote) Annotate(context.Context, Scope, int64, string) error {
	f.annotations++
	return f.annotationErr
}
func (f *fakeRemote) History(_ context.Context, scope Scope) (Discovery, error) {
	requests := f.history
	if requests == nil {
		requests = []NativeRequest{}
		if f.state.Request.ID != 0 {
			requests = append(requests, f.state.Request)
		}
	}
	return Discovery{Scope: scope, Requests: requests, Through: controlTime.Add(9 * time.Minute), Complete: f.complete}, f.historyErr
}
func liveFake(r NativeRequest) *fakeRemote {
	return &fakeRemote{state: RemoteState{true, true, r.Snapshot, r}, complete: true}
}
func TestUnknownAcknowledgmentSurvivesRestartAndOrdersRequest(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "command-first", true: "request-first"}[reverse], func(t *testing.T) {
			s, path := openFixtureStore(t)
			ctx := context.Background()
			request := native(1, controlTime.Add(7*time.Minute))
			event := commandEvent(1, "/sfl config mode=codex-only", controlTime.Add(time.Minute))
			if reverse {
				if err := s.RecordRequest(ctx, alphaScope(), request, true); err != nil {
					t.Fatal(err)
				}
			}
			mustCapture(t, s, []CommandEvent{event}, controlTime.Add(10*time.Minute))
			if !reverse {
				if err := s.RecordRequest(ctx, alphaScope(), request, true); err != nil {
					t.Fatal(err)
				}
			}
			remote := liveFake(request)
			remote.post = func(w WriteIntent) (RemoteReceipt, error) {
				remote.receipts = append(remote.receipts, RemoteReceipt{1, 9003, w.Marker, w.BodyHash, controlTime.Add(5 * time.Minute)})
				return RemoteReceipt{}, errors.New("crash after accepted ACK")
			}
			p := Publisher{Store: s, Remote: remote}
			ack := writes(t, s, "ack")[0]
			if err := p.Dispatch(ctx, ack.ID); !errors.Is(err, ErrDeferred) {
				t.Fatal(err)
			}
			if _, err := s.Admit(ctx, alphaScope(), 1, strings.Repeat("d", 40), "evidence", controlTime.Add(11*time.Minute)); !errors.Is(err, ErrDeferred) {
				t.Fatal("admitted before ACK reconciliation", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			p.Store = reopened
			remote.receipts = append(remote.receipts, RemoteReceipt{2, 666, ack.Marker, ack.BodyHash, controlTime.Add(5 * time.Minute)})
			remote.complete = false
			if err = p.Reconcile(ctx, ack.ID); !errors.Is(err, ErrDeferred) {
				t.Fatal("partial scan trusted", err)
			}
			remote.complete = true
			if err = p.Reconcile(ctx, ack.ID); err != nil {
				t.Fatal(err)
			}
			job, err := reopened.Admit(ctx, alphaScope(), 1, strings.Repeat("d", 40), "evidence", controlTime.Add(11*time.Minute))
			if err != nil || job.ConfigVersion != 2 || job.Config.Mode != CodexOnly {
				t.Fatal("wrong acknowledged config", job, err)
			}
			if err = p.Dispatch(ctx, ack.ID); !errors.Is(err, ErrDeferred) || remote.posts != 1 {
				t.Fatal("blind ACK retry", err)
			}
			event.Body = "/sfl config mode=claude-only"
			event.UpdatedAt = controlTime.Add(time.Hour)
			mustCapture(t, reopened, []CommandEvent{event}, controlTime.Add(time.Hour))
			d, _ := reopened.Snapshot(ctx)
			if d.PRs[alphaScope().key()].Commands[0].Config.Mode != CodexOnly {
				t.Fatal("edit rewrote accepted command")
			}
		})
	}
}
func TestEarlierAndEqualRequestConfiguration(t *testing.T) {
	for _, minutes := range []int{3, 5} {
		t.Run(time.Duration(minutes).String(), func(t *testing.T) {
			s, _ := openFixtureStore(t)
			r := native(1, controlTime.Add(time.Duration(minutes)*time.Minute))
			mustCapture(t, s, []CommandEvent{commandEvent(1, "/sfl config mode=claude-only", controlTime.Add(time.Minute))}, controlTime.Add(10*time.Minute))
			remote := liveFake(r)
			p := Publisher{Store: s, Remote: remote}
			if err := p.Dispatch(context.Background(), writes(t, s, "ack")[0].ID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordRequest(context.Background(), alphaScope(), r, true); err != nil {
				t.Fatal(err)
			}
			job, err := s.Admit(context.Background(), alphaScope(), 1, strings.Repeat("d", 40), "evidence", controlTime.Add(11*time.Minute))
			if minutes == 5 {
				if !errors.Is(err, ErrDeferred) {
					t.Fatal("equal ACK/request timestamps guessed", err)
				}
			} else if err != nil || job.ConfigVersion != 1 || job.Config.Mode != Adversarial {
				t.Fatal("earlier request changed", job, err)
			}
		})
	}
}
func admittedFixture(t *testing.T) (*Store, Job, *fakeRemote) {
	t.Helper()
	s, _ := openFixtureStore(t)
	r := native(1, controlTime.Add(time.Minute))
	mustCapture(t, s, nil, controlTime.Add(2*time.Minute))
	if err := s.RecordRequest(context.Background(), alphaScope(), r, true); err != nil {
		t.Fatal(err)
	}
	job, err := s.Admit(context.Background(), alphaScope(), 1, strings.Repeat("d", 40), "evidence", controlTime.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return s, job, liveFake(r)
}
func completeFixture(t *testing.T) (*Store, Outcome, *fakeRemote) {
	t.Helper()
	s, job, remote := admittedFixture(t)
	now := controlTime.Add(4 * time.Minute)
	token, err := s.Claim(context.Background(), job.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	engine := testEngine(t, func(_ context.Context, session Session) (Report, error) { return protocolReport(session), nil })
	for provider, inner := range engine.Reviewers {
		engine.Reviewers[provider] = DurableReviewer{Store: s, Job: job, Token: token, Inner: inner, Reader: engine.Reader, Now: func() time.Time { return now }}
	}
	out, err := engine.Run(context.Background(), job, Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseStopped(context.Background(), job.ID, token, out.State); err != nil {
		t.Fatal(err)
	}
	return s, out, remote
}
func TestOutboxUnknownPublicationAndDelayedVisibility(t *testing.T) {
	s, out, remote := completeFixture(t)
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	intent, err := p.Prepare(ctx, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.post = func(w WriteIntent) (RemoteReceipt, error) {
		remote.receipts = append(remote.receipts, RemoteReceipt{30, 9003, w.Marker, w.BodyHash, controlTime.Add(6 * time.Minute)})
		return RemoteReceipt{}, errors.New("lost response")
	}
	if err = p.Dispatch(ctx, intent.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	saved := remote.receipts
	remote.receipts = nil
	if err = p.Reconcile(ctx, intent.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("empty scan treated as safe retry", err)
	}
	if err = p.Dispatch(ctx, intent.ID); !errors.Is(err, ErrDeferred) || remote.posts != 1 {
		t.Fatal("unknown POST repeated", err)
	}
	remote.receipts = saved
	if err = p.Reconcile(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	if d.Writes[intent.ID].State != "confirmed" {
		t.Fatal("receipt not persisted")
	}
}
func TestPublicationARetainsRequestBWhenGitHubClearsB(t *testing.T) {
	s, out, remote := completeFixture(t)
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	intent, err := p.Prepare(ctx, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.post = func(w WriteIntent) (RemoteReceipt, error) {
		b := native(2, controlTime.Add(6*time.Minute))
		if err := s.RecordRequest(ctx, alphaScope(), b, true); err != nil {
			t.Fatal(err)
		}
		remote.state.Request = NativeRequest{} // GitHub A completion clears newly requested B
		return RemoteReceipt{33, 9003, w.Marker, w.BodyHash, controlTime.Add(7 * time.Minute)}, nil
	}
	if err = p.Dispatch(ctx, intent.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	pr := d.PRs[alphaScope().key()]
	if pr.CurrentRequest != 2 || !pr.Paused || d.Writes[intent.ID].State != "superseded" || remote.annotations != 1 {
		t.Fatal("A silently fulfilled B", pr, remote.annotations)
	}
	if _, err = s.Admit(ctx, alphaScope(), 2, out.Job.Release, "evidence", controlTime.Add(8*time.Minute)); !errors.Is(err, ErrDeferred) {
		t.Fatal("ambiguous B inferred", err)
	}
}
func TestMovedBaseAndCancelFenceDispatch(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "base-moved", true: "cancelled"}[cancel], func(t *testing.T) {
			s, out, remote := completeFixture(t)
			ctx := context.Background()
			p := Publisher{Store: s, Remote: remote}
			intent, err := p.Prepare(ctx, out, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				mustCapture(t, s, []CommandEvent{commandEvent(3, "/sfl cancel", controlTime.Add(3*time.Minute))}, controlTime.Add(4*time.Minute))
			} else {
				remote.state.Snapshot.Base = strings.Repeat("f", 40)
			}
			if err = p.Dispatch(ctx, intent.ID); err == nil || remote.posts != 0 {
				t.Fatal("stale/cancelled dispatch occurred", err)
			}
		})
	}
}
func TestLeaseDoesNotStealExpiredWorkerAndCrashConsumesBudget(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	now := controlTime.Add(4 * time.Minute)
	token, err := s.Claim(ctx, job.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, job.ID, now.Add(time.Hour)); !errors.Is(err, ErrDeferred) {
		t.Fatal("expired worker stolen", err)
	}
	limits, _ := protocolLimits(job)
	session, _ := NewSession(job.ID, job.Snapshot, job.Config.Hash(), Codex, 1, job.Packet, nil, nil, limits)
	crashed := DurableReviewer{Store: s, Job: job, Token: token, Now: func() time.Time { return now }, Inner: reviewFunc(func(context.Context, Session) (Report, error) { panic("simulated worker death") })}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing injected crash")
			}
		}()
		_, _ = crashed.Review(ctx, session)
	}()
	d, _ := s.Snapshot(ctx)
	if d.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 || len(d.Jobs[job.ID].Checkpoint.Artifacts) != 0 {
		t.Fatal("crash reset session accounting")
	}
	if err = s.ReleaseStopped(ctx, job.ID, token, "queued"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, job.ID, now); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteConcurrentAdmissionAcrossHandles(t *testing.T) {
	s, path := openFixtureStore(t)
	other, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	mustCapture(t, s, nil, controlTime.Add(2*time.Minute))
	r := native(1, controlTime.Add(time.Minute))
	if err = s.RecordRequest(ctx, alphaScope(), r, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, err := store.Admit(ctx, alphaScope(), 1, strings.Repeat("d", 40), "evidence", controlTime.Add(3*time.Minute))
			errs <- err
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	d, _ := s.Snapshot(ctx)
	if len(d.Jobs) != 1 {
		t.Fatal("duplicate durable job", len(d.Jobs))
	}
}
func TestWrongScopeAndNonOwnerHaveNoProviderOrWriteEffect(t *testing.T) {
	s, _ := openFixtureStore(t)
	ctx := context.Background()
	scope := alphaScope()
	scope.RepositoryID++
	if err := s.RecordRequest(ctx, scope, native(1, controlTime), true); err == nil {
		t.Fatal("wrong repository accepted")
	}
	r := native(1, controlTime)
	r.RequesterID = 44
	if err := s.RecordRequest(ctx, alphaScope(), r, true); !errors.Is(err, ErrIgnored) {
		t.Fatal(err)
	}
	event := commandEvent(1, "/sfl cancel", controlTime)
	event.AuthorID = 44
	mustCapture(t, s, []CommandEvent{event}, controlTime.Add(time.Minute))
	d, _ := s.Snapshot(ctx)
	if len(d.Writes) != 0 || len(d.Jobs) != 0 {
		t.Fatal("untrusted event created work")
	}
}

func TestFIFOConfigAcknowledgmentsAndResetDefaults(t *testing.T) {
	s, _ := openFixtureStore(t)
	ctx := context.Background()
	events := []CommandEvent{commandEvent(2, "/sfl config reset", controlTime.Add(2*time.Minute)), commandEvent(1, "/sfl config mode=codex-only", controlTime.Add(time.Minute))}
	mustCapture(t, s, events, controlTime.Add(3*time.Minute))
	remote := liveFake(native(1, controlTime.Add(10*time.Minute)))
	p := Publisher{Store: s, Remote: remote}
	d, _ := s.Snapshot(ctx)
	commands := d.PRs[alphaScope().key()].Commands
	if err := p.Dispatch(ctx, commands[1].WriteID); !errors.Is(err, ErrDeferred) || remote.posts != 0 {
		t.Fatal("ACK FIFO violated", err)
	}
	if err := p.Dispatch(ctx, commands[0].WriteID); err != nil {
		t.Fatal(err)
	}
	remote.post = func(w WriteIntent) (RemoteReceipt, error) {
		return RemoteReceipt{2, 9003, w.Marker, w.BodyHash, controlTime.Add(6 * time.Minute)}, nil
	}
	if err := p.Dispatch(ctx, commands[1].WriteID); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Snapshot(ctx)
	configs := d.PRs[alphaScope().key()].Configs
	if len(configs) != 3 || configs[2].Config.Hash() != DefaultConfig().Hash() {
		t.Fatal("reset defaults lost")
	}
}
func TestDurablePhaseResumesWithoutRepeatingValidatedWork(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	now := controlTime.Add(4 * time.Minute)
	token, err := s.Claim(ctx, job.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	engine := testEngine(t, func(_ context.Context, session Session) (Report, error) { return protocolReport(session), nil })
	limits, _ := protocolLimits(job)
	session, _ := NewSession(job.ID, job.Snapshot, job.Config.Hash(), Codex, 1, job.Packet, nil, nil, limits)
	// A phase completed and persisted, then the process died before Engine returned.
	worker := DurableReviewer{Store: s, Job: job, Token: token, Inner: engine.Reviewers[Codex], Reader: engine.Reader, Now: func() time.Time { return now }}
	if _, err = worker.Review(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseStopped(ctx, job.ID, token, "queued"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	for provider := range engine.Reviewers {
		engine.Reviewers[provider] = reviewFunc(func(_ context.Context, session Session) (Report, error) {
			calls++
			if session.Round == 1 && session.Identity.Provider == Codex {
				t.Fatal("repeated validated R1")
			}
			return protocolReport(session), nil
		})
	}
	out, err := s.RunStopped(ctx, job.ID, engine, func() time.Time { return now })
	if err != nil || calls != 3 || out.ConsumedSessions != 4 {
		t.Fatal("resume accounting", err, calls, out.ConsumedSessions)
	}
}
func TestUnknownPublicationSurvivesRetentionAndSecretRedaction(t *testing.T) {
	s, out, remote := completeFixture(t)
	ctx := context.Background()
	secret := "sensitive fixture content"
	out.Job.Packet = secret
	// Known-secret redaction covers final summaries AND inline bodies.
	doc := redactDocument(Document{Summary: secret, Inline: []InlineComment{{"source.go", 1, "text " + secret}}}, []string{secret})
	if strings.Contains(doc.Summary, secret) || strings.Contains(doc.Inline[0].Body, secret) {
		t.Fatal("secret survived final redaction")
	}
	// Use the original immutable job for publication.
	d, _ := s.Snapshot(ctx)
	for _, j := range d.Jobs {
		out.Job = j.Job
	}
	p := Publisher{Store: s, Remote: remote}
	intent, err := p.Prepare(ctx, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.post = func(WriteIntent) (RemoteReceipt, error) { return RemoteReceipt{}, errors.New("lost") }
	_ = p.Dispatch(ctx, intent.ID)
	if err = s.Prune(ctx, controlTime.Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Snapshot(ctx)
	if d.Jobs[out.Job.ID] == nil || d.Writes[intent.ID] == nil {
		t.Fatal("unknown write lost on retention")
	}
}

func TestInvalidOwnerCommandCannotPoisonDiscovery(t *testing.T) {
	s, _ := openFixtureStore(t)
	bad := commandEvent(1, "/sfl config endpoint=https://invalid.example", controlTime.Add(time.Minute))
	good := commandEvent(2, "/sfl config mode=codex-only", controlTime.Add(2*time.Minute))
	mustCapture(t, s, []CommandEvent{bad, good}, controlTime.Add(3*time.Minute))
	d, _ := s.Snapshot(context.Background())
	p := d.PRs[alphaScope().key()]
	if len(p.Commands) != 2 || p.Commands[0].Command.Action != "reject" || p.Commands[1].Config.Mode != CodexOnly || p.Commands[1].Version != 2 {
		t.Fatal("invalid command poisoned following configuration", p)
	}
}

func TestDiscoveryIsAtomicAndRemovedLatestFencesOldWork(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	before, _ := s.Snapshot(ctx)
	event := commandEvent(4, "/sfl config mode=codex-only", controlTime.Add(4*time.Minute))
	bad := native(0, controlTime.Add(5*time.Minute))
	err := s.Discover(ctx, Discovery{Scope: alphaScope(), Commands: []CommandEvent{event}, Requests: []NativeRequest{bad}, Through: controlTime.Add(6 * time.Minute), Complete: true})
	after, _ := s.Snapshot(ctx)
	if err == nil || Digest(before) != Digest(after) {
		t.Fatal("partial discovery batch committed", err)
	}
	removed := native(2, controlTime.Add(4*time.Minute))
	removed.Removed = true
	if err = s.Discover(ctx, Discovery{Scope: alphaScope(), Requests: []NativeRequest{removed}, Through: controlTime.Add(6 * time.Minute), Complete: true}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	p := d.PRs[alphaScope().key()]
	if p.CurrentRequest != 0 || p.Generation <= job.Generation {
		t.Fatal("removed latest left older job current", p)
	}
	if _, err = s.Claim(ctx, job.ID, controlTime.Add(7*time.Minute)); !errors.Is(err, ErrFenced) {
		t.Fatal("old job still runnable", err)
	}
}
func TestBeforeWorkRefundRemovesItsDailyCharge(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	now := controlTime.Add(4 * time.Minute)
	engine := testEngine(t, func(context.Context, Session) (Report, error) {
		return Report{}, PhaseError{State: "quota_wait", BeforeWork: true}
	})
	out, err := s.RunStopped(ctx, job.ID, engine, func() time.Time { return now })
	if err == nil || out.State != "quota_wait" {
		t.Fatal(out, err)
	}
	d, _ := s.Snapshot(ctx)
	if d.Jobs[job.ID].Checkpoint.ConsumedSessions != 0 || len(d.Jobs[job.ID].ChargeDays) != 0 {
		t.Fatal("before-work failure retained daily charge")
	}
	if _, err = s.Claim(ctx, job.ID, now); err != nil {
		t.Fatal("refunded job could not resume", err)
	}
}
func TestPostPublicationChecksAndAnnotationRemainRetryable(t *testing.T) {
	for _, annotation := range []bool{false, true} {
		t.Run(fmt.Sprint(annotation), func(t *testing.T) {
			s, out, remote := completeFixture(t)
			ctx := context.Background()
			p := Publisher{Store: s, Remote: remote}
			intent, err := p.Prepare(ctx, out, nil)
			if err != nil {
				t.Fatal(err)
			}
			remote.post = func(w WriteIntent) (RemoteReceipt, error) {
				if annotation {
					remote.state.Snapshot.Base = strings.Repeat("f", 40)
					remote.annotationErr = errors.New("annotation response lost")
				} else {
					remote.currentErr = errors.New("transient postcheck read")
				}
				return RemoteReceipt{55, 9003, w.Marker, w.BodyHash, controlTime.Add(6 * time.Minute)}, nil
			}
			if err = p.Dispatch(ctx, intent.ID); err == nil {
				t.Fatal("injected recovery failure ignored")
			}
			d, _ := s.Snapshot(ctx)
			expected := "postcheck_pending"
			if annotation {
				expected = "annotation_pending"
			}
			if d.Writes[intent.ID].State != expected {
				t.Fatal("lost retry state", d.Writes[intent.ID].State)
			}
			remote.currentErr = nil
			remote.annotationErr = nil
			err = p.Reconcile(ctx, intent.ID)
			if annotation {
				if !errors.Is(err, ErrDeferred) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			d, _ = s.Snapshot(ctx)
			expected = "confirmed"
			if annotation {
				expected = "superseded"
			}
			if d.Writes[intent.ID].State != expected || remote.posts != 1 {
				t.Fatal("postcheck did not recover safely", d.Writes[intent.ID].State, remote.posts)
			}
		})
	}
}
func TestPostHistoryFindsBThatWasNeverLocallyObserved(t *testing.T) {
	s, out, remote := completeFixture(t)
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	intent, err := p.Prepare(ctx, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.post = func(w WriteIntent) (RemoteReceipt, error) {
		b := native(2, controlTime.Add(6*time.Minute))
		b.Removed = true
		remote.history = []NativeRequest{native(1, controlTime.Add(time.Minute)), b}
		remote.state.Request = NativeRequest{}
		return RemoteReceipt{56, 9003, w.Marker, w.BodyHash, controlTime.Add(7 * time.Minute)}, nil
	}
	if err = p.Dispatch(ctx, intent.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("unobserved B disappeared silently", err)
	}
	d, _ := s.Snapshot(ctx)
	pr := d.PRs[alphaScope().key()]
	if !pr.Paused || pr.PauseRequestID != 2 || len(pr.Requests) != 2 {
		t.Fatal("B history not retained", pr)
	}
	// A later unambiguous fresh owner request can recover the paused PR.
	c := native(3, controlTime.Add(10*time.Minute))
	if err = s.Discover(ctx, Discovery{Scope: alphaScope(), Requests: []NativeRequest{c}, Through: controlTime.Add(11 * time.Minute), Complete: true}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Snapshot(ctx)
	pr = d.PRs[alphaScope().key()]
	if pr.Paused || pr.CurrentRequest != 3 {
		t.Fatal("reconciled fresh request cannot recover", pr)
	}
}
func TestKnownSecretsAreRedactedAfterRenderingTransforms(t *testing.T) {
	for _, secret := range []string{"sensitive_secret", "a<b&c", "secret with spaces", "path/with?query"} {
		doc := Document{Summary: safeText(secret) + " " + secret, Inline: []InlineComment{{"source.go", 1, safeText(secret)}}}
		redacted := redactDocument(doc, []string{secret})
		if strings.Contains(redacted.Summary, secret) || strings.Contains(redacted.Summary, safeText(secret)) || strings.Contains(redacted.Inline[0].Body, safeText(secret)) {
			t.Fatal("rendered encoding leaked known secret", secret)
		}
	}
}

func TestResumedDailyCapKeepsQuotaWaitRetryable(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	firstDay := controlTime.Add(4 * time.Minute)
	engine := testEngine(t, func(_ context.Context, session Session) (Report, error) {
		if session.Identity.Provider == Claude {
			return Report{}, PhaseError{State: "quota_wait", BeforeWork: true}
		}
		return protocolReport(session), nil
	})
	if out, err := s.RunStopped(ctx, job.ID, engine, func() time.Time { return firstDay }); err == nil || out.ConsumedSessions != 1 {
		t.Fatal("partial fixture", out, err)
	}
	// Four independent jobs on other PRs consume the next day's quota.
	nextDay := controlTime.Add(24*time.Hour + 2*time.Minute)
	for i := 2; i <= 5; i++ {
		scope := alphaScope()
		scope.PR = i
		r := native(int64(i), controlTime.Add(24*time.Hour))
		r.Snapshot.PR = i
		if err := s.Discover(ctx, Discovery{Scope: scope, Requests: []NativeRequest{r}, Through: nextDay, Complete: true}); err != nil {
			t.Fatal(err)
		}
		b, err := s.Admit(ctx, scope, r.ID, job.Release, "evidence", nextDay)
		if err != nil {
			t.Fatal(err)
		}
		successful := testEngine(t, func(_ context.Context, session Session) (Report, error) { return protocolReport(session), nil })
		if _, err = s.RunStopped(ctx, b.ID, successful, func() time.Time { return nextDay }); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.RunStopped(ctx, job.ID, engine, func() time.Time { return nextDay })
	if err == nil || out.State != "quota_wait" || out.ConsumedSessions != 1 {
		t.Fatal("resumed quota changed to outage", out, err)
	}
	d, _ := s.Snapshot(ctx)
	if d.Jobs[job.ID].State != "quota_wait" {
		t.Fatal("job permanently non-claimable")
	}
}
