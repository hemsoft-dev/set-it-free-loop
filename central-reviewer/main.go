// SFL's central, single-repository pilot. Consumer repositories run no SFL Actions.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	StateDirectory    string `json:"state_directory"`
	AppID             int64  `json:"app_id"`
	InstallationID    int64  `json:"installation_id"`
	RepositoryID      int64  `json:"repository_id"`
	Repository        string `json:"repository"`
	PrivateKeyFile    string `json:"private_key_file"`
	WebhookSecretFile string `json:"webhook_secret_file"`
}

func readConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("configuration must contain one object")
	}
	if c.Repository != "hemsoft-dev/hs-buddy" || c.RepositoryID != 1229335234 || c.AppID != 4448946 || c.InstallationID != 169090497 {
		return c, fmt.Errorf("this pilot permits only the recorded hs-buddy/SFL App installation")
	}
	host, _, e := net.SplitHostPort(c.Listen)
	if e != nil || host != "127.0.0.1" {
		return c, fmt.Errorf("listen must use IPv4 loopback")
	}
	for _, p := range []string{c.StateDirectory, c.PrivateKeyFile, c.WebhookSecretFile} {
		if !filepath.IsAbs(p) {
			return c, fmt.Errorf("state and credential paths must be absolute")
		}
	}
	return c, nil
}

