package reviewer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func triggerFixture(t *testing.T) (*GitHubTriggerDiscoveryHTTP, *discoveryFixture, *Store) {
	t.Helper()
	store, _ := openFixtureStore(t)
	reader, f := discoveryHTTPFixture(t, store)
	f.alter = func(_ *discoveryFixture, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, "/requested_reviewers") || strings.HasSuffix(q.URL.Path, "/events") {
			t.Error("selected trigger reader consulted native reviewer routes")
		}
	}
	return &GitHubTriggerDiscoveryHTTP{reader: reader, policy: TriggerPolicy{EnabledAt: controlTime}}, f, store
}
func reviewComment(id int64, at time.Time) map[string]any {
	return map[string]any{"id": id, "user": map[string]any{"id": OwnerID}, "body": "@sfl review", "issue_url": githubOrigin + githubIssue(alphaScope()), "created_at": at, "updated_at": at}
}
func TestPollingTriggersOpenPushAndManual(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	state, err := g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 1 || state.Request.Origin.Kind != "opened" || state.Request.Origin.ActorRole != "pr_author" {
		t.Fatal(state, err)
	}
	first := state.Request
	// Native events, target pending state and participant visibility are irrelevant.
	f.mu.Lock()
	f.pending = false
	f.events = nil
	f.through = f.through.Add(time.Minute)
	f.mu.Unlock()
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != first.ID {
		t.Fatal("duplicate observation", state, err)
	}
	job, err := s.Admit(ctx, alphaScope(), first.ID, strings.Repeat("d", 40), "same immutable evidence", f.through)
	if err != nil || job.Origin == nil || job.Origin.Kind != "opened" {
		t.Fatal(job, err)
	}
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.snapshot.Head = strings.Repeat("e", 40)
	f.mu.Unlock()
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 2 || state.Request.Origin.Kind != "synchronize" || !state.Request.Origin.EventAt.IsZero() {
		t.Fatal("push", state, err)
	}
	d, _ := s.Snapshot(ctx)
	if d.Jobs[job.ID].State != "stale" || d.PRs[alphaScope().key()].Generation <= job.Generation {
		t.Fatal("push did not fence old work", d.Jobs[job.ID])
	}
	// A manual re-request is fresh even for this same head, and records its actual
	// author/comment identity, not the local request number.
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.comments = []map[string]any{reviewComment(991, f.through.Add(-time.Second))}
	f.mu.Unlock()
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 3 || state.Request.Origin.CommentID != 991 || state.Request.Origin.Kind != "comment" {
		t.Fatal("manual", state, err)
	}
	id := state.Request.ID
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.comments = []map[string]any{}
	f.mu.Unlock()
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != id {
		t.Fatal("accepted comment deletion rewrote request", state, err)
	}
	// An observed ABA head transition is not deduplicated by the old head SHA.
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.snapshot.Head = first.Snapshot.Head
	f.mu.Unlock()
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 4 || state.Request.Origin.Sequence != 3 {
		t.Fatal("ABA", state, err)
	}
}
func TestPollingTriggersBaselineBaseAndEpoch(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	f.created = controlTime.Add(-time.Hour)
	f.updated = f.created
	state, err := g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 0 {
		t.Fatal("pre-epoch backlog auto-run", state, err)
	}
	f.through = f.through.Add(time.Minute)
	f.snapshot.Base = strings.Repeat("e", 40)
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 0 {
		t.Fatal("base-only auto-run", state, err)
	}
	f.through = f.through.Add(time.Minute)
	f.snapshot.Head = strings.Repeat("f", 40)
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.Origin.Kind != "synchronize" {
		t.Fatal(state, err)
	}
	g.policy.EnabledAt = g.policy.EnabledAt.Add(time.Second)
	_, err = g.Current(ctx, alphaScope())
	if !errors.Is(err, ErrDeferred) {
		t.Fatal("epoch changed silently", err)
	}
	d, _ := s.Snapshot(ctx)
	if len(d.PRs[alphaScope().key()].Requests) != 1 {
		t.Fatal(d.PRs)
	}
}
func TestPollingTriggersCommentAuthorizationAndCoalescing(t *testing.T) {
	for _, kind := range []string{"owner", "stranger", "edited", "quoted", "flags", "case", "prefix", "candidate-body"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			g, f, s := triggerFixture(t)
			c := reviewComment(991, controlTime.Add(2*time.Minute))
			f.comments = []map[string]any{c}
			switch kind {
			case "stranger":
				c["user"] = map[string]any{"id": OwnerID + 1}
			case "edited":
				c["updated_at"] = controlTime.Add(3 * time.Minute)
			case "quoted":
				c["body"] = "> @sfl review"
			case "flags":
				c["body"] = "@sfl review --model=evil"
			case "case":
				c["body"] = "@SFL review"
			case "prefix":
				c["body"] = "please @sfl review"
			case "candidate-body":
				f.comments = []map[string]any{}
			}
			state, err := g.Current(ctx, alphaScope())
			if err != nil {
				t.Fatal(err)
			}
			want := "opened"
			if kind == "owner" {
				want = "comment"
			}
			if state.Request.Origin.Kind != want {
				t.Fatal(state.Request)
			}
			d, _ := s.Snapshot(ctx)
			if len(d.PRs[alphaScope().key()].Requests) != 1 || len(d.Writes) != 0 {
				t.Fatal("duplicate request or unsolicited reply", d.PRs, d.Writes)
			}
		})
	}
}
func TestPollingTriggersAtomicScopeAndSnapshot(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	_, batch, err := g.PollScope(ctx, alphaScope())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	if len(d.PRs) != 0 {
		t.Fatal("read committed discovery")
	}
	if err = s.Discover(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err = s.Discover(ctx, batch); !errors.Is(err, ErrDeferred) {
		t.Fatal("stale read replay allocated request", err)
	}
	old, _ := s.Snapshot(ctx)
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.comments = []map[string]any{reviewComment(2, f.through.Add(-time.Second)), reviewComment(3, f.through.Add(-time.Second)), reviewComment(4, f.through.Add(-time.Second))}
	f.failSecondPage = true
	f.mu.Unlock()
	if _, _, err = g.PollScope(ctx, alphaScope()); err == nil {
		t.Fatal("partial comments admitted")
	}
	after, _ := s.Snapshot(ctx)
	if Digest(after) != Digest(old) {
		t.Fatal("partial batch mutated local state")
	}
}
func TestPollingTriggersRetainManualConfigAndCancel(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	if _, err := g.Current(ctx, alphaScope()); err != nil {
		t.Fatal(err)
	}
	f.through = f.through.Add(time.Minute)
	c := reviewComment(991, f.through.Add(-time.Second))
	c["body"] = "/sfl cancel"
	f.comments = []map[string]any{c}
	state, err := g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 0 {
		t.Fatal("cancel", state, err)
	}
	f.through = f.through.Add(time.Minute)
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.ID != 0 {
		t.Fatal("same head undid cancel", state, err)
	}
	f.through = f.through.Add(time.Minute)
	f.snapshot.Head = strings.Repeat("e", 40)
	state, err = g.Current(ctx, alphaScope())
	if err != nil || state.Request.Origin.Kind != "synchronize" {
		t.Fatal("new head did not replace cancellation", state, err)
	}
	d, _ := s.Snapshot(ctx)
	if len(d.Writes) != 1 {
		t.Fatal("cancel ack changed", d.Writes)
	}
}

func TestPollingTriggersSameSecondHeadChangeAndLateComment(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	first, err := g.Current(ctx, alphaScope())
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot.Head = strings.Repeat("e", 40)
	second, err := g.Current(ctx, alphaScope())
	if err != nil || second.Request.ID == first.Request.ID {
		t.Fatal(second, err)
	}
	d, _ := s.Snapshot(ctx)
	if d.PRs[alphaScope().key()].Paused {
		t.Fatal("serialized same-second head observation paused", d.PRs)
	}
	old := Digest(d)
	f.through = f.through.Add(time.Minute)
	f.comments = []map[string]any{reviewComment(991, controlTime.Add(2*time.Minute))}
	if _, err = g.Current(ctx, alphaScope()); !errors.Is(err, ErrDeferred) {
		t.Fatal("late comment hidden behind prior complete cutoff", err)
	}
	d, _ = s.Snapshot(ctx)
	if Digest(d) != old {
		t.Fatal("late provenance mutated state")
	}
}

func TestPollingTriggersRestartDoesNotRerunAndEditsDoNotRewrite(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	f.comments = []map[string]any{reviewComment(991, controlTime.Add(2*time.Minute))}
	first, err := g.Current(ctx, alphaScope())
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.Admit(ctx, alphaScope(), first.Request.ID, testJob().Release, "same immutable evidence", f.through)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	g.reader.store = reopened
	f.through = f.through.Add(time.Minute)
	f.comments[0]["body"] = "/sfl config mode=claude-only"
	f.comments[0]["updated_at"] = f.through
	// An accepted review comment edited into a config command must not acquire a
	// second meaning. The original request's body hash remains authoritative.
	state, err := g.Current(ctx, alphaScope())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := reopened.Snapshot(ctx)
	if state.Request.ID != first.Request.ID || Digest(d.Jobs[job.ID].Job) != Digest(job) || len(d.Jobs) != 1 || len(d.Writes) != 0 {
		t.Fatal("restart/edit changed admission", state, d.Writes)
	}
}

// triggerPollingRemote exercises the actual controller/engine/outbox using the
// normalized polling seam. The HTTP shape/pagination have separate TLS tests.
type triggerPollingRemote struct {
	*pollingFixture
	policy   TriggerPolicy
	comments []CommandEvent
}

func (f *triggerPollingRemote) captureTriggers(ctx context.Context) (RemoteState, Discovery, error) {
	f.mu.Lock()
	snapshot := f.remote.Snapshot
	open, access := f.remote.Open, f.remote.Access
	commands := append([]CommandEvent{}, f.batch.Commands...)
	now := f.clock.Now()
	failure := f.failure
	f.reads++
	f.mu.Unlock()
	if failure != nil {
		return RemoteState{}, Discovery{}, failure
	}
	if !open || !access {
		return RemoteState{Access: access, Open: open}, Discovery{}, nil
	}
	d, err := f.store.Snapshot(ctx)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	batch, err := buildTriggerBatch(f.policy, alphaScope(), d.PRs[alphaScope().key()], 1234, OwnerID+1, controlTime.Add(time.Minute), now, snapshot, commands, append([]CommandEvent{}, f.comments...), open)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	for i := range batch.Requests {
		batch.Requests[i].BotID = 9003
	}
	if err = applyTriggers(&d, batch); err != nil {
		return RemoteState{}, Discovery{}, err
	}
	state := RemoteState{Access: true, Open: true, Snapshot: snapshot}
	pr := d.PRs[alphaScope().key()]
	for _, r := range pr.Requests {
		if r.ID == pr.CurrentRequest {
			state.Request = r
		}
	}
	return state, batch, nil
}
func (f *triggerPollingRemote) PollScope(ctx context.Context, _ Scope) (RemoteState, Discovery, error) {
	return f.captureTriggers(ctx)
}
func (f *triggerPollingRemote) Current(ctx context.Context, _ Scope) (RemoteState, error) {
	r, b, e := f.captureTriggers(ctx)
	if e == nil && r.Access && r.Open {
		e = f.store.Discover(ctx, b)
	}
	return r, e
}
func (f *triggerPollingRemote) History(ctx context.Context, _ Scope) (Discovery, error) {
	_, b, e := f.captureTriggers(ctx)
	return b, e
}
func setTriggerPolling(t *testing.T, c *PollingController, f *pollingFixture) *triggerPollingRemote {
	t.Helper()
	r := &triggerPollingRemote{pollingFixture: f, policy: TriggerPolicy{EnabledAt: controlTime}}
	c.options.Discovery = r
	c.options.Publisher.Remote = r
	return r
}

func TestPollingTriggersRunCompletedPushAndExplicitRerequest(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	r := setTriggerPolling(t, c, f)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || f.posts != 1 {
		t.Fatal("initial review", calls.Load(), f.posts)
	}
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("same observation reran completed review")
	}
	f.mu.Lock()
	f.remote.Snapshot.Head = f.remote.Snapshot.Base
	f.remote.Snapshot.MergeBase = f.remote.Snapshot.Base
	f.mu.Unlock()
	f.clock.Advance(time.Second)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 || f.posts != 2 {
		t.Fatal("push after completion", calls.Load(), f.posts)
	}
	f.clock.Advance(time.Second)
	r.comments = []CommandEvent{commandEvent(991, "@sfl review", f.clock.Now())}
	f.clock.Advance(time.Second)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 12 || f.posts != 3 {
		t.Fatal("explicit same-head rerequest", calls.Load(), f.posts)
	}
	d, _ := f.store.Snapshot(context.Background())
	for _, w := range d.Writes {
		if w.State != "confirmed" {
			t.Fatal(w.State)
		}
	}
}
func TestPollingTriggersStalePublicationDoesNotConsumeOrStrandB(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	setTriggerPolling(t, c, f)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	f.onPost = func(f *pollingFixture) {
		f.remote.Snapshot.Head = f.remote.Snapshot.Base
		f.remote.Snapshot.MergeBase = f.remote.Snapshot.Base
	}
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); !errors.Is(err, ErrDeferred) {
		t.Fatal("A publication did not detect B", err)
	}
	f.onPost = nil
	d, _ := f.store.Snapshot(context.Background())
	pr := d.PRs[alphaScope().key()]
	if pr.CurrentRequest != 2 || !pr.Paused {
		t.Fatal("B missing from durable reconciliation", pr)
	}
	f.clock.Advance(time.Second)
	result, err := pollOnce(t, c)
	if err != nil || !result.Active {
		t.Fatal("known B stranded waiting for C", result, err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 || f.posts != 2 || f.annotations != 1 {
		t.Fatal("B fulfilled by A or rerun", calls.Load(), f.posts, f.annotations)
	}
}

func TestPollingTriggersScopeIdentityAndClockChangesRefuse(t *testing.T) {
	for _, kind := range []string{"pull-id", "author", "created-time", "clock-backwards", "future-update", "foreign-pages", "scope", "legacy-state"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			g, f, s := triggerFixture(t)
			if kind == "legacy-state" {
				if err := s.RecordRequest(ctx, alphaScope(), native(1, controlTime.Add(time.Minute)), true); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := g.Current(ctx, alphaScope()); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.Snapshot(ctx)
			f.through = f.through.Add(time.Minute)
			switch kind {
			case "pull-id":
				f.pullID++
			case "author":
				f.authorID++
			case "created-time":
				f.created = f.created.Add(time.Second)
			case "clock-backwards":
				f.through = controlTime
			case "future-update":
				f.updated = f.through.Add(time.Second)
			case "foreign-pages":
				f.comments = []map[string]any{reviewComment(2, f.through), reviewComment(3, f.through), reviewComment(4, f.through)}
				f.foreignLink = true
			}
			scope := alphaScope()
			if kind == "scope" {
				scope.RepositoryID++
			}
			if _, err := g.Current(ctx, scope); err == nil {
				t.Fatal("accepted changed provenance", kind)
			}
			after, _ := s.Snapshot(ctx)
			if Digest(before) != Digest(after) {
				t.Fatal("failed read mutated state")
			}
		})
	}
}

