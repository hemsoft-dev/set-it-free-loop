package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptrace"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrGitHubInstallation deliberately excludes credential and remote error text.
var ErrGitHubInstallation = errors.New("selected GitHub installation qualification required")

type GitHubCredentialRole string

const (
	GitHubReader    GitHubCredentialRole = "reader"
	GitHubPublisher GitHubCredentialRole = "publisher"
)

func githubPermissions(role GitHubCredentialRole) map[string]string {
	switch role {
	case GitHubReader:
		return map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read"}
	case GitHubPublisher:
		return map[string]string{"metadata": "read", "pull_requests": "write"}
	}
	return nil
}

// GitHubInstallationOptions is supplied only by the trusted activation layer.
// No environment, key store, default network transport or JWT signer is used.
// Invoking AcquireGitHubInstallation performs HTTP; construction is not an
// activation grant. Offline callers supply synthetic JWTs and local transports.
type GitHubInstallationOptions struct {
	Enrollment Enrollment
	Role       GitHubCredentialRole
	AppJWT     []byte
	Transport  http.RoundTripper
	Now        func() time.Time
}

type GitHubInstallationReceipt struct {
	Enrollment           Enrollment
	Role                 GitHubCredentialRole
	CheckedAt, ExpiresAt time.Time
}

// GitHubInstallationTransport owns one role's credential. Give only the reader
// to discovery/object acquisition and only the publisher to publication. Neither
// transport belongs in a worker, source reader, model prompt or public status.
type GitHubInstallationTransport struct{ state *githubInstallationState }

type githubInstallationState struct {
	mu      sync.Mutex
	token   []byte
	inner   http.RoundTripper
	now     func() time.Time
	receipt GitHubInstallationReceipt
}

func (GitHubInstallationTransport) String() string   { return "GitHubInstallationTransport{redacted}" }
func (GitHubInstallationTransport) GoString() string { return "GitHubInstallationTransport{redacted}" }
func (GitHubInstallationTransport) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubInstallationTransport{redacted}"`), nil
}
func (t *GitHubInstallationTransport) Receipt() GitHubInstallationReceipt {
	if t == nil || t.state == nil {
		return GitHubInstallationReceipt{}
	}
	return t.state.receipt
}
func (t *GitHubInstallationTransport) Close() error {
	if t != nil && t.state != nil {
		t.state.mu.Lock()
		defer t.state.mu.Unlock()
		clear(t.state.token)
		t.state.token = nil
	}
	return nil
}

var githubPublisherPath = regexp.MustCompile(`^/repos/hemsoft-dev/set-it-free-loop/(pulls/[1-9][0-9]*/reviews(/[1-9][0-9]*)?|issues/[1-9][0-9]*/comments)$`)

func githubAmbientAuthorization(header http.Header) bool {
	for name := range header {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
			return true
		}
	}
	return false
}

func (t *GitHubInstallationTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	delegated := false
	defer func() {
		if !delegated && q != nil && q.Body != nil {
			q.Body.Close()
		}
	}()
	if t == nil || t.state == nil || q == nil || q.URL == nil || q.URL.Scheme != "https" || q.URL.Host != "api.github.com" || q.URL.User != nil || q.URL.Opaque != "" || q.URL.Fragment != "" || (q.Host != "" && q.Host != "api.github.com") || githubAmbientAuthorization(q.Header) || githubAmbientAuthorization(q.Trailer) || httptrace.ContextClientTrace(q.Context()) != nil {
		return nil, ErrGitHubInstallation
	}
	p := q.URL.Path
	if path.Clean(p) != p || (p != githubRepoPath && !strings.HasPrefix(p, githubRepoPath+"/")) {
		return nil, ErrGitHubInstallation
	}
	if q.Method != http.MethodGet {
		match := githubPublisherPath.MatchString(p)
		post := q.Method == http.MethodPost && (strings.HasSuffix(p, "/reviews") || strings.HasSuffix(p, "/comments"))
		put := q.Method == http.MethodPut && strings.Contains(p, "/reviews/")
		if t.state.receipt.Role != GitHubPublisher || !match || (!post && !put) || q.GetBody != nil {
			return nil, ErrGitHubInstallation
		}
	}
	t.state.mu.Lock()
	if len(t.state.token) == 0 || !t.state.now().Before(t.state.receipt.ExpiresAt.Add(-time.Minute)) {
		t.state.mu.Unlock()
		return nil, ErrGitHubInstallation
	}
	clone := q.Clone(q.Context())
	clone.Header.Set("Authorization", "Bearer "+string(t.state.token))
	t.state.mu.Unlock()
	delegated = true // The trusted inner RoundTripper now owns Body on every path.
	response, err := t.state.inner.RoundTrip(clone)
	// The caller's request never gains Authorization, including on errors. Avoid
	// retaining it in Response.Request or forwarding arbitrary transport errors.
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, ErrGitHubInstallation
	}
	if response == nil || response.Body == nil {
		return nil, ErrGitHubInstallation
	}
	response.Request = q
	return response, nil
}

