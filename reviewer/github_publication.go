package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const githubOrigin = "https://api.github.com"
const githubRepoPath = "/repos/hemsoft-dev/set-it-free-loop"

// Four maximum-sized raw comment bodies, even with six-byte JSON escaping,
// leave room for ordinary API metadata under the two-MiB read cap. Keep the
// original ten-thousand-item scan ceiling while requesting smaller pages.
const githubPageSize = 4
const githubMaxPages = 2500

// Every accepted document is below 750 KiB before encoding. JSON can expand
// each input byte to at most six bytes; eight MiB covers all accepted writes.
const githubWriteBytes = 8 << 20

// GitHubPublicationReader supplies separately qualified native-request history.
// This adapter does not infer requester attribution from pending reviewer names.
type GitHubPublicationReader interface {
	Current(context.Context, Scope) (RemoteState, error)
	History(context.Context, Scope) (Discovery, error)
}

// GitHubPublicationHTTP owns only publisher HTTP access. Construction neither
// obtains credentials nor starts polling. A future activation layer must attest
// the selected-repository installation token attached by its trusted transport.
// No default transport, environment proxy, cookie jar or redirect is inherited.
type GitHubPublicationHTTP struct {
	enrollment Enrollment
	reader     GitHubPublicationReader
	client     *http.Client
}

func NewGitHubPublicationHTTP(e Enrollment, reader GitHubPublicationReader, transport http.RoundTripper) (*GitHubPublicationHTTP, error) {
	if e.Validate() != nil || reader == nil || transport == nil {
		return nil, ErrIgnored
	}
	return &GitHubPublicationHTTP{e, reader, &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (g *GitHubPublicationHTTP) Current(ctx context.Context, scope Scope) (RemoteState, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !g.enrollment.accepts(scope) {
		return RemoteState{}, ErrIgnored
	}
	s, err := g.reader.Current(ctx, scope)
	if err != nil {
		return RemoteState{}, err
	}
	if s.Access && s.Open && (s.Snapshot.Validate() != nil || s.Snapshot.PR != scope.PR || s.Snapshot.RepositoryID != scope.RepositoryID) {
		return RemoteState{}, ErrDeferred
	}
	return s, nil
}
func (g *GitHubPublicationHTTP) History(ctx context.Context, scope Scope) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !g.enrollment.accepts(scope) {
		return Discovery{}, ErrIgnored
	}
	d, err := g.reader.History(ctx, scope)
	if err != nil {
		return Discovery{}, err
	}
	if d.Scope != scope || !d.Complete || d.Through.IsZero() {
		return Discovery{}, ErrDeferred
	}
	return d, nil
}

// Only status and bounded delay are exposed. GitHub error bodies can contain
// submitted content; they must not enter controller logs or public status.
type GitHubHTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *GitHubHTTPError) Error() string { return fmt.Sprintf("GitHub HTTP status %d", e.Status) }

func (g *GitHubPublicationHTTP) request(ctx context.Context, method, path string, payload any, status int, out any) (http.Header, error) {
	var body io.ReadCloser
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil || len(raw) > githubWriteBytes {
			return nil, ErrDeferred
		}
		// No GetBody, idempotency key or automatic application retry for writes.
		body = io.NopCloser(bytes.NewReader(raw))
	}
	q, err := http.NewRequestWithContext(ctx, method, githubOrigin+path, body)
	if err != nil {
		return nil, ErrDeferred
	}
	q.Header.Set("Accept", "application/vnd.github+json")
	q.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	q.Header.Set("User-Agent", "sfl-offline-alpha-publication")
	if payload != nil {
		q.Header.Set("Content-Type", "application/json")
	}
	r, err := g.client.Do(q)
	if err != nil {
		return nil, ErrDeferred
	}
	defer r.Body.Close()
	if r.StatusCode != status {
		delay := time.Duration(0)
		if seconds, err := strconv.Atoi(r.Header.Get("Retry-After")); err == nil && seconds > 0 && seconds <= 3600 {
			delay = time.Duration(seconds) * time.Second
		}
		return nil, &GitHubHTTPError{r.StatusCode, delay}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
		return nil, ErrDeferred
	}
	if json.Unmarshal(raw, out) != nil {
		return nil, ErrDeferred
	}
	return r.Header, nil
}
func (g *GitHubPublicationHTTP) repository(ctx context.Context, scope Scope) error {
	if !g.enrollment.accepts(scope) {
		return ErrIgnored
	}
	var repo struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	_, err := g.request(ctx, "GET", githubRepoPath, nil, 200, &repo)
	if err != nil {
		return err
	}
	if repo.ID != g.enrollment.RepositoryID || repo.FullName != "hemsoft-dev/set-it-free-loop" {
		return ErrIgnored
	}
	return nil
}
func githubPR(scope Scope) string    { return githubRepoPath + "/pulls/" + strconv.Itoa(scope.PR) }
func githubIssue(scope Scope) string { return githubRepoPath + "/issues/" + strconv.Itoa(scope.PR) }