func TestPollingTriggersConfigAcknowledgmentStillGatesManualRequest(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	r := setTriggerPolling(t, c, f)
	f.batch.Commands = []CommandEvent{commandEvent(41, "/sfl config mode=codex-only", controlTime.Add(2*time.Minute))}
	r.comments = []CommandEvent{commandEvent(42, "@sfl review", controlTime.Add(3*time.Minute))}
	f.lost = true
	if result, err := pollOnce(t, c); !errors.Is(err, ErrDeferred) || result.Active || calls.Load() != 0 {
		t.Fatal("unknown acknowledgment admitted inference", result, err, calls.Load())
	}
	f.lost = false
	f.clock.Advance(time.Second)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	awaitPollWorker(t, c)
	// This request predates the confirmed config ACK, so retains adversarial v1.
	if calls.Load() != 4 {
		t.Fatal("retroactive config change", calls.Load())
	}
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	r.comments = append(r.comments, commandEvent(43, "@sfl review", f.clock.Now()))
	f.clock.Advance(time.Second)
	if result, err := pollOnce(t, c); err != nil || !result.Active {
		t.Fatal(result, err)
	}
	awaitPollWorker(t, c)
	if calls.Load() != 5 {
		t.Fatal("new request did not use acknowledged codex-only config", calls.Load())
	}
	d, _ := f.store.Snapshot(context.Background())
	if len(d.Jobs) != 2 {
		t.Fatal(d.Jobs)
	}
	for _, j := range d.Jobs {
		if j.RequestID == 2 && (j.Job.ConfigVersion != 2 || j.Job.Config.Mode != CodexOnly) {
			t.Fatal(j.Job)
		}
	}
}

