package reviewer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestShutdownDrainGrantsOneGraceOnlyToAHeldExport(t *testing.T) {
	d := newShutdownDrain()
	if d.begin() {
		t.Fatal("a drain without a held export granted a grace")
	}
	if _, ok := d.hold(); ok {
		t.Fatal("an export was admitted after shutdown began")
	}
	d = newShutdownDrain()
	release, ok := d.hold()
	if !ok || d.draining() {
		t.Fatal("hold refused before shutdown", ok)
	}
	if !d.begin() || !d.draining() || d.signal.Err() == nil {
		t.Fatal("a held export did not get the grace and its signal")
	}
	if d.begin() {
		t.Fatal("a second Close got a second grace")
	}
	release()
	release()
	if d.held != 0 {
		t.Fatal("release is not idempotent", d.held)
	}
	if shutdownDrainOf(context.Background()) != nil || shutdownDraining(context.Background()) {
		t.Fatal("an unrelated context carries a drain")
	}
	if shutdownDrainOf(withShutdownDrain(context.Background(), d)) != d {
		t.Fatal("the invocation context lost its drain")
	}
}

// graceClock is the fake shutdown timer: it records each requested grace and
// fires only when the test says so.
type graceClock struct {
	mu        sync.Mutex
	requested []time.Duration
	fire      chan time.Time
}

func newGraceClock(c *PollingController) *graceClock {
	g := &graceClock{fire: make(chan time.Time)}
	c.after = func(d time.Duration) <-chan time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.requested = append(g.requested, d)
		return g.fire
	}
	return g
}

func (g *graceClock) calls() []time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Duration(nil), g.requested...)
}

// heldSession is a fake provider session that holds the drain like an
// exported native launch: it reports when shutdown signals it and whether its
// context was cancelled by then.
type heldSession struct {
	held, signalled chan struct{}
	cancelledEarly  bool
}

func newHeldSession() *heldSession {
	return &heldSession{held: make(chan struct{}), signalled: make(chan struct{})}
}

func (h *heldSession) hold(t *testing.T, ctx context.Context) func() {
	t.Helper()
	d := shutdownDrainOf(ctx)
	if d == nil {
		t.Error("controller invocation carries no shutdown drain")
		return func() {}
	}
	release, ok := d.hold()
	if !ok {
		t.Error("drain refused a session before Close")
		return func() {}
	}
	close(h.held)
	select {
	case <-d.signal.Done():
	case <-ctx.Done():
	}
	h.cancelledEarly = d.signal.Err() == nil || ctx.Err() != nil
	close(h.signalled)
	return release
}

func awaitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(what)
	}
}

func closeAsync(c *PollingController, ctx context.Context) chan error {
	done := make(chan error, 1)
	go func() { done <- c.Close(ctx) }()
	return done
}

// A held session that returns auth after its intake is revoked ends within
// the grace: Close proves completion and the stopped job is requeued with its
// consumed session. No later phase starts.
func TestPollingControllerShutdownGraceLetsAHeldSessionReturn(t *testing.T) {
	h := newHeldSession()
	c, f, calls := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		release := h.hold(t, ctx)
		defer release()
		// The stopped native launch reports the shutdown with its own state.
		return Report{}, errors.Join(PhaseError{State: "provider_unavailable"}, context.Canceled)
	})
	g := newGraceClock(c)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, h.held, "session never held the drain")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal("Close did not prove a session that ended within the grace", err)
	}
	if h.cancelledEarly {
		t.Fatal("the held session was cancelled instead of signalled")
	}
	if got := g.calls(); len(got) != 1 || got[0] != providerShutdownGrace {
		t.Fatal("Close did not bound its wait by the documented grace", got)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "queued" || j.LeaseToken != "" || j.Outstanding || j.Checkpoint.ConsumedSessions != 1 || len(j.Checkpoint.Artifacts) != 0 {
			t.Fatal("drained session lost its stop, charge or requeue", j.State, j.LeaseToken, j.Outstanding, j.Checkpoint.ConsumedSessions)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("drain admitted another phase", calls.Load())
	}
}

