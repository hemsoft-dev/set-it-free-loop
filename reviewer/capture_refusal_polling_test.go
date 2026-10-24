package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureRefusalCauses are the leading status words for each permanent
// capture refusal reason.
var captureRefusalCauses = map[string]string{
	"missing_history":   "INCOMPLETE: the commit history of this snapshot is missing from the trusted object store, so its changes could not be captured.",
	"ambiguous_history": "INCOMPLETE: the merge base of this snapshot is ambiguous or differs from the recorded merge base, so its changes could not be captured.",
	"inventory_limit":   "INCOMPLETE: this snapshot changes more than 128 files, over the capture inventory limit, so its changes could not be captured.",
}

// historyLimitCause is the leading status words of history_limit, which only
// acquisition quarantine verification answers, never in-process capture.
const historyLimitCause = "INCOMPLETE: the fetched history of this snapshot is over an object, commit or byte limit of the acquisition quarantine, so its changes could not be captured."

// captureRefusalCause returns the leading status words of any known reason.
func captureRefusalCause(reason string) string {
	if reason == "history_limit" {
		return historyLimitCause
	}
	return captureRefusalCauses[reason]
}

// refusedSnapshot returns a snapshot of git whose capture the trusted object
// store refuses for reason. Every case is real Git history, not a fake error.
func refusedSnapshot(t *testing.T, git snapshotGitFixture, reason string) Snapshot {
	t.Helper()
	s := git.snapshot
	switch reason {
	case "missing_history":
		s.Head = strings.Repeat("f", 40)
	case "ambiguous_history":
		// A criss-cross merge has two best common ancestors.
		tree := git.run(t, "rev-parse", s.Head+"^{tree}")
		one := git.run(t, "commit-tree", tree, "-p", s.MergeBase, "-m", "criss-cross one")
		two := git.run(t, "commit-tree", tree, "-p", s.MergeBase, "-m", "criss-cross two")
		s.Head = git.run(t, "commit-tree", tree, "-p", one, "-p", two, "-m", "merge one")
		s.Base = git.run(t, "commit-tree", tree, "-p", two, "-p", one, "-m", "merge two")
		s.MergeBase = one
	case "inventory_limit":
		for i := 0; i <= snapshotFileLimit; i++ {
			git.write(t, "many/"+strconv.Itoa(i), "x\n")
		}
		git.run(t, "add", "--all")
		git.run(t, "commit", "--quiet", "-m", "too many paths")
		s.Head = git.run(t, "rev-parse", "HEAD")
	default:
		t.Fatal("unknown refusal", reason)
	}
	return s
}

// refusalPacketReason decodes the reason a refused job's packet records.
func refusalPacketReason(t *testing.T, job Job) string {
	t.Helper()
	var p struct {
		Version  string
		Snapshot Snapshot
		Reason   string
	}
	if err := json.Unmarshal([]byte(job.Packet), &p); err != nil || p.Version != "sfl-capture-refusal-1" || p.Snapshot != job.Snapshot {
		t.Fatal("refused job does not record its refusal", job.Packet, err)
	}
	return p.Reason
}

// useCapture points the fixture's single request at snapshot s and counts
// captures from git.
func useCapture(c *PollingController, f *pollingFixture, git snapshotGitFixture, s Snapshot) *atomic.Int32 {
	captures := &atomic.Int32{}
	f.mu.Lock()
	f.remote.Snapshot = s
	f.remote.Request.Snapshot = s
	f.batch.Requests[0].Snapshot = s
	f.mu.Unlock()
	c.options.Capture = func(ctx context.Context, snapshot Snapshot) (*SnapshotEvidence, error) {
		captures.Add(1)
		return CaptureSnapshotEvidence(ctx, git.store(), snapshot)
	}
	c.options.Restore = func(ctx context.Context, job Job) (*JobEvidence, error) {
		return RestoreSnapshotEvidence(ctx, git.store(), job)
	}
	return captures
}