// nextPage accepts only a monotonically increasing next link for this exact
// collection, on the fixed origin. Never follow an API-supplied arbitrary URL.
func nextGitHubPage(header http.Header, collection string, page int) (bool, error) {
	found := false
	for _, line := range header.Values("Link") {
		for _, part := range strings.Split(line, ",") {
			fields := strings.Split(strings.TrimSpace(part), ";")
			if len(fields) != 2 {
				return false, ErrDeferred
			}
			rel := strings.TrimSpace(fields[1])
			switch rel {
			case `rel="first"`, `rel="prev"`, `rel="last"`:
				continue
			case `rel="next"`:
			default:
				return false, ErrDeferred
			}
			if found || page >= githubMaxPages {
				return false, ErrDeferred
			}
			found = true
			u, err := url.Parse(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(fields[0]), "<"), ">"))
			if err != nil || u.Scheme != "https" || u.Host != "api.github.com" || u.User != nil || (u.Path != collection && u.Path != "/repositories/"+strconv.FormatInt(RepositoryID, 10)+strings.TrimPrefix(collection, githubRepoPath)) || u.Fragment != "" {
				return false, ErrDeferred
			}
			q, queryErr := url.ParseQuery(u.RawQuery)
			if queryErr != nil || len(q) != 2 || len(q["page"]) != 1 || q.Get("page") != strconv.Itoa(page+1) || len(q["per_page"]) != 1 || q.Get("per_page") != strconv.Itoa(githubPageSize) {
				return false, ErrDeferred
			}
		}
	}
	return found, nil
}
func githubPages[T any](ctx context.Context, g *GitHubPublicationHTTP, path string) ([]T, error) {
	all := []T{}
	for page := 1; page <= githubMaxPages; page++ {
		var items []T
		h, err := g.request(ctx, "GET", path+"?per_page="+strconv.Itoa(githubPageSize)+"&page="+strconv.Itoa(page), nil, 200, &items)
		if err != nil || items == nil || len(items) > githubPageSize {
			return nil, errors.Join(ErrDeferred, err)
		}
		all = append(all, items...)
		next, err := nextGitHubPage(h, path, page)
		if err != nil {
			return nil, err
		}
		if !next {
			return all, nil
		}
	}
	return nil, ErrDeferred
}

type githubAuthor struct {
	ID int64 `json:"id"`
}
type githubReview struct {
	ID             int64        `json:"id"`
	User           githubAuthor `json:"user"`
	Body, State    string
	CommitID       string    `json:"commit_id"`
	PullRequestURL string    `json:"pull_request_url"`
	SubmittedAt    time.Time `json:"submitted_at"`
}
type githubComment struct {
	ID               int64        `json:"id"`
	User             githubAuthor `json:"user"`
	Body             string       `json:"body"`
	IssueURL         string       `json:"issue_url"`
	CreatedAt        time.Time    `json:"created_at"`
	ReviewID         int64        `json:"pull_request_review_id"`
	Path, Side       string
	OriginalLine     int    `json:"original_line"`
	OriginalCommitID string `json:"original_commit_id"`
	InReplyToID      int64  `json:"in_reply_to_id"`
}

var publicationMarker = regexp.MustCompile(`<!-- sfl-alpha:[0-9a-f]{32} -->`)

