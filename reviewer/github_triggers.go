package reviewer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHubTriggerDiscoveryHTTP is the selected comment/automatic polling reader.
// It satisfies both PollDiscovery and PublicationRemote's provenance methods.
// No constructor reads a key, starts a service, installs a webhook or authorizes
// inference. The explicit epoch must be preserved with the service's policy.
type GitHubTriggerDiscoveryHTTP struct {
	reader *GitHubDiscoveryHTTP
	policy TriggerPolicy
}

func NewGitHubTriggerDiscoveryHTTP(e Enrollment, store *Store, transport http.RoundTripper, policy TriggerPolicy) (*GitHubTriggerDiscoveryHTTP, error) {
	if policy.validate() != nil {
		return nil, ErrIgnored
	}
	reader, err := newGitHubDiscoveryHTTP(e, store, transport)
	if err != nil {
		return nil, err
	}
	return &GitHubTriggerDiscoveryHTTP{reader: reader, policy: policy}, nil
}
func (g *GitHubTriggerDiscoveryHTTP) OpenScopes(ctx context.Context) ([]Scope, error) {
	return g.reader.OpenScopes(ctx)
}
func (g *GitHubTriggerDiscoveryHTTP) PollScope(ctx context.Context, s Scope) (RemoteState, Discovery, error) {
	return g.read(ctx, s, false)
}
func (g *GitHubTriggerDiscoveryHTTP) History(ctx context.Context, s Scope) (Discovery, error) {
	_, batch, err := g.read(ctx, s, true)
	return batch, err
}
func (g *GitHubTriggerDiscoveryHTTP) Current(ctx context.Context, s Scope) (RemoteState, error) {
	state, batch, err := g.read(ctx, s, false)
	if err == nil && state.Access && state.Open {
		err = g.reader.store.Discover(ctx, batch)
	}
	return state, err
}

// read exhausts PR issue comments with the same bounded, same-origin pagination
// as the old reader. Native request/event routes are deliberately never read.
func (g *GitHubTriggerDiscoveryHTTP) read(ctx context.Context, scope Scope, history bool) (RemoteState, Discovery, error) {
	r := g.reader
	if !r.enrollment.accepts(scope) {
		return RemoteState{}, Discovery{}, ErrIgnored
	}
	ctx, cancel := context.WithTimeout(ctx, githubDiscoveryTimeout)
	defer cancel()
	local, err := r.store.Snapshot(ctx)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	if local.Enrollment != r.enrollment {
		return RemoteState{}, Discovery{}, ErrIgnored
	}
	var repo struct {
		ID       int64
		FullName string `json:"full_name"`
	}
	header, err := r.http.request(ctx, "GET", githubRepoPath, nil, 200, &repo)
	if err != nil {
		if !history && githubAccessLost(err) {
			return RemoteState{}, Discovery{}, nil
		}
		return RemoteState{}, Discovery{}, err
	}
	if repo.ID != r.enrollment.RepositoryID || repo.FullName != "hemsoft-dev/set-it-free-loop" {
		return RemoteState{}, Discovery{}, ErrIgnored
	}
	through, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return RemoteState{}, Discovery{}, ErrDeferred
	}
	before, err := r.captureTrigger(ctx, scope, history)
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
	if before.UpdatedAt.After(through) {
		return RemoteState{}, Discovery{}, ErrDeferred
	}
	snapshot, err := r.mergeBase(ctx, before.Snapshot)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	commands, comments := []CommandEvent{}, []CommandEvent{}
	seen := map[int64]bool{}
	err = visitGitHubPages(ctx, r.http, githubIssue(scope)+"/comments", func(c githubCommandComment) error {
		if c.ID <= 0 || seen[c.ID] {
			return ErrDeferred
		}
		seen[c.ID] = true
		if c.User.ID != OwnerID || (!strings.HasPrefix(c.Body, "/sfl ") && c.Body != "@sfl review") {
			return nil
		}
		if c.IssueURL != githubOrigin+githubIssue(scope) || c.CreatedAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) || c.CreatedAt.After(through) || len(c.Body) > 65<<10 {
			return ErrDeferred
		}
		event := CommandEvent{ID: c.ID, AuthorID: c.User.ID, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
		if c.Body == "@sfl review" {
			comments = append(comments, event)
		} else {
			commands = append(commands, event)
		}
		if len(commands)+len(comments) > 128 {
			return ErrDeferred
		}
		return nil
	})
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	after, err := r.captureTrigger(ctx, scope, history)
	if err != nil || before != after {
		return RemoteState{}, Discovery{}, errors.Join(ErrDeferred, err)
	}
	batch, err := buildTriggerBatch(g.policy, scope, local.PRs[scope.key()], before.PullID, before.AuthorID, before.CreatedAt, through, snapshot, commands, comments)
	if err != nil {
		return RemoteState{}, Discovery{}, err
	}
	if !after.Open {
		// A closed PR cannot originate a new automatic request during reconciliation.
		filtered := batch.Requests[:0]
		for _, request := range batch.Requests {
			if request.Origin.Kind == "comment" {
				filtered = append(filtered, request)
			}
		}
		batch.Requests = filtered
	}
	for i := range batch.Requests {
		batch.Requests[i].BotID = r.enrollment.BotID
	}
	// Apply to this isolated snapshot to return the same request that the actual
	// controller transaction will select. The hash fence rejects concurrent local
	// changes. Local ACK intents created here are discarded, never transmitted.
	if err = applyTriggers(&local, batch); err != nil {
		return RemoteState{}, Discovery{}, err
	}
	state := RemoteState{Access: true, Open: after.Open, Snapshot: snapshot}
	pr := local.PRs[scope.key()]
	for _, request := range pr.Requests {
		if request.ID == pr.CurrentRequest {
			state.Request = request
		}
	}
	return state, batch, nil
}

