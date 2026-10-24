package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// statusFixture admits request 1 and releases its job in state.
func statusFixture(t *testing.T, state string) (*Store, Job, *fakeRemote) {
	t.Helper()
	s, job, remote := admittedFixture(t)
	ctx := context.Background()
	token, err := s.Claim(ctx, job.ID, controlTime.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseStopped(ctx, job.ID, token, state); err != nil {
		t.Fatal(err)
	}
	return s, job, remote
}

func soleStatus(t *testing.T, s *Store) WriteIntent {
	t.Helper()
	statuses := writes(t, s, "status")
	if len(statuses) != 1 {
		t.Fatal("want exactly one status write", len(statuses))
	}
	return statuses[0]
}

// durableDigests digests every job and each PR's request ledger: its
// generation, pause and requests. A history cutoff may still advance.
func durableDigests(t *testing.T, s *Store) (jobs, prs string) {
	t.Helper()
	d, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ledgers := map[string]any{}
	for key, pr := range d.PRs {
		ledgers[key] = []any{pr.Generation, pr.Paused, pr.PauseRequestID, pr.CurrentRequest, pr.Requests}
	}
	return Digest(d.Jobs), Digest(ledgers)
}

// annotationRemote records each supersession reason.
type annotationRemote struct {
	*fakeRemote
	reasons []string
}

func (a *annotationRemote) Annotate(ctx context.Context, scope Scope, id int64, reason string) error {
	a.reasons = append(a.reasons, reason)
	return a.fakeRemote.Annotate(ctx, scope, id, reason)
}

// Every status persists its intent and marker before POST. A lost response is
// publication_unknown: no second POST, an empty scan defers, and only the
// marked receipt confirms it. The status changes no job or request.
func TestStatusOutboxReconcilesLostResponseForEachState(t *testing.T) {
	for _, state := range []string{"quota_wait", "auth_failed", "provider_unavailable", "invalid_output", "incomplete_expired", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s, job, remote := statusFixture(t, state)
			ctx := context.Background()
			p := Publisher{Store: s, Remote: remote, Now: func() time.Time { return controlTime.Add(5 * time.Minute) }}
			jobsBefore, prsBefore := durableDigests(t, s)
			if err := p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			w := soleStatus(t, s)
			if w.State != "prepared" || w.Status != state || w.JobHash != Digest(job) || w.RequestID != 1 || w.Generation != job.Generation || w.Snapshot != job.Snapshot || w.Marker != "<!-- sfl-alpha:"+w.ID+" -->" || !strings.HasSuffix(w.Document.Summary, w.Marker) || len(w.Document.Inline) != 0 || w.BodyHash != Digest(w.Document) {
				t.Fatalf("status intent not bound to its job: %+v", w)
			}
			if !strings.Contains(w.Document.Summary, statusCause(job, state)) {
				t.Fatal("status does not name its cause", w.Document.Summary)
			}
			remote.post = func(w WriteIntent) (RemoteReceipt, error) {
				remote.receipts = append(remote.receipts, RemoteReceipt{40, 9003, w.Marker, w.BodyHash, controlTime.Add(6 * time.Minute), w.Snapshot.Head})
				return RemoteReceipt{}, errors.New("lost response")
			}
			if err := p.Dispatch(ctx, w.ID); !errors.Is(err, ErrDeferred) {
				t.Fatal(err)
			}
			if got := soleStatus(t, s); got.State != "unknown" {
				t.Fatal("lost response is not publication_unknown", got.State)
			}
			if err := p.Dispatch(ctx, w.ID); !errors.Is(err, ErrDeferred) || remote.posts != 1 {
				t.Fatal("unknown status POSTed again", err, remote.posts)
			}
			saved := remote.receipts
			remote.receipts = nil
			if err := p.Reconcile(ctx, w.ID); !errors.Is(err, ErrDeferred) {
				t.Fatal("empty scan confirmed or retried", err)
			}
			remote.receipts = append(saved, RemoteReceipt{41, 666, w.Marker, w.BodyHash, controlTime.Add(6 * time.Minute), w.Snapshot.Head})
			if err := p.Reconcile(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
			if got := soleStatus(t, s); got.State != "confirmed" || got.ReceiptID != 40 {
				t.Fatal("marked receipt not confirmed", got.State, got.ReceiptID)
			}
			if err := p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			soleStatus(t, s)
			if jobs, prs := durableDigests(t, s); jobs != jobsBefore || prs != prsBefore || remote.posts != 1 {
				t.Fatal("status changed a job or request, or POSTed twice", remote.posts)
			}
		})
	}
}

