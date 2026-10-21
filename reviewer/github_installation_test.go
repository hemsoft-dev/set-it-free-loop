package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type installationFixture struct {
	mu                 sync.Mutex
	calls, mints, uses int
	role               GitHubCredentialRole
	mutate             func(string, map[string]any)
	redirect, loseMint bool
	transport          http.RoundTripper
}

type installationRoute struct {
	base  *url.URL
	inner http.RoundTripper
	t     *testing.T
}

func (r installationRoute) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.URL.Scheme != "https" || q.URL.Host != "api.github.com" || q.GetBody != nil {
		r.t.Error("wrong destination or replayable token/write request")
	}
	if q.Header.Get("Accept") != "application/vnd.github+json" || q.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
		r.t.Error("missing pinned API headers")
	}
	clone := q.Clone(q.Context())
	u := *q.URL
	u.Scheme, u.Host = r.base.Scheme, r.base.Host
	clone.URL, clone.Host = &u, r.base.Host
	return r.inner.RoundTrip(clone)
}

func installationHTTPFixture(t *testing.T, role GitHubCredentialRole, appJWT ...string) (*installationFixture, GitHubInstallationOptions) {
	t.Helper()
	f := &installationFixture{role: role}
	jwt := "synthetic-app-jwt"
	if len(appJWT) == 1 {
		jwt = appJWT[0]
	} else if len(appJWT) > 1 {
		t.Fatal("one explicit fixture App JWT required")
	}
	repo := func() map[string]any {
		return map[string]any{"id": RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop", "owner": map[string]any{"id": int64(71), "login": "hemsoft-dev", "type": "Organization"}}
	}
	installation := func() map[string]any {
		return map[string]any{"id": alphaEnrollment().InstallationID, "app_id": alphaEnrollment().AppID, "target_id": int64(71), "target_type": "Organization", "account": map[string]any{"id": int64(71), "login": "hemsoft-dev", "type": "Organization"}, "repository_selection": "selected", "suspended_at": nil, "permissions": map[string]string{"metadata": "read", "contents": "read", "pull_requests": "write"}}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		p := q.URL.Path
		appRequest := p == "/app" || p == githubRepoPath+"/installation" || strings.HasPrefix(p, "/app/installations/")
		expected := "Bearer synthetic-installation-" + string(role)
		if appRequest {
			expected = "Bearer " + jwt
		}
		if q.Header.Get("Authorization") != expected {
			t.Error("credential crossed app/installation role boundary", p)
		}
		status := 200
		var doc map[string]any
		switch {
		case p == "/app":
			doc = map[string]any{"id": alphaEnrollment().AppID, "slug": "sfl-test"}
		case p == githubRepoPath+"/installation" || (strings.HasPrefix(p, "/app/installations/") && q.Method == "GET"):
			doc = installation()
		case strings.HasSuffix(p, "/access_tokens"):
			f.mints++
			var request struct {
				RepositoryIDs []int64 `json:"repository_ids"`
				Permissions   map[string]string
			}
			if q.Method != "POST" || json.NewDecoder(q.Body).Decode(&request) != nil || len(request.RepositoryIDs) != 1 || request.RepositoryIDs[0] != RepositoryID {
				t.Error("token request omitted exact repository IDs")
			}
			raw, _ := json.Marshal(request.Permissions)
			want, _ := json.Marshal(githubPermissions(role))
			if string(raw) != string(want) {
				t.Error("token request widened role permissions", string(raw))
			}
			status = 201
			doc = map[string]any{"token": "synthetic-installation-" + string(role), "expires_at": controlTime.Add(time.Hour), "permissions": githubPermissions(role), "repository_selection": "selected", "repositories": []any{repo()}}
			if f.loseMint {
				w.WriteHeader(503)
				io.WriteString(w, `{"message":"synthetic-app-jwt and synthetic-installation-publisher"}`)
				return
			}
		case p == "/installation/repositories":
			if q.URL.RawQuery != "per_page=100&page=1" {
				t.Error("missing explicit collection bound")
			}
			doc = map[string]any{"total_count": 1, "repositories": []any{repo()}}
		case p == "/users/sfl-test[bot]":
			doc = map[string]any{"id": alphaEnrollment().BotID, "login": "sfl-test[bot]", "type": "Bot"}
		case p == githubRepoPath:
			doc = repo()
		case p == githubIssue(alphaScope())+"/comments" && q.Method == "POST":
			f.uses++
			var body struct{ Body string }
			if json.NewDecoder(q.Body).Decode(&body) != nil {
				t.Error("invalid acknowledgment body")
			}
			status = 201
			doc = map[string]any{"id": 91, "user": map[string]any{"id": alphaEnrollment().BotID}, "body": body.Body, "issue_url": githubOrigin + githubIssue(alphaScope()), "created_at": controlTime.Add(5 * time.Minute)}
		default:
			f.uses++
			doc = map[string]any{"ok": true}
		}
		if f.redirect {
			w.Header().Set("Location", "https://other.example/credential-trap")
			w.WriteHeader(307)
			return
		}
		if f.mutate != nil {
			f.mutate(p, doc)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	f.transport = installationRoute{base, server.Client().Transport, t}
	return f, GitHubInstallationOptions{alphaEnrollment(), role, []byte(jwt), f.transport, func() time.Time { return controlTime }}
}

func TestGitHubInstallationComposesWithPublisherAcknowledgment(t *testing.T) {
	f, o := installationHTTPFixture(t, GitHubPublisher)
	transport, err := AcquireGitHubInstallation(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	g, err := NewGitHubPublicationHTTP(o.Enrollment, liveFake(native(1, controlTime)), transport)
	if err != nil {
		t.Fatal(err)
	}
	w := newIntent("ack", alphaScope(), 1, 0, Snapshot{}, "", Document{Summary: "configuration version 2", Inline: []InlineComment{}}, controlTime)
	r, err := g.Post(context.Background(), w)
	if err != nil || !validateReceipt(w, r, o.Enrollment.BotID) || f.uses != 1 {
		t.Fatal("qualified publisher ACK failed", r, err, f.uses)
	}
}

func TestGitHubInstallationRejectsMalformedWireResponses(t *testing.T) {
	for name, raw := range map[string]string{"null": "null", "duplicate": `{"id":9002,"id":9002,"slug":"sfl-test"}`, "trailing": `{"id":9002,"slug":"sfl-test"} {}`, "oversize": strings.Repeat(" ", 1<<20) + `{}`} {
		t.Run(name, func(t *testing.T) {
			f, o := installationHTTPFixture(t, GitHubReader)
			inner := o.Transport
			o.Transport = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
				r, err := inner.RoundTrip(q)
				if err != nil {
					return r, err
				}
				r.Body.Close()
				r.Body = io.NopCloser(strings.NewReader(raw))
				r.ContentLength = int64(len(raw))
				return r, nil
			})
			if tr, err := AcquireGitHubInstallation(context.Background(), o); tr != nil || err != ErrGitHubInstallation || f.mints != 0 {
				t.Fatal("malformed response accepted or token minted", tr, err, f.mints)
			}
		})
	}
	t.Run("unconsumed pagination", func(t *testing.T) {
		f, o := installationHTTPFixture(t, GitHubReader)
		inner := o.Transport
		o.Transport = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
			r, err := inner.RoundTrip(q)
			if err == nil {
				r.Header.Set("Link", `<https://api.github.com/installation/repositories?page=2>; rel="next"`)
			}
			return r, err
		})
		if tr, err := AcquireGitHubInstallation(context.Background(), o); tr != nil || err != ErrGitHubInstallation || f.mints != 0 {
			t.Fatal("ignored pagination accepted", tr, err)
		}
	})
}