func documentMarker(body string) string {
	all := publicationMarker.FindAllString(body, -1)
	if len(all) != 1 {
		return ""
	}
	return all[0]
}
func (g *GitHubPublicationHTTP) reviewReceipt(ctx context.Context, scope Scope, r githubReview) (RemoteReceipt, error) {
	if r.ID <= 0 || r.User.ID != g.enrollment.BotID || r.State != "COMMENTED" || !objectID.MatchString(r.CommitID) || r.SubmittedAt.IsZero() || r.PullRequestURL != githubOrigin+githubPR(scope) {
		return RemoteReceipt{}, ErrDeferred
	}
	marker := documentMarker(r.Body)
	if marker == "" {
		return RemoteReceipt{}, ErrDeferred
	}
	comments, err := githubPages[githubComment](ctx, g, githubPR(scope)+"/reviews/"+strconv.FormatInt(r.ID, 10)+"/comments")
	if err != nil || len(comments) > 40 {
		return RemoteReceipt{}, errors.Join(ErrDeferred, err)
	}
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	doc := Document{Summary: r.Body, Inline: []InlineComment{}}
	var previous int64
	for _, c := range comments {
		if c.ID <= previous || c.User.ID != g.enrollment.BotID || c.ReviewID != r.ID || c.OriginalCommitID != r.CommitID || c.Side != "RIGHT" || c.OriginalLine <= 0 || !validPath(c.Path) || len(c.Body) == 0 || len(c.Body) > 16<<10 || c.InReplyToID != 0 {
			return RemoteReceipt{}, ErrDeferred
		}
		previous = c.ID
		doc.Inline = append(doc.Inline, InlineComment{c.Path, c.OriginalLine, c.Body})
	}
	if len(doc.Summary) > 66<<10 {
		return RemoteReceipt{}, ErrDeferred
	}
	return RemoteReceipt{ID: r.ID, AuthorID: r.User.ID, Marker: marker, BodyHash: Digest(canonicalPublicationDocument(doc)), CreatedAt: r.SubmittedAt, CommitID: r.CommitID}, nil
}
func (g *GitHubPublicationHTTP) ackReceipt(scope Scope, c githubComment) (RemoteReceipt, error) {
	marker := documentMarker(c.Body)
	if c.ID <= 0 || c.User.ID != g.enrollment.BotID || c.CreatedAt.IsZero() || c.IssueURL != githubOrigin+githubIssue(scope) || marker == "" || len(c.Body) > 65<<10 {
		return RemoteReceipt{}, ErrDeferred
	}
	return RemoteReceipt{ID: c.ID, AuthorID: c.User.ID, Marker: marker, BodyHash: Digest(Document{Summary: c.Body, Inline: []InlineComment{}}), CreatedAt: c.CreatedAt}, nil
}
func validHTTPIntent(w WriteIntent) bool {
	if w.Generation <= 0 || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(w.ID) || w.Marker != "<!-- sfl-alpha:"+w.ID+" -->" || documentMarker(w.Document.Summary) != w.Marker || Digest(w.Document) != w.BodyHash || len(w.Document.Summary) > 65<<10 || len(w.Document.Inline) > 40 || (w.State != "prepared" && w.State != "dispatched") {
		return false
	}
	if w.Kind == "ack" {
		return len(w.Document.Inline) == 0
	}
	if w.Kind != "review" || w.RequestID <= 0 || w.Snapshot.Validate() != nil || w.Snapshot.RepositoryID != w.Scope.RepositoryID || w.Snapshot.PR != w.Scope.PR {
		return false
	}
	for _, c := range w.Document.Inline {
		if !validPath(c.Path) || c.Line <= 0 || len(c.Body) == 0 || len(c.Body) > 16<<10 {
			return false
		}
	}
	return true
}
func (g *GitHubPublicationHTTP) Post(ctx context.Context, w WriteIntent) (RemoteReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !g.enrollment.accepts(w.Scope) || !validHTTPIntent(w) {
		return RemoteReceipt{}, ErrIgnored
	}
	if err := g.repository(ctx, w.Scope); err != nil {
		return RemoteReceipt{}, err
	}
	if w.Kind == "ack" {
		var c githubComment
		_, err := g.request(ctx, "POST", githubIssue(w.Scope)+"/comments", map[string]any{"body": w.Document.Summary}, 201, &c)
		if err != nil {
			return RemoteReceipt{}, err
		}
		r, err := g.ackReceipt(w.Scope, c)
		if err != nil || !validateReceipt(w, r, g.enrollment.BotID) {
			return RemoteReceipt{}, ErrDeferred
		}
		return r, nil
	}
	comments := []map[string]any{}
	for _, c := range w.Document.Inline {
		comments = append(comments, map[string]any{"path": c.Path, "line": c.Line, "side": "RIGHT", "body": c.Body})
	}
	var review githubReview
	_, err := g.request(ctx, "POST", githubPR(w.Scope)+"/reviews", map[string]any{"event": "COMMENT", "commit_id": w.Snapshot.Head, "body": w.Document.Summary, "comments": comments}, 200, &review)
	if err != nil {
		return RemoteReceipt{}, err
	}
	r, err := g.reviewReceipt(ctx, w.Scope, review)
	if err != nil || !validateReceipt(w, r, g.enrollment.BotID) {
		return RemoteReceipt{}, ErrDeferred
	}
	return r, nil
}
func (g *GitHubPublicationHTTP) Scan(ctx context.Context, scope Scope, kind string) ([]RemoteReceipt, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !g.enrollment.accepts(scope) || (kind != "ack" && kind != "review") {
		return nil, false, ErrIgnored
	}
	if err := g.repository(ctx, scope); err != nil {
		return nil, false, err
	}
	result := []RemoteReceipt{}
	seen := map[int64]bool{}
	if kind == "ack" {
		items, err := githubPages[githubComment](ctx, g, githubIssue(scope)+"/comments")
		if err != nil {
			return nil, false, err
		}
		for _, c := range items {
			if c.ID <= 0 || seen[c.ID] {
				return nil, false, ErrDeferred
			}
			seen[c.ID] = true
			if c.User.ID != g.enrollment.BotID || !strings.Contains(c.Body, "<!-- sfl-alpha:") {
				continue
			}
			r, err := g.ackReceipt(scope, c)
			if err != nil {
				return nil, false, err
			}
			result = append(result, r)
		}
	} else {
		items, err := githubPages[githubReview](ctx, g, githubPR(scope)+"/reviews")
		if err != nil {
			return nil, false, err
		}
		for _, v := range items {
			if v.ID <= 0 || seen[v.ID] {
				return nil, false, ErrDeferred
			}
			seen[v.ID] = true
			if v.User.ID != g.enrollment.BotID || !strings.Contains(v.Body, "<!-- sfl-alpha:") {
				continue
			}
			r, err := g.reviewReceipt(ctx, scope, v)
			if err != nil {
				return nil, false, err
			}
			result = append(result, r)
		}
	}
	return result, true, nil
}

