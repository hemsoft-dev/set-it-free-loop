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

// One hundred maximum-sized bodies with six-byte JSON escaping fit the
// collection-only 48-MiB cap. Other responses retain the two-MiB cap.
const githubPageSize = 100
const githubMaxPages = 100
const githubCollectionBytes = 48 << 20
const githubScanTimeout = 25 * time.Minute

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
	ctx, cancel := context.WithTimeout(ctx, githubDiscoveryTimeout)
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
	ctx, cancel := context.WithTimeout(ctx, githubDiscoveryTimeout)
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
	return g.requestBounded(ctx, method, path, payload, status, out, 2<<20)
}
func (g *GitHubPublicationHTTP) requestBounded(ctx context.Context, method, path string, payload any, status int, out any, limit int64) (http.Header, error) {
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
		if delay == 0 && r.Header.Get("X-Ratelimit-Remaining") == "0" {
			reset, resetErr := strconv.ParseInt(r.Header.Get("X-Ratelimit-Reset"), 10, 64)
			serverTime, dateErr := http.ParseTime(r.Header.Get("Date"))
			if resetErr == nil && dateErr == nil {
				until := time.Unix(reset, 0).Sub(serverTime)
				if until > 0 && until <= time.Hour {
					delay = until
				}
			}
		}
		return nil, &GitHubHTTPError{r.StatusCode, delay}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(raw)) > limit || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
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
func visitGitHubPages[T any](ctx context.Context, g *GitHubPublicationHTTP, path string, visit func(T) error) error {
	for page := 1; page <= githubMaxPages; page++ {
		var items []T
		h, err := g.requestBounded(ctx, "GET", path+"?per_page="+strconv.Itoa(githubPageSize)+"&page="+strconv.Itoa(page), nil, 200, &items, githubCollectionBytes)
		if err != nil || items == nil || len(items) > githubPageSize {
			return errors.Join(ErrDeferred, err)
		}
		next, err := nextGitHubPage(h, path, page)
		if err != nil {
			return err
		}
		if next && len(items) == 0 {
			return ErrDeferred
		}
		for _, item := range items {
			if err := visit(item); err != nil {
				return err
			}
		}
		if !next {
			return nil
		}
	}
	return ErrDeferred
}

// Only a direct POST validation rejection proves that this attempt wrote
// nothing. A 422 from a later receipt GET must remain an unknown write.
func rejectedGitHubPost(err error) error {
	var status *GitHubHTTPError
	if errors.As(err, &status) && status.Status == http.StatusUnprocessableEntity {
		return &publicationRejected{err}
	}
	return err
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
	comments := []githubComment{}
	err := visitGitHubPages(ctx, g, githubPR(scope)+"/reviews/"+strconv.FormatInt(r.ID, 10)+"/comments", func(c githubComment) error {
		if len(comments) == 40 {
			return ErrDeferred
		}
		comments = append(comments, c)
		return nil
	})
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
			return RemoteReceipt{}, rejectedGitHubPost(err)
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
		return RemoteReceipt{}, rejectedGitHubPost(err)
	}
	r, err := g.reviewReceipt(ctx, w.Scope, review)
	if err != nil || !validateReceipt(w, r, g.enrollment.BotID) {
		return RemoteReceipt{}, ErrDeferred
	}
	return r, nil
}

// ScanIntent exhausts the entire collection but expands only the requested
// marker. At most 100 collection pages, one repository lookup and forty
// nonempty inline pages fit within twenty-five minutes at the ten-second
// request limit. Two matching markers fail closed, so historical App reviews cannot
// multiply the request budget or the retained body memory.
func (g *GitHubPublicationHTTP) ScanIntent(ctx context.Context, w WriteIntent) ([]RemoteReceipt, bool, error) {
	if w.Marker != "<!-- sfl-alpha:"+w.ID+" -->" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(w.ID) {
		return nil, false, ErrIgnored
	}
	return g.scan(ctx, w.Scope, w.Kind, w.Marker)
}
func (g *GitHubPublicationHTTP) Scan(ctx context.Context, scope Scope, kind string) ([]RemoteReceipt, bool, error) {
	return g.scan(ctx, scope, kind, "")
}
func (g *GitHubPublicationHTTP) scan(ctx context.Context, scope Scope, kind, marker string) ([]RemoteReceipt, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, githubScanTimeout)
	defer cancel()
	if !g.enrollment.accepts(scope) || (kind != "ack" && kind != "review") {
		return nil, false, ErrIgnored
	}
	if err := g.repository(ctx, scope); err != nil {
		return nil, false, err
	}
	result := []RemoteReceipt{}
	seen := map[int64]bool{}
	candidate := func(id, author int64, body string) (bool, error) {
		if id <= 0 || seen[id] {
			return false, ErrDeferred
		}
		seen[id] = true
		if author != g.enrollment.BotID || !strings.Contains(body, "<!-- sfl-alpha:") {
			return false, nil
		}
		if marker != "" {
			if !strings.Contains(body, marker) {
				return false, nil
			}
			if len(result) != 0 {
				return false, ErrDeferred
			}
		}
		return true, nil
	}
	var err error
	if kind == "ack" {
		err = visitGitHubPages(ctx, g, githubIssue(scope)+"/comments", func(c githubComment) error {
			ok, err := candidate(c.ID, c.User.ID, c.Body)
			if err != nil || !ok {
				return err
			}
			r, err := g.ackReceipt(scope, c)
			if err == nil {
				result = append(result, r)
			}
			return err
		})
	} else {
		err = visitGitHubPages(ctx, g, githubPR(scope)+"/reviews", func(v githubReview) error {
			ok, err := candidate(v.ID, v.User.ID, v.Body)
			if err != nil || !ok {
				return err
			}
			r, err := g.reviewReceipt(ctx, scope, v)
			if err == nil {
				result = append(result, r)
			}
			return err
		})
	}
	if err != nil {
		return nil, false, err
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
