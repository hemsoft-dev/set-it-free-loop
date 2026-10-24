package reviewer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// twoPRPolling is a controller over a fake upstream with two independent
// PRs. PR 1's request is older than PR 2's, so plain oldest-first selection
// always prefers PR 1. Only capture and restore are supplied by the test.
func twoPRPolling(t *testing.T, first, second Snapshot, capture func(context.Context, Snapshot) (*SnapshotEvidence, error), restore func(context.Context, Job) (*JobEvidence, error)) (*PollingController, *Store, *refusalUpstream, *pollClock, *atomic.Int32) {
	t.Helper()
	store, _ := openFixtureStore(t)
	clock := &pollClock{now: controlTime.Add(10 * time.Minute)}
	first.PR, second.PR = 1, 2
	one := native(1, controlTime.Add(time.Minute))
	one.Snapshot = first
	two := native(2, controlTime.Add(2*time.Minute))
	two.Snapshot = second
	u := &refusalUpstream{store: store, clock: clock, prs: map[int]*refusalPR{
		1: {remote: RemoteState{Access: true, Open: true, Snapshot: first, Request: one}, requests: []NativeRequest{one}},
		2: {remote: RemoteState{Access: true, Open: true, Snapshot: second, Request: two}, requests: []NativeRequest{two}},
	}}
	calls := &atomic.Int32{}
	fake := reviewFunc(func(_ context.Context, s Session) (Report, error) {
		calls.Add(1)
		if s.Snapshot.PR != 2 {
			return Report{}, errors.New("PR 1 reached a provider")
		}
		return fakeReport(s), nil
	})
	o := PollOptions{Store: store, Enrollment: alphaEnrollment(), Release: testJob().Release, Discovery: u, Publisher: Publisher{Store: store, Remote: u}, Capture: capture, Restore: restore, Runtime: func(context.Context, Job, *JobEvidence) (Engine, error) {
		return Engine{Reviewers: map[Provider]Reviewer{Codex: fake, Claude: fake}}, nil
	}, Check: func(context.Context, Enrollment, string) error { return nil }, Now: clock.Now, Interval: time.Second, MaxBackoff: time.Minute, TickTimeout: 3 * time.Second}
	c, err := OpenPollingController(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c, store, u, clock, calls
}

// pollEverySecond runs n poll cycles one second apart, waiting for each
// worker a cycle starts, like Run with an immediate wake on completion.
func pollEverySecond(t *testing.T, c *PollingController, clock *pollClock, n int) {
	t.Helper()
	for range n {
		result, _ := pollOnce(t, c)
		if result.Active {
			awaitPollWorker(t, c)
		}
		clock.Advance(time.Second)
	}
}

// failingCaptures records the clock time of each PR 1 capture, which always
// fails transiently, and captures every other PR from git.
type failingCaptures struct {
	mu    sync.Mutex
	clock *pollClock
	at    map[int][]time.Time
}

func (f *failingCaptures) capture(git snapshotGitFixture) func(context.Context, Snapshot) (*SnapshotEvidence, error) {
	return func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
		f.mu.Lock()
		f.at[s.PR] = append(f.at[s.PR], f.clock.Now())
		f.mu.Unlock()
		if s.PR == 1 {
			return nil, errors.Join(ErrSourceService, errors.New("synthetic source VM outage"))
		}
		return CaptureSnapshotEvidence(ctx, git.store(), s)
	}
}

func (f *failingCaptures) times(pr int) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.at[pr]...)
}