func TestPollingTriggersLegacyMutationSeamsRefusePopulatedCursor(t *testing.T) {
	ctx := context.Background()
	g, _, s := triggerFixture(t)
	if _, err := g.Current(ctx, alphaScope()); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot(ctx)
	if err := s.CaptureCommands(ctx, alphaScope(), []CommandEvent{commandEvent(44, "/sfl cancel", controlTime.Add(time.Hour))}, controlTime.Add(2*time.Hour), true); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, alphaScope(), native(55, controlTime.Add(time.Hour)), true); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal(err)
	}
	if err := s.Discover(ctx, Discovery{Scope: alphaScope(), Complete: true, Through: controlTime.Add(2 * time.Hour)}); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal(err)
	}
	after, _ := s.Snapshot(ctx)
	if Digest(after) != Digest(before) {
		t.Fatal("legacy seam changed selected ledger")
	}
}

func TestPollingTriggersUnknownPublicationBlocksBWithoutRepostingA(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	setTriggerPolling(t, c, f)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	f.lost = true
	f.onPost = func(f *pollingFixture) {
		f.remote.Snapshot.Head = f.remote.Snapshot.Base
		f.remote.Snapshot.MergeBase = f.remote.Snapshot.Base
	}
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, c); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	f.mu.Lock()
	held := append([]RemoteReceipt{}, f.receipts...)
	f.receipts = nil
	f.onPost = nil
	f.lost = false
	f.mu.Unlock()
	f.clock.Advance(time.Minute)
	result, err := pollOnce(t, c)
	if !errors.Is(err, ErrDeferred) || result.Active || calls.Load() != 4 || f.posts != 1 {
		t.Fatal("unknown A started B or repeated POST", result, err, calls.Load(), f.posts)
	}
	f.mu.Lock()
	f.receipts = held
	f.mu.Unlock()
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, c); !errors.Is(err, ErrDeferred) {
		t.Fatal("late A receipt failed supersession", err)
	}
	f.clock.Advance(time.Minute)
	result, err = pollOnce(t, c)
	if err != nil || !result.Active {
		t.Fatal("B stranded after known A reconciliation", result, err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Minute)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 || f.posts != 2 {
		t.Fatal("unknown replay caused duplicate work", calls.Load(), f.posts)
	}
}

