package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pollClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *pollClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *pollClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type pollingFixture struct {
	mu                                      sync.Mutex
	store                                   *Store
	clock                                   *pollClock
	remote                                  RemoteState
	batch                                   Discovery
	scopes                                  []Scope
	failure                                 error
	lists, reads, posts, scans, annotations int
	receipts                                []RemoteReceipt
	lost                                    bool
	onPost                                  func(*pollingFixture)
}

func (f *pollingFixture) OpenScopes(context.Context) ([]Scope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return append([]Scope{}, f.scopes...), f.failure
}
func (f *pollingFixture) capture() (RemoteState, Discovery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	raw, _ := json.Marshal(f.batch)
	var batch Discovery
	json.Unmarshal(raw, &batch)
	batch.Through = f.clock.Now()
	return f.remote, batch, f.failure
}
func (f *pollingFixture) PollScope(context.Context, Scope) (RemoteState, Discovery, error) {
	return f.capture()
}
func (f *pollingFixture) Current(ctx context.Context, scope Scope) (RemoteState, error) {
	r, b, err := f.capture()
	if err == nil && r.Access && r.Open {
		err = f.store.Discover(ctx, b)
	}
	return r, err
}
func (f *pollingFixture) History(context.Context, Scope) (Discovery, error) {
	_, b, err := f.capture()
	return b, err
}
func (f *pollingFixture) Post(_ context.Context, w WriteIntent) (RemoteReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	if f.onPost != nil {
		f.onPost(f)
	}
	r := RemoteReceipt{ID: int64(100 + f.posts), AuthorID: 9003, Marker: w.Marker, BodyHash: w.BodyHash, CreatedAt: f.clock.Now(), CommitID: w.Snapshot.Head}
	f.receipts = append(f.receipts, r)
	if f.lost {
		return RemoteReceipt{}, errors.New("lost synthetic response")
	}
	return r, nil
}
func (f *pollingFixture) Scan(context.Context, Scope, string) ([]RemoteReceipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans++
	return append([]RemoteReceipt{}, f.receipts...), true, f.failure
}
func (f *pollingFixture) Annotate(context.Context, Scope, int64, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.annotations++
	return nil
}

func pollControllerFixture(t *testing.T, review reviewFunc) (*PollingController, *pollingFixture, *atomic.Int32) {
	t.Helper()
	store, _ := openFixtureStore(t)
	git := newSnapshotGitFixture(t)
	clock := &pollClock{now: controlTime.Add(10 * time.Minute)}
	request := native(1, controlTime.Add(time.Minute))
	request.Snapshot = git.snapshot
	f := &pollingFixture{store: store, clock: clock, scopes: []Scope{alphaScope()}, remote: RemoteState{Access: true, Open: true, Snapshot: git.snapshot, Request: request}, batch: Discovery{Scope: alphaScope(), Requests: []NativeRequest{request}, Complete: true}}
	calls := &atomic.Int32{}
	fake := reviewFunc(func(ctx context.Context, s Session) (Report, error) {
		calls.Add(1)
		if review != nil {
			return review(ctx, s)
		}
		return fakeReport(s), nil
	})
	o := PollOptions{Store: store, Enrollment: alphaEnrollment(), Release: testJob().Release, Discovery: f, Publisher: Publisher{Store: store, Remote: f}, Capture: func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
		return CaptureSnapshotEvidence(ctx, git.store(), s)
	}, Runtime: func(context.Context, Job, *JobEvidence) (Engine, error) {
		return Engine{Reviewers: map[Provider]Reviewer{Codex: fake, Claude: fake}}, nil
	}, Check: func(context.Context, Enrollment, string) error { return nil }, Now: clock.Now, Interval: time.Second, MaxBackoff: time.Minute, TickTimeout: time.Second}
	c, err := OpenPollingController(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c, f, calls
}
func awaitPollWorker(t *testing.T, c *PollingController) {
	t.Helper()
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		t.Fatal("no background worker")
	}
	select {
	case <-active.done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not finish")
	}
}
func pollOnce(t *testing.T, c *PollingController) (PollResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.Poll(ctx)
}

