package reviewer

import (
	"context"
	"errors"
	"sync"
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
	if ctx.Err() != nil || store != s.options.Store || len(s.pending) != 0 || len(s.active) != 0 {
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
			if source.prunes != 2 || source.capturesReleased != 1 || len(source.pending) != 0 || len(source.active) != 0 {
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
