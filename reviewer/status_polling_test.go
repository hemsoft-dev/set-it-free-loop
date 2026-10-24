package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// statusWrites returns every status write in the controller's store.
func statusWrites(t *testing.T, f *pollingFixture) []*WriteIntent {
	t.Helper()
	d, err := f.store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []*WriteIntent
	for _, w := range d.Writes {
		if w.Kind == "status" {
			out = append(out, w)
		}
	}
	return out
}

// settleWorker waits for the active worker and drains its expected
// non-complete error, as TestPollingFactoryDeadline does.
func settleWorker(t *testing.T, c *PollingController) {
	t.Helper()
	awaitPollWorker(t, c)
	c.drainWork()
}

// pollAgain advances the fixture clock and runs one more cycle, settling a
// worker it starts.
func pollAgain(t *testing.T, c *PollingController, f *pollingFixture) {
	t.Helper()
	f.clock.Advance(2 * time.Second)
	result, err := pollOnce(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if result.Active {
		settleWorker(t, c)
	}
}

// Each non-complete outcome publishes exactly one status naming its cause,
// through the outbox, with no findings and no inline comments. A quota_wait
// job resumes every cycle and must not publish its status again.
func TestPollingPublishesOneStatusForEachNonCompleteOutcome(t *testing.T) {
	for state, cause := range map[string]string{
		"quota_wait":           "INCOMPLETE: the daily review quota is exhausted.",
		"auth_failed":          "INCOMPLETE: provider authentication is unavailable.",
		"provider_unavailable": "INCOMPLETE: a selected provider is unavailable.",
		"invalid_output":       "INCOMPLETE: a provider returned output that failed validation.",
		"incomplete_expired":   "INCOMPLETE: this job expired before every required phase completed.",
	} {
		t.Run(state, func(t *testing.T) {
			c, f, _ := pollControllerFixture(t, func(_ context.Context, s Session) (Report, error) {
				if s.Identity.Provider != Claude {
					r := fakeReport(s)
					r.Findings = []Finding{{ID: "codex:partial", Title: "Partial phase canary", Body: "Partial phase canary body", Severity: 1, Anchor: Anchor{"safe.go", 4, "FOUR", s.Snapshot.Head}}}
					return r, nil
				}
				switch state {
				case "quota_wait":
					return Report{}, PhaseError{State: state, BeforeWork: true}
				case "provider_unavailable":
					return Report{}, errors.New("synthetic provider outage")
				}
				return Report{}, PhaseError{State: state}
			})
			if _, err := pollOnce(t, c); err != nil {
				t.Fatal(err)
			}
			settleWorker(t, c)
			for range 3 {
				pollAgain(t, c, f)
			}
			statuses := statusWrites(t, f)
			if len(statuses) != 1 || f.posts != 1 {
				t.Fatal("status not published exactly once", len(statuses), f.posts)
			}
			w := statuses[0]
			d, _ := f.store.Snapshot(context.Background())
			var job *DurableJob
			for _, j := range d.Jobs {
				job = j
			}
			if w.State != "confirmed" || job == nil || job.State != state || w.JobHash != Digest(job.Job) || w.RequestID != job.RequestID || w.Generation != job.Job.Generation || w.Snapshot != job.Job.Snapshot {
				t.Fatal("status not bound to its job and confirmed", w.State, job)
			}
			summary := w.Document.Summary
			if !strings.Contains(summary, "## SFL advisory review status") || !strings.Contains(summary, cause) || !strings.Contains(summary, "No substantive partial findings are published.") || len(w.Document.Inline) != 0 {
				t.Fatal("status does not name its cause", summary)
			}
			if len(job.Checkpoint.Artifacts) != 1 || strings.Contains(summary, "Partial phase canary") || strings.Contains(summary, "safe.go") {
				t.Fatal("status published a partial finding", len(job.Checkpoint.Artifacts))
			}
			if len(d.Writes) != 1 {
				t.Fatal("status wrote anything else", len(d.Writes))
			}
		})
	}
}

// A lost POST response leaves the status unknown. Later cycles reconcile it
// by its marker and never POST it again.
func TestPollingReconcilesLostStatusResponseByMarker(t *testing.T) {
	c, f, _ := pollControllerFixture(t, func(context.Context, Session) (Report, error) {
		return Report{}, PhaseError{State: "auth_failed"}
	})
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	settleWorker(t, c)
	f.lost = true
	f.clock.Advance(2 * time.Second)
	if _, err := pollOnce(t, c); !errors.Is(err, ErrDeferred) {
		t.Fatal("lost status response not reported", err)
	}
	statuses := statusWrites(t, f)
	if len(statuses) != 1 || statuses[0].State != "unknown" || f.posts != 1 {
		t.Fatal("lost response not retained as unknown", len(statuses), f.posts)
	}
	f.lost = false
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	statuses = statusWrites(t, f)
	if len(statuses) != 1 || statuses[0].State != "confirmed" || f.posts != 1 || f.scans == 0 {
		t.Fatal("unknown status not reconciled by marker without a second POST", len(statuses), f.posts, f.scans)
	}
}

// Owner cancellation acknowledges the command and publishes one cancelled
// status for the request's job, with its own wording.
func TestPollingPublishesCancelledStatus(t *testing.T) {
	started := make(chan struct{})
	c, f, _ := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		close(started)
		<-ctx.Done()
		return Report{}, ctx.Err()
	})
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	<-started
	f.clock.Advance(2 * time.Second)
	f.mu.Lock()
	f.batch.Commands = []CommandEvent{commandEvent(50, "/sfl cancel", f.clock.Now())}
	f.mu.Unlock()
	if _, err := pollOnce(t, c); err != nil && !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	settleWorker(t, c)
	for range 2 {
		pollAgain(t, c, f)
	}
	statuses := statusWrites(t, f)
	if len(statuses) != 1 || statuses[0].State != "confirmed" || !strings.Contains(statuses[0].Document.Summary, "CANCELLED: this review request was cancelled before the job completed.") {
		t.Fatal("cancelled status not published once", len(statuses))
	}
	if f.posts != 2 {
		t.Fatal("expected one acknowledgment and one status", f.posts)
	}
}