// An older PR whose capture keeps failing transiently does not hold back a
// later PR: while it backs off, the later PR is admitted and reviewed. It is
// still captured again after each backoff, which doubles per consecutive
// failure, and it is never admitted, published or refused.
func TestPollingTransientCaptureFailureDoesNotBlockOtherPRs(t *testing.T) {
	git := newSnapshotGitFixture(t)
	captures := &failingCaptures{at: map[int][]time.Time{}}
	c, store, u, clock, calls := twoPRPolling(t, git.snapshot, git.snapshot, captures.capture(git), func(ctx context.Context, job Job) (*JobEvidence, error) {
		return RestoreSnapshotEvidence(ctx, git.store(), job)
	})
	captures.clock = clock
	pollEverySecond(t, c, clock, 40)
	d, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[int]string{}
	for _, j := range d.Jobs {
		states[j.Scope.PR] = j.State
	}
	failed, reviewed := captures.times(1), captures.times(2)
	if states[2] != "dual_complete" || calls.Load() != 4 || len(reviewed) != 1 {
		t.Fatal("transiently failing older PR blocked a later PR", states, calls.Load(), len(failed), len(reviewed))
	}
	if len(failed) < 2 || !reviewed[0].Before(failed[1]) {
		t.Fatal("later PR not admitted while the older PR backed off", failed, reviewed)
	}
	if len(failed) < 4 {
		t.Fatal("failing PR not captured again after its backoff", failed)
	}
	// The controller-wide backoff (one interval) and then the request's own
	// delay, doubled per consecutive failure, pass between its captures.
	for i := 1; i < len(failed); i++ {
		gap := failed[i].Sub(failed[i-1])
		want := time.Second + time.Second<<(i-1)
		if gap < want || i > 1 && gap < failed[i-1].Sub(failed[i-2]) {
			t.Fatal("failing PR retried before its backoff", i, gap, want, failed)
		}
	}
	pr := d.PRs[Scope{RepositoryID, 9001, 1}.key()]
	if states[1] != "" || pr == nil || pr.Requests[0].Admitted {
		t.Fatal("transient failure admitted or refused the older PR", states)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.posts) != 1 || u.posts[0].Scope.PR != 2 || u.posts[0].Kind != "review" {
		t.Fatal("wrong publications", len(u.posts))
	}
}

// With nothing else ready, a failing request leaves the controller-wide
// backoff in place and then waits for its own: no cycle in between starts
// work, so there is no busy retry. Its backoff lives in controller memory, so
// a restarted controller still honours the durable schedule and then
// captures the request once more as a first failure.
func TestPollingTransientCaptureBackoffAppliesWhenNothingElseIsReady(t *testing.T) {
	c, f, _ := pollControllerFixture(t, nil)
	captures := &atomic.Int32{}
	failure := errors.New("synthetic network outage")
	c.options.Capture = func(context.Context, Snapshot) (*SnapshotEvidence, error) {
		captures.Add(1)
		return nil, failure
	}
	// cycle polls once at the current fixture time and reports whether it
	// started a worker, waiting for it.
	cycle := func() (bool, error) {
		t.Helper()
		result, err := pollOnce(t, c)
		if result.Active {
			awaitPollWorker(t, c)
		}
		return result.Active, err
	}
	expect := func(failures int, wait time.Duration) {
		t.Helper()
		if active, err := cycle(); !errors.Is(err, failure) || active {
			t.Fatal("failure not drained into the controller-wide backoff", err, active)
		}
		d, err := f.store.Snapshot(context.Background())
		if err != nil || d.Polling == nil || d.Polling.Failures != 1 || d.Polling.Reason != "deferred" || !d.Polling.Next.Equal(f.clock.Now().Add(time.Second)) {
			t.Fatal("controller-wide backoff not applied", d.Polling, err)
		}
		// Every cycle before the request's own backoff ends starts nothing.
		for elapsed := time.Second; elapsed < time.Second+wait; elapsed += time.Second {
			f.clock.Advance(time.Second)
			if active, err := cycle(); err != nil || active {
				t.Fatal("request retried during its backoff", failures, elapsed, err)
			}
		}
		f.clock.Advance(time.Second)
		if active, _ := cycle(); !active || captures.Load() != int32(failures+1) {
			t.Fatal("request not captured again after its backoff", failures, captures.Load())
		}
	}
	if active, err := cycle(); err != nil || !active || captures.Load() != 1 {
		t.Fatal("first capture not started", err)
	}
	expect(1, time.Second)
	expect(2, 2*time.Second)
	expect(3, 4*time.Second)
	if active, err := cycle(); !errors.Is(err, failure) || active {
		t.Fatal("fourth failure not drained", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenPollingController(context.Background(), c.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = restarted.Close(ctx)
	})
	c = restarted
	if active, err := cycle(); err != nil || active {
		t.Fatal("restart ignored the durable controller-wide backoff", err, active)
	}
	f.clock.Advance(time.Second)
	if active, _ := cycle(); !active || captures.Load() != 5 {
		t.Fatal("restarted controller did not capture the request once more", captures.Load())
	}
	if active, err := cycle(); !errors.Is(err, failure) || active {
		t.Fatal("failure after restart not drained", err)
	}
	f.clock.Advance(time.Second)
	if active, err := cycle(); err != nil || active {
		t.Fatal("first failure after restart not backed off", err)
	}
}