func TestPollingTriggersAccessLossDoesNotReleaseOldB(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	a, err := g.Current(ctx, alphaScope())
	if err != nil {
		t.Fatal(err)
	}
	f.through = f.through.Add(time.Minute)
	f.snapshot.Head = strings.Repeat("e", 40)
	b, err := g.Current(ctx, alphaScope())
	if err != nil {
		t.Fatal(err)
	}
	w := newIntent("review", alphaScope(), 1, a.Request.ID, a.Snapshot, Digest("fake-job"), Document{}, f.through)
	if err = s.update(ctx, func(d *DurableState) error { d.Writes[w.ID] = &w; return nil }); err != nil {
		t.Fatal(err)
	}
	p := Publisher{Store: s}
	if err = p.markStale(ctx, w, RemoteState{Access: false}, "access lost"); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	f.through = f.through.Add(time.Minute)
	if _, err = g.Current(ctx, alphaScope()); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(ctx)
	pr := d.PRs[alphaScope().key()]
	if !pr.Paused || pr.PauseRequestID != b.Request.ID {
		t.Fatal("restored access reused an old request", pr)
	}
}

func TestPollingTriggersSameSecondPublicationReleasesKnownB(t *testing.T) {
	c, f, calls := pollControllerFixture(t, nil)
	setTriggerPolling(t, c, f)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	awaitPollWorker(t, c)
	a := writes(t, f.store, "review")[0]
	f.onPost = func(f *pollingFixture) {
		f.remote.Snapshot.Head = f.remote.Snapshot.Base
		f.remote.Snapshot.MergeBase = f.remote.Snapshot.Base
	}
	// Dispatch in the initial observation's clock second. B is causally newer
	// because its head is observed after A, despite identical platform seconds.
	if err := c.options.Publisher.Dispatch(context.Background(), a.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	f.onPost = nil
	d, _ := f.store.Snapshot(context.Background())
	pr := d.PRs[alphaScope().key()]
	if len(pr.Requests) != 2 || !pr.Requests[0].CreatedAt.Equal(pr.Requests[1].CreatedAt) {
		t.Fatal("fixture did not share a platform second", pr.Requests)
	}
	f.clock.Advance(time.Second)
	result, err := pollOnce(t, c)
	if err != nil || !result.Active {
		t.Fatal("known same-second B stranded", result, err)
	}
	awaitPollWorker(t, c)
	f.clock.Advance(time.Second)
	if _, err := pollOnce(t, c); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 || f.posts != 2 || f.annotations != 1 {
		t.Fatal("A consumed or replayed B", calls.Load(), f.posts, f.annotations)
	}
}

func TestPollingTriggersCanceledCommentCannotSuppressNewHead(t *testing.T) {
	for _, editedCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-cancel", true: "edited-cancel"}[editedCancel], func(t *testing.T) {
			ctx := context.Background()
			g, f, s := triggerFixture(t)
			if _, err := g.Current(ctx, alphaScope()); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.through = controlTime.Add(24 * time.Minute)
			f.snapshot.Head = strings.Repeat("e", 40)
			cancel := reviewComment(992, controlTime.Add(22*time.Minute))
			cancel["body"] = "/sfl cancel"
			if editedCancel {
				cancel["updated_at"] = controlTime.Add(23 * time.Minute)
			}
			f.comments = []map[string]any{reviewComment(991, controlTime.Add(21*time.Minute)), cancel}
			f.mu.Unlock()
			state, err := g.Current(ctx, alphaScope())
			if err != nil {
				t.Fatal(err)
			}
			want := "synchronize"
			if editedCancel {
				want = "comment"
			}
			if state.Request.ID == 0 || state.Request.Origin.Kind != want || state.Request.Snapshot != f.snapshot {
				t.Fatal("head consumed by canceled comment", state.Request)
			}
			id := state.Request.ID
			f.through = f.through.Add(time.Minute)
			next, err := g.Current(ctx, alphaScope())
			if err != nil || next.Request.ID != id {
				t.Fatal("next unchanged poll lost or replayed request", next, err)
			}
			d, _ := s.Snapshot(ctx)
			pr := d.PRs[alphaScope().key()]
			if !editedCancel && !state.Request.CreatedAt.After(pr.CancelledThrough) {
				t.Fatal("automatic request predates cancel", state.Request, pr.CancelledThrough)
			}
		})
	}
}

