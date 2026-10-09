package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type githubClient struct {
	config    Config
	client    *http.Client
	baseURL   string
	notBefore time.Time
	token     string
	expires   time.Time
}

func newGitHub(c Config) *githubClient {
	return &githubClient{config: c, baseURL: "https://api.github.com", client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (g *githubClient) jwt() (string, error) {
	raw, err := os.ReadFile(g.config.PrivateKeyFile)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", fmt.Errorf("invalid App key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		k, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return "", fmt.Errorf("invalid RSA App key")
		}
		var ok bool
		key, ok = k.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("App key must be RSA")
		}
	}
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(8 * time.Minute).Unix(), "iss": strconv.FormatInt(g.config.AppID, 10)})
	input := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

type rateLimitError struct{ Until time.Time }

func (e *rateLimitError) Error() string {
	return "GitHub rate limited until " + e.Until.UTC().Format(time.RFC3339)
}
func retryDeadline(resp *http.Response, now time.Time) time.Time {
	var until time.Time
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
		until = date
	}
	if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && resp.Header.Get("X-RateLimit-Remaining") == "0" && time.Unix(reset, 0).After(until) {
		until = time.Unix(reset, 0)
	}
	if !until.After(now) {
		until = now.Add(time.Minute)
	}
	return until.Add(time.Second)
}
func (g *githubClient) request(ctx context.Context, method, path, token string, input, output any) error {
	if time.Now().Before(g.notBefore) {
		return &rateLimitError{Until: g.notBefore}
	}
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sfl-org-reviewer")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0") {
		g.notBefore = retryDeadline(resp, time.Now())
		return &rateLimitError{Until: g.notBefore}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub %s %s returned HTTP %d", method, path, resp.StatusCode)
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(output)
	}
	return nil
}
func (g *githubClient) call(ctx context.Context, method, path string, input, output any) error {
	if time.Until(g.expires) < time.Minute {
		jwt, err := g.jwt()
		if err != nil {
			return err
		}
		var result struct {
			Token   string
			Expires time.Time `json:"expires_at"`
		}
		err = g.request(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", g.config.InstallationID), jwt, map[string]any{"repository_ids": []int64{g.config.RepositoryID}, "permissions": map[string]string{"checks": "write", "contents": "read", "pull_requests": "read", "issues": "read"}}, &result)
		if err != nil {
			return err
		}
		if result.Token == "" {
			return fmt.Errorf("missing installation token")
		}
		g.token = result.Token
		g.expires = result.Expires
	}
	return g.request(ctx, method, path, g.token, input, output)
}
func (g *githubClient) repoPath() string { return "/repos/" + g.config.Repository }
func fetchPages[T any](ctx context.Context, g *githubClient, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	all := []T{}
	for page := 1; page <= 100; page++ {
		var batch []T
		err := g.call(ctx, "GET", path+sep+"per_page=100&page="+strconv.Itoa(page), nil, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("GitHub pagination exceeds safety limit")
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}
type App struct {
	ID    int64  `json:"id"`
	Slug  string `json:"slug"`
	Owner User   `json:"owner"`
}
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}
type Pull struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Updated string `json:"updated_at"`
	Created string `json:"created_at"`
	URL     string `json:"html_url"`
	Head    struct {
		SHA  string     `json:"sha"`
		Repo Repository `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string     `json:"sha"`
		Ref  string     `json:"ref"`
		Repo Repository `json:"repo"`
	} `json:"base"`
}
type Artifact struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id"`
	User      User   `json:"user"`
	App       *App   `json:"performed_via_github_app"`
	Body      string `json:"body"`
	Commit    string `json:"commit_id"`
	State     string `json:"state"`
	Created   string `json:"created_at"`
	Submitted string `json:"submitted_at"`
	Updated   string `json:"updated_at"`
	URL       string `json:"html_url"`
}

func (g *githubClient) pull(ctx context.Context, n int) (Pull, Repository, error) {
	var p Pull
	var r Repository
	err := g.call(ctx, "GET", g.repoPath()+"/pulls/"+strconv.Itoa(n), nil, &p)
	if err == nil {
		err = g.call(ctx, "GET", g.repoPath(), nil, &r)
	}
	return p, r, err
}
func (g *githubClient) timeline(ctx context.Context, n int) (string, error) {
	events, err := fetchPages[json.RawMessage](ctx, g, g.repoPath()+"/issues/"+strconv.Itoa(n)+"/timeline")
	if err != nil {
		return "", err
	}
	selected := []json.RawMessage{}
	for _, raw := range events {
		var e struct {
			Event string `json:"event"`
		}
		if err = json.Unmarshal(raw, &e); err != nil {
			return "", err
		}
		switch e.Event {
		case "base_ref_changed", "head_ref_force_pushed", "head_ref_deleted", "closed", "reopened", "converted_to_draft", "ready_for_review":
			selected = append(selected, raw)
		}
	}
	data, _ := json.Marshal(selected)
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash), nil
}
func (g *githubClient) uniqueHead(ctx context.Context, p Pull) error {
	pulls, err := fetchPages[Pull](ctx, g, g.repoPath()+"/commits/"+url.PathEscape(p.Head.SHA)+"/pulls")
	if err != nil {
		return err
	}
	if len(pulls) != 1 || pulls[0].Number != p.Number {
		return fmt.Errorf("head is associated with multiple or unknown pull requests")
	}
	// The commit association endpoint omits closed-unmerged pull requests.
	// Independently scan their recorded heads rather than treating omission
	// as proof of exclusivity.
	owner, name, _ := strings.Cut(g.config.Repository, "/")
	cursor := ""
	for page := 0; page < 100; page++ {
		var response struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Repository *struct {
					Pulls struct {
						Nodes []struct {
							Number int    `json:"number"`
							Head   string `json:"headRefOid"`
						} `json:"nodes"`
						Page struct {
							More   bool   `json:"hasNextPage"`
							Cursor string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"pullRequests"`
				} `json:"repository"`
			} `json:"data"`
		}
		variables := map[string]any{"owner": owner, "name": name}
		if cursor != "" {
			variables["after"] = cursor
		}
		query := `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){pullRequests(states:[CLOSED],first:100,after:$after){nodes{number headRefOid} pageInfo{hasNextPage endCursor}}}}`
		if err = g.call(ctx, "POST", "/graphql", map[string]any{"query": query, "variables": variables}, &response); err != nil {
			return err
		}
		if len(response.Errors) != 0 || response.Data.Repository == nil {
			return fmt.Errorf("GitHub closed-head history query failed")
		}
		connection := response.Data.Repository.Pulls
		for _, other := range connection.Nodes {
			if other.Number != p.Number && other.Head == p.Head.SHA {
				return fmt.Errorf("head is also recorded on a closed-unmerged pull request")
			}
		}
		if !connection.Page.More {
			return nil
		}
		if connection.Page.Cursor == "" || connection.Page.Cursor == cursor {
			return fmt.Errorf("invalid closed-head pagination")
		}
		cursor = connection.Page.Cursor
	}
	return fmt.Errorf("closed-head history exceeds safety limit")
}

type Reaction struct {
	Content string `json:"content"`
	Created string `json:"created_at"`
	User    User   `json:"user"`
}

func (g *githubClient) codexCommentProvenance(ctx context.Context, a Artifact) (bool, error) {
	if a.NodeID == "" {
		return false, nil
	}
	var result struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Node *struct {
				ID       string `json:"id"`
				Database string `json:"fullDatabaseId"`
				Editor   *struct {
					ID    int64  `json:"databaseId"`
					Login string `json:"login"`
				} `json:"editor"`
				Author struct {
					ID    int64  `json:"databaseId"`
					Login string `json:"login"`
				} `json:"author"`
			} `json:"node"`
		} `json:"data"`
	}
	query := `query($id:ID!){node(id:$id){... on IssueComment{id fullDatabaseId editor{login ... on Bot{databaseId}} author{login ... on Bot{databaseId}}}}}`
	if e := g.call(ctx, "POST", "/graphql", map[string]any{"query": query, "variables": map[string]string{"id": a.NodeID}}, &result); e != nil {
		return false, e
	}
	if len(result.Errors) > 0 {
		return false, fmt.Errorf("GitHub summary provenance query failed")
	}
	n := result.Data.Node
	if n == nil || n.ID != a.NodeID || n.Database != strconv.FormatInt(a.ID, 10) || n.Author.ID != 199175422 || n.Author.Login != "chatgpt-codex-connector" {
		return false, nil
	}
	if n.Editor != nil {
		return n.Editor.ID == 199175422 && n.Editor.Login == "chatgpt-codex-connector", nil
	}
	return a.Created == a.Updated, nil
}