// A permanent capture refusal is recorded once as a terminal job with its
// reason. It publishes exactly one INCOMPLETE status, is never retried,
// starts no provider, never reads as clean and leaves no backoff behind.
func TestPollingRecordsPermanentCaptureRefusalOnce(t *testing.T) {
	for reason, cause := range captureRefusalCauses {
		t.Run(reason, func(t *testing.T) {
			c, f, calls := pollControllerFixture(t, nil)
			git := newSnapshotGitFixture(t)
			s := refusedSnapshot(t, git, reason)
			captures := useCapture(c, f, git, s)
			if _, err := pollOnce(t, c); err != nil {
				t.Fatal(err)
			}
			settleWorker(t, c)
			for range 4 {
				f.clock.Advance(2 * time.Second)
				result, err := pollOnce(t, c)
				if err != nil {
					t.Fatal("refusal retried or deferred", err)
				}
				if result.Active {
					t.Fatal("refused request started another worker")
				}
			}
			d, err := f.store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Jobs) != 1 {
				t.Fatal("refusal not recorded as one job", len(d.Jobs))
			}
			var job *DurableJob
			for _, j := range d.Jobs {
				job = j
			}
			if job.State != "capture_refused" || job.Job.Snapshot != s || job.RequestID != 1 || job.LeaseToken != "" || job.Outstanding || len(job.Checkpoint.Artifacts) != 0 || job.Checkpoint.ConsumedSessions != 0 {
				t.Fatal("refusal not a terminal unclaimed job", job.State, job.Checkpoint.ConsumedSessions)
			}
			if got := refusalPacketReason(t, job.Job); got != reason {
				t.Fatal("wrong persisted refusal reason", got)
			}
			pr := d.PRs[alphaScope().key()]
			if pr.Paused || pr.CurrentRequest != 1 || len(pr.Requests) != 1 || !pr.Requests[0].Admitted {
				t.Fatal("refused request not admitted exactly once", pr.Paused, pr.CurrentRequest, pr.Requests)
			}
			if captures.Load() != 1 || calls.Load() != 0 {
				t.Fatal("refusal retried capture or started a provider", captures.Load(), calls.Load())
			}
			if d.Polling == nil || d.Polling.Failures != 0 || d.Polling.Reason != "" {
				t.Fatal("refusal recorded as a deferred backoff", d.Polling)
			}
			statuses := statusWrites(t, f)
			if len(statuses) != 1 || f.posts != 1 || len(d.Writes) != 1 {
				t.Fatal("refusal not published exactly once", len(statuses), f.posts, len(d.Writes))
			}
			w := statuses[0]
			if w.State != "confirmed" || w.Status != "capture_refused" || w.JobHash != Digest(job.Job) || w.RequestID != 1 || w.Snapshot != s || len(w.Document.Inline) != 0 {
				t.Fatal("refusal status not bound to its job and confirmed", w.State, w.Status)
			}
			summary := w.Document.Summary
			if !strings.Contains(summary, "## SFL advisory review status") || !strings.Contains(summary, cause) || !strings.Contains(summary, "Completed phases: 0 of 4.") {
				t.Fatal("refusal status does not name its cause", summary)
			}
			if strings.Contains(summary, "No findings reported") || strings.Contains(summary, "required phases") || strings.Contains(summary, "## SFL advisory review\n") {
				t.Fatal("refusal reads as a clean review", summary)
			}
		})
	}
}

// Transient capture failures, and refusal-shaped errors a trusted capture
// cannot vouch for, keep the deferred backoff. Nothing is admitted or
// published, and the same request captures again once the backoff ends.
func TestPollingTransientCaptureFailureStillBacksOff(t *testing.T) {
	for name, failure := range map[string]error{
		"network":         errors.New("synthetic network outage"),
		"access":          ErrRepositoryAccessLost,
		"deferred":        ErrDeferred,
		"source VM start": ErrSourceService,
		"rate limit":      &GitHubHTTPError{Status: 429, RetryAfter: 30 * time.Second},
		"wrapped refusal": fmt.Errorf("source VM: %w", CaptureRefusal{CaptureMissingHistory}),
		"joined refusal":  errors.Join(CaptureRefusal{CaptureInventoryLimit}, errors.New("synthetic release failure")),
		"unknown reason":  CaptureRefusal{"synthetic_reason"},
	} {
		t.Run(name, func(t *testing.T) {
			c, f, calls := pollControllerFixture(t, nil)
			captures := &atomic.Int32{}
			capture := c.options.Capture
			failing := true
			c.options.Capture = func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
				captures.Add(1)
				if failing {
					return nil, failure
				}
				return capture(ctx, s)
			}
			if _, err := pollOnce(t, c); err != nil {
				t.Fatal(err)
			}
			awaitPollWorker(t, c)
			f.clock.Advance(2 * time.Second)
			if _, err := pollOnce(t, c); !errors.Is(err, failure) {
				t.Fatal("transient capture failure not reported", err)
			}
			d, err := f.store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if d.Polling == nil || d.Polling.Failures != 1 || d.Polling.Reason != "deferred" {
				t.Fatal("transient capture failure did not back off", d.Polling)
			}
			pr := d.PRs[alphaScope().key()]
			if len(d.Jobs) != 0 || len(d.Writes) != 0 || f.posts != 0 || pr.Requests[0].Admitted {
				t.Fatal("transient capture failure admitted or published", len(d.Jobs), len(d.Writes), f.posts)
			}
			failing = false
			f.clock.Advance(2 * time.Minute)
			result, err := pollOnce(t, c)
			if err != nil || !result.Active {
				t.Fatal("request not captured again after backoff", result, err)
			}
			awaitPollWorker(t, c)
			if captures.Load() != 2 || calls.Load() != 4 {
				t.Fatal("retry did not capture and review", captures.Load(), calls.Load())
			}
		})
	}
}