func TestPollingTriggersClosedHistoryPreservesHeadForReopen(t *testing.T) {
	for _, kind := range []string{"new-head", "observed-closed-aba", "base-only", "initial-new-closed", "initial-old-closed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			g, f, s := triggerFixture(t)
			initial := strings.HasPrefix(kind, "initial-")
			if kind == "initial-old-closed" {
				f.created = controlTime.Add(-time.Hour)
				f.updated = f.created
			}
			var prior NativeRequest
			if !initial {
				state, err := g.Current(ctx, alphaScope())
				if err != nil {
					t.Fatal(err)
				}
				prior = state.Request
			}
			f.open = false
			f.through = f.through.Add(time.Minute)
			if kind == "base-only" {
				f.snapshot.Base = strings.Repeat("e", 40)
			} else if !initial {
				f.snapshot.Head = strings.Repeat("e", 40)
			}
			history, err := g.History(ctx, alphaScope())
			if err != nil {
				t.Fatal(err)
			}
			if len(history.Requests) != 0 {
				t.Fatal("closed PR originated automatic work", history.Requests)
			}
			if err = s.Discover(ctx, history); err != nil {
				t.Fatal(err)
			}
			if kind == "observed-closed-aba" {
				f.snapshot.Head = prior.Snapshot.Head
				f.through = f.through.Add(time.Minute)
				history, err = g.History(ctx, alphaScope())
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Discover(ctx, history); err != nil {
					t.Fatal(err)
				}
			}
			f.open = true
			f.through = f.through.Add(time.Minute)
			current, err := g.Current(ctx, alphaScope())
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "new-head", "observed-closed-aba":
				if current.Request.ID == prior.ID || current.Request.ID == 0 || current.Request.Origin.Kind != "synchronize" {
					t.Fatal("closed reconciliation consumed head change", current)
				}
			case "base-only":
				if current.Request.ID != prior.ID {
					t.Fatal("base-only reopen originated automatic work", current)
				}
			case "initial-new-closed":
				if current.Request.ID == 0 || current.Request.Origin.Kind != "opened" {
					t.Fatal("closed first observation consumed opening", current)
				}
			case "initial-old-closed":
				if current.Request.ID != 0 {
					t.Fatal("pre-epoch closed backlog auto-ran", current)
				}
			}
		})
	}
}

