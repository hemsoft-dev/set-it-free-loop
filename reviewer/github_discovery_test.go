package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type discoveryFixture struct {
	failurePath, limitKind           string
	failureStatus                    int
	mu                               sync.Mutex
	pullID, authorID                 int64
	created, updated                 time.Time
	snapshot                         Snapshot
	through                          time.Time
	open, pending                    bool
	events, comments                 []map[string]any
	calls, writes, prReads, refReads int
	failSecondPage, foreignLink      bool
	alter                            func(*discoveryFixture, *http.Request)
}

func nativeEvent(id int64, kind string, actor int64, at time.Time) map[string]any {
	return map[string]any{"id": id, "event": kind, "url": githubOrigin + githubRepoPath + "/issues/events/" + strconv.FormatInt(id, 10), "actor": map[string]any{"id": actor}, "requested_reviewer": map[string]any{"id": 9003, "type": "Bot"}, "review_requester": map[string]any{"id": actor}, "created_at": at}
}
func discoveryHTTPFixture(t *testing.T, store *Store) (*GitHubDiscoveryHTTP, *discoveryFixture) {
	t.Helper()
	f := &discoveryFixture{pullID: 1234, authorID: OwnerID, created: controlTime.Add(time.Minute), updated: controlTime.Add(time.Minute), snapshot: testSnapshot(), through: controlTime.Add(20 * time.Minute), open: true, pending: true, events: []map[string]any{nativeEvent(1, "review_requested", OwnerID, controlTime.Add(time.Minute))}, comments: []map[string]any{}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if q.Method != "GET" {
			f.writes++
			t.Error("discovery attempted a GitHub write")
			w.WriteHeader(403)
			return
		}
		if f.alter != nil {
			f.alter(f, q)
		}
		w.Header().Set("Date", f.through.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/json")
		if f.failureStatus != 0 && (f.failurePath == "" || q.URL.Path == f.failurePath) {
			if f.limitKind == "secondary" {
				w.Header().Set("Retry-After", "37")
			}
			if f.limitKind == "primary" {
				w.Header().Set("X-Ratelimit-Remaining", "0")
				w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(f.through.Add(37*time.Second).Unix(), 10))
			}
			w.WriteHeader(f.failureStatus)
			json.NewEncoder(w).Encode(map[string]any{"message": "private upstream detail"})
			return
		}
		var value any
		switch q.URL.Path {
		case githubRepoPath:
			value = map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop"}
		case githubPR(alphaScope()):
			f.prReads++
			state := "open"
			if !f.open {
				state = "closed"
			}
			value = map[string]any{"id": f.pullID, "user": map[string]any{"id": f.authorID}, "created_at": f.created, "updated_at": f.updated, "number": 1, "state": state, "head": map[string]any{"sha": f.snapshot.Head, "repo": map[string]any{"id": f.snapshot.HeadRepositoryID}}, "base": map[string]any{"ref": "main", "sha": strings.Repeat("f", 40), "repo": map[string]any{"id": RepositoryID}}}
		case githubRepoPath + "/git/ref/heads/main":
			f.refReads++
			value = map[string]any{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": f.snapshot.Base}}
		case githubPR(alphaScope()) + "/requested_reviewers":
			users := []any{}
			if f.pending {
				users = append(users, map[string]any{"id": 9003})
			}
			value = map[string]any{"users": users, "teams": []any{}}
		case githubIssue(alphaScope()) + "/events", githubIssue(alphaScope()) + "/comments":
			items := f.events
			if strings.HasSuffix(q.URL.Path, "/comments") {
				items = f.comments
			}
			page, _ := strconv.Atoi(q.URL.Query().Get("page"))
			size, _ := strconv.Atoi(q.URL.Query().Get("per_page"))
			if page < 1 || size != 100 {
				t.Error("missing bounded pagination", q.URL)
				w.WriteHeader(400)
				return
			}
			if page == 2 && f.failSecondPage {
				w.WriteHeader(503)
				return
			}
			// Deliberately short pages preserve actual multi-page transport coverage.
			start := min((page-1)*2, len(items))
			end := min(start+2, len(items))
			if end < len(items) {
				alias := "/repositories/" + strconv.FormatInt(RepositoryID, 10) + strings.TrimPrefix(q.URL.Path, githubRepoPath)
				origin := githubOrigin
				if f.foreignLink {
					origin = "https://foreign.invalid"
				}
				w.Header().Set("Link", "<"+origin+alias+"?per_page=100&page="+strconv.Itoa(page+1)+">; rel=\"next\"")
			}
			value = items[start:end]
		default:
			if q.URL.Path == githubRepoPath+"/compare/"+f.snapshot.Base+"..."+f.snapshot.Head && q.URL.RawQuery == "per_page=1&page=2" {
				value = map[string]any{"base_commit": map[string]any{"sha": f.snapshot.Base}, "merge_base_commit": map[string]any{"sha": f.snapshot.MergeBase}}
			} else {
				t.Error("unexpected discovery destination", q.URL)
				w.WriteHeader(404)
				return
			}
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	g, err := newGitHubDiscoveryHTTP(alphaEnrollment(), store, githubFixtureRoute{base, server.Client().Transport, t})
	if err != nil {
		t.Fatal(err)
	}
	return g, f
}
func TestGitHubDiscoveryCapturesActualBaseAndDurableRequest(t *testing.T) {
	s, path := openFixtureStore(t)
	g, f := discoveryHTTPFixture(t, s)
	f.snapshot.HeadRepositoryID = 5555
	expected := f.snapshot
	current, err := g.Current(context.Background(), alphaScope())
	if err != nil || current.Snapshot != expected || current.Request.ID != 1 || !current.Access || !current.Open {
		t.Fatal(current, err)
	}
	original := current.Request.Snapshot
	f.mu.Lock()
	f.snapshot.Head = strings.Repeat("d", 40)
	f.snapshot.Base = strings.Repeat("e", 40)
	f.mu.Unlock()
	current, err = g.Current(context.Background(), alphaScope())
	if err != nil || current.Snapshot == original || current.Request.Snapshot != original {
		t.Fatal("movement rewrote native request capture", current, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	g.store = reopened
	current, err = g.Current(context.Background(), alphaScope())
	if err != nil || current.Request.Snapshot != original {
		t.Fatal("restart rebound snapshot", current, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writes != 0 || f.refReads != 6 {
		t.Fatal("read-only actual base resolution missing", f.writes, f.refReads)
	}
}
func TestGitHubDiscoveryRemovalRerequestAndAbsentPending(t *testing.T) {
	s, _ := openFixtureStore(t)
	g, f := discoveryHTTPFixture(t, s)
	f.events = append(f.events, nativeEvent(2, "review_request_removed", 700, controlTime.Add(2*time.Minute)), nativeEvent(3, "review_requested", OwnerID, controlTime.Add(3*time.Minute)))
	for range 2 {
		current, err := g.Current(context.Background(), alphaScope())
		if err != nil || current.Request.ID != 3 {
			t.Fatal("rerequest history not idempotent", current, err)
		}
	}
	f.mu.Lock()
	f.pending = false
	f.mu.Unlock()
	current, err := g.Current(context.Background(), alphaScope())
	if err != nil || current.Request.ID != 0 {
		t.Fatal(current, err)
	}
	batch, err := g.History(context.Background(), alphaScope())
	if err != nil || !batch.Complete || len(batch.Requests) != 2 || !batch.Requests[0].Removed || batch.Requests[1].Removed {
		t.Fatal("absent pending erased B", batch, err)
	}
	d, _ := s.Snapshot(context.Background())
	if d.PRs[alphaScope().key()].CurrentRequest != 3 {
		t.Fatal("B was cleared locally")
	}
}
func TestGitHubDiscoveryNonOwnerReplacementCannotAdmitOwnerA(t *testing.T) {
	s, _ := openFixtureStore(t)
	g, f := discoveryHTTPFixture(t, s)
	f.events = append(f.events, nativeEvent(2, "review_requested", 700, controlTime.Add(2*time.Minute)))
	current, err := g.Current(context.Background(), alphaScope())
	if err != nil || current.Request.RequesterID != 700 {
		t.Fatal(current, err)
	}
	d, _ := s.Snapshot(context.Background())
	pr := d.PRs[alphaScope().key()]
	if pr.CurrentRequest != 0 || len(pr.Requests) != 1 || !pr.Requests[0].Removed || len(d.Jobs) != 0 || len(d.Writes) != 0 {
		t.Fatal("non-owner replacement authorized A", pr)
	}
}
func TestGitHubDiscoveryOwnerCommandsUseOriginalACKOrdering(t *testing.T) {
	s, _ := openFixtureStore(t)
	g, f := discoveryHTTPFixture(t, s)
	f.events[0]["created_at"] = controlTime.Add(3 * time.Minute)
	f.comments = []map[string]any{
		{"id": 1, "user": map[string]any{"id": 700}, "body": "/sfl cancel"},
		{"id": 2, "user": map[string]any{"id": OwnerID}, "body": "/sfl config mode=codex-only", "issue_url": githubOrigin + githubIssue(alphaScope()), "created_at": controlTime.Add(2 * time.Minute), "updated_at": controlTime.Add(2 * time.Minute)},
		{"id": 3, "user": map[string]any{"id": 700}, "body": "/sfl config mode=claude-only"},
	}
	if _, err := g.Current(context.Background(), alphaScope()); err != nil {
		t.Fatal(err)
	}
	if len(writes(t, s, "ack")) != 1 {
		t.Fatal("non-owner command generated ACK")
	}
	if _, err := s.Admit(context.Background(), alphaScope(), 1, strings.Repeat("d", 40), "packet", controlTime.Add(21*time.Minute)); !errors.Is(err, ErrDeferred) {
		t.Fatal("unconfirmed ACK admitted job", err)
	}
	publisher, _ := githubHTTPFixture(t, g)
	p := Publisher{Store: s, Remote: publisher}
	if err := p.Dispatch(context.Background(), writes(t, s, "ack")[0].ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.Admit(context.Background(), alphaScope(), 1, strings.Repeat("d", 40), "packet", controlTime.Add(21*time.Minute))
	if err != nil || job.ConfigVersion != 1 || job.Config.Mode != Adversarial {
		t.Fatal("earlier request used later ACK", job, err)
	}
	f.mu.Lock()
	f.events = append(f.events, nativeEvent(4, "review_requested", OwnerID, controlTime.Add(21*time.Minute)))
	f.through = controlTime.Add(22 * time.Minute)
	f.mu.Unlock()
	if _, err = g.Current(context.Background(), alphaScope()); err != nil {
		t.Fatal(err)
	}
	job, err = s.Admit(context.Background(), alphaScope(), 4, strings.Repeat("d", 40), "packet", controlTime.Add(22*time.Minute))
	if err != nil || job.ConfigVersion != 2 || job.Config.Mode != CodexOnly {
		t.Fatal("later request missed confirmed ACK", job, err)
	}
}
func TestGitHubDiscoveryFencesNewCancellationBeforePublisherPOST(t *testing.T) {
	s, out, _ := completeFixture(t)
	g, f := discoveryHTTPFixture(t, s)
	f.comments = []map[string]any{{"id": 5, "user": map[string]any{"id": OwnerID}, "body": "/sfl cancel", "issue_url": githubOrigin + githubIssue(alphaScope()), "created_at": controlTime.Add(5 * time.Minute), "updated_at": controlTime.Add(5 * time.Minute)}}
	publisher, pub := githubHTTPFixture(t, g)
	p := Publisher{Store: s, Remote: publisher}
	w, err := p.Prepare(context.Background(), out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Dispatch(context.Background(), w.ID); !errors.Is(err, ErrFenced) {
		t.Fatal("fresh owner cancel missed local fence", err)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.posts != 0 {
		t.Fatal("cancelled review published")
	}
}
func TestGitHubDiscoveryAWriteCannotFulfillDisappearingB(t *testing.T) {
	s, out, _ := completeFixture(t)
	g, f := discoveryHTTPFixture(t, s)
	publisher, pub := githubHTTPFixture(t, g)
	pub.mutate = func(pub *githubFixture) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, nativeEvent(2, "review_requested", OwnerID, controlTime.Add(21*time.Minute)))
		pub.review["submitted_at"] = controlTime.Add(22 * time.Minute)
		f.through = controlTime.Add(23 * time.Minute)
		f.pending = false
	}
	p := Publisher{Store: s, Remote: publisher}
	w, err := p.Prepare(context.Background(), out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Dispatch(context.Background(), w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("A fulfilled B", err)
	}
	d, _ := s.Snapshot(context.Background())
	pr := d.PRs[alphaScope().key()]
	if !pr.Paused || pr.CurrentRequest != 2 || d.Writes[w.ID].State != "superseded" {
		t.Fatal("B not retained unresolved", pr, d.Writes[w.ID])
	}
}
func TestGitHubDiscoveryRejectsAmbiguousOrPartialProvenance(t *testing.T) {
	for name, mutate := range map[string]func(*discoveryFixture){
		"same timestamp": func(f *discoveryFixture) {
			f.events = append(f.events, nativeEvent(2, "review_requested", OwnerID, controlTime.Add(time.Minute)))
		},
		"missing requester": func(f *discoveryFixture) { delete(f.events[0], "review_requester") },
		"actor mismatch":    func(f *discoveryFixture) { f.events[0]["review_requester"] = map[string]any{"id": 700} },
		"foreign URL":       func(f *discoveryFixture) { f.events[0]["url"] = "https://foreign.invalid/event/1" },
		"future event":      func(f *discoveryFixture) { f.events[0]["created_at"] = f.through.Add(time.Second) },
		"foreign next": func(f *discoveryFixture) {
			f.foreignLink = true
			f.events = append(f.events, nativeEvent(2, "review_request_removed", OwnerID, controlTime.Add(2*time.Minute)), nativeEvent(3, "review_requested", OwnerID, controlTime.Add(3*time.Minute)))
		},
		"partial second page": func(f *discoveryFixture) {
			f.failSecondPage = true
			f.events = append(f.events, nativeEvent(2, "review_request_removed", OwnerID, controlTime.Add(2*time.Minute)), nativeEvent(3, "review_requested", OwnerID, controlTime.Add(3*time.Minute)))
		},
		"unknown pending": func(f *discoveryFixture) { f.events = []map[string]any{} },
		"duplicate event": func(f *discoveryFixture) { f.events = append(f.events, f.events[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := openFixtureStore(t)
			g, f := discoveryHTTPFixture(t, s)
			mutate(f)
			if _, err := g.Current(context.Background(), alphaScope()); err == nil {
				t.Fatal("ambiguous discovery accepted")
			}
			d, _ := s.Snapshot(context.Background())
			if len(d.PRs) != 0 || len(d.Jobs) != 0 || len(d.Writes) != 0 {
				t.Fatal("partial discovery advanced durable state")
			}
		})
	}
}
func TestGitHubDiscoveryChangedRevisionDuringReadDefers(t *testing.T) {
	for _, field := range []string{"head", "base", "fork"} {
		t.Run(field, func(t *testing.T) {
			s, _ := openFixtureStore(t)
			g, f := discoveryHTTPFixture(t, s)
			f.alter = func(f *discoveryFixture, q *http.Request) {
				if q.URL.Path == githubPR(alphaScope()) && f.prReads == 1 {
					switch field {
					case "head":
						f.snapshot.Head = strings.Repeat("d", 40)
					case "base":
						f.snapshot.Base = strings.Repeat("e", 40)
					case "fork":
						f.snapshot.HeadRepositoryID++
					}
				}
			}
			if _, err := g.Current(context.Background(), alphaScope()); !errors.Is(err, ErrDeferred) {
				t.Fatal("changed capture accepted", err)
			}
			d, _ := s.Snapshot(context.Background())
			if len(d.PRs) != 0 {
				t.Fatal("unstable capture persisted")
			}
		})
	}
}
func TestGitHubDiscoveryLostKnownEventAndWrongScopeFailClosed(t *testing.T) {
	s, _ := openFixtureStore(t)
	g, f := discoveryHTTPFixture(t, s)
	if _, err := g.Current(context.Background(), alphaScope()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = []map[string]any{}
	f.pending = false
	before := f.calls
	f.mu.Unlock()
	if _, err := g.Current(context.Background(), Scope{RepositoryID + 1, 9001, 1}); !errors.Is(err, ErrIgnored) {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.calls != before {
		t.Fatal("wrong scope made HTTP request")
	}
	f.mu.Unlock()
	if _, err := g.History(context.Background(), alphaScope()); err == nil {
		t.Fatal("missing known provenance accepted")
	}
}
func TestGitHubDiscoveryReadTransportRejectsWrites(t *testing.T) {
	calls := 0
	transport := githubReadTransport{githubResponseTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unused") })}
	q, _ := http.NewRequest("POST", githubOrigin+githubRepoPath, nil)
	if _, err := transport.RoundTrip(q); !errors.Is(err, ErrIgnored) || calls != 0 {
		t.Fatal("read credential used for write", calls, err)
	}
	for _, ref := range []string{"../main", "main/../other", "main?x=1", "main#x", "https://evil", "main//other", ".hidden", "main.lock"} {
		if githubBranch(ref) {
			t.Fatal("ambiguous ref accepted", ref)
		}
	}
}

func TestGitHubDiscoveryThrottledPostcheckRetainsPendingReview(t *testing.T) {
	for _, kind := range []string{"primary", "secondary"} {
		t.Run(kind, func(t *testing.T) {
			s, out, _ := completeFixture(t)
			g, f := discoveryHTTPFixture(t, s)
			publisher, pub := githubHTTPFixture(t, g)
			pub.mutate = func(*githubFixture) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.failureStatus = 403
				f.failurePath = githubRepoPath
				f.limitKind = kind
			}
			p := Publisher{Store: s, Remote: publisher}
			w, err := p.Prepare(context.Background(), out, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = p.Dispatch(context.Background(), w.ID)
			var status *GitHubHTTPError
			if !errors.As(err, &status) || status.Status != 403 || status.RetryAfter != 37*time.Second || strings.Contains(err.Error(), "private") {
				t.Fatal("throttle was misclassified or lost retry timing", err)
			}
			d, _ := s.Snapshot(context.Background())
			pr := d.PRs[w.Scope.key()]
			if d.Writes[w.ID].State != "postcheck_pending" || pr.Paused {
				t.Fatal("transient throttle superseded review", d.Writes[w.ID].State, pr.Paused)
			}
			f.mu.Lock()
			f.failureStatus = 0
			f.mu.Unlock()
			if err = p.Reconcile(context.Background(), w.ID); err != nil {
				t.Fatal("throttle recovery failed", err)
			}
			d, _ = s.Snapshot(context.Background())
			if d.Writes[w.ID].State != "confirmed" {
				t.Fatal("review did not recover")
			}
			pub.mu.Lock()
			defer pub.mu.Unlock()
			if pub.posts != 1 || pub.puts != 0 {
				t.Fatal("recovery duplicated or annotated review", pub.posts, pub.puts)
			}
		})
	}
}
func TestGitHubDiscoveryOnlyScope404EstablishesAccessLoss(t *testing.T) {
	for _, path := range []string{githubRepoPath, githubPR(alphaScope()), githubRepoPath + "/git/ref/heads/main", githubPR(alphaScope()) + "/requested_reviewers"} {
		for _, status := range []int{403, 404} {
			t.Run(path+strconv.Itoa(status), func(t *testing.T) {
				s, _ := openFixtureStore(t)
				g, f := discoveryHTTPFixture(t, s)
				f.failurePath = path
				f.failureStatus = status
				state, err := g.Current(context.Background(), alphaScope())
				unavailable := status == 404 && (path == githubRepoPath || path == githubPR(alphaScope()))
				if unavailable {
					if err != nil || state.Access {
						t.Fatal("missing scope not conservative access loss", state, err)
					}
				} else {
					var httpErr *GitHubHTTPError
					if !errors.As(err, &httpErr) || httpErr.Status != status {
						t.Fatal("metadata/throttle failure became access loss", state, err)
					}
				}
			})
		}
	}
}
func TestGitHubDiscoveryLateRequestBehindCutoffCannotAdmit(t *testing.T) {
	for _, minutes := range []int{2, 20} {
		t.Run(strconv.Itoa(minutes), func(t *testing.T) {
			s, _ := openFixtureStore(t)
			g, f := discoveryHTTPFixture(t, s)
			if _, err := g.Current(context.Background(), alphaScope()); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Snapshot(context.Background())
			f.mu.Lock()
			f.events = append(f.events, nativeEvent(2, "review_requested", OwnerID, controlTime.Add(time.Duration(minutes)*time.Minute)))
			f.through = controlTime.Add(21 * time.Minute)
			f.mu.Unlock()
			if _, err := g.Current(context.Background(), alphaScope()); !errors.Is(err, ErrDeferred) {
				t.Fatal("late request became current", err)
			}
			if batch, err := g.History(context.Background(), alphaScope()); !errors.Is(err, ErrDeferred) || batch.Complete {
				t.Fatal("contradicted cutoff claimed complete", batch, err)
			}
			after, _ := s.Snapshot(context.Background())
			if Digest(before) != Digest(after) {
				t.Fatal("late request changed durable state")
			}
			if _, err := s.Admit(context.Background(), alphaScope(), 2, strings.Repeat("d", 40), "packet", controlTime.Add(22*time.Minute)); !errors.Is(err, ErrDeferred) {
				t.Fatal("late request admitted work", err)
			}
		})
	}
}
func TestGitHubDiscoveryRetainsRemovalIdentityAcrossRestart(t *testing.T) {
	for _, mutation := range []string{"missing", "id", "time", "actor"} {
		t.Run(mutation, func(t *testing.T) {
			s, path := openFixtureStore(t)
			g, f := discoveryHTTPFixture(t, s)
			f.events = append(f.events, nativeEvent(2, "review_request_removed", OwnerID, controlTime.Add(2*time.Minute)))
			f.pending = false
			if _, err := g.Current(context.Background(), alphaScope()); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			g.store = reopened
			before, _ := reopened.Snapshot(context.Background())
			f.mu.Lock()
			f.through = controlTime.Add(21 * time.Minute)
			switch mutation {
			case "missing":
				f.events = f.events[:1]
			case "id":
				f.events[1]["id"] = int64(3)
				f.events[1]["url"] = githubOrigin + githubRepoPath + "/issues/events/3"
			case "time":
				f.events[1]["created_at"] = controlTime.Add(3 * time.Minute)
			case "actor":
				f.events[1]["actor"] = map[string]any{"id": 700}
			}
			f.mu.Unlock()
			if batch, err := g.History(context.Background(), alphaScope()); !errors.Is(err, ErrDeferred) || batch.Complete {
				t.Fatal("lost/mutated removal history trusted", batch, err)
			}
			if _, err := g.Current(context.Background(), alphaScope()); !errors.Is(err, ErrDeferred) {
				t.Fatal("lost/mutated removal advanced cutoff", err)
			}
			after, _ := reopened.Snapshot(context.Background())
			if Digest(before) != Digest(after) {
				t.Fatal("removal contradiction changed durable state")
			}
		})
	}
}
