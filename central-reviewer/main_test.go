package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const baseA = "cccccccccccccccccccccccccccccccccccccccc"
const baseB = "dddddddddddddddddddddddddddddddddddddddd"

func testConfig(t *testing.T) Config {
	t.Helper()
	d := t.TempDir()
	c := Config{Enabled: true, Listen: "127.0.0.1:3791", StateDirectory: filepath.Join(d, "state"), AppID: 4448946, InstallationID: 169090497, RepositoryID: 1229335234, Repository: "hemsoft-dev/hs-buddy", PrivateKeyFile: filepath.Join(d, "app.pem"), WebhookSecretFile: filepath.Join(d, "webhook-secret")}
	if e := os.WriteFile(c.WebhookSecretFile, bytes.Repeat([]byte("s"), 40), 0600); e != nil {
		t.Fatal(e)
	}
	return c
}
func testServer(t *testing.T) *Server {
	t.Helper()
	s, e := newServer(testConfig(t), "")
	if e != nil {
		t.Fatal(e)
	}
	s.state.Started = time.Now().Add(-time.Minute)
	return s
}
func openedEvent(s *Server) Event {
	var e Event
	e.Action = "opened"
	e.Repository = Repository{ID: s.config.RepositoryID, FullName: s.config.Repository, DefaultBranch: "main"}
	e.Installation.ID = s.config.InstallationID
	e.Pull.Number = 17
	e.Pull.Head.SHA = headA
	e.Pull.Head.Repo = e.Repository
	e.Pull.Base.SHA = baseA
	e.Pull.Base.Ref = "main"
	e.Pull.Base.Repo = e.Repository
	e.Pull.State = "open"
	e.Pull.Created = time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)
	return e
}
func request(s *Server, kind, id string, event Event, signature bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(event)
	r := httptest.NewRequest("POST", "/webhook", bytes.NewReader(raw))
	r.Header.Set("X-GitHub-Delivery", id)
	r.Header.Set("X-GitHub-Event", kind)
	if signature {
		mac := hmac.New(sha256.New, s.secret)
		mac.Write(raw)
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestWebhookTrustScopeAndRestart(t *testing.T) {
	s := testServer(t)
	e := openedEvent(s)
	if w := request(s, "pull_request", "good", e, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, change := range []func(*Event){func(e *Event) { e.Repository.ID++ }, func(e *Event) { e.Repository.FullName = "hemsoft-dev/codexbar" }, func(e *Event) { e.Installation.ID++ }} {
		copy := e
		change(&copy)
		if w := request(s, "pull_request", "excluded", copy, true); w.Code != 202 {
			t.Fatal(w.Code)
		}
	}
	if len(s.state.Jobs) != 0 {
		t.Fatal("untrusted/excluded event enqueued")
	}
	for i := 0; i < 2; i++ {
		if w := request(s, "pull_request", "good", e, true); w.Code != 202 && w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if len(s.state.Jobs) != 1 {
		t.Fatal("duplicate queued")
	}
	restored, err := newServer(s.config, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.state.Jobs) != 1 || restored.state.Jobs[0].ID != "good" {
		t.Fatal("restart lost event")
	}
	stat, err := os.Stat(s.statePath)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("state permissions")
	}
}
func TestPauseRejectsWorkAndReloads(t *testing.T) {
	s := testServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	c := s.config
	c.Enabled = false
	if e := atomicJSON(path, c); e != nil {
		t.Fatal(e)
	}
	s.configPath = path
	if w := request(s, "pull_request", "paused", openedEvent(s), true); w.Code != 202 || len(s.state.Jobs) != 0 {
		t.Fatal("paused event ran")
	}
	c.Enabled = true
	if e := atomicJSON(path, c); e != nil {
		t.Fatal(e)
	}
	if !s.enabled() {
		t.Fatal("did not reload enable")
	}
	c.Repository = "hemsoft-dev/codexbar"
	if e := atomicJSON(path, c); e != nil {
		t.Fatal(e)
	}
	if s.enabled() {
		t.Fatal("scope could widen")
	}
}
func TestInvalidConfig(t *testing.T) {
	c := testConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	for _, change := range []func(*Config){func(c *Config) { c.RepositoryID++ }, func(c *Config) { c.InstallationID++ }, func(c *Config) { c.Listen = "0.0.0.0:3791" }, func(c *Config) { c.StateDirectory = "relative" }} {
		copy := c
		change(&copy)
		atomicJSON(path, copy)
		if _, e := readConfig(path); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
	atomicJSON(path, c)
	if _, e := readConfig(path); e != nil {
		t.Fatal(e)
	}
}
func TestStateLock(t *testing.T) {
	d := t.TempDir()
	one, e := lockState(d)
	if e != nil {
		t.Fatal(e)
	}
	defer one.Close()
	two, e := lockState(d)
	if e == nil {
		two.Close()
		t.Fatal("second worker obtained same state")
	}
}

type fakeGitHub struct {
	mu               sync.Mutex
	pull             Pull
	repo             Repository
	comments         []Artifact
	reviews          []Artifact
	timeline         []map[string]any
	associated       []Pull
	writes           []map[string]any
	failPath         string
	pullGets         int
	changeAfterWrite bool
	failAfterSuccess bool
	reactions        []Reaction
	editorID         int64
}

func fakeFor(s *Server) *fakeGitHub {
	e := openedEvent(s)
	e.Pull.Updated = e.Pull.Created
	return &fakeGitHub{pull: e.Pull, repo: e.Repository, timeline: []map[string]any{}, associated: []Pull{e.Pull}, comments: []Artifact{cleanArtifact()}}
}
func cleanArtifact() Artifact {
	created := time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	return Artifact{ID: 91, User: User{ID: 199175422, Login: "chatgpt-codex-connector[bot]", Type: "Bot"}, App: &App{ID: 1144995, Slug: "chatgpt-codex-connector", Owner: User{Login: "openai"}}, Body: "Codex Review: Didn't find any major issues.\n\n**Reviewed commit:** `aaaaaaaa`", Created: created, Updated: created}
}
func (f *fakeGitHub) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.failAfterSuccess && strings.HasSuffix(r.URL.Path, "/pulls/17") && len(f.writes) > 0 && f.writes[0]["conclusion"] == "success" {
		w.WriteHeader(500)
		return
	}
	if r.URL.Path == f.failPath {
		w.WriteHeader(500)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/hemsoft-dev/hs-buddy")
	var value any
	switch {
	case path == "":
		value = f.repo
	case path == "/pulls/17":
		f.pullGets++
		if f.changeAfterWrite && len(f.writes) > 0 {
			f.pull.Base.SHA = baseB
		}
		value = f.pull
	case path == "/issues/17/timeline":
		value = f.timeline
	case path == "/issues/17/comments":
		value = f.comments
	case path == "/issues/17/reactions":
		value = f.reactions
	case path == "/graphql":
		value = map[string]any{"data": map[string]any{"node": map[string]any{"id": "comment-91", "fullDatabaseId": "91", "author": map[string]any{"login": "chatgpt-codex-connector", "databaseId": 199175422}, "editor": map[string]any{"login": "chatgpt-codex-connector", "databaseId": f.editorID}}}}
	case path == "/pulls/17/reviews":
		value = f.reviews
	case strings.HasPrefix(path, "/commits/") && strings.HasSuffix(path, "/pulls"):
		value = f.associated
	case strings.HasPrefix(path, "/commits/") && strings.HasSuffix(path, "/check-runs"):
		value = map[string]any{"total_count": 0, "check_runs": []any{}}
	case strings.HasPrefix(path, "/commits/"):
		sha := strings.TrimPrefix(path, "/commits/")
		if len(sha) >= 7 && len(sha) < 40 && strings.HasPrefix(headA, sha) {
			sha = headA
		}
		value = map[string]string{"sha": sha}
	case strings.HasPrefix(path, "/check-runs") && (r.Method == "POST" || r.Method == "PATCH"):
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		f.writes = append(f.writes, body)
		sha := f.pull.Head.SHA
		if h, ok := body["head_sha"].(string); ok {
			sha = h
		}
		value = map[string]any{"id": 42, "app": map[string]any{"id": 4448946}, "head_sha": sha}
	default:
		w.WriteHeader(404)
		return
	}
	json.NewEncoder(w).Encode(value)
}
func attachFake(t *testing.T, s *Server, f *fakeGitHub) {
	t.Helper()
	h := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(h.Close)
	s.github.baseURL = h.URL
	s.github.client = h.Client()
	s.github.token = "test-only-installation-token"
	s.github.expires = time.Now().Add(time.Hour)
}
func openedJob(s *Server) Job {
	raw, _ := json.Marshal(openedEvent(s))
	return Job{ID: "opened-17", Kind: "pull_request", Payload: raw, Received: time.Now().Add(-10 * time.Second)}
}
func lastConclusion(t *testing.T, f *fakeGitHub) string {
	t.Helper()
	if len(f.writes) == 0 {
		t.Fatal("no check written")
	}
	x, _ := f.writes[len(f.writes)-1]["conclusion"].(string)
	return x
}
func TestNativeReviewPublicationAndRestart(t *testing.T) {
	s := testServer(t)
	f := fakeFor(s)
	attachFake(t, s, f)
	if err := s.process(context.Background(), openedJob(s)); err != nil {
		t.Fatal(err)
	}
	if got := lastConclusion(t, f); got != "success" {
		t.Fatal(got)
	}
	restored, e := newServer(s.config, "")
	if e != nil {
		t.Fatal(e)
	}
	if restored.state.Pulls[17].CheckID != 42 || restored.state.Pulls[17].Base != baseA {
		t.Fatal("lost binding")
	}
	if f.writes[0]["name"] != "SFL PR Reviewer" {
		t.Fatal("wrong check publisher contract")
	}
}
func TestNativeNegativeControls(t *testing.T) {
	cases := []struct {
		name       string
		change     func(*fakeGitHub)
		conclusion string
	}{
		{"spoofed-bot-id", func(f *fakeGitHub) { f.comments[0].User.ID++ }, ""},
		{"spoofed-app", func(f *fakeGitHub) { f.comments[0].App.ID++ }, ""},
		{"missing-app", func(f *fakeGitHub) { f.comments[0].App = nil }, ""},
		{"conflicting-owner", func(f *fakeGitHub) { f.comments[0].App.Owner.Login = "attacker" }, ""},
		{"old-result", func(f *fakeGitHub) { f.comments[0].Created = time.Now().Add(-time.Hour).Format(time.RFC3339) }, ""},
		{"edited-result", func(f *fakeGitHub) { f.comments[0].Updated = time.Now().Format(time.RFC3339) }, "action_required"},
		{"stale-head", func(f *fakeGitHub) {
			f.comments[0].Body = "Codex Review: Didn't find any major issues.\n**Reviewed commit:** `bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`"
		}, ""},
		{"unknown-result", func(f *fakeGitHub) { f.comments[0].Body = "Review complete\n**Reviewed commit:** `aaaaaaaa`" }, "failure"},
		{"no-commit-marker", func(f *fakeGitHub) { f.comments[0].Body = "Codex Review: Didn't find any major issues." }, ""},
		{"findings", func(f *fakeGitHub) {
			f.reviews = []Artifact{{ID: 99, User: f.comments[0].User, App: f.comments[0].App, Commit: headA, Submitted: time.Now().Add(-4 * time.Second).Format(time.RFC3339), State: "COMMENTED", Body: "P1: bug"}}
		}, "failure"},
		{"shared-head", func(f *fakeGitHub) { f.associated = append(f.associated, Pull{Number: 18}) }, "action_required"},
		{"draft", func(f *fakeGitHub) { f.pull.Draft = true }, "action_required"},
		{"closed", func(f *fakeGitHub) { f.pull.State = "closed" }, "action_required"},
		{"unsupported-base", func(f *fakeGitHub) { f.pull.Base.Ref = "release" }, "action_required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			f := fakeFor(s)
			c.change(f)
			attachFake(t, s, f)
			if err := s.process(context.Background(), openedJob(s)); err != nil {
				t.Fatal(err)
			}
			if got := lastConclusion(t, f); got != c.conclusion {
				t.Fatalf("got %q want %q", got, c.conclusion)
			}
		})
	}
}
func TestContextChangeCannotReuseHead(t *testing.T) {
	s := testServer(t)
	f := fakeFor(s)
	attachFake(t, s, f)
	job := openedJob(s)
	if e := s.process(context.Background(), job); e != nil {
		t.Fatal(e)
	}
	f.pull.Base.SHA = baseB
	event := openedEvent(s)
	event.Action = "edited"
	raw, _ := json.Marshal(event)
	job = Job{ID: "retarget", Kind: "pull_request", Payload: raw, Received: time.Now()}
	if e := s.process(context.Background(), job); e != nil {
		t.Fatal(e)
	}
	if lastConclusion(t, f) != "action_required" {
		t.Fatal("base change stayed green")
	}
	f.pull.Base.SHA = baseA
	event.Action = "edited"
	raw, _ = json.Marshal(event)
	job.Payload = raw
	if e := s.process(context.Background(), job); e != nil {
		t.Fatal(e)
	}
	if lastConclusion(t, f) != "action_required" {
		t.Fatal("return to old base reused review")
	}
}
func TestFreshNewHeadCanReview(t *testing.T) {
	s := testServer(t)
	f := fakeFor(s)
	attachFake(t, s, f)
	if e := s.process(context.Background(), openedJob(s)); e != nil {
		t.Fatal(e)
	}
	f.pull.Head.SHA = headB
	f.associated = []Pull{f.pull}
	event := openedEvent(s)
	event.Action = "synchronize"
	event.Pull.Head.SHA = headB
	raw, _ := json.Marshal(event)
	f.comments = []Artifact{cleanArtifact()}
	f.comments[0].Body = "Codex Review: Didn't find any major issues.\n**Reviewed commit:** `" + headB + "`"
	job := Job{ID: "fresh-head", Kind: "pull_request", Payload: raw, Received: time.Now().Add(-8 * time.Second)}
	if e := s.process(context.Background(), job); e != nil {
		t.Fatal(e)
	}
	if lastConclusion(t, f) != "success" {
		t.Fatal("new head did not qualify")
	}
}
func TestQueuedAndPublicationChangesBlockSuccess(t *testing.T) {
	for _, mode := range []string{"queued-push", "publication-race", "unknown-first-event", "historical-pr"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			f := fakeFor(s)
			attachFake(t, s, f)
			job := openedJob(s)
			switch mode {
			case "queued-push":
				event := openedEvent(s)
				event.Ref = "refs/heads/main"
				raw, _ := json.Marshal(event)
				s.state.Jobs = []Job{{ID: "push", Kind: "push", Payload: raw}}
			case "publication-race":
				f.changeAfterWrite = true
			case "unknown-first-event":
				job.Kind = "pull_request_review"
			case "historical-pr":
				f.pull.Created = time.Now().Add(-time.Hour).Format(time.RFC3339)
			}
			if e := s.process(context.Background(), job); e != nil {
				t.Fatal(e)
			}
			if mode == "unknown-first-event" || mode == "historical-pr" {
				if len(f.writes) != 0 {
					t.Fatal("wrote a check on an unrelated existing PR")
				}
				return
			}
			if lastConclusion(t, f) != "action_required" {
				t.Fatal("unproven context remained green")
			}
		})
	}
}
func TestAPIErrorCannotPublishSuccess(t *testing.T) {
	s := testServer(t)
	f := fakeFor(s)
	f.failPath = "/repos/hemsoft-dev/hs-buddy/issues/17/comments"
	attachFake(t, s, f)
	if e := s.process(context.Background(), openedJob(s)); e == nil {
		t.Fatal("API failure swallowed")
	}
	if len(f.writes) != 0 {
		t.Fatal("API failure wrote success")
	}
}
func TestInstallationTokenScopeAndJWT(t *testing.T) {
	c := testConfig(t)
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if e = os.WriteFile(c.PrivateKeyFile, raw, 0600); e != nil {
		t.Fatal(e)
	}
	minted := false
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			var body struct {
				IDs         []int64           `json:"repository_ids"`
				Permissions map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.IDs) != 1 || body.IDs[0] != 1229335234 || body.Permissions["contents"] != "read" || body.Permissions["pull_requests"] != "read" || body.Permissions["checks"] != "write" || len(body.Permissions) != 4 {
				t.Errorf("token scope widened: %+v", body)
			}
			if len(strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")) != 3 {
				t.Error("missing JWT")
			}
			minted = true
			fmt.Fprintf(w, `{"token":"test-installation","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-installation" {
			t.Error("wrong identity")
		}
		fmt.Fprint(w, `{"id":1229335234}`)
	}))
	defer h.Close()
	g := newGitHub(c)
	g.baseURL = h.URL
	var result Repository
	if e = g.call(context.Background(), "GET", "/repos/hemsoft-dev/hs-buddy", nil, &result); e != nil {
		t.Fatal(e)
	}
	if !minted || result.ID != 1229335234 {
		t.Fatal("token mint failed")
	}
}

func TestDeletedCleanResultWithdrawsSuccess(t *testing.T) {
	s := testServer(t)
	f := fakeFor(s)
	attachFake(t, s, f)
	if e := s.process(context.Background(), openedJob(s)); e != nil {
		t.Fatal(e)
	}
	if lastConclusion(t, f) != "success" {
		t.Fatal("positive control failed")
	}
	f.comments = nil
	e := openedEvent(s)
	e.Issue.Number = 17
	e.Issue.Pull = json.RawMessage(`{"url":"test"}`)
	e.Action = "deleted"
	raw, _ := json.Marshal(e)
	if err := s.process(context.Background(), Job{ID: "deleted", Kind: "issue_comment", Payload: raw, Received: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if lastConclusion(t, f) != "action_required" {
		t.Fatal("deleted result retained green check")
	}
}
func TestWorkerProcessesDurableQueueAfterRestart(t *testing.T) {
	s := testServer(t)
	e := openedEvent(s)
	if w := request(s, "pull_request", "restart", e, true); w.Code != 202 {
		t.Fatal(w.Code)
	}
	restored, err := newServer(s.config, "")
	if err != nil {
		t.Fatal(err)
	}
	f := fakeFor(restored)
	f.comments = nil
	attachFake(t, restored, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { restored.run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		restored.mu.Lock()
		complete := len(restored.state.Done) == 1
		restored.mu.Unlock()
		if complete {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	restored.mu.Lock()
	defer restored.mu.Unlock()
	if len(restored.state.Jobs) != 0 || len(restored.state.Done) != 1 {
		t.Fatal("restart did not drain queued delivery")
	}
	if len(f.writes) != 1 || f.writes[0]["status"] != "in_progress" {
		t.Fatal("restart did not publish actual waiting check")
	}
}
func TestWorkerFailureRetriesAndRecovers(t *testing.T) {
	s := testServer(t)
	e := openedEvent(s)
	request(s, "pull_request", "retry", e, true)
	f := fakeFor(s)
	f.comments = nil
	f.failPath = "/repos/hemsoft-dev/hs-buddy/issues/17/timeline"
	attachFake(t, s, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	deadline := time.Now().Add(6 * time.Second)
	sawFailure := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.state.Jobs) > 0 && s.state.Jobs[0].Attempts > 0 {
			sawFailure = true
		}
		complete := len(s.state.Done) > 0
		s.mu.Unlock()
		if sawFailure {
			f.mu.Lock()
			f.failPath = ""
			f.mu.Unlock()
		}
		if complete {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	if !sawFailure || len(s.state.Jobs) != 0 || len(s.state.Done) != 1 {
		t.Fatal("transient error did not retry and recover")
	}
}
func TestBadSignatureAndOversizedBodyNeverPersist(t *testing.T) {
	s := testServer(t)
	raw := bytes.Repeat([]byte("x"), (1<<20)+1)
	r := httptest.NewRequest("POST", "/webhook", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
	r = httptest.NewRequest("POST", "/webhook", strings.NewReader(`{"repository":{"id":1229335234}}`))
	r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	if len(s.state.Jobs) != 0 {
		t.Fatal("untrusted work persisted")
	}
}
func TestDeadLetterHealth(t *testing.T) {
	s := testServer(t)
	s.state.Jobs = []Job{{ID: "dead", Dead: true}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatal("dead letter is not visible")
	}
}

func TestCurrentAutomaticCodexSummary(t *testing.T) {
	for _, mode := range []string{"clean", "human-edited", "old-reaction", "still-running", "pending-summary", "findings", "old-head"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			f := fakeFor(s)
			a := cleanArtifact()
			a.NodeID = "comment-91"
			completed := time.Now().Add(-5 * time.Second).UTC()
			a.Created = time.Now().Add(-9 * time.Second).UTC().Format(time.RFC3339)
			a.Updated = time.Now().Add(-4 * time.Second).UTC().Format(time.RFC3339)
			a.Body = "<!-- codex-pull-request-review-summary -->\n\n## Codex Review Summary\n\n| Review | Status | Commit | Review trigger |\n| --- | --- | --- | --- |\n| 📝 **Code Review** | ✅ **Completed** <relative-time datetime=\"" + completed.Format(time.RFC3339Nano) + "\">completed</relative-time> | `aaaaaaa` | PR opened |\n"
			f.comments = []Artifact{a}
			f.editorID = 199175422
			f.reactions = []Reaction{{Content: "+1", Created: time.Now().Add(-3 * time.Second).UTC().Format(time.RFC3339), User: a.User}}
			switch mode {
			case "human-edited":
				f.editorID = 42
			case "old-reaction":
				f.reactions[0].Created = time.Now().Add(-time.Hour).Format(time.RFC3339)
			case "still-running":
				f.reactions = append(f.reactions, Reaction{Content: "eyes", User: a.User})
			case "pending-summary":
				f.comments[0].Body = strings.ReplaceAll(a.Body, "✅ **Completed**", "👀 **In progress**")
			case "findings":
				f.reviews = []Artifact{{User: a.User, Commit: headA, Submitted: completed.Format(time.RFC3339), State: "COMMENTED", Body: "P1 finding"}}
			case "old-head":
				f.comments[0].Body = strings.ReplaceAll(a.Body, "`aaaaaaa`", "`bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`")
			}
			attachFake(t, s, f)
			if e := s.process(context.Background(), openedJob(s)); e != nil {
				t.Fatal(e)
			}
			got := lastConclusion(t, f)
			if mode == "clean" && got != "success" {
				t.Fatal("current native summary did not qualify", got)
			}
			if mode != "clean" && got == "success" {
				t.Fatal("unsafe automatic result qualified")
			}
		})
	}
}

func TestVerificationErrorsWithdrawExistingSuccess(t *testing.T) {
	for _, mode := range []string{"later-review-read", "post-publication-read"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			f := fakeFor(s)
			attachFake(t, s, f)
			if mode == "post-publication-read" {
				f.failAfterSuccess = true
			} else {
				if err := s.process(context.Background(), openedJob(s)); err != nil {
					t.Fatal(err)
				}
				f.failPath = "/repos/hemsoft-dev/hs-buddy/issues/17/comments"
			}
			if err := s.process(context.Background(), openedJob(s)); err == nil {
				t.Fatal("verification error swallowed")
			}
			if lastConclusion(t, f) != "action_required" {
				t.Fatal("unverified success was retained")
			}
		})
	}
}