// A fork head is provenance, not a second source. The selected reader records
// its repository ID in the pinned snapshot and otherwise reads, requests and
// admits exactly as for a same-repository head: canonical routes only, the same
// immutable revisions and no extra GitHub reads or writes.
func TestPollingTriggersForkHeadRecordsHeadRepository(t *testing.T) {
	const forkRepositoryID = int64(5555)
	observe := func(t *testing.T, headRepositoryID int64) (RemoteState, Job, *discoveryFixture, *DurableState) {
		ctx := context.Background()
		g, f, s := triggerFixture(t)
		f.snapshot.HeadRepositoryID = headRepositoryID
		state, err := g.Current(ctx, alphaScope())
		if err != nil {
			t.Fatal(err)
		}
		job, err := s.Admit(ctx, alphaScope(), state.Request.ID, testJob().Release, "same immutable evidence", f.through)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := s.Snapshot(ctx)
		return state, job, f, &d
	}
	fork, forkJob, f, d := observe(t, forkRepositoryID)
	if forkRepositoryID == RepositoryID || fork.Snapshot != f.snapshot || fork.Snapshot.HeadRepositoryID != forkRepositoryID || fork.Snapshot.RepositoryID != RepositoryID || fork.Snapshot.Validate() != nil {
		t.Fatal("fork head not recorded", fork.Snapshot)
	}
	if fork.Request.ID != 1 || fork.Request.Origin.Kind != "opened" || fork.Request.Origin.ActorRole != "pr_author" || fork.Request.Snapshot != fork.Snapshot {
		t.Fatal("fork head changed request handling", fork.Request)
	}
	if forkJob.Snapshot != fork.Snapshot || forkJob.Origin == nil || *forkJob.Origin != *fork.Request.Origin {
		t.Fatal("admitted job lost fork provenance", forkJob)
	}
	pr := d.PRs[alphaScope().key()]
	if pr.Triggers == nil || pr.Triggers.Snapshot != fork.Snapshot || pr.Triggers.PendingHead {
		t.Fatal("cursor did not pin fork head", pr.Triggers)
	}
	f.mu.Lock()
	forkCalls, forkWrites, forkPRReads, forkRefReads := f.calls, f.writes, f.prReads, f.refReads
	f.mu.Unlock()

	same, sameJob, plain, _ := observe(t, RepositoryID)
	plain.mu.Lock()
	defer plain.mu.Unlock()
	if forkWrites != 0 || plain.writes != 0 || forkCalls != plain.calls || forkPRReads != plain.prReads || forkRefReads != plain.refReads {
		t.Fatal("fork head changed GitHub reads", forkCalls, plain.calls, forkPRReads, plain.prReads, forkRefReads, plain.refReads, forkWrites)
	}
	// Apart from the recorded head repository, source and request are identical:
	// acquisition still names only the canonical repository and immutable IDs.
	expected := fork
	expected.Snapshot.HeadRepositoryID = RepositoryID
	expected.Request.Snapshot.HeadRepositoryID = RepositoryID
	if Digest(same) != Digest(expected) {
		t.Fatal("fork head handled differently", same, fork)
	}
	forkJob.Snapshot.HeadRepositoryID = RepositoryID
	if Digest(sameJob) != Digest(forkJob) {
		t.Fatal("fork job handled differently", sameJob, forkJob)
	}
}