// A refused head does not refuse the PR. A new head with a fresh request is
// captured and reviewed; the refusal stays a terminal record of its own head
// and is not annotated or reposted.
func TestPollingCaptureRefusalRetriesOnNewHead(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	git := newSnapshotGitFixture(t)
	good := git.snapshot
	refused := refusedSnapshot(t, git, "missing_history")
	captures := useCapture(c, f, git, refused)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	settleWorker(t, c)
	pollAgain(t, c, f)
	if statuses := statusWrites(t, f); len(statuses) != 1 || statuses[0].State != "confirmed" || f.posts != 1 {
		t.Fatal("refusal status not confirmed", len(statuses), f.posts)
	}
	f.clock.Advance(time.Minute)
	f.mu.Lock()
	next := native(2, f.clock.Now())
	next.Snapshot = good
	f.batch.Requests = append(f.batch.Requests, next)
	f.remote.Snapshot = good
	f.remote.Request = next
	f.mu.Unlock()
	for range 3 {
		pollAgain(t, c, f)
	}
	d, err := f.store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var reviewed, old *DurableJob
	for _, j := range d.Jobs {
		switch j.RequestID {
		case 1:
			old = j
		case 2:
			reviewed = j
		}
	}
	if reviewed == nil || reviewed.State != "dual_complete" || reviewed.Job.Snapshot != good || calls.Load() != 4 || captures.Load() != 2 {
		t.Fatal("new head not captured and reviewed", reviewed, calls.Load(), captures.Load())
	}
	if old == nil || old.State != "stale" || refusalPacketReason(t, old.Job) != "missing_history" {
		t.Fatal("refused head record lost", old)
	}
	reviews := 0
	for _, w := range d.Writes {
		if w.Kind == "review" && w.State == "confirmed" && w.JobHash == Digest(reviewed.Job) {
			reviews++
		}
	}
	statuses := statusWrites(t, f)
	if reviews != 1 || len(statuses) != 1 || statuses[0].State != "confirmed" || f.posts != 2 || f.annotations != 0 {
		t.Fatal("new head review or terminal refusal status wrong", reviews, len(statuses), f.posts, f.annotations)
	}
}

// refusalUpstream is a fake GitHub with independent PRs: per-scope remote
// state and request history, one shared outbox.
type refusalUpstream struct {
	mu          sync.Mutex
	store       *Store
	clock       *pollClock
	prs         map[int]*refusalPR
	posts       []WriteIntent
	receipts    []RemoteReceipt
	annotations int
}
type refusalPR struct {
	remote   RemoteState
	requests []NativeRequest
}

