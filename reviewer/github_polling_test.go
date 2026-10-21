package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestGitHubPollingCandidatePagesAreCompleteAndReadOnly(t *testing.T) {
	for _, kind := range []string{"valid", "duplicate", "foreign-link", "wrong-repository", "closed", "failed-page", "too-many"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := openFixtureStore(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.Method != "GET" {
					t.Error("candidate listing attempted write")
					w.WriteHeader(400)
					return
				}
				if q.URL.Path == githubRepoPath {
					json.NewEncoder(w).Encode(map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop"})
					return
				}
				if q.URL.Path != githubRepoPath+"/pulls" || q.URL.Query().Get("per_page") != "100" || len(q.URL.Query()) != 2 {
					t.Error("wrong list destination or query", q.URL)
					w.WriteHeader(400)
					return
				}
				page := q.URL.Query().Get("page")
				number := 2
				if page == "2" {
					number = 1
					if kind == "duplicate" {
						number = 2
					}
					if kind == "failed-page" {
						w.WriteHeader(503)
						return
					}
				}
				if page == "1" {
					origin := githubOrigin
					if kind == "foreign-link" {
						origin = "https://outside.invalid"
					}
					w.Header().Set("Link", fmt.Sprintf(`<%s%s/pulls?per_page=100&page=2>; rel="next"`, origin, githubRepoPath))
				}
				id := RepositoryID
				if kind == "wrong-repository" {
					id++
				}
				state := "open"
				if kind == "closed" {
					state = "closed"
				}
				items := []any{map[string]any{"number": number, "state": state, "base": map[string]any{"repo": map[string]any{"id": id}}}}
				if kind == "too-many" {
					items = []any{}
					for i := 1; i <= 65; i++ {
						items = append(items, map[string]any{"number": i, "state": "open", "base": map[string]any{"repo": map[string]any{"id": RepositoryID}}})
					}
				}
				json.NewEncoder(w).Encode(items)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			g, err := newGitHubDiscoveryHTTP(alphaEnrollment(), store, githubFixtureRoute{base, server.Client().Transport, t})
			if err != nil {
				t.Fatal(err)
			}
			scopes, err := g.OpenScopes(context.Background())
			if kind == "valid" {
				if err != nil || !reflect.DeepEqual(scopes, []Scope{alphaScope(), {RepositoryID, 9001, 2}}) {
					t.Fatal(scopes, err)
				}
			} else if err == nil || scopes != nil {
				t.Fatal("incomplete candidate list accepted", kind, scopes, err)
			}
		})
	}
}

func TestGitHubPollingScopeLeavesAtomicDiscoveryToController(t *testing.T) {
	store, _ := openFixtureStore(t)
	g, _ := discoveryHTTPFixture(t, store)
	remote, batch, err := g.PollScope(context.Background(), alphaScope())
	if err != nil || !remote.Access || !remote.Open || remote.Request.ID != 1 || !batch.Complete {
		t.Fatal(remote, batch, err)
	}
	before, err := store.Snapshot(context.Background())
	if err != nil || len(before.PRs) != 0 {
		t.Fatal("observation partially committed discovery", err)
	}
	if err := store.Discover(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Snapshot(context.Background())
	if after.PRs[alphaScope().key()].CurrentRequest != 1 {
		t.Fatal("complete batch did not commit")
	}
}

func TestGitHubPollingOnlyRepositoryLookup404MarksAccessLost(t *testing.T) {
	for _, kind := range []string{"repository404", "repository403", "repository503", "collection404"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := openFixtureStore(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.Method != "GET" {
					t.Error("unexpected write")
					w.WriteHeader(400)
					return
				}
				if q.URL.Path == githubRepoPath {
					switch kind {
					case "repository404":
						w.WriteHeader(404)
						return
					case "repository403":
						w.WriteHeader(403)
						return
					case "repository503":
						w.WriteHeader(503)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop"})
					return
				}
				w.WriteHeader(404)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			g, err := newGitHubDiscoveryHTTP(alphaEnrollment(), store, githubFixtureRoute{base, server.Client().Transport, t})
			if err != nil {
				t.Fatal(err)
			}
			scopes, err := g.OpenScopes(context.Background())
			if err == nil || scopes != nil || errors.Is(err, ErrRepositoryAccessLost) != (kind == "repository404") {
				t.Fatal(kind, scopes, err)
			}
		})
	}
}