type Event struct {
	Action       string     `json:"action"`
	Repository   Repository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Pull  Pull `json:"pull_request"`
	Issue struct {
		Number int             `json:"number"`
		Pull   json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Ref     string   `json:"ref"`
	Comment Artifact `json:"comment"`
	Review  Artifact `json:"review"`
}
type Job struct {
	ID       string
	Kind     string
	Payload  json.RawMessage
	Received time.Time
	Attempts int
	Next     time.Time
	Dead     bool
	Error    string
}
type PRState struct {
	Head      string
	Base      string
	Timeline  string
	Since     time.Time
	Blocked   string
	CheckID   int64
	SeenHeads []string
	NextPoll  time.Time
	Closed    bool
}
type State struct {
	Started time.Time
	Jobs    []Job
	Done    []string
	Pulls   map[int]*PRState
}
type Server struct {
	config       Config
	configPath   string
	secret       []byte
	github       *githubClient
	mu           sync.Mutex
	state        State
	wake         chan struct{}
	statePath    string
	workerFailed bool
}

var deliveryID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,100}$`)

func atomicJSON(path string, value any) error {
	raw, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".sfl-state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(raw, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func newServer(c Config, path string) (*Server, error) {
	if e := os.MkdirAll(c.StateDirectory, 0700); e != nil {
		return nil, e
	}
	secret, e := os.ReadFile(c.WebhookSecretFile)
	if e != nil {
		return nil, e
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	if len(secret) < 32 {
		return nil, fmt.Errorf("webhook secret must contain at least 32 bytes")
	}
	s := &Server{config: c, configPath: path, secret: secret, github: newGitHub(c), wake: make(chan struct{}, 1), statePath: filepath.Join(c.StateDirectory, "state.json"), state: State{Started: time.Now().UTC(), Pulls: map[int]*PRState{}}}
	raw, e := os.ReadFile(s.statePath)
	if e == nil {
		e = json.Unmarshal(raw, &s.state)
		if e != nil {
			return nil, e
		}
		if s.state.Pulls == nil {
			return nil, fmt.Errorf("invalid persisted state")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	} else if e = atomicJSON(s.statePath, s.state); e != nil {
		return nil, e
	}
	return s, nil
}
func (s *Server) enabled() bool {
	if s.configPath == "" {
		return s.config.Enabled
	}
	c, e := readConfig(s.configPath)
	return e == nil && c.Enabled && c.Listen == s.config.Listen && c.StateDirectory == s.config.StateDirectory && c.PrivateKeyFile == s.config.PrivateKeyFile && c.WebhookSecretFile == s.config.WebhookSecretFile
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		s.mu.Lock()
		dead := 0
		for _, j := range s.state.Jobs {
			if j.Dead {
				dead++
			}
		}
		pending := len(s.state.Jobs)
		failed := s.workerFailed
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if dead > 0 || failed {
			w.WriteHeader(503)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": s.enabled(), "pending": pending, "dead_letters": dead})
		return
	}
	if r.URL.Path != "/webhook" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if e != nil {
		http.Error(w, "payload too large", 413)
		return
	}
	sig, e := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(raw)
	if e != nil || !strings.HasPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=") || !hmac.Equal(sig, mac.Sum(nil)) {
		http.Error(w, "invalid signature", 401)
		return
	}
	kind, id := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	if !deliveryID.MatchString(id) {
		http.Error(w, "invalid delivery ID", 400)
		return
	}
	var event Event
	if json.Unmarshal(raw, &event) != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	if kind == "ping" {
		w.WriteHeader(200)
		return
	}
	if event.Repository.ID != s.config.RepositoryID || event.Repository.FullName != s.config.Repository || event.Installation.ID != s.config.InstallationID || !supported(kind, event) || !s.enabled() {
		w.WriteHeader(202)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, done := range s.state.Done {
		if done == id {
			w.WriteHeader(200)
			return
		}
	}
	for _, job := range s.state.Jobs {
		if job.ID == id {
			w.WriteHeader(200)
			return
		}
	}
	if len(s.state.Jobs) >= 1000 {
		http.Error(w, "queue full", 503)
		return
	}
	job := Job{ID: id, Kind: kind, Payload: raw, Received: time.Now().UTC()}
	s.state.Jobs = append(s.state.Jobs, job)
	if e = atomicJSON(s.statePath, s.state); e != nil {
		s.state.Jobs = s.state.Jobs[:len(s.state.Jobs)-1]
		http.Error(w, "durable enqueue failed", 503)
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	w.WriteHeader(202)
}
func supported(kind string, e Event) bool {
	switch kind {
	case "pull_request":
		switch e.Action {
		case "opened", "synchronize", "reopened", "edited", "closed", "converted_to_draft", "ready_for_review":
			return true
		}
	case "push":
		return e.Ref == "refs/heads/"+e.Repository.DefaultBranch
	case "issue_comment":
		return len(e.Issue.Pull) > 0 && (e.Action == "created" || e.Action == "edited" || e.Action == "deleted")
	case "pull_request_review":
		return e.Action == "submitted" || e.Action == "edited" || e.Action == "dismissed"
	}
	return false
}
func (s *Server) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		if !s.enabled() {
			continue
		}
		if err := s.schedulePolls(); err != nil {
			log.Printf("poll scheduling failed: %v", err)
			continue
		}
		s.mu.Lock()
		var job *Job
		for i := range s.state.Jobs {
			j := s.state.Jobs[i]
			if j.Dead || j.Next.After(time.Now()) {
				continue
			}
			job = &j
			break
		}
		s.mu.Unlock()
		if job == nil {
			continue
		}
		workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := s.process(workCtx, *job)
		cancel()
		s.mu.Lock()
		for i := range s.state.Jobs {
			if s.state.Jobs[i].ID != job.ID {
				continue
			}
			if err == nil {
				s.state.Done = append(s.state.Done, job.ID)
				if len(s.state.Done) > 10000 {
					s.state.Done = s.state.Done[len(s.state.Done)-10000:]
				}
				s.state.Jobs = append(s.state.Jobs[:i], s.state.Jobs[i+1:]...)
			} else {
				j := &s.state.Jobs[i]
				j.Attempts++
				j.Error = err.Error()
				j.Dead = j.Attempts >= 5
				j.Next = time.Now().Add(time.Duration(1<<j.Attempts) * time.Second)
				log.Printf("delivery %s attempt %d failed: %v", j.ID, j.Attempts, err)
			}
			break
		}
		if e := atomicJSON(s.statePath, s.state); e != nil {
			s.workerFailed = true
			log.Printf("state persistence failed; stopping worker: %v", e)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}
func (s *Server) process(ctx context.Context, j Job) error {
	var e Event
	if err := json.Unmarshal(j.Payload, &e); err != nil {
		return err
	}
	n := e.Pull.Number
	if j.Kind == "issue_comment" {
		n = e.Issue.Number
	}
	if j.Kind == "push" {
		pulls, err := fetchPages[Pull](ctx, s.github, s.github.repoPath()+"/pulls?state=open")
		if err != nil {
			return err
		}
		for _, p := range pulls {
			if err = s.reconcile(ctx, p.Number, j, e); err != nil {
				return err
			}
		}
		return nil
	}
	if n < 1 {
		return fmt.Errorf("missing pull request number")
	}
	return s.reconcile(ctx, n, j, e)
}
func (s *Server) persistPull(n int, p *PRState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := *p
	snapshot.SeenHeads = append([]string(nil), p.SeenHeads...)
	s.state.Pulls[n] = &snapshot
	return atomicJSON(s.statePath, s.state)
}
func (s *Server) queuedContextChange(n int, current string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.state.Jobs {
		if j.ID == current {
			continue
		}
		var e Event
		if json.Unmarshal(j.Payload, &e) != nil {
			return true
		}
		if j.Kind == "push" || j.Kind == "pull_request" && e.Pull.Number == n {
			return true
		}
	}
	return false
}
func (s *Server) reconcile(ctx context.Context, n int, j Job, event Event) (err error) {
	// Withdraw existing proof if a later API read cannot verify its context.
	// GitHub write failures remain visible through durable retry and health.
	defer func() {
		if err == nil || !s.enabled() {
			return
		}
		s.mu.Lock()
		previous := s.state.Pulls[n]
		var state PRState
		if previous != nil {
			state = *previous
		}
		s.mu.Unlock()
		if state.CheckID != 0 {
			if withdraw := s.publish(ctx, n, &state, "action_required", "Current review context could not be verified; retry pending."); withdraw != nil {
				err = fmt.Errorf("%w; check withdrawal failed: %v", err, withdraw)
			}
		}
	}()
	p, repo, err := s.github.pull(ctx, n)
	if err != nil {
		return err
	}
	if repo.ID != s.config.RepositoryID || repo.FullName != s.config.Repository {
		return fmt.Errorf("live repository identity changed")
	}
	if p.Head.Repo.ID != repo.ID || p.Base.Repo.ID != repo.ID {
		return nil
	}
	timeline, err := s.github.timeline(ctx, n)
	if err != nil {
		return err
	}
	s.mu.Lock()
	previous := s.state.Pulls[n]
	var state PRState
	if previous != nil {
		state = *previous
		state.SeenHeads = append([]string(nil), previous.SeenHeads...)
	}
	started := s.state.Started
	s.mu.Unlock()
	created, _ := time.Parse(time.RFC3339, p.Created)
	if previous == nil && (!created.After(started) || j.Kind != "pull_request" || event.Action != "opened") {
		// Never attach pilot checks to another agent's pre-existing PR.
		return nil
	}
	freshEvent := j.Kind == "pull_request" && (event.Action == "opened" || event.Action == "synchronize") && event.Pull.Head.SHA == p.Head.SHA && event.Pull.Base.SHA == p.Base.SHA && event.Pull.Base.Ref == repo.DefaultBranch
	if state.Head != p.Head.SHA && !freshEvent {
		// Native reviews and old PR deliveries can arrive before synchronize.
		// Enroll an observed new PR, but do not consume its unobserved head.
		state.Blocked = "Head changed before its matching signed PR event was observed; waiting for delivery."
		if state.Head == "" {
			return s.persistPull(n, &state)
		}
		return s.publish(ctx, n, &state, "action_required", state.Blocked)
	}
	state.Closed = p.State != "open"
	if state.Head != p.Head.SHA {
		fresh := freshEvent
		created, _ := time.Parse(time.RFC3339, p.Created)
		if previous == nil && (!created.After(started) || event.Action != "opened") {
			fresh = false
		}
		for _, head := range state.SeenHeads {
			if head == p.Head.SHA {
				fresh = false
			}
		}
		state.SeenHeads = append(state.SeenHeads, p.Head.SHA)
		state.Head = p.Head.SHA
		state.Base = p.Base.SHA
		state.Timeline = timeline
		state.Since = j.Received
		state.CheckID = 0
		state.Blocked = ""
		if !fresh {
			state.Blocked = "This head has no fresh, observed PR context. Open a new PR or push a substantive new commit after pilot setup."
		}
	} else if state.Base != p.Base.SHA || state.Timeline != timeline || j.Kind == "push" || j.Kind == "pull_request" && (event.Action == "edited" || event.Action == "reopened" || event.Action == "converted_to_draft") {
		state.Blocked = "The PR context changed on this head. A substantive new commit and a fresh Codex review are required."
		state.Base = p.Base.SHA
		state.Timeline = timeline
	}
	if err = s.persistPull(n, &state); err != nil {
		return err
	}
	result, reason := "", "Waiting for an authenticated current-head native Codex review."
	if p.State != "open" || p.Draft || p.Base.Ref != repo.DefaultBranch {
		result = "action_required"
		reason = "Only open, ready PRs targeting the default branch are supported by this pilot."
	} else if state.Blocked != "" {
		result = "action_required"
		reason = state.Blocked
	} else if err = s.github.uniqueHead(ctx, p); err != nil {
		result = "action_required"
		reason = "Cannot establish an exclusive PR head: " + err.Error()
	} else {
		result, reason, err = s.evaluate(ctx, p, state)
		if err != nil {
			return err
		}
	}
	if result == "success" {
		fresh, _, e := s.github.pull(ctx, n)
		if e != nil {
			return e
		}
		freshTimeline, e := s.github.timeline(ctx, n)
		if e != nil {
			return e
		}
		if fresh.Head.SHA != state.Head || fresh.Base.SHA != state.Base || freshTimeline != state.Timeline || fresh.State != "open" || fresh.Draft || fresh.Base.Ref != repo.DefaultBranch || s.queuedContextChange(n, j.ID) {
			result = "action_required"
			reason = "Context changed before check publication. Waiting for reconciliation."
		}
	}
	if !s.enabled() {
		return fmt.Errorf("pilot paused before publication")
	}
	if err = s.publish(ctx, n, &state, result, reason); err != nil {
		return err
	}
	if result == "success" {
		fresh, _, e := s.github.pull(ctx, n)
		if e != nil {
			return e
		}
		freshTimeline, e := s.github.timeline(ctx, n)
		if e != nil {
			return e
		}
		if fresh.Head.SHA != state.Head || fresh.Base.SHA != state.Base || freshTimeline != state.Timeline || fresh.State != "open" || fresh.Draft || fresh.Base.Ref != repo.DefaultBranch || s.queuedContextChange(n, j.ID) {
			state.Blocked = "Context changed during publication; the prior result is invalid."
			if e = s.publish(ctx, n, &state, "action_required", state.Blocked); e != nil {
				return e
			}
		}
	}
	return s.persistPull(n, &state)
}

var reviewedCommit = regexp.MustCompile("(?i)\\*\\*Reviewed commit:\\*\\*\\s+`([0-9a-f]{7,40})`")
var summaryResult = regexp.MustCompile(`(?m)^\|\s*📝\s+\*\*Code Review\*\*\s*\|\s*✅\s+\*\*Completed\*\*\s+<relative-time datetime="([^"]+)">[^<]+</relative-time>\s*\|\s*\x60([0-9a-f]{7,40})\x60\s*\|[^|]+\|\s*$`)
var cleanResult = regexp.MustCompile(`^Codex Review: (?:Didn't|Did not) find any major issues\.`)

