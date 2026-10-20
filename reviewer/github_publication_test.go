package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type githubFixture struct {
	wireBytes              int
	mu                     sync.Mutex
	posts, puts, calls     int
	review                 map[string]any
	comments               []map[string]any
	ack                    map[string]any
	lost, hidden, redirect bool
	mutate                 func(*githubFixture)
}
type githubFixtureRoute struct {
	base      *url.URL
	transport http.RoundTripper
	t         *testing.T
}

func (r githubFixtureRoute) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.URL.Scheme != "https" || q.URL.Host != "api.github.com" || q.Header.Get("Authorization") != "" || q.GetBody != nil {
		r.t.Error("untrusted destination, credential inheritance or replayable write", q.URL)
	}
	if q.Header.Get("X-GitHub-Api-Version") != "2026-03-10" || q.Header.Get("Accept") != "application/vnd.github+json" {
		r.t.Error("missing API version/media type")
	}
	clone := q.Clone(q.Context())
	u := *q.URL
	u.Scheme = r.base.Scheme
	u.Host = r.base.Host
	clone.URL = &u
	clone.Host = r.base.Host
	clone.Header.Set("Authorization", "Bearer synthetic-publisher-only")
	return r.transport.RoundTrip(clone)
}
func githubHTTPFixture(t *testing.T, reader GitHubPublicationReader) (*GitHubPublicationHTTP, *githubFixture) {
	t.Helper()
	f := &githubFixture{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if q.Header.Get("Authorization") != "Bearer synthetic-publisher-only" {
			t.Error("missing synthetic publisher credential")
		}
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		path := q.URL.Path
		pr := githubPR(alphaScope())
		issue := githubIssue(alphaScope())
		switch {
		case path == githubRepoPath:
			write(map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop"})
		case path == pr+"/reviews" && q.Method == "POST":
			f.posts++
			if f.redirect {
				w.Header().Set("Location", githubOrigin+pr+"/review-again")
				w.WriteHeader(307)
				return
			}
			var v struct {
				Event, Body string
				CommitID    string `json:"commit_id"`
				Comments    []struct {
					Path, Side, Body string
					Line             int
				}
			}
			raw, readErr := io.ReadAll(q.Body)
			f.wireBytes = len(raw)
			if readErr != nil || json.Unmarshal(raw, &v) != nil || v.Event != "COMMENT" || v.CommitID == "" {
				t.Error("write did not pin COMMENT and commit")
			}
			f.review = map[string]any{"id": int64(80), "user": map[string]any{"id": int64(9003)}, "body": v.Body, "state": "COMMENTED", "commit_id": v.CommitID, "pull_request_url": githubOrigin + pr, "submitted_at": controlTime.Add(6 * time.Minute)}
			f.comments = nil
			for i, c := range v.Comments {
				f.comments = append(f.comments, map[string]any{"id": 100 + i, "user": map[string]any{"id": int64(9003)}, "body": c.Body, "path": c.Path, "side": c.Side, "original_line": c.Line, "line": nil, "original_commit_id": v.CommitID, "pull_request_review_id": 80})
			}
			if f.mutate != nil {
				f.mutate(f)
			}
			if f.lost {
				w.WriteHeader(503)
				write(map[string]any{"message": "sensitive submitted content"})
				return
			}
			write(f.review)
		case path == pr+"/reviews" && q.Method == "GET":
			if f.hidden || f.review == nil {
				write([]any{})
				return
			}
			write([]any{f.review})
		case path == pr+"/reviews/80/comments":
			size, _ := strconv.Atoi(q.URL.Query().Get("per_page"))
			page, _ := strconv.Atoi(q.URL.Query().Get("page"))
			if size <= 0 || page <= 0 {
				t.Error("missing explicit page bounds")
				return
			}
			start := min((page-1)*size, len(f.comments))
			end := min(start+size, len(f.comments))
			if end < len(f.comments) {
				alias := "/repositories/" + strconv.FormatInt(RepositoryID, 10) + strings.TrimPrefix(path, githubRepoPath)
				w.Header().Set("Link", "<"+githubOrigin+alias+"?per_page="+strconv.Itoa(size)+"&page="+strconv.Itoa(page+1)+">; rel=\"next\"")
			}
			items := f.comments[start:end]
			if items == nil {
				items = []map[string]any{}
			}
			write(items)
		case path == pr+"/reviews/80" && q.Method == "GET":
			write(f.review)
		case path == pr+"/reviews/80" && q.Method == "PUT":
			f.puts++
			var v struct{ Body string }
			if json.NewDecoder(q.Body).Decode(&v) != nil {
				t.Error("invalid update")
			}
			f.review["body"] = v.Body
			write(f.review)
		case path == issue+"/comments" && q.Method == "POST":
			f.posts++
			var v struct{ Body string }
			if json.NewDecoder(q.Body).Decode(&v) != nil {
				t.Error("invalid ACK")
			}
			f.ack = map[string]any{"id": 90, "user": map[string]any{"id": 9003}, "body": v.Body, "issue_url": githubOrigin + issue, "created_at": controlTime.Add(5 * time.Minute)}
			w.WriteHeader(201)
			write(f.ack)
		case path == issue+"/comments" && q.Method == "GET":
			if f.ack == nil {
				write([]any{})
			} else {
				write([]any{f.ack})
			}
		default:
			t.Errorf("unexpected HTTP operation %s %s", q.Method, q.URL)
			w.WriteHeader(404)
			write(map[string]any{})
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	g, err := NewGitHubPublicationHTTP(alphaEnrollment(), reader, githubFixtureRoute{base, server.Client().Transport, t})
	if err != nil {
		t.Fatal(err)
	}
	return g, f
}
func httpReviewIntent() WriteIntent {
	return newIntent("review", alphaScope(), 1, 1, testSnapshot(), strings.Repeat("d", 64), Document{Summary: "Advisory snapshot review", Inline: []InlineComment{{"source.go", 1, "first finding"}, {"other.go", 2, "second finding"}, {"third.go", 3, "third finding"}, {"fourth.go", 4, "fourth finding"}, {"fifth.go", 5, "fifth finding"}}}, controlTime)
}
func TestGitHubPublicationActualHTTPRoundTripAndInlinePagination(t *testing.T) {
	g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	w := httpReviewIntent()
	r, err := g.Post(context.Background(), w)
	if err != nil || !validateReceipt(w, r, 9003) || r.CommitID != w.Snapshot.Head {
		t.Fatal("actual document/commit not confirmed", r, err)
	}
	rr, complete, err := g.Scan(context.Background(), alphaScope(), "review")
	if err != nil || !complete || len(rr) != 1 || rr[0] != r {
		t.Fatal("paginated document differs", rr, complete, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("duplicate write", f.posts)
	}
}
func TestGitHubPublicationHashIgnoresCommentCreationOrder(t *testing.T) {
	g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	f.mutate = func(f *githubFixture) { f.comments[0]["id"] = 200; f.comments[1]["id"] = 100 }
	w := httpReviewIntent()
	r, err := g.Post(context.Background(), w)
	if err != nil || r.BodyHash != w.BodyHash {
		t.Fatal("creation order changed document identity", r, err)
	}
}
func TestGitHubPublicationRejectsRemoteDocumentAndIdentityMutations(t *testing.T) {
	for name, mutate := range map[string]func(*githubFixture){
		"wrong-author": func(f *githubFixture) { f.review["user"] = map[string]any{"id": 666} },
		"wrong-commit": func(f *githubFixture) { f.review["commit_id"] = strings.Repeat("f", 40) },
		"wrong-consistent-commit": func(f *githubFixture) {
			f.review["commit_id"] = strings.Repeat("f", 40)
			for _, c := range f.comments {
				c["original_commit_id"] = strings.Repeat("f", 40)
			}
		},
		"wrong-repo": func(f *githubFixture) {
			f.review["pull_request_url"] = "https://api.github.com/repos/other/repo/pulls/1"
		},
		"altered-inline":      func(f *githubFixture) { f.comments[0]["body"] = "altered" },
		"missing-inline":      func(f *githubFixture) { f.comments = f.comments[:1] },
		"wrong-inline-commit": func(f *githubFixture) { f.comments[0]["original_commit_id"] = strings.Repeat("f", 40) },
		"wrong-review-id":     func(f *githubFixture) { f.comments[0]["pull_request_review_id"] = 666 },
		"duplicate-inline-id": func(f *githubFixture) { f.comments[1]["id"] = f.comments[0]["id"] },
		"wrong-side":          func(f *githubFixture) { f.comments[0]["side"] = "LEFT" },
		"reply-added":         func(f *githubFixture) { f.comments[0]["in_reply_to_id"] = 777 },
		"approval":            func(f *githubFixture) { f.review["state"] = "APPROVED" },
	} {
		t.Run(name, func(t *testing.T) {
			g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
			f.mutate = mutate
			if _, err := g.Post(context.Background(), httpReviewIntent()); err == nil {
				t.Fatal("mutated receipt confirmed")
			}
		})
	}
}
func TestGitHubPublicationUnknownWriteReconcilesWithoutAnotherPost(t *testing.T) {
	s, out, reader := completeFixture(t)
	g, f := githubHTTPFixture(t, reader)
	f.lost = true
	p := Publisher{Store: s, Remote: g}
	w, err := p.Prepare(context.Background(), out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Dispatch(context.Background(), w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.hidden = true
	f.mu.Unlock()
	if err = p.Reconcile(context.Background(), w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("empty scan authorized retry", err)
	}
	if err = p.Dispatch(context.Background(), w.ID); !errors.Is(err, ErrDeferred) {
		t.Fatal("unknown write dispatched twice", err)
	}
	f.mu.Lock()
	f.hidden = false
	f.mu.Unlock()
	if err = p.Reconcile(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Snapshot(context.Background())
	if d.Writes[w.ID].State != "confirmed" {
		t.Fatal("lost response not recovered")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("blind retry", f.posts)
	}
}
func TestGitHubPublicationACKAndIdempotentSupersession(t *testing.T) {
	g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	w := newIntent("ack", alphaScope(), 1, 0, Snapshot{}, "", Document{Summary: "configuration version 2", Inline: []InlineComment{}}, controlTime)
	r, err := g.Post(context.Background(), w)
	if err != nil || !validateReceipt(w, r, 9003) {
		t.Fatal(r, err)
	}
	rr, complete, err := g.Scan(context.Background(), alphaScope(), "ack")
	if err != nil || !complete || len(rr) != 1 || rr[0] != r {
		t.Fatal(rr, complete, err)
	}
	if _, err = g.Post(context.Background(), httpReviewIntent()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = g.Annotate(context.Background(), alphaScope(), 80, "head moved @owner <script>"); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 1 || f.posts != 2 {
		t.Fatal("annotation appended/duplicated publication", f.puts, f.posts)
	}
	body := f.review["body"].(string)
	if strings.Contains(body, "@owner") || strings.Contains(body, "<script>") {
		t.Fatal("unsafe reason", body)
	}
}
func TestGitHubPublicationWrongScopeAndIntentMakeZeroHTTPCalls(t *testing.T) {
	g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	for _, scope := range []Scope{{RepositoryID + 1, 9001, 1}, {RepositoryID, 9002, 1}, {RepositoryID, 9001, 0}} {
		w := httpReviewIntent()
		w.Scope = scope
		if _, err := g.Post(context.Background(), w); !errors.Is(err, ErrIgnored) {
			t.Fatal(err)
		}
		if _, _, err := g.Scan(context.Background(), scope, "review"); !errors.Is(err, ErrIgnored) {
			t.Fatal(err)
		}
		if err := g.Annotate(context.Background(), scope, 80, "moved"); !errors.Is(err, ErrIgnored) {
			t.Fatal(err)
		}
		if _, err := g.Current(context.Background(), scope); !errors.Is(err, ErrIgnored) {
			t.Fatal(err)
		}
		if _, err := g.History(context.Background(), scope); !errors.Is(err, ErrIgnored) {
			t.Fatal(err)
		}
	}
	w := httpReviewIntent()
	w.Document.Inline[0].Path = "../escape"
	w.BodyHash = Digest(w.Document)
	if _, err := g.Post(context.Background(), w); !errors.Is(err, ErrIgnored) {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 0 {
		t.Fatal("invalid identity made HTTP calls", f.calls)
	}
}
func TestGitHubPublicationRedirectNeverReplaysPost(t *testing.T) {
	g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	f.redirect = true
	_, err := g.Post(context.Background(), httpReviewIntent())
	var status *GitHubHTTPError
	if !errors.As(err, &status) || status.Status != 307 {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("redirect replay", f.posts)
	}
}
func TestGitHubPaginationRejectsForeignLoopingAndAmbiguousLinks(t *testing.T) {
	path := githubPR(alphaScope()) + "/reviews"
	for _, link := range []string{
		"<https://other.example" + path + "?per_page=4&page=2>; rel=\"next\"",
		"<" + githubOrigin + "/repositories/1/pulls/1/reviews?per_page=4&page=2>; rel=\"next\"",
		"<" + githubOrigin + path + "?per_page=4&page=1>; rel=\"next\"",
		"<" + githubOrigin + path + "?per_page=4&page=2&page=3>; rel=\"next\"",
		"<" + githubOrigin + path + "?per_page=4&page=2>; rel=next",
		"<" + githubOrigin + path + "?per_page=4&page=2>; rel=\"unknown\"",
	} {
		if _, err := nextGitHubPage(http.Header{"Link": []string{link}}, path, 1); err == nil {
			t.Fatal("unsafe cutoff/link accepted", link)
		}
	}
}
func TestGitHubReceiptRequiresActualReviewedCommit(t *testing.T) {
	w := httpReviewIntent()
	r := RemoteReceipt{ID: 1, AuthorID: 9003, Marker: w.Marker, BodyHash: w.BodyHash, CreatedAt: controlTime, CommitID: strings.Repeat("f", 40)}
	if validateReceipt(w, r, 9003) {
		t.Fatal("summary hash substituted for actual reviewed commit")
	}
}

type githubResponseTransport func(*http.Request) (*http.Response, error)

func (f githubResponseTransport) RoundTrip(q *http.Request) (*http.Response, error) { return f(q) }
func TestGitHubPublicationBoundsAndSanitizesHTTPFailures(t *testing.T) {
	for name, body := range map[string]string{"duplicate": `{"id":1169772257,"id":1169772257,"full_name":"hemsoft-dev/set-it-free-loop"}`, "oversize": strings.Repeat("x", (2<<20)+1), "trailing": `{"id":1169772257,"full_name":"hemsoft-dev/set-it-free-loop"} {}`} {
		t.Run(name, func(t *testing.T) {
			g, _ := NewGitHubPublicationHTTP(alphaEnrollment(), liveFake(native(1, controlTime)), githubResponseTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			if _, _, err := g.Scan(context.Background(), alphaScope(), "review"); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
	g, _ := NewGitHubPublicationHTTP(alphaEnrollment(), liveFake(native(1, controlTime)), githubResponseTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{strconv.Itoa(30)}}, Body: io.NopCloser(strings.NewReader("sensitive submitted content"))}, nil
	}))
	_, _, err := g.Scan(context.Background(), alphaScope(), "review")
	var status *GitHubHTTPError
	if !errors.As(err, &status) || status.RetryAfter != 30*time.Second || strings.Contains(fmt.Sprint(err), "sensitive") {
		t.Fatal(err)
	}
}

func TestGitHubPublicationWrongRemoteRepositoryIDPreventsWrite(t *testing.T) {
	writes := 0
	g, _ := NewGitHubPublicationHTTP(alphaEnrollment(), liveFake(native(1, controlTime)), githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		if q.Method != "GET" {
			writes++
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":1,"full_name":"hemsoft-dev/set-it-free-loop"}`))}, nil
	}))
	if _, err := g.Post(context.Background(), httpReviewIntent()); !errors.Is(err, ErrIgnored) || writes != 0 {
		t.Fatal("wrong repository identity wrote", writes, err)
	}
}
func TestGitHubPublicationPartialInlinePaginationCannotConfirm(t *testing.T) {
	g, _ := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	if _, err := g.Post(context.Background(), httpReviewIntent()); err != nil {
		t.Fatal(err)
	}
	transport := g.client.Transport
	g.client.Transport = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		if strings.HasSuffix(q.URL.Path, "/comments") && q.URL.Query().Get("page") == "2" {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"unavailable"}`))}, nil
		}
		return transport.RoundTrip(q)
	})
	receipts, complete, err := g.Scan(context.Background(), alphaScope(), "review")
	if err == nil || complete || len(receipts) != 0 {
		t.Fatal("partial document confirmed", receipts, complete, err)
	}
}

func TestGitHubPublicationAnnotatedLargeReviewDoesNotPoisonScan(t *testing.T) {
	g, _ := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	w := newIntent("review", alphaScope(), 1, 1, testSnapshot(), strings.Repeat("d", 64), Document{Summary: strings.Repeat("s", 64<<10), Inline: []InlineComment{}}, controlTime)
	if _, err := g.Post(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := g.Annotate(context.Background(), alphaScope(), 80, strings.Repeat("r", 1024)); err != nil {
		t.Fatal(err)
	}
	receipts, complete, err := g.Scan(context.Background(), alphaScope(), "review")
	if err != nil || !complete || len(receipts) != 1 {
		t.Fatal("superseded review poisoned later reconciliation", receipts, complete, err)
	}
}

func TestGitHubPublicationMaximumEscapedDocumentFitsWireLimit(t *testing.T) {
	g, f := githubHTTPFixture(t, liveFake(native(1, controlTime)))
	doc := Document{Summary: strings.Repeat("<", 64<<10), Inline: []InlineComment{}}
	for i := 0; i < 40; i++ {
		doc.Inline = append(doc.Inline, InlineComment{fmt.Sprintf("source-%02d.go", i), i + 1, strings.Repeat("<", 16<<10)})
	}
	w := newIntent("review", alphaScope(), 1, 1, testSnapshot(), strings.Repeat("d", 64), doc, controlTime)
	if !validHTTPIntent(w) {
		t.Fatal("maximum document unexpectedly rejected")
	}
	receipt, err := g.Post(context.Background(), w)
	if err != nil || !validateReceipt(w, receipt, 9003) {
		t.Fatal("accepted document became unknown before write", receipt, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.wireBytes <= 1<<20 {
		t.Fatal("fixture did not exercise old wire cap", f.posts, f.wireBytes)
	}
}
func TestGitHubPublicationLargeRawCollectionsFitReadPages(t *testing.T) {
	for _, kind := range []string{"review", "ack"} {
		t.Run(kind, func(t *testing.T) {
			pr, issue := githubPR(alphaScope()), githubIssue(alphaScope())
			collection := pr + "/reviews"
			if kind == "ack" {
				collection = issue + "/comments"
			}
			items := []map[string]any{}
			for i := 0; i < 40; i++ {
				marker := fmt.Sprintf("<!-- sfl-alpha:%032x -->", i+1)
				body := strings.Repeat("<", 64<<10) + "\n" + marker
				if kind == "review" {
					items = append(items, map[string]any{"id": i + 1, "user": map[string]any{"id": 9003}, "body": body, "state": "COMMENTED", "commit_id": testSnapshot().Head, "pull_request_url": githubOrigin + pr, "submitted_at": controlTime})
				} else {
					items = append(items, map[string]any{"id": i + 1, "user": map[string]any{"id": 1}, "body": body, "issue_url": githubOrigin + issue, "created_at": controlTime})
				}
			}
			if kind == "ack" {
				items = append(items, map[string]any{"id": 41, "user": map[string]any{"id": 9003}, "body": "accepted ACK\n<!-- sfl-alpha:ffffffffffffffffffffffffffffffff -->", "issue_url": githubOrigin + issue, "created_at": controlTime})
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var out any
				switch {
				case q.URL.Path == githubRepoPath:
					out = map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop"}
				case q.URL.Path == collection:
					size, _ := strconv.Atoi(q.URL.Query().Get("per_page"))
					page, _ := strconv.Atoi(q.URL.Query().Get("page"))
					if size <= 0 || page <= 0 {
						t.Error("missing page controls")
						w.WriteHeader(400)
						return
					}
					start := min((page-1)*size, len(items))
					end := min(start+size, len(items))
					out = items[start:end]
					if end < len(items) {
						alias := "/repositories/" + strconv.FormatInt(RepositoryID, 10) + strings.TrimPrefix(collection, githubRepoPath)
						w.Header().Set("Link", "<"+githubOrigin+alias+"?per_page="+strconv.Itoa(size)+"&page="+strconv.Itoa(page+1)+">; rel=\"next\"")
					}
				case strings.HasPrefix(q.URL.Path, pr+"/reviews/") && strings.HasSuffix(q.URL.Path, "/comments"):
					out = []any{}
				default:
					t.Error("unexpected collection route", q.URL)
					w.WriteHeader(404)
					return
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			g, err := NewGitHubPublicationHTTP(alphaEnrollment(), liveFake(native(1, controlTime)), githubFixtureRoute{base, server.Client().Transport, t})
			if err != nil {
				t.Fatal(err)
			}
			receipts, complete, err := g.Scan(context.Background(), alphaScope(), kind)
			want := 40
			if kind == "ack" {
				want = 1
			}
			if err != nil || !complete || len(receipts) != want {
				t.Fatal("valid large collection could not reconcile", len(receipts), complete, err)
			}
		})
	}
}

func TestGitHubPublicationRepositoryIDPaginationAlias(t *testing.T) {
	collection := githubIssue(alphaScope()) + "/comments"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if q.URL.Path == githubRepoPath {
			json.NewEncoder(w).Encode(map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop"})
			return
		}
		if q.URL.Path != collection {
			t.Error("wrong collection", q.URL)
			w.WriteHeader(404)
			return
		}
		page, _ := strconv.Atoi(q.URL.Query().Get("page"))
		size := q.URL.Query().Get("per_page")
		if page == 1 {
			alias := "/repositories/" + strconv.FormatInt(RepositoryID, 10) + strings.TrimPrefix(collection, githubRepoPath)
			w.Header().Set("Link", "<"+githubOrigin+alias+"?per_page="+size+"&page=2>; rel=\"next\"")
		}
		if page < 1 || page > 2 {
			t.Error("pagination did not terminate", q.URL)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"id": page, "user": map[string]any{"id": 1}, "body": "unrelated"}})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	g, err := NewGitHubPublicationHTTP(alphaEnrollment(), liveFake(native(1, controlTime)), githubFixtureRoute{base, server.Client().Transport, t})
	if err != nil {
		t.Fatal(err)
	}
	receipts, complete, err := g.Scan(context.Background(), alphaScope(), "ack")
	if err != nil || !complete || len(receipts) != 0 {
		t.Fatal("documented repository-ID next link rejected", receipts, complete, err)
	}
}