// A head-repository change with an unchanged head SHA is a new observation. It
// fences old work and creates a synchronize request; it is never merged into
// the earlier capture, and a repeated observation cannot rerun it.
func TestPollingTriggersHeadRepositoryOnlyChangeIsNewObservation(t *testing.T) {
	ctx := context.Background()
	g, f, s := triggerFixture(t)
	first, err := g.Current(ctx, alphaScope())
	if err != nil || first.Request.ID != 1 || first.Request.Origin.Kind != "opened" {
		t.Fatal(first, err)
	}
	job, err := s.Admit(ctx, alphaScope(), first.Request.ID, testJob().Release, "same immutable evidence", f.through)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.snapshot.HeadRepositoryID = 5555
	moved := f.snapshot
	f.mu.Unlock()
	if moved.Head != first.Snapshot.Head || moved.Base != first.Snapshot.Base || moved.MergeBase != first.Snapshot.MergeBase {
		t.Fatal("fixture changed more than the head repository")
	}
	second, err := g.Current(ctx, alphaScope())
	if err != nil || second.Request.ID != 2 || second.Request.Origin.Kind != "synchronize" || second.Request.Origin.Sequence != 2 || !second.Request.Origin.EventAt.IsZero() {
		t.Fatal("head-repository change not observed", second, err)
	}
	if second.Snapshot != moved || second.Request.Snapshot != moved {
		t.Fatal("head-repository change merged into old capture", second)
	}
	d, _ := s.Snapshot(ctx)
	pr := d.PRs[alphaScope().key()]
	if d.Jobs[job.ID].State != "stale" || pr.Generation <= job.Generation || pr.CurrentRequest != second.Request.ID {
		t.Fatal("head-repository change did not fence old work", d.Jobs[job.ID], pr)
	}
	if pr.Requests[0].Snapshot != first.Snapshot || pr.Triggers.Snapshot != moved || pr.Triggers.PendingHead {
		t.Fatal("earlier capture rewritten or cursor not advanced", pr.Requests, pr.Triggers)
	}
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.mu.Unlock()
	again, err := g.Current(ctx, alphaScope())
	if err != nil || again.Request.ID != second.Request.ID {
		t.Fatal("repeated head-repository observation reran", again, err)
	}
	// Returning to the original head repository is another observation, not a
	// deduplication against the first capture.
	f.mu.Lock()
	f.through = f.through.Add(time.Minute)
	f.snapshot.HeadRepositoryID = first.Snapshot.HeadRepositoryID
	f.mu.Unlock()
	third, err := g.Current(ctx, alphaScope())
	if err != nil || third.Request.ID != 3 || third.Request.Origin.Kind != "synchronize" || third.Request.Origin.Sequence != 3 || third.Request.Snapshot != first.Snapshot {
		t.Fatal("head-repository ABA deduplicated", third, err)
	}
}

