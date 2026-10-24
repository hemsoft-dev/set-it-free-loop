package reviewer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionBudgetFitsFollowsTheEngineSchedule(t *testing.T) {
	job := testJob()
	artifact := Artifact{}
	for _, c := range []struct {
		consumed, artifacts int
		fits                bool
	}{
		{0, 0, true}, {1, 1, true}, {3, 3, true}, {4, 4, true},
		{1, 0, false}, {2, 1, false}, {4, 3, false}, {4, 0, false},
	} {
		cp := Checkpoint{ConsumedSessions: c.consumed}
		for range c.artifacts {
			cp.Artifacts = append(cp.Artifacts, artifact)
		}
		if sessionBudgetFits(job, cp) != c.fits {
			t.Error("budget rule disagrees with the schedule", c)
		}
	}
	// A paired third round widens schedule and budget alike.
	job.Config.PeerRounds = 2
	if !sessionBudgetFits(job, Checkpoint{ConsumedSessions: 4, Artifacts: make([]Artifact, 4)}) || sessionBudgetFits(job, Checkpoint{ConsumedSessions: 5, Artifacts: make([]Artifact, 4)}) {
		t.Fatal("budget rule ignores the configured rounds")
	}
}

// heldShutdownFixture runs the shutdown grace end to end on the fake
// controller: the first session holds the drain, returns its auth and reports
// the shutdown, and Close requeues the stopped job with that charged session.
func heldShutdownFixture(t *testing.T) (*PollingController, *pollingFixture, *atomic.Int32) {
	t.Helper()
	h := newHeldSession()
	var started atomic.Bool
	c, f, calls := pollControllerFixture(t, func(ctx context.Context, s Session) (Report, error) {
		// Only the first session is the held one; any later session is
		// counted by the fixture and must never run.
		if started.Swap(true) {
			return fakeReport(s), nil
		}
		release := h.hold(t, ctx)
		defer release()
		return Report{}, errors.Join(PhaseError{State: "provider_unavailable"}, context.Canceled)
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
	return c, f, calls
}

// After a shutdown that proved completion, the requeued job keeps a charged
// session without a result and can no longer finish its schedule. The
// restarted controller ends it incomplete without restoring evidence,
// building a runtime, claiming it or starting a session, and publishes one
// INCOMPLETE status that names the cause.
func TestPollingEndsAnUnfinishableJobWithoutFurtherSessions(t *testing.T) {
	c, f, calls := heldShutdownFixture(t)
	var restores, runtimes atomic.Int32
	options := c.options
	restore, runtime := options.Restore, options.Runtime
	options.Restore = func(ctx context.Context, job Job) (*JobEvidence, error) {
		restores.Add(1)
		return restore(ctx, job)
	}
	options.Runtime = func(ctx context.Context, job Job, bound *JobEvidence) (Engine, error) {
		runtimes.Add(1)
		return runtime(ctx, job, bound)
	}
	reopened, err := OpenPollingController(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close(context.Background()) })
	f.clock.Advance(2 * time.Second)
	if _, err := pollOnce(t, reopened); err != nil {
		t.Fatal(err)
	}
	settleWorker(t, reopened)
	for range 3 {
		pollAgain(t, reopened, f)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, j := range d.Jobs {
		if j.State != "incomplete_expired" || j.LeaseToken != "" || j.Outstanding || j.Checkpoint.ConsumedSessions != 1 || len(j.Checkpoint.Artifacts) != 0 {
			t.Fatal("unfinishable job not ended at once", j.State, j.Checkpoint.ConsumedSessions)
		}
	}
	if calls.Load() != 1 || restores.Load() != 0 || runtimes.Load() != 0 {
		t.Fatal("an unfinishable job spent work", calls.Load(), restores.Load(), runtimes.Load())
	}
	statuses := statusWrites(t, f)
	if len(statuses) != 1 || f.posts != 1 || statuses[0].Status != "incomplete_expired" || statuses[0].State != "confirmed" {
		t.Fatal("not exactly one confirmed INCOMPLETE status", len(statuses), f.posts)
	}
	if !strings.Contains(statuses[0].Document.Summary, "INCOMPLETE: the captured session budget is exhausted. 1 of its 4 captured sessions are charged, 1 of them without a result") {
		t.Fatal("status does not name the exhausted budget", statuses[0].Document.Summary)
	}
}

// A job that cannot finish never waits for quota: the claim gate ends it
// before the daily limit, without a lease or session.
func TestClaimEndsAnUnfinishableQuotaWaitBeforeTheDailyLimit(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	if err := s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[job.ID]
		j.State, j.Checkpoint.ConsumedSessions, j.ChargeDays = "quota_wait", 1, []string{"2026-10-01"}
		for i := range j.Job.Config.DailyReviews {
			other := *j
			other.Job.ID = job.ID + "-charged-" + string(rune('a'+i))
			other.State, other.ChargeDays = "dual_complete", []string{controlTime.UTC().Format("2006-01-02")}
			d.Jobs[other.Job.ID] = &other
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, job.ID, controlTime.Add(4*time.Minute)); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	if j := d.Jobs[job.ID]; j.State != "incomplete_expired" || j.LeaseToken != "" || j.Checkpoint.ConsumedSessions != 1 {
		t.Fatal("unfinishable job kept waiting for quota", j.State)
	}
}

// ResumeUnavailable never requeues a job its budget cannot finish, and still
// resumes one it can.
func TestResumeUnavailableRefusesAnUnfinishableJob(t *testing.T) {
	for _, c := range []struct {
		consumed, artifacts int
		want                error
		state               string
	}{{2, 1, ErrSessionBudgetExhausted, "provider_unavailable"}, {1, 1, nil, "queued"}} {
		s, job, _ := admittedFixture(t)
		ctx := context.Background()
		if err := s.update(ctx, func(d *DurableState) error {
			j := d.Jobs[job.ID]
			j.State, j.Checkpoint.ConsumedSessions, j.Checkpoint.Artifacts = "provider_unavailable", c.consumed, make([]Artifact, c.artifacts)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.ResumeUnavailable(ctx, job.ID, controlTime.Add(4*time.Minute)); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Fatal("resume decision", c, err)
		}
		d, _ := s.Snapshot(ctx)
		if d.Jobs[job.ID].State != c.state {
			t.Fatal("resume changed the wrong job state", c, d.Jobs[job.ID].State)
		}
	}
}

// A resumable status of a job that can no longer finish does not promise a
// resume.
func TestStatusOfAnUnfinishableJobPromisesNoResume(t *testing.T) {
	job := testJob()
	for _, state := range []string{"auth_failed", "provider_unavailable"} {
		cause := statusBudgetCause(job, state, 1, nil)
		if !strings.Contains(cause, "1 of its 4 captured sessions are charged, 1 of them without a result") || !strings.Contains(cause, "cannot be resumed") || strings.Contains(cause, "explicitly resumes") {
			t.Fatal("unfinishable status still promises a resume", state, cause)
		}
		if statusBudgetCause(job, state, 1, make([]Artifact, 1)) != statusCause(job, state) {
			t.Fatal("a finishable job's status changed", state)
		}
	}
}