func (u *refusalUpstream) OpenScopes(context.Context) ([]Scope, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	scopes := []Scope{}
	for pr := range u.prs {
		scopes = append(scopes, Scope{RepositoryID, 9001, pr})
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].PR < scopes[j].PR })
	return scopes, nil
}
func (u *refusalUpstream) state(scope Scope) (RemoteState, Discovery) {
	u.mu.Lock()
	defer u.mu.Unlock()
	pr := u.prs[scope.PR]
	if pr == nil {
		return RemoteState{}, Discovery{Scope: scope, Complete: true, Through: u.clock.Now()}
	}
	return pr.remote, Discovery{Scope: scope, Requests: append([]NativeRequest{}, pr.requests...), Complete: true, Through: u.clock.Now()}
}
func (u *refusalUpstream) PollScope(_ context.Context, scope Scope) (RemoteState, Discovery, error) {
	r, b := u.state(scope)
	return r, b, nil
}
func (u *refusalUpstream) Current(ctx context.Context, scope Scope) (RemoteState, error) {
	r, b := u.state(scope)
	if r.Access && r.Open {
		if err := u.store.Discover(ctx, b); err != nil {
			return RemoteState{}, err
		}
	}
	return r, nil
}
func (u *refusalUpstream) History(_ context.Context, scope Scope) (Discovery, error) {
	_, b := u.state(scope)
	return b, nil
}
func (u *refusalUpstream) Post(_ context.Context, w WriteIntent) (RemoteReceipt, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.posts = append(u.posts, w)
	r := RemoteReceipt{ID: int64(100 + len(u.posts)), AuthorID: 9003, Marker: w.Marker, BodyHash: w.BodyHash, CreatedAt: u.clock.Now(), CommitID: w.Snapshot.Head}
	u.receipts = append(u.receipts, r)
	return r, nil
}
func (u *refusalUpstream) Scan(context.Context, Scope, string) ([]RemoteReceipt, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]RemoteReceipt{}, u.receipts...), true, nil
}
func (u *refusalUpstream) Annotate(context.Context, Scope, int64, string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.annotations++
	return nil
}