func installationQuery(t *testing.T, method, p string) *http.Request {
	t.Helper()
	q, err := http.NewRequest(method, githubOrigin+p, nil)
	if err != nil {
		t.Fatal(err)
	}
	q.Header.Set("Accept", "application/vnd.github+json")
	q.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	return q
}

func TestGitHubInstallationActualTLSAndRoleBoundary(t *testing.T) {
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		t.Run(string(role), func(t *testing.T) {
			f, o := installationHTTPFixture(t, role)
			transport, err := AcquireGitHubInstallation(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Close()
			if f.mints != 1 || f.calls != 7 || transport.Receipt().Enrollment != o.Enrollment || transport.Receipt().Role != role {
				t.Fatal("incomplete handshake", f.mints, f.calls, transport.Receipt())
			}
			for _, methodPath := range [][2]string{{"GET", "/pulls?state=open"}, {"POST", "/pulls/1/reviews"}, {"POST", "/issues/1/comments"}, {"PUT", "/pulls/1/reviews/2"}} {
				q := installationQuery(t, methodPath[0], githubRepoPath+methodPath[1])
				if q.Method == "POST" {
					q.Body = io.NopCloser(strings.NewReader(`{"body":"synthetic acknowledgment"}`))
				}
				r, err := transport.RoundTrip(q)
				if role == GitHubReader && q.Method != "GET" {
					if !errors.Is(err, ErrGitHubInstallation) {
						t.Fatal("read credential permitted write")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				r.Body.Close()
				if q.Header.Get("Authorization") != "" || r.Request != q {
					t.Fatal("bearer retained in caller request/response")
				}
			}
			for _, value := range []any{transport, *transport} {
				s := fmt.Sprintf("%v %+v %#v", value, value, value)
				raw, _ := json.Marshal(value)
				if strings.Contains(s, "synthetic") || strings.Contains(string(raw), "synthetic") {
					t.Fatal("credential formatting disclosure")
				}
			}
			copy := *transport
			copy.Close()
			if len(transport.state.token) != 0 {
				t.Fatal("copied handle did not share close")
			}
			if _, err := transport.RoundTrip(installationQuery(t, "GET", githubRepoPath)); !errors.Is(err, ErrGitHubInstallation) {
				t.Fatal("closed credential reused")
			}
		})
	}
}

func TestGitHubInstallationRejectsUnattestedIdentityAndScope(t *testing.T) {
	mutations := map[string]func(string, map[string]any){
		"wrong app": func(p string, d map[string]any) {
			if p == "/app" {
				d["id"] = int64(1)
			}
		},
		"all repositories": func(p string, d map[string]any) {
			if p == githubRepoPath+"/installation" {
				d["repository_selection"] = "all"
			}
		},
		"wrong installation": func(p string, d map[string]any) {
			if p == githubRepoPath+"/installation" {
				d["id"] = int64(1)
			}
		},
		"wrong target": func(p string, d map[string]any) {
			if p == githubRepoPath+"/installation" {
				d["target_id"] = int64(2)
			}
		},
		"suspended": func(p string, d map[string]any) {
			if p == githubRepoPath+"/installation" {
				d["suspended_at"] = controlTime
			}
		},
		"missing suspension": func(p string, d map[string]any) {
			if p == githubRepoPath+"/installation" {
				delete(d, "suspended_at")
			}
		},
		"extra app permissions": func(p string, d map[string]any) {
			if p == githubRepoPath+"/installation" {
				d["permissions"].(map[string]string)["administration"] = "write"
			}
		},
		"extra token permissions": func(p string, d map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				d["permissions"].(map[string]string)["contents"] = "write"
			}
		},
		"wrong token selection": func(p string, d map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				d["repository_selection"] = "all"
			}
		},
		"missing token permissions": func(p string, d map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				delete(d, "permissions")
			}
		},
		"expired": func(p string, d map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				d["expires_at"] = controlTime.Add(time.Minute)
			}
		},
		"unbounded expiry": func(p string, d map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				d["expires_at"] = controlTime.Add(2 * time.Hour)
			}
		},
		"bad bearer": func(p string, d map[string]any) {
			if strings.HasSuffix(p, "/access_tokens") {
				d["token"] = "synthetic\r\nheader"
			}
		},
		"wrong repository": func(p string, d map[string]any) {
			if p == "/installation/repositories" {
				d["repositories"].([]any)[0].(map[string]any)["id"] = int64(1)
			}
		},
		"extra repository": func(p string, d map[string]any) {
			if p == "/installation/repositories" {
				d["total_count"] = 2
			}
		},
		"wrong org": func(p string, d map[string]any) {
			if p == githubRepoPath {
				d["owner"].(map[string]any)["id"] = int64(2)
			}
		},
		"wrong bot": func(p string, d map[string]any) {
			if p == "/users/sfl-test[bot]" {
				d["id"] = int64(1)
			}
		},
		"changed installation": func(p string, d map[string]any) {
			if strings.HasPrefix(p, "/app/installations/") && !strings.HasSuffix(p, "/access_tokens") {
				d["repository_selection"] = "all"
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f, o := installationHTTPFixture(t, GitHubPublisher)
			f.mutate = mutate
			transport, err := AcquireGitHubInstallation(context.Background(), o)
			if transport != nil || !errors.Is(err, ErrGitHubInstallation) {
				t.Fatal("accepted unverifiable enrollment", transport, err)
			}
			if f.mints > 1 || f.uses != 0 {
				t.Fatal("retried or used rejected credentials")
			}
		})
	}
}