type githubTriggerSnapshot struct {
	Snapshot             Snapshot
	BaseRef              string
	Open                 bool
	PullID, AuthorID     int64
	CreatedAt, UpdatedAt time.Time
}

func (g *GitHubDiscoveryHTTP) captureTrigger(ctx context.Context, scope Scope, closedOK bool) (githubTriggerSnapshot, error) {
	var p githubPull
	if _, err := g.http.request(ctx, "GET", githubPR(scope), nil, 200, &p); err != nil {
		if githubAccessLost(err) {
			return githubTriggerSnapshot{}, &githubPullMissing{err}
		}
		return githubTriggerSnapshot{}, err
	}
	if p.Number != scope.PR || p.ID <= 0 || p.User.ID <= 0 || p.Base.Repo.ID != g.enrollment.RepositoryID || (p.State != "open" && p.State != "closed") || p.CreatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) {
		return githubTriggerSnapshot{}, ErrDeferred
	}
	s := githubTriggerSnapshot{Open: p.State == "open", BaseRef: p.Base.Ref, PullID: p.ID, AuthorID: p.User.ID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	if !s.Open && !closedOK {
		return s, nil
	}
	if !githubBranch(p.Base.Ref) || !objectID.MatchString(p.Head.SHA) || p.Head.Repo.ID <= 0 {
		return githubTriggerSnapshot{}, ErrDeferred
	}
	// Share immutable base-tip handling while avoiding requested_reviewers, which
	// cannot authorize or complete either of the selected trigger kinds.
	var ref struct {
		Ref    string
		Object struct{ SHA, Type string }
	}
	if _, err := g.http.request(ctx, "GET", githubRepoPath+"/git/ref/heads/"+url.PathEscape(p.Base.Ref), nil, 200, &ref); err != nil {
		return githubTriggerSnapshot{}, err
	}
	if ref.Ref != "refs/heads/"+p.Base.Ref || ref.Object.Type != "commit" || !objectID.MatchString(ref.Object.SHA) {
		return githubTriggerSnapshot{}, ErrDeferred
	}
	s.Snapshot = Snapshot{RepositoryID: g.enrollment.RepositoryID, HeadRepositoryID: p.Head.Repo.ID, PR: scope.PR, Head: p.Head.SHA, Base: ref.Object.SHA}
	return s, nil
}
