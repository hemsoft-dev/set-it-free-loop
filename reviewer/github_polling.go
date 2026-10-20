package reviewer

import (
	"context"
	"errors"
	"sort"
)

// ErrRepositoryAccessLost is emitted only for a definitive loss observed at the
// enrolled repository lookup, never a failed/partial PR collection or rate limit.
var ErrRepositoryAccessLost = errors.New("enrolled repository access lost")

// OpenScopes lists every open PR so owner configuration can be acknowledged
// before its first native request. Current/History still establish provenance;
// neither the candidate list nor a reviewer login can authorize inference.
func (g *GitHubDiscoveryHTTP) OpenScopes(ctx context.Context) ([]Scope, error) {
	d, err := g.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if d.Enrollment != g.enrollment {
		return nil, ErrIgnored
	}
	ctx, cancel := context.WithTimeout(ctx, githubDiscoveryTimeout)
	defer cancel()
	scope := Scope{RepositoryID: g.enrollment.RepositoryID, InstallationID: g.enrollment.InstallationID, PR: 1}
	if err := g.http.repository(ctx, scope); err != nil {
		if githubAccessLost(err) {
			return nil, errors.Join(ErrRepositoryAccessLost, err)
		}
		return nil, err
	}
	scopes := []Scope{}
	seen := map[int]bool{}
	// GitHub's documented default state is open. Keeping only page/per_page
	// preserves the existing strict same-collection pagination contract.
	err = visitGitHubPages[githubPull](ctx, g.http, githubRepoPath+"/pulls", func(p githubPull) error {
		if p.Number <= 0 || p.State != "open" || p.Base.Repo.ID != g.enrollment.RepositoryID || seen[p.Number] || len(scopes) >= 64 {
			return ErrDeferred
		}
		seen[p.Number] = true
		scopes = append(scopes, Scope{g.enrollment.RepositoryID, g.enrollment.InstallationID, p.Number})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].PR < scopes[j].PR })
	return scopes, nil
}

// PollScope leaves the complete batch to the controller's transaction. The
// publication Current method separately commits discovery before returning.
func (g *GitHubDiscoveryHTTP) PollScope(ctx context.Context, scope Scope) (RemoteState, Discovery, error) {
	return g.read(ctx, scope, false)
}