func (g *GitHubPublicationHTTP) Annotate(ctx context.Context, scope Scope, id int64, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !g.enrollment.accepts(scope) || id <= 0 || len(reason) == 0 || len(reason) > 1024 {
		return ErrIgnored
	}
	if err := g.repository(ctx, scope); err != nil {
		return err
	}
	path := githubPR(scope) + "/reviews/" + strconv.FormatInt(id, 10)
	var review githubReview
	if _, err := g.request(ctx, "GET", path, nil, 200, &review); err != nil {
		return err
	}
	if review.ID != id || review.User.ID != g.enrollment.BotID || review.PullRequestURL != githubOrigin+githubPR(scope) || review.State != "COMMENTED" || documentMarker(review.Body) == "" {
		return ErrDeferred
	}
	annotation := "\n\n<!-- sfl-alpha-supersession -->\nSUPERSEDED: " + safeText(reason)
	if strings.HasSuffix(review.Body, annotation) {
		return nil
	}
	// Never replace another recorded supersession with a different reason.
	if strings.Contains(review.Body, "<!-- sfl-alpha-supersession -->") || len(review.Body)+len(annotation) > 66<<10 {
		return ErrDeferred
	}
	var updated githubReview
	if _, err := g.request(ctx, "PUT", path, map[string]any{"body": review.Body + annotation}, 200, &updated); err != nil {
		return err
	}
	if updated.ID != id || updated.User.ID != g.enrollment.BotID || updated.PullRequestURL != review.PullRequestURL || updated.CommitID != review.CommitID || updated.State != "COMMENTED" || updated.Body != review.Body+annotation {
		return ErrDeferred
	}
	return nil
}