func TestGitHubInstallationDoesNotReplayOrFollowRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		f, o := installationHTTPFixture(t, GitHubPublisher)
		f.redirect = redirect
		f.loseMint = !redirect
		transport, err := AcquireGitHubInstallation(context.Background(), o)
		if transport != nil || !errors.Is(err, ErrGitHubInstallation) || strings.Contains(err.Error(), "synthetic") {
			t.Fatal("leaked remote error or accepted failure")
		}
		if redirect && f.calls != 1 || !redirect && f.mints != 1 {
			t.Fatal("redirected or replayed token acquisition", f.calls, f.mints)
		}
	}
}

func TestGitHubInstallationRejectsDestinationsAndExpiredTokensBeforeHTTP(t *testing.T) {
	f, o := installationHTTPFixture(t, GitHubPublisher)
	now := controlTime
	o.Now = func() time.Time { return now }
	transport, err := AcquireGitHubInstallation(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	for _, entry := range [][2]string{{"DELETE", githubOrigin + githubRepoPath}, {"POST", githubOrigin + githubRepoPath + "/merges"}, {"PUT", githubOrigin + githubRepoPath + "/pulls/1/merge"}, {"GET", "http://api.github.com" + githubRepoPath}, {"GET", "https://api.github.com.evil" + githubRepoPath}, {"GET", githubOrigin + "/repos/other/private"}, {"GET", githubOrigin + githubRepoPath + "/../other"}, {"GET", "https://user@api.github.com" + githubRepoPath}, {"GET", githubOrigin + githubRepoPath + "#fragment"}} {
		q, _ := http.NewRequest(entry[0], entry[1], nil)
		before := f.calls
		if _, err := transport.RoundTrip(q); !errors.Is(err, ErrGitHubInstallation) || f.calls != before {
			t.Fatal("untrusted request reached transport", entry)
		}
	}
	q := installationQuery(t, "GET", githubRepoPath)
	q.Header.Set("Authorization", "Bearer ambient")
	before := f.calls
	if _, err := transport.RoundTrip(q); !errors.Is(err, ErrGitHubInstallation) || f.calls != before {
		t.Fatal("ambient credential accepted")
	}
	now = controlTime.Add(59 * time.Minute)
	if _, err := transport.RoundTrip(installationQuery(t, "GET", githubRepoPath)); !errors.Is(err, ErrGitHubInstallation) || f.calls != before {
		t.Fatal("expiry did not fail closed before HTTP")
	}
}

func TestGitHubInstallationInputAndTransportErrorsArePrivate(t *testing.T) {
	for _, mutate := range []func(*GitHubInstallationOptions){func(o *GitHubInstallationOptions) { o.Enrollment.RepositoryID++ }, func(o *GitHubInstallationOptions) { o.Role = "other" }, func(o *GitHubInstallationOptions) { o.AppJWT = nil }, func(o *GitHubInstallationOptions) { o.Transport = nil }} {
		f, o := installationHTTPFixture(t, GitHubReader)
		mutate(&o)
		if _, err := AcquireGitHubInstallation(context.Background(), o); !errors.Is(err, ErrGitHubInstallation) || f.calls != 0 {
			t.Fatal("bad input reached HTTP")
		}
	}
	f, o := installationHTTPFixture(t, GitHubReader)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AcquireGitHubInstallation(ctx, o); !errors.Is(err, ErrGitHubInstallation) || f.calls != 0 {
		t.Fatal("cancelled acquisition reached server")
	}
	o.Transport = githubResponseTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic-app-jwt") })
	if _, err := AcquireGitHubInstallation(context.Background(), o); err != ErrGitHubInstallation {
		t.Fatal("transport error leaked", err)
	}
}

type installationBodyProbe struct{ closed int }

func (*installationBodyProbe) Read([]byte) (int, error) { return 0, io.EOF }
func (b *installationBodyProbe) Close() error           { b.closed++; return nil }

func TestGitHubInstallationRejectsAllAuthorizationHeaderSpellings(t *testing.T) {
	for _, name := range []string{"authorization", "aUtHoRiZaTiOn", "proxy-authorization", "PROXY-AUTHORIZATION", "Authorization"} {
		for _, trailer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/trailer=%t", name, trailer), func(t *testing.T) {
				f, o := installationHTTPFixture(t, GitHubReader)
				tr, err := AcquireGitHubInstallation(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Close()
				q := installationQuery(t, "GET", githubRepoPath)
				if trailer {
					q.Trailer = http.Header{name: []string{"ambient-secret"}}
				} else {
					q.Header[name] = []string{"ambient-secret"}
				}
				body := &installationBodyProbe{}
				q.Body = body
				before := f.calls
				if response, err := tr.RoundTrip(q); response != nil || err != ErrGitHubInstallation || f.calls != before || body.closed != 1 {
					if response != nil {
						response.Body.Close()
					}
					t.Fatal("ambient authorization reached HTTP or body remained open", f.calls-before, body.closed, err)
				}
			})
		}
	}
}

