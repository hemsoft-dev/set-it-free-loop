package reviewer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Two fully paginated collections plus stable snapshot reads fit this budget
// at the shared ten-second request limit. Caller deadlines still take priority.
const githubDiscoveryTimeout = 45 * time.Minute

// GitHubDiscoveryHTTP collects native provenance with an explicitly supplied
// read transport. It never acquires credentials or starts a polling service.
// Current commits complete discovery before returning, so newly observed owner
// cancellation/replacement is visible to the publisher's local dispatch fence.
type GitHubDiscoveryHTTP struct {
	enrollment Enrollment
	store      *Store
	http       *GitHubPublicationHTTP
}

type githubReadTransport struct{ inner http.RoundTripper }

func (t githubReadTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.Method != http.MethodGet {
		return nil, ErrIgnored
	}
	return t.inner.RoundTrip(q)
}
func NewGitHubDiscoveryHTTP(e Enrollment, store *Store, transport http.RoundTripper) (*GitHubDiscoveryHTTP, error) {
	if e.Validate() != nil || store == nil || store.db == nil || transport == nil {
		return nil, ErrIgnored
	}
	client := &http.Client{Transport: githubReadTransport{transport}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &GitHubDiscoveryHTTP{e, store, &GitHubPublicationHTTP{enrollment: e, client: client}}, nil
}

type githubPull struct {
	Number int
	State  string
	Head   struct {
		SHA  string
		Repo struct{ ID int64 }
	}
	Base struct {
		Ref  string
		Repo struct{ ID int64 }
	}
}
type githubNativeEvent struct {
	ID                int64
	Event, URL        string
	Actor             githubAuthor
	RequestedReviewer *githubAuthor       `json:"requested_reviewer"`
	RequestedTeam     *struct{ ID int64 } `json:"requested_team"`
	Requester         *githubAuthor       `json:"review_requester"`
	CreatedAt         time.Time           `json:"created_at"`
}
type githubCommandComment struct {
	ID        int64
	User      githubAuthor
	Body      string
	IssueURL  string    `json:"issue_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type githubReadSnapshot struct {
	Snapshot      Snapshot
	BaseRef       string
	Open, Pending bool
}

// A branch name is data in one fixed Git API path, never a selected URL or
// executable argument. Reject ambiguous Git refs before escaping the component.
func githubBranch(ref string) bool {
	if len(ref) == 0 || len(ref) > 240 || !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`).MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// Only a missing repository or PR lookup can establish inaccessible scope.
// A missing base ref is a failed snapshot read, not repository access loss.
type githubPullMissing struct{ cause error }

func (e *githubPullMissing) Error() string { return e.cause.Error() }
func (e *githubPullMissing) Unwrap() error { return e.cause }

func (g *GitHubDiscoveryHTTP) capture(ctx context.Context, scope Scope, closedOK bool) (githubReadSnapshot, error) {
	var p githubPull
	if _, err := g.http.request(ctx, "GET", githubPR(scope), nil, 200, &p); err != nil {
		if githubAccessLost(err) {
			return githubReadSnapshot{}, &githubPullMissing{err}
		}
		return githubReadSnapshot{}, err
	}
	if p.Number != scope.PR || p.Base.Repo.ID != g.enrollment.RepositoryID || (p.State != "open" && p.State != "closed") {
		return githubReadSnapshot{}, ErrDeferred
	}
	s := githubReadSnapshot{Open: p.State == "open", BaseRef: p.Base.Ref}
	if !s.Open && !closedOK {
		return s, nil
	}
	if !githubBranch(p.Base.Ref) || !objectID.MatchString(p.Head.SHA) || p.Head.Repo.ID <= 0 {
		return githubReadSnapshot{}, ErrDeferred
	}
	var ref struct {
		Ref    string
		Object struct{ SHA, Type string }
	}
	// Pull-request base.sha can lag the actual branch tip after another PR merges.
	// Resolve that ref independently; neither merge-base nor cached base.sha is it.
	if _, err := g.http.request(ctx, "GET", githubRepoPath+"/git/ref/heads/"+url.PathEscape(p.Base.Ref), nil, 200, &ref); err != nil {
		return githubReadSnapshot{}, err
	}
	if ref.Ref != "refs/heads/"+p.Base.Ref || ref.Object.Type != "commit" || !objectID.MatchString(ref.Object.SHA) {
		return githubReadSnapshot{}, ErrDeferred
	}
	s.Snapshot = Snapshot{RepositoryID: g.enrollment.RepositoryID, HeadRepositoryID: p.Head.Repo.ID, PR: scope.PR, Head: p.Head.SHA, Base: ref.Object.SHA}
	var pending struct {
		Users []githubAuthor
		Teams []githubAuthor
	}
	h, err := g.http.request(ctx, "GET", githubPR(scope)+"/requested_reviewers", nil, 200, &pending)
	if err != nil {
		return githubReadSnapshot{}, err
	}
	// This endpoint returns all users/teams, not an array page. Never interpret an
	// unexpected partial result or missing arrays as a complete negative.
	if pending.Users == nil || pending.Teams == nil || len(pending.Users) > 100 || len(pending.Teams) > 100 || len(h.Values("Link")) != 0 {
		return githubReadSnapshot{}, ErrDeferred
	}
	seen := map[int64]bool{}
	for _, u := range pending.Users {
		if u.ID <= 0 || seen[u.ID] {
			return githubReadSnapshot{}, ErrDeferred
		}
		seen[u.ID] = true
		if u.ID == g.enrollment.BotID {
			s.Pending = true
		}
	}
	return s, nil
}
func (g *GitHubDiscoveryHTTP) mergeBase(ctx context.Context, s Snapshot) (Snapshot, error) {
	var comparison struct {
		Base      struct{ SHA string } `json:"base_commit"`
		MergeBase struct{ SHA string } `json:"merge_base_commit"`
	}
	// GitHub includes changed-file patches only on page one. This read needs only
	// immutable comparison metadata, not candidate patches or a commit listing.
	_, err := g.http.request(ctx, "GET", githubRepoPath+"/compare/"+s.Base+"..."+s.Head+"?per_page=1&page=2", nil, 200, &comparison)
	if err != nil {
		return Snapshot{}, err
	}
	s.MergeBase = comparison.MergeBase.SHA
	if comparison.Base.SHA != s.Base || s.Validate() != nil {
		return Snapshot{}, ErrDeferred
	}
	return s, nil
}
func (g *GitHubDiscoveryHTTP) nativeRequests(ctx context.Context, scope Scope, snapshot Snapshot, through time.Time, saved *PRRecord) ([]NativeRequest, []NativeHistoryEvent, error) {
	events := []githubNativeEvent{}
	seen := map[int64]bool{}
	err := visitGitHubPages(ctx, g.http, githubIssue(scope)+"/events", func(e githubNativeEvent) error {
		if e.Event != "review_requested" && e.Event != "review_request_removed" {
			return nil
		}
		if e.RequestedReviewer == nil {
			if e.RequestedTeam != nil && e.RequestedTeam.ID > 0 {
				return nil
			}
			return ErrDeferred
		}
		if e.RequestedReviewer.ID <= 0 {
			return ErrDeferred
		}
		if e.RequestedReviewer.ID != g.enrollment.BotID {
			return nil
		}
		if e.ID <= 0 || seen[e.ID] || e.Actor.ID <= 0 || e.CreatedAt.IsZero() || e.CreatedAt.After(through) || e.URL != githubOrigin+githubRepoPath+"/issues/events/"+strconv.FormatInt(e.ID, 10) {
			return ErrDeferred
		}
		seen[e.ID] = true
		if e.Event == "review_requested" && (e.Requester == nil || e.Requester.ID <= 0 || e.Requester.ID != e.Actor.ID) {
			return ErrDeferred
		}
		events = append(events, e)
		if len(events) > 128 {
			return ErrDeferred
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt.Before(events[j].CreatedAt) })
	requests := []NativeRequest{}
	known := map[int64]NativeRequest{}
	if saved != nil {
		for _, r := range saved.Requests {
			known[r.ID] = r
		}
	}
	last := time.Time{}
	active := false
	for _, e := range events {
		// Platform timestamps have finite resolution. Numeric IDs do not supply a
		// documented causal tie-break between request and removal events.
		if e.CreatedAt.Equal(last) {
			return nil, nil, ErrDeferred
		}
		last = e.CreatedAt
		if e.Event == "review_request_removed" {
			if len(requests) == 0 || !active {
				return nil, nil, ErrDeferred
			}
			requests[len(requests)-1].Removed = true
			active = false
			continue
		}
		if len(requests) > 0 {
			requests[len(requests)-1].Removed = true
		}
		r := NativeRequest{ID: e.ID, RequesterID: e.Requester.ID, BotID: e.RequestedReviewer.ID, CreatedAt: e.CreatedAt, Snapshot: snapshot}
		if old, ok := known[r.ID]; ok {
			if old.RequesterID != r.RequesterID || old.BotID != r.BotID || !old.CreatedAt.Equal(r.CreatedAt) || old.Snapshot.Validate() != nil || old.Snapshot.PR != scope.PR || old.Snapshot.RepositoryID != scope.RepositoryID {
				return nil, nil, ErrDeferred
			}
			r.Snapshot = old.Snapshot
			delete(known, r.ID)
		}
		requests = append(requests, r)
		active = true
	}
	// A complete remote read cannot silently discard previously persisted native
	// provenance. Lost visibility/retention must remain explicit uncertainty.
	if len(known) != 0 {
		return nil, nil, ErrDeferred
	}
	if saved != nil {
		for _, old := range saved.Requests {
			if !old.Removed {
				continue
			}
			reproduced := false
			for _, r := range requests {
				if r.ID == old.ID && r.Removed {
					reproduced = true
					break
				}
			}
			if !reproduced {
				return nil, nil, ErrDeferred
			}
		}
	}
	normalized := make([]NativeHistoryEvent, 0, len(events))
	for _, event := range events {
		requester := int64(0)
		if event.Requester != nil {
			requester = event.Requester.ID
		}
		normalized = append(normalized, NativeHistoryEvent{ID: event.ID, ActorID: event.Actor.ID, RequesterID: requester, BotID: event.RequestedReviewer.ID, Kind: event.Event, CreatedAt: event.CreatedAt})
	}
	return requests, normalized, nil
}
func (g *GitHubDiscoveryHTTP) commands(ctx context.Context, scope Scope, through time.Time) ([]CommandEvent, error) {
	commands := []CommandEvent{}
	seen := map[int64]bool{}
	err := visitGitHubPages(ctx, g.http, githubIssue(scope)+"/comments", func(c githubCommandComment) error {
		if c.ID <= 0 || seen[c.ID] {
			return ErrDeferred
		}
		seen[c.ID] = true
		if c.User.ID != OwnerID || !strings.HasPrefix(c.Body, "/sfl ") {
			return nil
		}
		if c.IssueURL != githubOrigin+githubIssue(scope) || c.CreatedAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) || c.CreatedAt.After(through) || len(c.Body) > 65<<10 {
			return ErrDeferred
		}
		commands = append(commands, CommandEvent{ID: c.ID, AuthorID: c.User.ID, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
		if len(commands) > 64 {
			return ErrDeferred
		}
		return nil
	})
	return commands, err
}
func githubAccessLost(err error) bool {
	var status *GitHubHTTPError
	return errors.As(err, &status) && status.Status == 404
}
func (g *GitHubDiscoveryHTTP) read(ctx context.Context, scope Scope, history bool) (RemoteState, Discovery, error) {
	if !g.enrollment.accepts(scope) {
		return RemoteState{}, Discovery{}, ErrIgnored
	}
	ctx, cancel := context.WithTimeout(ctx, githubDiscoveryTimeout)
	defer cancel()
	local, err := g.store.Snapshot(ctx)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	if local.Enrollment != g.enrollment {
		return RemoteState{}, Discovery{}, ErrIgnored
	}
	var repo struct {
		ID       int64
		FullName string `json:"full_name"`
	}
	h, err := g.http.request(ctx, "GET", githubRepoPath, nil, 200, &repo)
	if err != nil {
		if !history && githubAccessLost(err) {
			return RemoteState{}, Discovery{}, nil
		}
		return RemoteState{}, Discovery{}, err
	}
	if repo.ID != g.enrollment.RepositoryID || repo.FullName != "hemsoft-dev/set-it-free-loop" {
		return RemoteState{}, Discovery{}, ErrIgnored
	}
	through, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return RemoteState{}, Discovery{}, ErrDeferred
	}
	before, err := g.capture(ctx, scope, history)
	if err != nil {
		var missing *githubPullMissing
		if !history && errors.As(err, &missing) {
			return RemoteState{}, Discovery{}, nil
		}
		return RemoteState{}, Discovery{}, err
	}
	if !before.Open && !history {
		return RemoteState{Access: true}, Discovery{}, nil
	}
	snapshot, err := g.mergeBase(ctx, before.Snapshot)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	requests, nativeEvents, err := g.nativeRequests(ctx, scope, snapshot, through, local.PRs[scope.key()])
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	saved := local.PRs[scope.key()]
	if saved == nil {
		saved = &PRRecord{}
	}
	if err = retainNativeHistory(g.enrollment, saved, nativeEvents, through); err != nil {
		return RemoteState{}, Discovery{}, err
	}
	commands, err := g.commands(ctx, scope, through)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	after, err := g.capture(ctx, scope, history)
	if err != nil || before != after {
		return RemoteState{}, Discovery{}, errors.Join(ErrDeferred, err)
	}
	state := RemoteState{Access: true, Open: after.Open, Snapshot: snapshot}
	if after.Pending {
		if len(requests) == 0 || requests[len(requests)-1].Removed {
			return RemoteState{}, Discovery{}, ErrDeferred
		}
		state.Request = requests[len(requests)-1]
	}
	return state, Discovery{Scope: scope, Commands: commands, Requests: requests, NativeEvents: nativeEvents, Through: through, Complete: true}, nil
}
func (g *GitHubDiscoveryHTTP) Current(ctx context.Context, scope Scope) (RemoteState, error) {
	state, batch, err := g.read(ctx, scope, false)
	if err == nil && state.Access && state.Open {
		err = g.store.Discover(ctx, batch)
	}
	return state, err
}
func (g *GitHubDiscoveryHTTP) History(ctx context.Context, scope Scope) (Discovery, error) {
	_, batch, err := g.read(ctx, scope, true)
	return batch, err
}