type githubInstallation struct {
	ID                  int64
	AppID               int64  `json:"app_id"`
	TargetID            int64  `json:"target_id"`
	TargetType          string `json:"target_type"`
	RepositorySelection string `json:"repository_selection"`
	Account             struct {
		ID          int64
		Login, Type string
	}
	Permissions map[string]string
	SuspendedAt json.RawMessage `json:"suspended_at"`
}

func (i githubInstallation) valid(e Enrollment, orgID int64) bool {
	return orgID > 0 && i.ID == e.InstallationID && i.AppID == e.AppID && i.TargetID == orgID && i.TargetType == "Organization" && i.Account.ID == orgID && i.Account.Login == "hemsoft-dev" && i.Account.Type == "Organization" && i.RepositorySelection == "selected" && string(i.SuspendedAt) == "null" && maps.Equal(i.Permissions, map[string]string{"metadata": "read", "contents": "read", "pull_requests": "write"})
}

type githubInstallationRepo struct {
	ID       int64
	FullName string `json:"full_name"`
	Owner    struct {
		ID          int64
		Login, Type string
	}
}

func (r githubInstallationRepo) valid() bool {
	return r.ID == RepositoryID && r.FullName == "hemsoft-dev/set-it-free-loop" && r.Owner.ID > 0 && r.Owner.Login == "hemsoft-dev" && r.Owner.Type == "Organization"
}