// A permanently refused PR whose request is older than every other PR's must
// not hold their admission back. One job runs at a time, so before refusals
// were recorded the oldest refused request was selected and failed forever.
func TestPollingCaptureRefusalDoesNotBlockOtherPRs(t *testing.T) {
	for reason := range captureRefusalCauses {
		t.Run(reason, func(t *testing.T) {
			store, _ := openFixtureStore(t)
			git := newSnapshotGitFixture(t)
			clock := &pollClock{now: controlTime.Add(10 * time.Minute)}
			refused := refusedSnapshot(t, git, reason)
			refused.PR = 1
			good := git.snapshot
			good.PR = 2
			first := native(1, controlTime.Add(time.Minute))
			first.Snapshot = refused
			second := native(2, controlTime.Add(2*time.Minute))
			second.Snapshot = good
			u := &refusalUpstream{store: store, clock: clock, prs: map[int]*refusalPR{
				1: {remote: RemoteState{Access: true, Open: true, Snapshot: refused, Request: first}, requests: []NativeRequest{first}},
				2: {remote: RemoteState{Access: true, Open: true, Snapshot: good, Request: second}, requests: []NativeRequest{second}},
			}}
			calls := &atomic.Int32{}
			captured := map[int]int{}
			var capturedMu sync.Mutex
			fake := reviewFunc(func(_ context.Context, s Session) (Report, error) {
				calls.Add(1)
				if s.Snapshot.PR != 2 {
					return Report{}, errors.New("refused PR reached a provider")
				}
				return fakeReport(s), nil
			})
			o := PollOptions{Store: store, Enrollment: alphaEnrollment(), Release: testJob().Release, Discovery: u, Publisher: Publisher{Store: store, Remote: u}, Capture: func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
				capturedMu.Lock()
				captured[s.PR]++
				capturedMu.Unlock()
				return CaptureSnapshotEvidence(ctx, git.store(), s)
			}, Restore: func(ctx context.Context, job Job) (*JobEvidence, error) {
				return RestoreSnapshotEvidence(ctx, git.store(), job)
			}, Runtime: func(context.Context, Job, *JobEvidence) (Engine, error) {
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
			for range 6 {
				result, _ := pollOnce(t, c)
				if result.Active {
					awaitPollWorker(t, c)
				}
				// Past any deferred backoff, so only selection decides progress.
				clock.Advance(2 * time.Minute)
			}
			d, err := store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			states := map[int]string{}
			for _, j := range d.Jobs {
				states[j.Scope.PR] = j.State
			}
			if states[2] != "dual_complete" || calls.Load() != 4 {
				t.Fatal("refused PR blocked another PR's admission", states, calls.Load(), captured)
			}
			if states[1] != "capture_refused" || captured[1] != 1 || captured[2] != 1 {
				t.Fatal("refused PR not recorded once", states, captured)
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			kinds := map[int]string{}
			for _, w := range u.posts {
				kinds[w.Scope.PR] += w.Kind + ":" + w.Status + " "
			}
			if len(u.posts) != 2 || kinds[1] != "status:capture_refused " || kinds[2] != "review: " {
				t.Fatal("wrong publications", kinds)
			}
		})
	}
}

// A refusal is recorded only for the request and head it was captured for. A
// new head observed while the refused capture ran fences the refusal; nothing
// is admitted or published for either head by that invocation.
func TestPollingCaptureRefusalIsFencedByNewHead(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	git := newSnapshotGitFixture(t)
	refused := refusedSnapshot(t, git, CaptureInventoryLimit)
	useCapture(c, f, git, refused)
	c.options.Capture = func(context.Context, Snapshot) (*SnapshotEvidence, error) {
		f.mu.Lock()
		f.remote.Snapshot = git.snapshot
		f.mu.Unlock()
		return nil, CaptureRefusal{CaptureInventoryLimit}
	}
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	if err := c.drainWork(); !errors.Is(err, ErrFenced) {
		t.Fatal("refusal for a replaced head not fenced", err)
	}
	d, err := f.store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pr := d.PRs[alphaScope().key()]
	if len(d.Jobs) != 0 || len(d.Writes) != 0 || pr.Requests[0].Admitted || !pr.Paused || calls.Load() != 0 {
		t.Fatal("fenced refusal admitted, published or left the request current", len(d.Jobs), len(d.Writes), pr.Paused)
	}
}

// RefuseCapture passes the admission fences, binds the refusal to the
// request's own snapshot, admits the request once and never becomes
// claimable, restorable or prunable before retention.
func TestDurableCaptureRefusalIsATerminalAdmission(t *testing.T) {
	ctx := context.Background()
	s, _ := openFixtureStore(t)
	r := native(1, controlTime.Add(time.Minute))
	mustCapture(t, s, nil, controlTime.Add(2*time.Minute))
	if err := s.RecordRequest(ctx, alphaScope(), r, true); err != nil {
		t.Fatal(err)
	}
	release := strings.Repeat("d", 40)
	at := controlTime.Add(3 * time.Minute)
	other := r.Snapshot
	other.Head = strings.Repeat("e", 40)
	if _, err := s.RefuseCapture(ctx, alphaScope(), 1, release, other, CaptureMissingHistory, at); !errors.Is(err, ErrFenced) {
		t.Fatal("refusal of another head admitted", err)
	}
	if _, err := s.RefuseCapture(ctx, alphaScope(), 1, release, r.Snapshot, "synthetic_reason", at); !errors.Is(err, ErrIgnored) {
		t.Fatal("unknown refusal reason admitted", err)
	}
	if _, err := s.RefuseCapture(ctx, alphaScope(), 2, release, r.Snapshot, CaptureMissingHistory, at); !errors.Is(err, ErrDeferred) {
		t.Fatal("refusal of a non-current request admitted", err)
	}
	job, err := s.RefuseCapture(ctx, alphaScope(), 1, release, r.Snapshot, CaptureInventoryLimit, at)
	if err != nil {
		t.Fatal(err)
	}
	if captureRefusalReason(job) != CaptureInventoryLimit || snapshotEvidencePacket(job.Packet) {
		t.Fatal("refusal packet does not record its reason", job.Packet)
	}
	again, err := s.RefuseCapture(ctx, alphaScope(), 1, release, r.Snapshot, CaptureMissingHistory, at)
	if err != nil || Digest(again) != Digest(job) {
		t.Fatal("repeated refusal replaced the recorded job", again.Packet, err)
	}
	if admitted, err := s.Admit(ctx, alphaScope(), 1, release, "evidence", at); err != nil || Digest(admitted) != Digest(job) {
		t.Fatal("refused request admitted a capture", err)
	}
	d, _ := s.Snapshot(ctx)
	j := d.Jobs[job.ID]
	if j == nil || j.State != "capture_refused" || !d.PRs[alphaScope().key()].Requests[0].Admitted || len(d.Jobs) != 1 {
		t.Fatal("refusal not a single terminal admission", j)
	}
	if _, err := s.Claim(ctx, job.ID, at.Add(time.Minute)); !errors.Is(err, ErrDeferred) {
		t.Fatal("refused job claimable", err)
	}
	if _, err := RestoreSnapshotEvidence(ctx, GitStore{}, job); !errors.Is(err, ErrFenced) {
		t.Fatal("refused job restorable", err)
	}
	if err := s.Prune(ctx, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Snapshot(ctx); d.Jobs[job.ID] == nil {
		t.Fatal("refusal pruned before retention")
	}
	if err := s.Prune(ctx, at.Add(time.Duration(job.Config.RetentionDays+1)*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Snapshot(ctx); d.Jobs[job.ID] != nil {
		t.Fatal("terminal refusal never pruned")
	}
}