// A resumed quota_wait job publishes its waiting status once. A later
// transition to another state publishes that state once.
func TestStatusOncePerStateAcrossResume(t *testing.T) {
	s, job, remote := statusFixture(t, "quota_wait")
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Dispatch(ctx, soleStatus(t, s).ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		token, err := s.Claim(ctx, job.ID, controlTime.Add(5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.ReleaseStopped(ctx, job.ID, token, "quota_wait"); err != nil {
			t.Fatal(err)
		}
		if err = p.PrepareStatuses(ctx); err != nil {
			t.Fatal(err)
		}
		if w := soleStatus(t, s); w.State != "confirmed" || remote.posts != 1 {
			t.Fatal("resumed quota wait duplicated its status", w.State, remote.posts)
		}
	}
	if _, err := s.Claim(ctx, job.ID, controlTime.Add(25*time.Hour)); !errors.Is(err, ErrDeferred) {
		t.Fatal("expired claim", err)
	}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	all := writes(t, s, "status")
	if len(all) != 2 {
		t.Fatal("expiry did not publish its own status", len(all))
	}
	for _, w := range all {
		if w.Status == "incomplete_expired" {
			if err := p.Dispatch(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
		} else if w.State != "confirmed" {
			t.Fatal("earlier status changed", w.State)
		}
	}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if len(writes(t, s, "status")) != 2 || remote.posts != 2 {
		t.Fatal("status repeated", remote.posts)
	}
}

// Owner cancellation publishes a cancelled status for the latest request's
// job, bound to the cancelling generation. It never marks another request:
// a later request still admits its own job, and an earlier or published job
// gets no cancellation status.
func TestOwnerCancellationPublishesStatusWithoutConsumingRequests(t *testing.T) {
	s, job, remote := admittedFixture(t)
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	mustCapture(t, s, []CommandEvent{commandEvent(3, "/sfl cancel", controlTime.Add(3*time.Minute))}, controlTime.Add(4*time.Minute))
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	pr := d.PRs[alphaScope().key()]
	w := soleStatus(t, s)
	if d.Jobs[job.ID].State != "cancelled" || w.Status != "cancelled" || w.RequestID != 1 || w.Generation != pr.Generation || w.Generation == job.Generation || pr.CurrentRequest != 0 {
		t.Fatalf("cancellation status not bound to the cancelling generation: %+v", w)
	}
	if !strings.Contains(w.Document.Summary, "CANCELLED: this review request was cancelled before the job completed.") {
		t.Fatal("cancellation has no wording of its own", w.Document.Summary)
	}
	if err := p.Dispatch(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if soleStatus(t, s).State != "confirmed" || remote.posts != 1 {
		t.Fatal("cancellation status not confirmed")
	}
	// The status postcheck advanced the history cutoff to minute 9.
	if err := s.RecordRequest(ctx, alphaScope(), native(2, controlTime.Add(10*time.Minute)), true); err != nil {
		t.Fatal(err)
	}
	mustCapture(t, s, nil, controlTime.Add(11*time.Minute))
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if w := soleStatus(t, s); w.State != "confirmed" {
		t.Fatal("terminal cancellation status was annotated", w.State)
	}
	if err := p.Dispatch(ctx, writes(t, s, "ack")[0].ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.Admit(ctx, alphaScope(), 2, job.Release, "evidence", controlTime.Add(11*time.Minute))
	if err != nil || next.ID == job.ID {
		t.Fatal("status consumed the later request", next.ID, err)
	}
	mustCapture(t, s, []CommandEvent{commandEvent(4, "/sfl cancel", controlTime.Add(12*time.Minute))}, controlTime.Add(13*time.Minute))
	if err = p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	statuses := writes(t, s, "status")
	if len(statuses) != 2 {
		t.Fatal("latest cancelled job did not get exactly one new status", len(statuses))
	}
	for _, w := range statuses {
		if w.JobHash == Digest(job) && w.Status != "cancelled" {
			t.Fatal("earlier job reported again")
		}
	}
}

// A job whose completed review is published gets no cancellation status.
func TestPublishedReviewSuppressesCancellationStatus(t *testing.T) {
	s, out, remote := completeFixture(t)
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	intent, err := p.Prepare(ctx, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Dispatch(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	mustCapture(t, s, []CommandEvent{commandEvent(3, "/sfl cancel", controlTime.Add(10*time.Minute))}, controlTime.Add(11*time.Minute))
	if err = p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if len(writes(t, s, "status")) != 0 {
		t.Fatal("published review got a cancellation status")
	}
}

// Every review fence also guards a status before its POST: access defers,
// and a closed PR, moved head or base, other or non-owner request, wrong bot,
// local generation change, pause or changed job state retires the status
// without POST and without touching the PR's requests.
func TestStatusFencesBeforeDispatch(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *Store, *fakeRemote, Job) bool{
		"access-lost": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool { r.state.Access = false; return false },
		"closed":      func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool { r.state.Open = false; return true },
		"head-moved": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool {
			r.state.Snapshot.Head = strings.Repeat("e", 40)
			return true
		},
		"base-moved": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool {
			r.state.Snapshot.Base = strings.Repeat("f", 40)
			return true
		},
		"no-request": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool {
			r.state.Request = NativeRequest{}
			return true
		},
		"request-B": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool {
			r.state.Request = native(2, controlTime.Add(6*time.Minute))
			return true
		},
		"non-owner": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool {
			r.state.Request.RequesterID = 666
			return true
		},
		"wrong-bot":   func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool { r.state.Request.BotID = 1; return true },
		"removed":     func(_ *testing.T, _ *Store, r *fakeRemote, _ Job) bool { r.state.Request.Removed = true; return true },
		"local-pause": func(t *testing.T, s *Store, _ *fakeRemote, _ Job) bool { return pausePR(t, s) },
		"local-request": func(t *testing.T, s *Store, _ *fakeRemote, _ Job) bool {
			if err := s.RecordRequest(context.Background(), alphaScope(), native(2, controlTime.Add(6*time.Minute)), true); err != nil {
				t.Fatal(err)
			}
			return true
		},
		"job-resumed": func(t *testing.T, s *Store, _ *fakeRemote, job Job) bool {
			if err := s.ResumeUnavailable(context.Background(), job.ID, controlTime.Add(5*time.Minute)); err != nil {
				t.Fatal(err)
			}
			return true
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, job, remote := statusFixture(t, "auth_failed")
			ctx := context.Background()
			p := Publisher{Store: s, Remote: remote}
			if err := p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			w := soleStatus(t, s)
			retired := change(t, s, remote, job)
			_, prsBefore := durableDigests(t, s)
			err := p.Dispatch(ctx, w.ID)
			got := soleStatus(t, s)
			if remote.posts != 0 || err == nil {
				t.Fatal("fenced status was POSTed", err, remote.posts)
			}
			if retired && (got.State != "stale" || !errors.Is(err, ErrFenced)) {
				t.Fatal("fenced status not retired", got.State, err)
			}
			if !retired && (got.State != "prepared" || !errors.Is(err, ErrDeferred)) {
				t.Fatal("lost access did not defer", got.State, err)
			}
			if _, prs := durableDigests(t, s); prs != prsBefore {
				t.Fatal("pre-dispatch status fence changed the PR record")
			}
			if err = p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			if len(writes(t, s, "status")) != 1 {
				t.Fatal("retired status was prepared again")
			}
		})
	}
}

func pausePR(t *testing.T, s *Store) bool {
	t.Helper()
	if err := s.update(context.Background(), func(d *DurableState) error {
		d.PRs[alphaScope().key()].Paused = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return true
}

// The local sweep retires a prepared status whose job left its state before
// dispatch, without any remote call.
func TestPrepareStatusesRetiresStatusOfChangedJob(t *testing.T) {
	s, job, remote := statusFixture(t, "auth_failed")
	ctx := context.Background()
	p := Publisher{Store: s, Remote: remote}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, alphaScope(), native(2, controlTime.Add(6*time.Minute)), true); err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if w := soleStatus(t, s); w.State != "stale" || remote.posts != 0 {
		t.Fatal("replaced job's prepared status not retired", w.State)
	}
	d, _ := s.Snapshot(ctx)
	if d.Jobs[job.ID].State != "stale" {
		t.Fatal("replacement did not supersede the job")
	}
}

// If request B appears while the status POST is in flight, the status cannot
// fulfill B: B stays unresolved and paused, and the status is annotated.
func TestStatusPublicationRaceRetainsBAndAnnotates(t *testing.T) {
	s, _, base := statusFixture(t, "auth_failed")
	ctx := context.Background()
	remote := &annotationRemote{fakeRemote: base}
	p := Publisher{Store: s, Remote: remote}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	w := soleStatus(t, s)
	base.post = func(w WriteIntent) (RemoteReceipt, error) {
		if err := s.RecordRequest(ctx, alphaScope(), native(2, controlTime.Add(6*time.Minute)), true); err != nil {
			t.Fatal(err)
		}
		base.state.Request = NativeRequest{}
		return RemoteReceipt{34, 9003, w.Marker, w.BodyHash, controlTime.Add(7 * time.Minute), w.Snapshot.Head}, nil
	}
	if err := p.Dispatch(ctx, w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	pr := d.PRs[alphaScope().key()]
	if pr.CurrentRequest != 2 || !pr.Paused || d.Writes[w.ID].State != "superseded" || len(remote.reasons) != 1 || remote.reasons[0] != statusSupersession {
		t.Fatal("status silently fulfilled B", pr.CurrentRequest, pr.Paused, d.Writes[w.ID].State, remote.reasons)
	}
	if _, err := s.Admit(ctx, alphaScope(), 2, testJob().Release, "evidence", controlTime.Add(8*time.Minute)); !errors.Is(err, ErrDeferred) {
		t.Fatal("ambiguous B inferred", err)
	}
}

// A confirmed status that promised later work is annotated in place once its
// job is replaced or cancelled, idempotently and only with access. Terminal
// statuses and statuses already followed by a later write are left alone.
func TestReplacedResumableStatusIsAnnotatedOnce(t *testing.T) {
	for _, tc := range []struct {
		state, replacement string
		annotate           bool
	}{
		{"auth_failed", "request", true},
		{"quota_wait", "cancel", true},
		{"provider_unavailable", "request", true},
		{"invalid_output", "request", false},
		{"incomplete_expired", "cancel", false},
	} {
		t.Run(tc.state+"-"+tc.replacement, func(t *testing.T) {
			s, _, base := statusFixture(t, tc.state)
			ctx := context.Background()
			remote := &annotationRemote{fakeRemote: base}
			p := Publisher{Store: s, Remote: remote}
			if err := p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			w := soleStatus(t, s)
			if err := p.Dispatch(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
			if tc.replacement == "request" {
				if err := s.RecordRequest(ctx, alphaScope(), native(2, controlTime.Add(10*time.Minute)), true); err != nil {
					t.Fatal(err)
				}
			} else {
				mustCapture(t, s, []CommandEvent{commandEvent(3, "/sfl cancel", controlTime.Add(10*time.Minute))}, controlTime.Add(11*time.Minute))
			}
			base.state.Access = false
			if err := p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			got := soleStatusFor(t, s, w.ID)
			if !tc.annotate {
				if got.State != "confirmed" || len(remote.reasons) != 0 {
					t.Fatal("terminal status annotated", got.State)
				}
				return
			}
			if got.State != "annotation_pending" {
				t.Fatal("replaced status not marked for annotation", got.State)
			}
			if err := p.Reconcile(ctx, w.ID); !errors.Is(err, ErrDeferred) || len(remote.reasons) != 0 {
				t.Fatal("annotation attempted without access", err)
			}
			base.state.Access = true
			if err := p.Reconcile(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
			if err := p.Reconcile(ctx, w.ID); !errors.Is(err, ErrDeferred) {
				t.Fatal("superseded status reconciled again", err)
			}
			if got = soleStatusFor(t, s, w.ID); got.State != "superseded" || len(remote.reasons) != 1 || remote.reasons[0] != statusSupersession || base.posts != 1 {
				t.Fatal("status not annotated exactly once in place", got.State, remote.reasons, base.posts)
			}
		})
	}
}

func soleStatusFor(t *testing.T, s *Store, id string) WriteIntent {
	t.Helper()
	for _, w := range writes(t, s, "status") {
		if w.ID == id {
			return w
		}
	}
	t.Fatal("status write missing", id)
	return WriteIntent{}
}

// The superseded form of a status is the rendered status with its marker
// and the fixed supersession annotation appended in place.
func TestSupersededStatusMarkdownGolden(t *testing.T) {
	job := testJob()
	doc, err := Render(Outcome{Job: job, State: "auth_failed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	marker := "<!-- sfl-alpha:" + strings.Repeat("0", 32) + " -->"
	checkGolden(t, "status-superseded.golden.md", doc.Summary+"\n\n"+marker+supersessionAnnotation(statusSupersession))
}

// Over actual TLS HTTP, a status is an advisory COMMENT review on the job's
// head with no inline comments. Its lost response reconciles by marker
// without another POST, and its supersession annotation updates it in place.
func TestGitHubStatusPublicationReconcilesLostResponse(t *testing.T) {
	s, job, reader := statusFixture(t, "auth_failed")
	g, f := githubHTTPFixture(t, reader)
	f.lost = true
	ctx := context.Background()
	p := Publisher{Store: s, Remote: g}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	w := soleStatus(t, s)
	if err := p.Dispatch(ctx, w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.hidden = true
	f.mu.Unlock()
	if err := p.Reconcile(ctx, w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("empty scan authorized retry", err)
	}
	if err := p.Dispatch(ctx, w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("unknown status dispatched twice", err)
	}
	f.mu.Lock()
	f.hidden = false
	f.mu.Unlock()
	if err := p.Reconcile(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if got := soleStatus(t, s); got.State != "confirmed" || got.ReceiptID != 80 {
		t.Fatal("lost status response not recovered", got.State, got.ReceiptID)
	}
	f.mu.Lock()
	body, _ := f.review["body"].(string)
	if f.posts != 1 || len(f.comments) != 0 || f.review["commit_id"] != job.Snapshot.Head || f.review["state"] != "COMMENTED" || !strings.Contains(body, statusCause(job, "auth_failed")) {
		f.mu.Unlock()
		t.Fatal("status was not one inline-free COMMENT on the job's head", f.posts, len(f.comments))
	}
	f.mu.Unlock()
	if err := g.Annotate(ctx, alphaScope(), 80, statusSupersession); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.review["body"] != body+supersessionAnnotation(statusSupersession) || f.posts != 1 || f.puts != 1 {
		t.Fatal("status supersession not annotated in place", f.posts, f.puts)
	}
}

// The HTTP adapter refuses a status intent with inline comments or an
// unpublishable state before any request.
func TestGitHubStatusIntentShapeRefusedBeforeHTTP(t *testing.T) {
	for name, change := range map[string]func(*WriteIntent){
		"inline":   func(w *WriteIntent) { w.Document.Inline = []InlineComment{{"source.go", 1, "partial finding"}} },
		"stale":    func(w *WriteIntent) { w.Status = "stale" },
		"no-state": func(w *WriteIntent) { w.Status = "" },
	} {
		t.Run(name, func(t *testing.T) {
			g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
			doc := Document{Summary: "SFL status", Inline: []InlineComment{}}
			w := newIntent("status", alphaScope(), 1, 1, testSnapshot(), strings.Repeat("d", 64), doc, controlTime)
			w.Status = "auth_failed"
			change(&w)
			w.Document = canonicalPublicationDocument(w.Document)
			w.BodyHash = Digest(w.Document)
			if _, err := g.Post(context.Background(), w); !errors.Is(err, ErrIgnored) {
				t.Fatal("malformed status accepted", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.calls != 0 {
				t.Fatal("malformed status reached HTTP", f.calls)
			}
		})
	}
}

// exhaustDailyReviews charges count completed jobs of other PRs on day, so a
// job that has not started yet meets the daily review limit at its claim.
func exhaustDailyReviews(t *testing.T, s *Store, day time.Time, count int) {
	t.Helper()
	err := s.update(context.Background(), func(d *DurableState) error {
		for i := range count {
			job := testJob()
			job.ID = fmt.Sprintf("charged-%d", i)
			scope := alphaScope()
			scope.PR = 100 + i
			d.Jobs[job.ID] = &DurableJob{Job: job, Scope: scope, CreatedAt: day, State: "dual_complete", ChargeDays: []string{day.UTC().Format("2006-01-02")}, Checkpoint: Checkpoint{JobHash: Digest(job), Artifacts: []Artifact{}}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A queued job below the daily review limit is transient: it publishes no
// status, and its claim starts work instead of waiting for quota.
func TestQueuedJobBelowDailyLimitPublishesNoStatus(t *testing.T) {
	s, job, remote := admittedFixture(t)
	ctx := context.Background()
	now := controlTime.Add(4 * time.Minute)
	exhaustDailyReviews(t, s, now, job.Config.DailyReviews-1)
	p := Publisher{Store: s, Remote: remote}
	jobsBefore, _ := durableDigests(t, s)
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := durableDigests(t, s); jobs != jobsBefore || len(writes(t, s, "status")) != 0 {
		t.Fatal("queued job published a status")
	}
	token, err := s.Claim(ctx, job.ID, now)
	if err != nil || token == "" {
		t.Fatal("job below the daily limit not claimed", err)
	}
	if err = p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if len(writes(t, s, "status")) != 0 || remote.posts != 0 {
		t.Fatal("running job published a status")
	}
}

// A newly admitted job whose claim meets the daily review limit waits for
// quota durably: it enters quota_wait without a lease, a session or a day
// charge, and publishes one fenced quota_wait status. Repeated claims leave the
// durable state unchanged and publish no second status.
func TestDailyLimitBeforeFirstPhasePublishesQuotaWaitOnce(t *testing.T) {
	s, job, base := admittedFixture(t)
	ctx := context.Background()
	now := controlTime.Add(4 * time.Minute)
	exhaustDailyReviews(t, s, now, job.Config.DailyReviews)
	remote := &annotationRemote{fakeRemote: base}
	p := Publisher{Store: s, Remote: remote}
	if _, err := s.Claim(ctx, job.ID, now); !errors.Is(err, ErrDeferred) {
		t.Fatal("claim at the daily limit not deferred", err)
	}
	d, _ := s.Snapshot(ctx)
	if j := d.Jobs[job.ID]; j.State != "quota_wait" || j.LeaseToken != "" || j.Outstanding || j.Checkpoint.ConsumedSessions != 0 || len(j.ChargeDays) != 0 {
		t.Fatal("daily limit did not leave the job waiting for quota", j.State, j.LeaseToken != "", j.Checkpoint.ConsumedSessions, j.ChargeDays)
	}
	jobsBefore, _ := durableDigests(t, s)
	if _, err := s.Claim(ctx, job.ID, now.Add(time.Minute)); !errors.Is(err, ErrDeferred) {
		t.Fatal("repeated claim at the daily limit not deferred", err)
	}
	if jobs, _ := durableDigests(t, s); jobs != jobsBefore {
		t.Fatal("repeated daily-limit claim rewrote the job")
	}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	w := soleStatus(t, s)
	if w.State != "prepared" || w.Status != "quota_wait" || w.JobHash != Digest(job) || w.RequestID != 1 || w.Generation != job.Generation || w.Snapshot != job.Snapshot || len(w.Document.Inline) != 0 || !strings.Contains(w.Document.Summary, statusCause(job, "quota_wait")) {
		t.Fatalf("quota_wait status not bound to its job: %+v", w)
	}
	if err := p.Dispatch(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Claim(ctx, job.ID, now.Add(2*time.Minute)); !errors.Is(err, ErrDeferred) {
			t.Fatal(err)
		}
		if err := p.PrepareStatuses(ctx); err != nil {
			t.Fatal(err)
		}
		if got := soleStatus(t, s); got.State != "confirmed" || base.posts != 1 {
			t.Fatal("waiting job duplicated its status", got.State, base.posts)
		}
	}
	// The status promised later work, so a replacing request annotates it.
	if err := s.RecordRequest(ctx, alphaScope(), native(2, controlTime.Add(10*time.Minute)), true); err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Reconcile(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if got := soleStatusFor(t, s, w.ID); got.State != "superseded" || len(remote.reasons) != 1 || remote.reasons[0] != statusSupersession || base.posts != 1 {
		t.Fatal("replaced quota_wait status not annotated once", got.State, remote.reasons, base.posts)
	}
}

// A daily-limit quota_wait status passes the same fences as every status: a
// moved head, a non-owner request or a local pause retires it without a POST.
// Once quota frees, it defers while its job runs and is retired when the job
// leaves quota_wait.
func TestDailyLimitQuotaWaitStatusFencesBeforeDispatch(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *Store, *fakeRemote, Job, func() error){
		"head-moved": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job, _ func() error) {
			r.state.Snapshot.Head = strings.Repeat("e", 40)
		},
		"non-owner": func(_ *testing.T, _ *Store, r *fakeRemote, _ Job, _ func() error) { r.state.Request.RequesterID = 666 },
		"local-pause": func(t *testing.T, s *Store, _ *fakeRemote, _ Job, _ func() error) {
			if !pausePR(t, s) {
				t.Fatal("PR not paused")
			}
		},
		"quota-freed": func(t *testing.T, s *Store, r *fakeRemote, job Job, dispatch func() error) {
			ctx := context.Background()
			token, err := s.Claim(ctx, job.ID, controlTime.Add(24*time.Hour))
			if err != nil {
				t.Fatal("job not claimed once quota freed", err)
			}
			if err = dispatch(); !errors.Is(err, ErrDeferred) || r.posts != 0 || soleStatus(t, s).State != "prepared" {
				t.Fatal("status of a running job did not defer", err, r.posts)
			}
			if err = s.ReleaseStopped(ctx, job.ID, token, "auth_failed"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, job, remote := admittedFixture(t)
			ctx := context.Background()
			now := controlTime.Add(4 * time.Minute)
			exhaustDailyReviews(t, s, now, job.Config.DailyReviews)
			if _, err := s.Claim(ctx, job.ID, now); !errors.Is(err, ErrDeferred) {
				t.Fatal(err)
			}
			p := Publisher{Store: s, Remote: remote}
			if err := p.PrepareStatuses(ctx); err != nil {
				t.Fatal(err)
			}
			w := soleStatus(t, s)
			dispatch := func() error { return p.Dispatch(ctx, w.ID) }
			change(t, s, remote, job, dispatch)
			if err := dispatch(); !errors.Is(err, ErrFenced) || remote.posts != 0 {
				t.Fatal("fenced quota_wait status was POSTed", err, remote.posts)
			}
			if got := soleStatusFor(t, s, w.ID); got.State != "stale" {
				t.Fatal("fenced quota_wait status not retired", got.State)
			}
		})
	}
}