func TestPollingControllerAdmitsFourFakePhasesAndPublishesOnce(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	first, err := pollOnce(t, c)
	if err != nil || !first.Active {
		t.Fatal(first, err)
	}
	awaitPollWorker(t, c)
	if calls.Load() != 4 {
		t.Fatal("wrong dual phase count", calls.Load())
	}
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	d, err := f.store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 || len(d.Writes) != 1 {
		t.Fatal("review not dispatched once", f.posts, len(d.Writes))
	}
	for _, w := range d.Writes {
		if w.State != "confirmed" {
			t.Fatal("review not reconciled", w.State)
		}
	}
	for i := 0; i < 3; i++ {
		f.clock.Advance(time.Second)
		if _, err := pollOnce(t, c); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 || f.posts != 1 {
		t.Fatal("duplicate discovery reran work", calls.Load(), f.posts)
	}
}

func TestPollingControllerPollsAndCancelsDuringBlockedWorker(t *testing.T) {
	started := make(chan struct{})
	c, f, calls := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		close(started)
		<-ctx.Done()
		return Report{}, ctx.Err()
	})
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started")
	}
	f.clock.Advance(2 * time.Second)
	f.mu.Lock()
	f.batch.Commands = []CommandEvent{commandEvent(50, "/sfl cancel", f.clock.Now())}
	f.mu.Unlock()
	if _, err := pollOnce(t, c); err != nil && !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.LeaseToken != "" || j.State != "cancelled" {
			t.Fatal("cancelled worker not stopped/fenced", j.State, j.LeaseToken)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("cancelled work ran another phase", calls.Load())
	}
}