// AcquireGitHubInstallation attests the authenticated App, immutable bot,
// repository and selected installation, then requests exactly one repository
// and one role's minimal permissions. It never retries POST or follows redirects.
// Caller cancellation/deadlines are honored; the complete handshake is bounded.
func AcquireGitHubInstallation(ctx context.Context, o GitHubInstallationOptions) (*GitHubInstallationTransport, error) {
	want := githubPermissions(o.Role)
	if o.Enrollment.Validate() != nil || want == nil || !githubCredential(o.AppJWT) || o.Transport == nil || httptrace.ContextClientTrace(ctx) != nil {
		return nil, ErrGitHubInstallation
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	jwt := append([]byte(nil), o.AppJWT...)
	defer clear(jwt)
	client := &http.Client{Transport: o.Transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var app struct {
		ID   int64
		Slug string
	}
	if installationRequest(ctx, client, jwt, "GET", "/app", nil, 200, &app) != nil || app.ID != o.Enrollment.AppID || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`).MatchString(app.Slug) {
		return nil, ErrGitHubInstallation
	}
	var before githubInstallation
	if installationRequest(ctx, client, jwt, "GET", githubRepoPath+"/installation", nil, 200, &before) != nil || !before.valid(o.Enrollment, before.Account.ID) {
		return nil, ErrGitHubInstallation
	}
	var minted struct {
		Token               string
		ExpiresAt           time.Time `json:"expires_at"`
		Permissions         map[string]string
		RepositorySelection string `json:"repository_selection"`
		Repositories        []githubInstallationRepo
	}
	endpoint := "/app/installations/" + strconv.FormatInt(o.Enrollment.InstallationID, 10)
	body := map[string]any{"repository_ids": []int64{RepositoryID}, "permissions": want}
	started := o.Now()
	if installationRequest(ctx, client, jwt, "POST", endpoint+"/access_tokens", body, 201, &minted) != nil {
		return nil, ErrGitHubInstallation
	}
	// String fields in JSON cannot be reliably erased by Go. Never log the raw
	// document or retain it beyond this private handshake.
	token := []byte(minted.Token)
	minted.Token = ""
	defer clear(token)
	if !githubCredential(token) || !maps.Equal(minted.Permissions, want) || minted.RepositorySelection != "selected" || len(minted.Repositories) != 1 || !minted.Repositories[0].valid() || minted.Repositories[0].Owner.ID != before.Account.ID || !minted.ExpiresAt.After(o.Now().Add(2*time.Minute)) || minted.ExpiresAt.After(started.Add(time.Hour+time.Minute)) {
		return nil, ErrGitHubInstallation
	}
	var repos struct {
		TotalCount   int `json:"total_count"`
		Repositories []githubInstallationRepo
	}
	if installationRequest(ctx, client, token, "GET", "/installation/repositories?per_page=100&page=1", nil, 200, &repos) != nil || repos.TotalCount != 1 || len(repos.Repositories) != 1 || !repos.Repositories[0].valid() || repos.Repositories[0].Owner.ID != before.Account.ID {
		return nil, ErrGitHubInstallation
	}
	var bot struct {
		ID          int64
		Login, Type string
	}
	if installationRequest(ctx, client, token, "GET", "/users/"+app.Slug+"%5Bbot%5D", nil, 200, &bot) != nil || bot.ID != o.Enrollment.BotID || bot.Type != "Bot" || bot.Login != app.Slug+"[bot]" {
		return nil, ErrGitHubInstallation
	}
	var repo githubInstallationRepo
	var after githubInstallation
	if installationRequest(ctx, client, token, "GET", githubRepoPath, nil, 200, &repo) != nil || !repo.valid() || repo.Owner.ID != before.Account.ID || installationRequest(ctx, client, jwt, "GET", endpoint, nil, 200, &after) != nil || !after.valid(o.Enrollment, repo.Owner.ID) || !o.Now().Before(minted.ExpiresAt.Add(-time.Minute)) || ctx.Err() != nil {
		return nil, ErrGitHubInstallation
	}
	return &GitHubInstallationTransport{state: &githubInstallationState{token: append([]byte(nil), token...), inner: o.Transport, now: o.Now, receipt: GitHubInstallationReceipt{o.Enrollment, o.Role, o.Now(), minted.ExpiresAt}}}, nil
}

func githubCredential(value []byte) bool {
	if len(value) == 0 || len(value) > 8192 {
		return false
	}
	for _, b := range value {
		if b < 33 || b > 126 {
			return false
		}
	}
	return true
}

func installationRequest(ctx context.Context, client *http.Client, credential []byte, method, endpoint string, body any, status int, out any) error {
	var input io.ReadCloser
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return ErrGitHubInstallation
		}
		input = io.NopCloser(bytes.NewReader(data))
	}
	q, err := http.NewRequestWithContext(ctx, method, githubOrigin+endpoint, input)
	if err != nil {
		return ErrGitHubInstallation
	}
	q.Header.Set("Authorization", "Bearer "+string(credential))
	q.Header.Set("Accept", "application/vnd.github+json")
	q.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	q.Header.Set("User-Agent", "sfl-offline-alpha-installation")
	if input != nil {
		q.Header.Set("Content-Type", "application/json")
	}
	r, err := client.Do(q)
	if err != nil {
		return ErrGitHubInstallation
	}
	defer r.Body.Close()
	if r.StatusCode != status || r.Header.Get("Link") != "" {
		return ErrGitHubInstallation
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	defer clear(raw)
	if err != nil || len(raw) > 1<<20 || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil || json.Unmarshal(raw, out) != nil {
		return ErrGitHubInstallation
	}
	return nil
}