// A session that completes during the grace keeps its artifact; the engine
// then starts no further phase and the job is requeued.
func TestPollingControllerShutdownGraceStartsNoFurtherPhase(t *testing.T) {
	h := newHeldSession()
	c, f, calls := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		release := h.hold(t, ctx)
		defer release()
		return fakeReport(s), nil
	})
	newGraceClock(c)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, h.held, "session never held the drain")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "queued" || j.LeaseToken != "" || j.Checkpoint.ConsumedSessions != 1 || len(j.Checkpoint.Artifacts) != 1 {
			t.Fatal("completed drained phase was lost or followed by another", j.State, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts))
		}
	}
	if calls.Load() != 1 {
		t.Fatal("drain admitted another phase", calls.Load())
	}
}

// A guest that never returns: Close waits exactly until the grace timer
// fires, never longer, then cancels; the uncertain stop keeps the lease and
// reports ErrWorkerStopPending as before.
func TestPollingControllerShutdownGraceExpiresIntoStopPending(t *testing.T) {
	h := newHeldSession()
	stopped := make(chan struct{})
	c, f, _ := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		release := h.hold(t, ctx)
		defer release()
		<-ctx.Done()
		close(stopped)
		return Report{}, errors.Join(ErrWorkerStopPending, ctx.Err())
	})
	g := newGraceClock(c)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, h.held, "session never held the drain")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := closeAsync(c, ctx)
	awaitClosed(t, h.signalled, "held session was never signalled")
	select {
	case err := <-done:
		t.Fatal("Close returned before the grace expired", err)
	case <-stopped:
		t.Fatal("the session was cancelled before the grace expired")
	case <-time.After(50 * time.Millisecond):
	}
	if got := g.calls(); len(got) != 1 || got[0] != providerShutdownGrace {
		t.Fatal("grace not requested once at its documented bound", got)
	}
	g.fire <- time.Now()
	awaitClosed(t, stopped, "grace expiry did not cancel the session")
	if err := <-done; !errors.Is(err, ErrWorkerStopPending) {
		t.Fatal("an unproved stop was hidden", err)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.LeaseToken == "" || !j.Outstanding {
			t.Fatal("uncertain session lost its lease or reservation", j.State)
		}
	}
	// A retry gets no second grace.
	retry, cancelRetry := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelRetry()
	if err := c.Close(retry); !errors.Is(err, ErrWorkerStopPending) {
		t.Fatal(err)
	}
	if got := g.calls(); len(got) != 1 {
		t.Fatal("a retried Close waited a second grace", got)
	}
}

// The caller's earlier deadline bounds the grace too.
func TestPollingControllerShutdownGraceNeverOutlastsCloseDeadline(t *testing.T) {
	h := newHeldSession()
	stopped := make(chan struct{})
	c, _, _ := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		release := h.hold(t, ctx)
		defer release()
		<-ctx.Done()
		close(stopped)
		return Report{}, errors.Join(ErrWorkerStopPending, ctx.Err())
	})
	newGraceClock(c) // never fires
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, h.held, "session never held the drain")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := c.Close(ctx); err == nil {
		t.Fatal("Close proved an uncertain session")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatal("Close outlasted its deadline", elapsed)
	}
	awaitClosed(t, stopped, "deadline did not cancel the held session")
}

// Close outside a held provider export cancels at once and never starts the
// grace timer.
func TestPollingControllerShutdownOutsideProviderSessionHasNoGrace(t *testing.T) {
	entered := make(chan struct{})
	c, f, calls := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		close(entered)
		<-ctx.Done()
		return Report{}, ctx.Err()
	})
	c.after = func(time.Duration) <-chan time.Time {
		t.Error("a session without exported auth started the grace")
		return make(chan time.Time)
	}
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, entered, "worker never started")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "queued" || j.LeaseToken != "" {
			t.Fatal("unheld shutdown changed the existing requeue", j.State)
		}
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}

	// Nor during factory work before any session.
	c2, _, _ := pollControllerFixture(t, nil)
	c2.after = c.after
	factory := make(chan struct{})
	c2.options.Runtime = func(ctx context.Context, _ Job, _ *JobEvidence) (Engine, error) {
		close(factory)
		<-ctx.Done()
		return Engine{}, ctx.Err()
	}
	if _, err := pollOnce(t, c2); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, factory, "factory never started")
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := c2.Close(ctx2); err != nil {
		t.Fatal(err)
	}
}