// A head repository that moves between the two captures of one read is a torn
// read. It defers without a request or cursor, before and after a durable
// observation, even when an owner review comment is present.
func TestPollingTriggersHeadRepositoryChangeDuringReadDefers(t *testing.T) {
	for _, kind := range []string{"first-observation", "established-cursor"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			g, f, s := triggerFixture(t)
			if kind == "established-cursor" {
				if _, err := g.Current(ctx, alphaScope()); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.Snapshot(ctx)
			native := f.alter
			f.mu.Lock()
			f.through = f.through.Add(time.Minute)
			f.comments = []map[string]any{reviewComment(991, f.through.Add(-time.Second))}
			reads := f.prReads
			f.alter = func(f *discoveryFixture, q *http.Request) {
				native(f, q)
				// Move only the head repository on the second capture of this read.
				if q.URL.Path == githubPR(alphaScope()) && f.prReads == reads+1 {
					f.snapshot.HeadRepositoryID = 5555
				}
			}
			head := f.snapshot.Head
			f.mu.Unlock()
			if _, batch, err := g.PollScope(ctx, alphaScope()); !errors.Is(err, ErrDeferred) || batch.Triggers != nil || len(batch.Requests) != 0 {
				t.Fatal("torn head-repository read produced a batch", batch, err)
			}
			f.mu.Lock()
			if f.prReads != reads+2 || f.snapshot.Head != head || f.writes != 0 {
				t.Fatal("fixture did not tear only the head repository", f.prReads, f.snapshot, f.writes)
			}
			f.mu.Unlock()
			after, _ := s.Snapshot(ctx)
			if Digest(after) != Digest(before) {
				t.Fatal("torn read mutated durable state")
			}
		})
	}
}