func TestGitHubInstallationRejectsCallerTracesBeforeCredentialAttachment(t *testing.T) {
	for _, acquisition := range []bool{true, false} {
		t.Run(fmt.Sprintf("acquisition=%t", acquisition), func(t *testing.T) {
			f, o := installationHTTPFixture(t, GitHubPublisher)
			leaked := false
			trace := &httptrace.ClientTrace{WroteHeaderField: func(key string, values []string) {
				if strings.EqualFold(key, "Authorization") {
					for _, value := range values {
						if strings.Contains(value, "synthetic-") {
							leaked = true
						}
					}
				}
			}}
			ctx := httptrace.WithClientTrace(context.Background(), trace)
			if acquisition {
				tr, err := AcquireGitHubInstallation(ctx, o)
				if tr != nil {
					tr.Close()
				}
				if tr != nil || err != ErrGitHubInstallation || leaked || f.calls != 0 {
					t.Fatal("caller trace observed credentials or acquisition HTTP", leaked, f.calls, err)
				}
				return
			}
			tr, err := AcquireGitHubInstallation(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			before := f.calls
			q := installationQuery(t, "GET", githubRepoPath).WithContext(ctx)
			body := &installationBodyProbe{}
			q.Body = body
			response, err := tr.RoundTrip(q)
			if response != nil {
				response.Body.Close()
			}
			if response != nil || err != ErrGitHubInstallation || leaked || f.calls != before || body.closed != 1 {
				t.Fatal("caller trace observed role credential or rejected body leaked", leaked, f.calls-before, body.closed, err)
			}
		})
	}
}

func TestGitHubInstallationClosesEveryPreTransportRejectionBody(t *testing.T) {
	for _, kind := range []string{"destination", "role", "route", "expiry", "closed", "replayable", "host"} {
		t.Run(kind, func(t *testing.T) {
			f, o := installationHTTPFixture(t, GitHubReader)
			now := controlTime
			o.Now = func() time.Time { return now }
			tr, err := AcquireGitHubInstallation(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			q := installationQuery(t, "GET", githubRepoPath)
			switch kind {
			case "destination":
				q.URL.Host = "other.example"
			case "role":
				q.Method = "POST"
			case "route":
				q.URL.Path = "/app"
			case "expiry":
				now = controlTime.Add(time.Hour)
			case "closed":
				tr.Close()
			case "replayable":
				q.Method = "POST"
				q.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("body")), nil }
			case "host":
				q.Host = "other.example"
			}
			body := &installationBodyProbe{}
			q.Body = body
			before := f.calls
			if _, err := tr.RoundTrip(q); err != ErrGitHubInstallation || f.calls != before || body.closed != 1 {
				t.Fatal("rejected request did not close body exactly once", kind, body.closed, err)
			}
		})
	}
}
