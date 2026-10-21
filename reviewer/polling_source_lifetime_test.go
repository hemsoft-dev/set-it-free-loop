package reviewer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pollingOwnedSource struct {
	mu                                     sync.Mutex
	options                                PollOptions
	pending                                map[*SnapshotEvidence]bool
	active                                 map[*JobEvidence]bool
	prunes, capturesReleased, jobsReleased int
	afterCapture                           func()
	captureErr                             error
}

func (s *pollingOwnedSource) Capture(ctx context.Context, snapshot Snapshot) (*SnapshotEvidence, error) {
	e, err := s.options.Capture(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.pending[e] = true
	s.mu.Unlock()
	if s.afterCapture != nil {
		s.afterCapture()
	}
	return e, s.captureErr
}

func (s *pollingOwnedSource) Restore(ctx context.Context, job Job) (*JobEvidence, error) {
	e, err := s.options.Restore(ctx, job)
	if err == nil {
		s.mu.Lock()
		s.active[e] = true
		s.mu.Unlock()
	}
	return e, err
}

func (s *pollingOwnedSource) ReleaseCapture(ctx context.Context, e *SnapshotEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || !s.pending[e] {
		return ErrFenced
	}
	delete(s.pending, e)
	s.capturesReleased++
	return nil
}

func (s *pollingOwnedSource) ReleaseJob(ctx context.Context, e *JobEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || !s.active[e] {
		return ErrFenced
	}
	delete(s.active, e)
	s.jobsReleased++
	return nil
}

func (s *pollingOwnedSource) PruneSource(ctx context.Context, store *Store, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || store != s.options.Store {
		return ErrFenced
	}
	s.prunes++
	return nil
}

func TestPollingOwnedSourceReleasesAfterAdmissionAndOnFencing(t *testing.T) {
	for _, mode := range []string{"complete", "fenced-after-capture", "capture-error"} {
		t.Run(mode, func(t *testing.T) {
			old, f, calls := pollControllerFixture(t, nil)
			source := &pollingOwnedSource{options: old.options, pending: map[*SnapshotEvidence]bool{}, active: map[*JobEvidence]bool{}}
			if mode == "fenced-after-capture" {
				source.afterCapture = func() { f.mu.Lock(); f.remote.Open = false; f.mu.Unlock() }
			} else if mode == "capture-error" {
				source.captureErr = errors.New("synthetic post-capture failure")
			}
			options := old.options
			options.Source = source
			if c, err := OpenPollingController(context.Background(), options); err == nil || c != nil {
				t.Fatal("mixed source and raw callbacks accepted")
			}
			if old.Close(context.Background()) != nil {
				t.Fatal("old fixture close")
			}
			options.Capture, options.Restore = nil, nil
			originalRuntime := options.Runtime
			options.Runtime = func(ctx context.Context, job Job, e *JobEvidence) (Engine, error) {
				source.mu.Lock()
				valid := len(source.pending) == 0 && source.active[e]
				source.mu.Unlock()
				if !valid {
					return Engine{}, errors.New("source released before work or capture held past admission")
				}
				return originalRuntime(ctx, job, e)
			}
			c, err := OpenPollingController(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(context.Background())
			if result, err := pollOnce(t, c); err != nil || !result.Active {
				t.Fatal(result, err)
			}
			awaitPollWorker(t, c)
			source.mu.Lock()
			defer source.mu.Unlock()
			if source.prunes != 3 || source.capturesReleased != 1 || len(source.pending) != 0 || len(source.active) != 0 {
				t.Fatal("source lifetime leaked", source.prunes, source.capturesReleased, len(source.pending), len(source.active))
			}
			if mode == "complete" {
				if calls.Load() != 4 || source.jobsReleased != 1 {
					t.Fatal("admitted source not held through work", calls.Load(), source.jobsReleased)
				}
			} else if calls.Load() != 0 || source.jobsReleased != 0 {
				t.Fatal("fenced source reached review", calls.Load(), source.jobsReleased)
			}
		})
	}
}

func TestPollingOwnedSourcePrunesWhileIdle(t *testing.T) {
	old, f, calls := pollControllerFixture(t, nil)
	options := old.options
	// Retain an actual expired terminal job, with no worker or pending writes.
	mustCapture(t, options.Store, nil, controlTime.Add(2*time.Minute))
	if options.Store.RecordRequest(context.Background(), alphaScope(), f.remote.Request, true) != nil {
		t.Fatal("retained request fixture")
	}
	evidence, err := options.Capture(context.Background(), f.remote.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job, err := options.Store.Admit(context.Background(), alphaScope(), f.remote.Request.ID, options.Release, evidence.Packet(), controlTime.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := options.Store.update(context.Background(), func(d *DurableState) error { d.Jobs[job.ID].State = "cancelled"; return nil }); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(40 * 24 * time.Hour)
	if old.Close(context.Background()) != nil {
		t.Fatal("old controller close")
	}
	source := &pollingOwnedSource{options: options, pending: map[*SnapshotEvidence]bool{}, active: map[*JobEvidence]bool{}}
	options.Source, options.Capture, options.Restore = source, nil, nil
	f.mu.Lock()
	f.scopes = nil
	f.remote.Open = false
	f.mu.Unlock()
	c, err := OpenPollingController(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	for i := 0; i < 2; i++ {
		if result, err := pollOnce(t, c); err != nil || result.Active {
			t.Fatal("idle poll", result, err)
		}
		f.clock.Advance(time.Second)
	}
	state, err := options.Store.Snapshot(context.Background())
	if err != nil || len(state.Jobs) != 0 {
		t.Fatal("idle retention did not prune expired job", err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.prunes != 2 || source.capturesReleased != 0 || source.jobsReleased != 0 || calls.Load() != 0 {
		t.Fatal("idle source retention did not run without work", source.prunes, calls.Load())
	}
}

func TestPollingOldReleaseQualificationPrecedesRestore(t *testing.T) {
	c, _, _ := pollControllerFixture(t, nil)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	awaitPollWorker(t, c)
	state, err := c.options.Store.Snapshot(context.Background())
	if err != nil || len(state.Jobs) != 1 {
		t.Fatal("durable job fixture", err)
	}
	var job Job
	for _, stored := range state.Jobs {
		job = stored.Job
	}
	c.options.Release = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	oldChecks, restores := 0, 0
	c.options.Check = func(_ context.Context, _ Enrollment, release string) error {
		if release == job.Release {
			oldChecks++
			return errors.New("old release revoked")
		}
		return nil
	}
	c.options.Restore = func(context.Context, Job) (*JobEvidence, error) {
		restores++
		return nil, errors.New("unexpected restore")
	}
	work := &pollWork{scope: alphaScope(), request: 1, generation: job.Generation, snapshot: job.Snapshot, job: job.ID}
	if bound, _, err := c.beforeWork(context.Background(), work); bound != nil || !errors.Is(err, ErrLiveBlocked) || oldChecks != 1 || restores != 0 {
		t.Fatal("old release source exposed before qualification", bound, err, oldChecks, restores)
	}
}

type pollingBusySource struct {
	*pollingOwnedSource
	entered, release chan struct{}
	busy             atomic.Bool
}

func (s *pollingBusySource) Capture(ctx context.Context, snapshot Snapshot) (*SnapshotEvidence, error) {
	s.busy.Store(true)
	close(s.entered)
	defer s.busy.Store(false)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
	}
	return s.pollingOwnedSource.Capture(ctx, snapshot)
}
func (s *pollingBusySource) PruneSource(ctx context.Context, store *Store, now time.Time) error {
	if s.busy.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.release:
		}
	}
	return s.pollingOwnedSource.PruneSource(ctx, store, now)
}
func TestPollingSkipsRetentionWhileCaptureHoldsSource(t *testing.T) {
	old, f, calls := pollControllerFixture(t, nil)
	options := old.options
	if old.Close(context.Background()) != nil {
		t.Fatal("old controller close")
	}
	source := &pollingBusySource{pollingOwnedSource: &pollingOwnedSource{options: options, pending: map[*SnapshotEvidence]bool{}, active: map[*JobEvidence]bool{}}, entered: make(chan struct{}), release: make(chan struct{})}
	options.Source, options.Capture, options.Restore = source, nil, nil
	c, err := OpenPollingController(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(source.release); _ = c.Close(context.Background()) }()
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("capture not entered")
	}
	f.clock.Advance(time.Second)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal("healthy capture caused retention backoff", result, err)
	}
	state, err := options.Store.Snapshot(context.Background())
	if err != nil || state.Polling.Failures != 0 || calls.Load() != 0 {
		t.Fatal("active capture polling recorded spurious failure", err, calls.Load())
	}
}