func TestPollingControllerPersistsBackoffAndNeverRepostsUnknown(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	f.failure = &GitHubHTTPError{Status: 429, RetryAfter: 37 * time.Second}
	result, err := pollOnce(t, c)
	if err == nil || result.Next != f.clock.Now().Add(37*time.Second) {
		t.Fatal(result, err)
	}
	before := f.lists
	if _, err := pollOnce(t, c); err != nil || f.lists != before {
		t.Fatal("backoff probed remote again", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPollingController(context.Background(), c.options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if _, err := pollOnce(t, reopened); err != nil || f.lists != before || calls.Load() != 0 {
		t.Fatal("restart lost backoff", err)
	}
	f.clock.Advance(37 * time.Second)
	f.failure = nil
	f.lost = true
	if _, err := pollOnce(t, reopened); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, reopened)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, reopened); !errors.Is(err, ErrDeferred) {
		t.Fatal("lost response not unknown", err)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, w := range d.Writes {
		if w.State != "unknown" {
			t.Fatal(w.State)
		}
	}
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, reopened); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 || f.scans != 1 || calls.Load() != 4 {
		t.Fatal("unknown intent reposted or reran work", f.posts, f.scans, calls.Load())
	}
}

func TestPollingControllerRejectsWrongEnrollmentAndQualificationBeforeWork(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := c.options
	bad.Enrollment.InstallationID++
	if _, err := OpenPollingController(context.Background(), bad); !errors.Is(err, ErrIgnored) {
		t.Fatal("foreign enrollment accepted", err)
	}
	bad = c.options
	bad.Check = func(context.Context, Enrollment, string) error { return ErrLiveBlocked }
	blocked, err := OpenPollingController(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close(context.Background())
	if _, err := pollOnce(t, blocked); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("missing qualification accepted", err)
	}
	if f.lists != 0 || f.reads != 0 || f.posts != 0 || calls.Load() != 0 {
		t.Fatal("identity/gate refusal performed work")
	}
}

func TestPollingControllerOwnerLockAndUncertainLeaseSurviveRestart(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := OpenPollingController(ctx, c.options); err == nil {
		t.Fatal("two controller owners")
	}
	if err := f.store.Discover(context.Background(), func() Discovery { _, b, _ := f.capture(); return b }()); err != nil {
		t.Fatal(err)
	}
	evidence, err := c.options.Capture(context.Background(), f.remote.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.store.Admit(context.Background(), alphaScope(), 1, c.options.Release, evidence.Packet(), f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.Claim(context.Background(), job.ID, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); !errors.Is(err, ErrWorkerStopPending) {
		t.Fatal("uncertain worker was hidden", err)
	}
	f.clock.Advance(6 * time.Minute)
	reopened, err := OpenPollingController(context.Background(), c.options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	result, err := pollOnce(t, reopened)
	if err != nil || !result.RecoveryRequired || calls.Load() != 0 {
		t.Fatal("expired lease was stolen", result, err)
	}
	d, _ := f.store.Snapshot(context.Background())
	if d.Jobs[job.ID].LeaseToken != token {
		t.Fatal("lease changed without stopped proof")
	}
}

func TestPollingControllerSnapshotMoveDoesNotCreateReplacementRun(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	f.mu.Lock()
	f.remote.Snapshot.Base = f.remote.Snapshot.Head
	f.mu.Unlock()
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	d, _ := f.store.Snapshot(context.Background())
	if !d.PRs[alphaScope().key()].Paused || len(d.Jobs) != 0 || calls.Load() != 0 {
		t.Fatal("moved snapshot admitted old request")
	}
}

func TestPollingControllerRetainsBWhenPublishingAClearsPendingState(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	f.onPost = func(f *pollingFixture) {
		f.clock.Advance(2 * time.Second)
		b := native(2, f.clock.Now())
		b.Snapshot = f.remote.Snapshot
		f.batch.Requests = append(f.batch.Requests, b)
		f.remote.Request = NativeRequest{}
		f.onPost = nil
	}
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); !errors.Is(err, ErrDeferred) {
		t.Fatal("adverse A/B boundary hidden", err)
	}
	d, _ := f.store.Snapshot(context.Background())
	pr := d.PRs[alphaScope().key()]
	if !pr.Paused || pr.CurrentRequest != 2 || f.posts != 1 || f.annotations != 1 || calls.Load() != 4 {
		t.Fatal("B consumed by A", pr, f.posts, f.annotations, calls.Load())
	}
	for _, r := range pr.Requests {
		if r.ID == 2 && r.Admitted {
			t.Fatal("B silently fulfilled")
		}
	}
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || f.posts != 1 {
		t.Fatal("paused B inferred automatically")
	}
}

func TestPollingControllerAcknowledgmentsAreFIFOAndRateLimited(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	f.batch.Commands = []CommandEvent{commandEvent(12, "/sfl config show", controlTime.Add(3*time.Minute)), commandEvent(11, "/sfl config mode=codex-only", controlTime.Add(2*time.Minute))}
	first, err := pollOnce(t, c)
	if err != nil || first.Active || f.posts != 1 || calls.Load() != 0 {
		t.Fatal(first, err, f.posts, calls.Load())
	}
	f.clock.Advance(time.Second)
	second, err := pollOnce(t, c)
	if err != nil || !second.Active || f.posts != 2 {
		t.Fatal(second, err, f.posts)
	}
	awaitPollWorker(t, c)
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.Job.ConfigVersion != 1 || j.Job.Config.Mode != Adversarial {
			t.Fatal("older request adopted later acknowledgment", j.Job.ConfigVersion, j.Job.Config.Mode)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("ack ordering changed old request mode", calls.Load())
	}
}

type blockedPollDiscovery struct {
	PollDiscovery
	entered chan struct{}
}

func (d blockedPollDiscovery) OpenScopes(ctx context.Context) ([]Scope, error) {
	close(d.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPollingControllerCloseCancelsPollBeforeReleasingOwner(t *testing.T) {
	c, _, _ := pollControllerFixture(t, nil)
	entered := make(chan struct{})
	c.options.Discovery = blockedPollDiscovery{c.options.Discovery, entered}
	done := make(chan error, 1)
	go func() { _, err := c.Poll(context.Background()); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("poll survived shutdown", err)
	}
	if _, err := pollOnce(t, c); !errors.Is(err, ErrFenced) {
		t.Fatal("closed controller started work", err)
	}
}

func TestPollingControllerShutdownTimeoutKeepsOwnerAndCharges(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c, f, calls := pollControllerFixture(t, func(_ context.Context, s Session) (Report, error) {
		close(entered)
		<-release
		return fakeReport(s), nil
	})
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx); !errors.Is(err, ErrWorkerStopPending) {
		t.Fatal("active worker hidden by shutdown", err)
	}
	ownerCtx, cancelOwner := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelOwner()
	if _, err := OpenPollingController(ownerCtx, c.options); err == nil {
		t.Fatal("shutdown timeout released owner")
	}
	close(release)
	awaitPollWorker(t, c)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "queued" || j.LeaseToken != "" || j.Checkpoint.ConsumedSessions != 1 {
			t.Fatal("shutdown reset accounting or lost resumable work", j.State, j.LeaseToken, j.Checkpoint.ConsumedSessions)
		}
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestPollingControllerBackoffSurvivesLegacyStateWriter(t *testing.T) {
	c, f, _ := pollControllerFixture(t, nil)
	f.failure = &GitHubHTTPError{Status: 429, RetryAfter: 37 * time.Second}
	result, err := pollOnce(t, c)
	if err == nil {
		t.Fatal("rate failure expected")
	}
	d, _ := f.store.Snapshot(context.Background())
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	// An older binary knows only the original state row. It must not erase
	// the new controller's independently persisted rate-limit schedule.
	if _, err := f.store.db.Exec("UPDATE state SET data=? WHERE id=1", raw); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(f.store.path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	read, err := other.Snapshot(context.Background())
	if err != nil || read.Polling == nil || read.Polling.Next != result.Next {
		t.Fatal("cold store lost schedule", read.Polling, err)
	}
}

func TestPollingControllerRecoversCompletionWithoutProviderCalls(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	_, batch, _ := f.capture()
	if err := f.store.Discover(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	evidence, err := c.options.Capture(context.Background(), f.remote.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.store.Admit(context.Background(), alphaScope(), 1, c.options.Release, evidence.Packet(), f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := evidence.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := c.options.Runtime(context.Background(), job, bound)
	if err != nil {
		t.Fatal(err)
	}
	engine.Reader = bound.Reader
	if _, err := f.store.RunStopped(context.Background(), job.ID, engine, f.clock.Now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal(calls.Load())
	}
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || f.posts != 1 {
		t.Fatal("completion recovery reran inference", calls.Load(), f.posts)
	}
}

func TestPollingControllerRunStopsItsLoopAndReleasesOwnership(t *testing.T) {
	c, _, calls := pollControllerFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	c.options.Check = func(context.Context, Enrollment, string) error { close(entered); return ErrLiveBlocked }
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run loop did not stop")
	}
	if calls.Load() != 0 {
		t.Fatal("blocked run invoked provider")
	}
	reopened, err := OpenPollingController(context.Background(), c.options)
	if err != nil {
		t.Fatal("stopped loop retained owner", err)
	}
	reopened.Close(context.Background())
}

func TestPollingControllerShutdownBeforeFactoryWorkPreservesQueuedRequest(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	entered := make(chan struct{})
	c.options.Runtime = func(ctx context.Context, _ Job, _ *JobEvidence) (Engine, error) {
		close(entered)
		<-ctx.Done()
		return Engine{}, ctx.Err()
	}
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "queued" || j.LeaseToken != "" || j.Checkpoint.ConsumedSessions != 0 {
			t.Fatal("pre-work shutdown stranded/charged request", j.State, j.Checkpoint.ConsumedSessions)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("factory cancellation invoked provider")
	}
}

func TestPollingControllerDoesNotAutomaticallyRepairBlockedRuntime(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	factories := 0
	c.options.Runtime = func(context.Context, Job, *JobEvidence) (Engine, error) { factories++; return Engine{}, ErrLiveBlocked }
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if factories != 1 || calls.Load() != 0 {
		t.Fatal("blocked factory retried", factories, calls.Load())
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "blocked" || j.Checkpoint.ConsumedSessions != 0 {
			t.Fatal(j.State)
		}
	}
}