func codex(a Artifact, comment bool) bool {
	if a.User.ID != 199175422 || a.User.Login != "chatgpt-codex-connector[bot]" {
		return false
	}
	if a.App == nil {
		return !comment
	}
	return a.App.ID == 1144995 && a.App.Slug == "chatgpt-codex-connector" && a.App.Owner.Login == "openai"
}
func (s *Server) evaluate(ctx context.Context, p Pull, state PRState) (string, string, error) {
	comments, err := fetchPages[Artifact](ctx, s.github, s.github.repoPath()+"/issues/"+strconv.Itoa(p.Number)+"/comments")
	if err != nil {
		return "", "", err
	}
	reviews, err := fetchPages[Artifact](ctx, s.github, s.github.repoPath()+"/pulls/"+strconv.Itoa(p.Number)+"/reviews")
	if err != nil {
		return "", "", err
	}
	clean := false
	for _, a := range comments {
		if !codex(a, true) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(a.Body), "<!-- codex-pull-request-review-summary -->") {
			ok, e := s.cleanSummary(ctx, p, state, a)
			if e != nil {
				return "", "", e
			}
			clean = clean || ok
			continue
		}
		created, e := time.Parse(time.RFC3339, a.Created)
		if e != nil || !created.After(state.Since) {
			continue
		}
		if a.Updated != a.Created {
			return "action_required", "An authenticated Codex result was edited; it cannot prove this review.", nil
		}
		match := reviewedCommit.FindStringSubmatch(a.Body)
		if len(match) != 2 {
			continue
		}
		var commit struct {
			SHA string `json:"sha"`
		}
		if err = s.github.call(ctx, "GET", s.github.repoPath()+"/commits/"+match[1], nil, &commit); err != nil {
			return "", "", err
		}
		if commit.SHA != state.Head {
			continue
		}
		if !cleanResult.MatchString(a.Body) {
			return "failure", "Authenticated Codex reported an unsupported result; inspect the review.", nil
		}
		clean = true
	}
	for _, a := range reviews {
		if !codex(a, false) || a.Commit != state.Head {
			continue
		}
		submitted, e := time.Parse(time.RFC3339, a.Submitted)
		if e != nil || !submitted.After(state.Since) {
			continue
		}
		return "failure", "Codex posted review findings on this head. Fix them and push a substantive new commit.", nil
	}
	if clean {
		return "success", "Native Codex found no major issues on the current head and unchanged PR context.", nil
	}
	return "", "Waiting for an authenticated current-head native Codex review.", nil
}
func (s *Server) publish(ctx context.Context, n int, state *PRState, result, reason string) error {
	external := fmt.Sprintf("sfl-org:%d:pr:%d:head:%s:base:%s", s.config.RepositoryID, n, state.Head, state.Base)
	if state.CheckID == 0 {
		var page struct {
			Total int `json:"total_count"`
			Runs  []struct {
				ID       int64  `json:"id"`
				App      App    `json:"app"`
				External string `json:"external_id"`
			} `json:"check_runs"`
		}
		for i := 1; i <= 100; i++ {
			if err := s.github.call(ctx, "GET", s.github.repoPath()+"/commits/"+state.Head+"/check-runs?filter=all&per_page=100&page="+strconv.Itoa(i), nil, &page); err != nil {
				return err
			}
			for _, c := range page.Runs {
				if c.App.ID == s.config.AppID && c.External == external {
					state.CheckID = c.ID
				}
			}
			if len(page.Runs) < 100 {
				break
			}
			if i == 100 {
				return fmt.Errorf("check pagination limit")
			}
		}
	}
	payload := map[string]any{"name": "SFL PR Reviewer", "head_sha": state.Head, "external_id": external, "output": map[string]string{"title": reason, "summary": "Organization App pilot in hs-buddy. Native Codex remains the review engine. No consumer Actions workflow or model credential is used. Base: `" + state.Base + "`."}}
	if result == "" && state.CheckID != 0 {
		result = "action_required"
	}
	if result == "" {
		payload["status"] = "in_progress"
	} else {
		payload["status"] = "completed"
		payload["conclusion"] = result
		payload["completed_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	method, path := "POST", s.github.repoPath()+"/check-runs"
	if state.CheckID != 0 {
		method = "PATCH"
		path += "/" + strconv.FormatInt(state.CheckID, 10)
		delete(payload, "head_sha")
	}
	var response struct {
		ID   int64  `json:"id"`
		App  App    `json:"app"`
		Head string `json:"head_sha"`
	}
	if err := s.github.call(ctx, method, path, payload, &response); err != nil {
		return err
	}
	if response.ID == 0 || response.App.ID != s.config.AppID || response.Head != state.Head {
		return fmt.Errorf("check response identity mismatch")
	}
	state.CheckID = response.ID
	return s.persistPull(n, state)
}
func main() {
	config := flag.String("config", "", "absolute configuration path")
	flag.Parse()
	if *config == "" {
		log.Fatal("--config is required")
	}
	c, e := readConfig(*config)
	if e != nil {
		log.Fatal(e)
	}
	lock, e := lockState(c.StateDirectory)
	if e != nil {
		log.Fatal(e)
	}
	defer lock.Close()
	s, e := newServer(c, *config)
	if e != nil {
		log.Fatal(e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.run(ctx)
	httpServer := &http.Server{Addr: c.Listen, Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	log.Printf("SFL reviewer listening on loopback, pilot enabled=%t", c.Enabled)
	if e = httpServer.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}

func (s *Server) cleanSummary(ctx context.Context, p Pull, state PRState, a Artifact) (bool, error) {
	matches := summaryResult.FindAllStringSubmatch(a.Body, -1)
	if len(matches) != 1 {
		return false, nil
	}
	completed, e := time.Parse(time.RFC3339Nano, matches[0][1])
	if e != nil || !completed.After(state.Since) {
		return false, nil
	}
	updated, e := time.Parse(time.RFC3339, a.Updated)
	if e != nil || completed.After(updated.Add(time.Second)) {
		return false, nil
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if e = s.github.call(ctx, "GET", s.github.repoPath()+"/commits/"+matches[0][2], nil, &commit); e != nil {
		return false, e
	}
	if commit.SHA != state.Head {
		return false, nil
	}
	editor, e := s.github.summaryEditor(ctx, a)
	if e != nil || !editor {
		return false, e
	}
	reactions, e := fetchPages[Reaction](ctx, s.github, s.github.repoPath()+"/issues/"+strconv.Itoa(p.Number)+"/reactions")
	if e != nil {
		return false, e
	}
	positive := false
	for _, r := range reactions {
		if r.User.ID != 199175422 || r.User.Login != "chatgpt-codex-connector[bot]" {
			continue
		}
		if r.Content == "eyes" {
			return false, nil
		}
		created, e := time.Parse(time.RFC3339, r.Created)
		if e == nil && r.Content == "+1" && created.After(state.Since) && !created.Before(completed.Truncate(time.Second)) {
			positive = true
		}
	}
	return positive, nil
}
func (s *Server) schedulePolls() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	changed := false
	for n, p := range s.state.Pulls {
		if p.Closed || p.NextPoll.After(now) {
			continue
		}
		exists := false
		for _, j := range s.state.Jobs {
			var e Event
			if j.Kind == "reconcile" && json.Unmarshal(j.Payload, &e) == nil && e.Pull.Number == n {
				exists = true
			}
		}
		if exists {
			continue
		}
		if len(s.state.Jobs) >= 1000 {
			return fmt.Errorf("poll queue full")
		}
		event := Event{}
		event.Pull.Number = n
		raw, _ := json.Marshal(event)
		s.state.Jobs = append(s.state.Jobs, Job{ID: fmt.Sprintf("poll-%d-%d", n, now.UnixNano()), Kind: "reconcile", Payload: raw, Received: now})
		p.NextPoll = now.Add(15 * time.Second)
		changed = true
	}
	if changed {
		return atomicJSON(s.statePath, s.state)
	}
	return nil
}