// A newer request replaces a job whose status promised later work. The
// status is annotated superseded in place, never reposted, and the replaced
// job gets no new status.
func TestPollingAnnotatesReplacedJobStatus(t *testing.T) {
	failing := true
	c, f, _ := pollControllerFixture(t, func(_ context.Context, s Session) (Report, error) {
		if failing {
			return Report{}, PhaseError{State: "auth_failed"}
		}
		return fakeReport(s), nil
	})
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	settleWorker(t, c)
	pollAgain(t, c, f)
	if statuses := statusWrites(t, f); len(statuses) != 1 || statuses[0].State != "confirmed" || f.posts != 1 {
		t.Fatal("auth_failed status not confirmed", len(statuses), f.posts)
	}
	failing = false
	f.clock.Advance(time.Minute)
	f.mu.Lock()
	b := native(2, f.clock.Now())
	b.Snapshot = f.remote.Snapshot
	f.batch.Requests = append(f.batch.Requests, b)
	f.remote.Request = b
	f.mu.Unlock()
	pollAgain(t, c, f)
	statuses := statusWrites(t, f)
	if len(statuses) != 1 || statuses[0].State != "superseded" || f.annotations != 1 {
		t.Fatal("replaced status not annotated exactly once", len(statuses), f.annotations)
	}
	for range 2 {
		pollAgain(t, c, f)
	}
	if statuses = statusWrites(t, f); len(statuses) != 1 || f.annotations != 1 {
		t.Fatal("superseded job published another status", len(statuses), f.annotations)
	}
}
