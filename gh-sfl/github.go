package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	gh "github.com/cli/go-gh/v2"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
)

var ghExec = gh.Exec

// fetchFileRaw retrieves a file's raw content from the motherrepo.
func fetchFileRaw(owner, repo, path, ref string) (string, error) {
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s", owner, repo, path)
	if ref != "" {
		apiPath += "?ref=" + url.QueryEscape(ref)
	}
	if client, ok, err := sourceReadClient(owner, repo); err != nil {
		return "", err
	} else if ok {
		var response struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := client.Get(apiPath, &response); err != nil {
			return "", fmt.Errorf("fetching %s: %w", path, err)
		}
		if response.Encoding != "base64" {
			return "", fmt.Errorf("fetching %s: unsupported content encoding %q", path, response.Encoding)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
		if err != nil {
			return "", fmt.Errorf("decoding %s: %w", path, err)
		}
		return string(decoded), nil
	}

	args := []string{"api", apiPath, "-H", "Accept: application/vnd.github.raw+json"}

	stdoutBuf, stderrBuf, err := gh.Exec(args...)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %s: %w", path, stderrBuf.String(), err)
	}
	return stdoutBuf.String(), nil
}

// fetchDirListing retrieves directory listing from a repo.
func fetchDirListing(owner, repo, path string) ([]dirEntry, error) {
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s", owner, repo, path)
	if client, ok, err := sourceReadClient(owner, repo); err != nil {
		return nil, err
	} else if ok {
		var entries []dirEntry
		if err := client.Get(apiPath, &entries); err != nil {
			return nil, fmt.Errorf("listing %s: %w", path, err)
		}
		return entries, nil
	}

	stdoutBuf, stderrBuf, err := gh.Exec("api", apiPath)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %s: %w", path, stderrBuf.String(), err)
	}

	var entries []dirEntry
	if err := json.Unmarshal(stdoutBuf.Bytes(), &entries); err != nil {
		return nil, fmt.Errorf("parsing dir listing: %w", err)
	}
	return entries, nil
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // "file" or "dir"
	SHA  string `json:"sha"`
}

// currentRepo detects the current repository from git config.
func currentRepo() (string, string, error) {
	stdoutBuf, stderrBuf, err := gh.Exec("repo", "view", "--json", "owner,name", "--jq", ".owner.login + \"/\" + .name")
	if err != nil {
		return "", "", fmt.Errorf("detecting repo: %s: %w", stderrBuf.String(), err)
	}
	parts := strings.SplitN(strings.TrimSpace(stdoutBuf.String()), "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected repo format: %s", stdoutBuf.String())
	}
	return parts[0], parts[1], nil
}

// parseRepoFlag splits an "OWNER/REPO" string. If empty, uses currentRepo().
func parseRepoFlag(repoFlag string) (string, string, error) {
	if repoFlag == "" {
		return currentRepo()
	}
	parts := strings.SplitN(repoFlag, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo format %q (expected OWNER/REPO)", repoFlag)
	}
	return parts[0], parts[1], nil
}

func parseMutationTarget(repoFlag string) (string, string, error) {
	owner, repo, err := parseRepoFlag(repoFlag)
	if err != nil {
		return "", "", err
	}
	if err := validateDeploymentTarget(owner, repo); err != nil {
		return "", "", err
	}
	return owner, repo, nil
}

func validateDeploymentTarget(owner, repo string) error {
	if !strings.EqualFold(owner, "HemSoft") {
		return fmt.Errorf("%s/%s is outside the private HemSoft repository scope", owner, repo)
	}
	if strings.EqualFold(repo, motherRepoName) {
		return fmt.Errorf("%s/%s is protected and cannot be targeted by SFL deployment operations", owner, repo)
	}

	loginOut, loginErr, err := ghExec("api", "user", "--jq", ".login")
	if err != nil {
		return fmt.Errorf("checking GitHub CLI identity: %s: %w", loginErr.String(), err)
	}
	login := strings.TrimSpace(loginOut.String())
	if !strings.EqualFold(login, "HemSoft") {
		return fmt.Errorf("GitHub CLI must be authenticated as HemSoft; active login is %q", login)
	}

	visibilityOut, visibilityErr, err := ghExec(
		"repo", "view", owner+"/"+repo,
		"--json", "visibility",
		"--jq", ".visibility",
	)
	if err != nil {
		return fmt.Errorf("checking repository visibility: %s: %w", visibilityErr.String(), err)
	}
	visibility := strings.TrimSpace(visibilityOut.String())
	if visibility != "PRIVATE" {
		return fmt.Errorf("%s/%s must be private; visibility is %q", owner, repo, visibility)
	}
	return nil
}

// ensureRepoVariable creates or updates a repository Actions variable.
func ensureRepoVariable(owner, repo, name, value string) error {
	client, err := newRESTClient()
	if err != nil {
		return fmt.Errorf("creating GitHub REST client: %w", err)
	}
	return ensureRepoVariableWithClient(client, owner, repo, name, value)
}

func ensureRepoVariableWithClient(client restAPI, owner, repo, name, value string) error {
	variablePath := fmt.Sprintf("repos/%s/%s/actions/variables/%s", owner, repo, name)
	var current struct {
		Name string `json:"name"`
	}
	err := client.Get(variablePath, &current)
	payload, payloadErr := jsonBody(map[string]string{"name": name, "value": value})
	if payloadErr != nil {
		return fmt.Errorf("encoding repository variable %s: %w", name, payloadErr)
	}
	if err == nil {
		if err := client.Patch(variablePath, payload, nil); err != nil {
			return fmt.Errorf("updating repository variable %s: %w", name, err)
		}
		return nil
	}

	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		return fmt.Errorf("checking repository variable %s: %w", name, err)
	}
	collectionPath := fmt.Sprintf("repos/%s/%s/actions/variables", owner, repo)
	if err := client.Post(collectionPath, payload, nil); err != nil {
		return fmt.Errorf("creating repository variable %s: %w", name, err)
	}
	return nil
}

func setRepoSecret(owner, repo, name, value string) error {
	_, stderrBuf, err := gh.Exec(
		"secret", "set", name,
		"--repo", owner+"/"+repo,
		"--body", value,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", stderrBuf.String(), err)
	}
	return nil
}
func ensureLabels(owner, repo string, labels []labelDef) (created, updated int, err error) {
	for _, l := range labels {
		_, _, checkErr := gh.Exec(
			"api", fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, escapedLabelPath(l.Name)),
			"--jq", ".name",
		)
		if checkErr != nil {
			// Label doesn't exist — create it
			_, stderrBuf, createErr := gh.Exec(
				"api", fmt.Sprintf("repos/%s/%s/labels", owner, repo),
				"--method", "POST",
				"-f", "name="+l.Name,
				"-f", "color="+l.Color,
				"-f", "description="+l.Description,
			)
			if createErr != nil {
				return created, updated, fmt.Errorf("creating label %s: %s: %w", l.Name, stderrBuf.String(), createErr)
			}
			created++
		} else {
			// Label exists — update it
			_, stderrBuf, updateErr := gh.Exec(
				"api", fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, escapedLabelPath(l.Name)),
				"--method", "PATCH",
				"-f", "color="+l.Color,
				"-f", "description="+l.Description,
			)
			if updateErr != nil {
				return created, updated, fmt.Errorf("updating label %s: %s: %w", l.Name, stderrBuf.String(), updateErr)
			}
			updated++
		}
	}
	return created, updated, nil
}

func escapedLabelPath(name string) string {
	return strings.ReplaceAll(url.PathEscape(name), ":", "%3A")
}

type labelDef struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// getDefaultBranch returns the default branch name for a repo.
func getDefaultBranch(owner, repo string) (string, error) {
	stdoutBuf, stderrBuf, err := gh.Exec(
		"api", fmt.Sprintf("repos/%s/%s", owner, repo),
		"--jq", ".default_branch",
	)
	if err != nil {
		return "", fmt.Errorf("getting default branch: %s: %w", stderrBuf.String(), err)
	}
	return strings.TrimSpace(stdoutBuf.String()), nil
}

// fetchWorkflowRuns retrieves recent workflow runs filtered to SFL workflows.
func fetchWorkflowRuns(owner, repo string, limit int) ([]workflowRun, error) {
	stdoutBuf, stderrBuf, err := gh.Exec(
		"run", "list",
		"--repo", owner+"/"+repo,
		"--limit", fmt.Sprintf("%d", limit),
		"--json", "databaseId,displayTitle,workflowName,headBranch,event,status,conclusion,url,createdAt,startedAt,updatedAt",
	)
	if err != nil {
		return nil, fmt.Errorf("listing runs: %s: %w", stderrBuf.String(), err)
	}

	var runs []workflowRun
	if err := json.Unmarshal(stdoutBuf.Bytes(), &runs); err != nil {
		return nil, fmt.Errorf("parsing runs: %w", err)
	}

	// Filter to SFL-prefixed workflows
	var sflRuns []workflowRun
	for _, r := range runs {
		name := strings.ToLower(r.WorkflowName)
		if strings.HasPrefix(name, "sfl") || strings.Contains(name, "set it free") ||
			strings.Contains(name, "simplisticate") || strings.Contains(name, "repo audit") ||
			strings.Contains(name, "full-spectrum") {
			sflRuns = append(sflRuns, r)
		}
	}
	return sflRuns, nil
}

// fetchPRsByLabel fetches open PRs with a specific label.
func fetchPRsByLabel(owner, repo, label string) ([]prInfo, error) {
	stdoutBuf, stderrBuf, err := gh.Exec(
		"pr", "list",
		"--repo", owner+"/"+repo,
		"--label", label,
		"--json", "number,title,headRefName,labels,url",
		"--state", "open",
	)
	if err != nil {
		return nil, fmt.Errorf("listing PRs: %s: %w", stderrBuf.String(), err)
	}

	var prs []prInfo
	if err := json.Unmarshal(stdoutBuf.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("parsing PRs: %w", err)
	}
	return prs, nil
}

type prInfo struct {
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	HeadBranch string    `json:"headRefName"`
	Labels     []prLabel `json:"labels"`
	URL        string    `json:"url"`
}

type prLabel struct {
	Name string `json:"name"`
}

// fetchRepoLabels retrieves all labels from a repo.
func fetchRepoLabels(owner, repo string) ([]labelDef, error) {
	stdoutBuf, stderrBuf, err := gh.Exec(
		"api", fmt.Sprintf("repos/%s/%s/labels?per_page=100", owner, repo),
		"--jq", "[.[] | {name: .name, color: .color, description: .description}]",
	)
	if err != nil {
		return nil, fmt.Errorf("listing labels: %s: %w", stderrBuf.String(), err)
	}

	var labels []labelDef
	if err := json.Unmarshal(stdoutBuf.Bytes(), &labels); err != nil {
		return nil, fmt.Errorf("parsing labels: %w", err)
	}
	return labels, nil
}

// deployViaGit clones the target repo, writes files, commits, and pushes in a single commit.
// fileMap keys are repo-relative paths using forward slashes (e.g. ".github/workflows/foo.yml").
func deployViaGit(
	owner, repo, branch string,
	fileMap map[string]string,
	commitMsg string,
	reconcile bool,
	w io.Writer,
) error {
	if err := applyHemSoftOwnership(fileMap); err != nil {
		return fmt.Errorf("applying HemSoft deployment policy: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "gh-sfl-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Fprintf(w, "  Cloning %s/%s...\n", owner, repo)
	cloneURL := fmt.Sprintf("git@github-personal1:%s/%s.git", owner, repo)
	cloneCmd := exec.Command("git", "clone", "--depth=1", cloneURL, tmpDir)
	if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
		return fmt.Errorf("cloning: %s: %w", string(out), cloneErr)
	}

	for _, managedPath := range obsoleteManagedPaths(fileMap, reconcile) {
		target := filepath.Join(tmpDir, filepath.FromSlash(managedPath))
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("removing obsolete managed path %s: %w", managedPath, removeErr)
		}
	}

	for fpath, content := range fileMap {
		dst := filepath.Join(tmpDir, filepath.FromSlash(fpath))
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0755); mkErr != nil {
			return fmt.Errorf("mkdir for %s: %w", fpath, mkErr)
		}
		if wErr := os.WriteFile(dst, []byte(content), 0644); wErr != nil {
			return fmt.Errorf("writing %s: %w", fpath, wErr)
		}
	}

	runGit := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if _, err := runGit("add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	checkCmd := exec.Command("git", "diff", "--cached", "--quiet")
	checkCmd.Dir = tmpDir
	if checkCmd.Run() == nil {
		fmt.Fprintf(w, "  No changes — already up to date.\n")
		return nil
	}

	if out, err := runGit("commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("git commit: %s: %w", out, err)
	}

	fmt.Fprintf(w, "  Pushing %d files to %s/%s...\n", len(fileMap), owner, repo)
	if out, err := runGit("push", "origin", "HEAD"); err != nil {
		if strings.Contains(out, "workflow") && strings.Contains(out, "scope") {
			return fmt.Errorf("push rejected — token lacks 'workflow' scope\n\n  Fix: gh auth refresh -s workflow\n  Then retry: gh sfl init")
		}
		return fmt.Errorf("git push: %s\nHint: your gh auth account may not have push access to %s/%s", out, owner, repo)
	}

	return nil
}

type fileAddition struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

type fileDeletion struct {
	Path string `json:"path"`
}

type restAPI interface {
	Get(path string, response interface{}) error
	GetWithETag(path string, response interface{}) (string, error)
	Post(path string, body io.Reader, response interface{}) error
	Put(path string, body io.Reader, response interface{}) error
	PutIfMatch(path string, body io.Reader, etag string, response interface{}) error
	Patch(path string, body io.Reader, response interface{}) error
	Delete(path string, response interface{}) error
	DeleteIfMatch(path, etag string, response interface{}) error
}

type conditionalRESTClient struct {
	rest    *api.RESTClient
	http    *http.Client
	baseURL string
}

func (c *conditionalRESTClient) Get(path string, response interface{}) error {
	return c.rest.Get(path, response)
}

func (c *conditionalRESTClient) Post(path string, body io.Reader, response interface{}) error {
	return c.rest.Post(path, body, response)
}

func (c *conditionalRESTClient) Put(path string, body io.Reader, response interface{}) error {
	return c.rest.Put(path, body, response)
}

func (c *conditionalRESTClient) Patch(path string, body io.Reader, response interface{}) error {
	return c.rest.Patch(path, body, response)
}

func (c *conditionalRESTClient) Delete(path string, response interface{}) error {
	return c.rest.Delete(path, response)
}

func (c *conditionalRESTClient) GetWithETag(
	path string,
	response interface{},
) (string, error) {
	return c.requestWithETag(http.MethodGet, path, nil, "", response)
}

func (c *conditionalRESTClient) PutIfMatch(
	path string,
	body io.Reader,
	etag string,
	response interface{},
) error {
	_, err := c.requestWithETag(http.MethodPut, path, body, etag, response)
	return err
}

func (c *conditionalRESTClient) DeleteIfMatch(
	path, etag string,
	response interface{},
) error {
	_, err := c.requestWithETag(http.MethodDelete, path, nil, etag, response)
	return err
}

func (c *conditionalRESTClient) requestWithETag(
	method, path string,
	body io.Reader,
	etag string,
	response interface{},
) (string, error) {
	request, err := http.NewRequest(method, c.baseURL+strings.TrimPrefix(path, "/"), body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	httpResponse, err := c.http.Do(request)
	if err != nil {
		return "", err
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < http.StatusOK ||
		httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", api.HandleHTTPError(httpResponse)
	}
	if response != nil && httpResponse.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(httpResponse.Body).Decode(response); err != nil {
			return "", err
		}
	}
	return httpResponse.Header.Get("ETag"), nil
}

func apiBaseURL(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" || strings.EqualFold(host, "github.com") {
		return "https://api.github.com/"
	}
	return "https://" + host + "/api/v3/"
}

func newConditionalRESTClient(options api.ClientOptions) (restAPI, error) {
	if strings.TrimSpace(options.Host) == "" {
		options.Host, _ = auth.DefaultHost()
	}
	restClient, err := api.NewRESTClient(options)
	if err != nil {
		return nil, err
	}
	httpClient, err := api.NewHTTPClient(options)
	if err != nil {
		return nil, err
	}
	return &conditionalRESTClient{
		rest:    restClient,
		http:    httpClient,
		baseURL: apiBaseURL(options.Host),
	}, nil
}

type graphQLAPI interface {
	Do(query string, variables map[string]interface{}, response interface{}) error
}

var (
	newRESTClient = func() (restAPI, error) {
		return newConditionalRESTClient(api.ClientOptions{})
	}
	newSourceRESTClient = func(token string) (restAPI, error) {
		return newConditionalRESTClient(api.ClientOptions{AuthToken: token})
	}
	newGraphQLClient = func() (graphQLAPI, error) {
		return api.DefaultGraphQLClient()
	}
	deploymentNow = time.Now
)

func sourceReadClient(owner, repo string) (restAPI, bool, error) {
	if owner != motherRepoOwner || repo != motherRepoName {
		return nil, false, nil
	}
	token := strings.TrimSpace(os.Getenv("SFL_SOURCE_TOKEN"))
	if token == "" {
		return nil, false, nil
	}
	client, err := newSourceRESTClient(token)
	if err != nil {
		return nil, false, fmt.Errorf("creating source repository client: %w", err)
	}
	return client, true, nil
}

func buildFileAdditions(fileMap map[string]string) []fileAddition {
	paths := make([]string, 0, len(fileMap))
	for path := range fileMap {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	additions := make([]fileAddition, 0, len(paths))
	for _, path := range paths {
		additions = append(additions, fileAddition{
			Path:     path,
			Contents: base64.StdEncoding.EncodeToString([]byte(fileMap[path])),
		})
	}
	return additions
}

func deploymentBranchName(operation string, now time.Time) string {
	operation = strings.ToLower(operation)
	var cleaned strings.Builder
	previousHyphen := false
	for _, r := range operation {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			cleaned.WriteRune(r)
			previousHyphen = false
		} else if cleaned.Len() > 0 && !previousHyphen {
			cleaned.WriteByte('-')
			previousHyphen = true
		}
	}
	slug := strings.Trim(cleaned.String(), "-")
	if slug == "" {
		slug = "deploy"
	}
	return fmt.Sprintf("sfl/%s-%s", slug, now.UTC().Format("20060102-150405.000000000"))
}

func splitCommitMessage(message string) (headline, body string) {
	parts := strings.SplitN(message, "\n", 2)
	headline = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		body = strings.TrimSpace(parts[1])
	}
	return headline, body
}

func jsonBody(value any) (io.Reader, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func updateDeploymentPRTitle(client restAPI, owner, repo string, number int, title string) error {
	if number == 0 {
		return fmt.Errorf("existing deployment pull request is missing its number")
	}
	body, err := jsonBody(map[string]string{"title": title})
	if err != nil {
		return fmt.Errorf("encoding pull request title update: %w", err)
	}
	if err := client.Patch(
		fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number),
		body,
		nil,
	); err != nil {
		return fmt.Errorf("updating existing pull request title: %w", err)
	}
	return nil
}

type openDeploymentPR struct {
	Number  int
	URL     string
	Branch  string
	HeadSHA string
}

func findOpenDeploymentPR(client restAPI, owner, repo, baseBranch, operation string) (openDeploymentPR, error) {
	prefix := "sfl/" + operation + "-"
	for page := 1; ; page++ {
		var pulls []struct {
			Number int    `json:"number"`
			URL    string `json:"html_url"`
			Head   struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		path := fmt.Sprintf("repos/%s/%s/pulls?state=open&per_page=100&page=%d", owner, repo, page)
		if err := client.Get(path, &pulls); err != nil {
			return openDeploymentPR{}, fmt.Errorf("checking open deployment pull requests: %w", err)
		}
		for _, pull := range pulls {
			if strings.EqualFold(pull.Head.Repo.FullName, owner+"/"+repo) &&
				pull.Base.Ref == baseBranch &&
				strings.HasPrefix(pull.Head.Ref, prefix) {
				return openDeploymentPR{
					Number:  pull.Number,
					URL:     pull.URL,
					Branch:  pull.Head.Ref,
					HeadSHA: pull.Head.SHA,
				}, nil
			}
		}
		if len(pulls) < 100 {
			return openDeploymentPR{}, nil
		}
	}
}

func deploymentFileContent(client restAPI, owner, repo, branch, path string) ([]byte, bool, error) {
	var response struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	apiPath := fmt.Sprintf(
		"repos/%s/%s/contents/%s?ref=%s",
		owner,
		repo,
		path,
		url.QueryEscape(branch),
	)
	if err := client.Get(apiPath, &response); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s from existing deployment pull request: %w", path, err)
	}
	if response.Encoding != "base64" {
		return nil, false, fmt.Errorf("reading %s from existing deployment pull request: unsupported encoding %q", path, response.Encoding)
	}
	encoded := strings.NewReplacer("\r", "", "\n", "").Replace(response.Content)
	currentBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("decoding %s from existing deployment pull request: %w", path, err)
	}
	return currentBytes, true, nil
}

func deploymentManifestContentsMatch(current []byte, desired string) (bool, error) {
	var currentManifest, desiredManifest sflManifest
	if err := json.Unmarshal(current, &currentManifest); err != nil {
		return false, nil
	}
	if err := json.Unmarshal([]byte(desired), &desiredManifest); err != nil {
		return false, fmt.Errorf("parsing desired deployment manifest: %w", err)
	}

	return currentManifest.Version == desiredManifest.Version &&
		currentManifest.Tier == desiredManifest.Tier &&
		currentManifest.MotherRepo == desiredManifest.MotherRepo &&
		currentManifest.SourceSHA == desiredManifest.SourceSHA &&
		slices.Equal(currentManifest.Components, desiredManifest.Components) &&
		slices.Equal(currentManifest.Addons, desiredManifest.Addons), nil
}

// Retired workflows remain managed tombstones so sync removes versions that
// older SFL releases installed but the current catalog no longer contains.
var retiredWorkflowPaths = []string{
	"sfl-copilot-review-bridge.yml",
}

func managedDeploymentPaths() []string {
	paths := make(map[string]struct{})
	for _, workflows := range tierWorkflows {
		for _, workflow := range workflows {
			paths[".github/workflows/"+workflow] = struct{}{}
		}
	}
	for _, workflows := range addonWorkflows {
		for _, workflow := range workflows {
			paths[".github/workflows/"+workflow] = struct{}{}
		}
	}
	for _, workflow := range retiredWorkflowPaths {
		paths[".github/workflows/"+workflow] = struct{}{}
	}
	for _, governanceFile := range governanceFiles {
		paths[".sfl/"+strings.TrimPrefix(governanceFile, "deployment/")] = struct{}{}
	}

	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func obsoleteManagedPaths(desired map[string]string, reconcile bool) []string {
	if !reconcile {
		return nil
	}
	var result []string
	for _, path := range managedDeploymentPaths() {
		if _, wanted := desired[path]; !wanted {
			result = append(result, path)
		}
	}
	return result
}

func deploymentFilesState(client restAPI, owner, repo, branch string, desired map[string]string) (bool, []fileDeletion, error) {
	paths := make([]string, 0, len(desired))
	for path := range desired {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	matches := true
	for _, path := range paths {
		current, exists, err := deploymentFileContent(client, owner, repo, branch, path)
		if err != nil {
			return false, nil, err
		}
		if !exists {
			matches = false
			continue
		}
		if path == ".sfl/sfl.json" {
			manifestMatches, err := deploymentManifestContentsMatch(current, desired[path])
			if err != nil {
				return false, nil, err
			}
			matches = matches && manifestMatches
			continue
		}
		if !bytes.Equal(current, []byte(desired[path])) {
			matches = false
		}
	}

	var deletions []fileDeletion
	for _, path := range obsoleteManagedPaths(desired, true) {
		_, exists, err := deploymentFileContent(client, owner, repo, branch, path)
		if err != nil {
			return false, nil, err
		}
		if exists {
			deletions = append(deletions, fileDeletion{Path: path})
		}
	}
	return matches && len(deletions) == 0, deletions, nil
}

// deployViaPullRequest creates a GitHub-authored commit so repositories that
// require signed commits can still receive SFL updates through normal review.
func deployViaPullRequest(owner, repo, baseBranch, operation string, fileMap map[string]string, commitMsg string, w io.Writer) (string, error) {
	if err := applyHemSoftOwnership(fileMap); err != nil {
		return "", fmt.Errorf("applying HemSoft deployment policy: %w", err)
	}
	restClient, err := newRESTClient()
	if err != nil {
		return "", fmt.Errorf("creating GitHub REST client: %w", err)
	}

	existingPR, err := findOpenDeploymentPR(restClient, owner, repo, baseBranch, operation)
	if err != nil {
		return "", err
	}
	headline, body := splitCommitMessage(commitMsg)

	branch := existingPR.Branch
	expectedHeadSHA := existingPR.HeadSHA
	createdBranch := false
	if existingPR.URL == "" {
		matches, _, stateErr := deploymentFilesState(restClient, owner, repo, baseBranch, fileMap)
		if stateErr != nil {
			return "", stateErr
		}
		if matches {
			fmt.Fprintf(w, "  SFL %s is already up to date; no pull request needed\n", operation)
			return "", nil
		}

		var refResponse struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		refPath := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s", owner, repo, baseBranch)
		if err := restClient.Get(refPath, &refResponse); err != nil {
			return "", fmt.Errorf("getting %s head: %w", baseBranch, err)
		}
		if refResponse.Object.SHA == "" {
			return "", fmt.Errorf("getting %s head: GitHub returned an empty commit SHA", baseBranch)
		}

		branch = deploymentBranchName(operation, deploymentNow())
		expectedHeadSHA = refResponse.Object.SHA
		createRefBody, err := jsonBody(map[string]string{
			"ref": "refs/heads/" + branch,
			"sha": expectedHeadSHA,
		})
		if err != nil {
			return "", fmt.Errorf("encoding branch request: %w", err)
		}
		if err := restClient.Post(fmt.Sprintf("repos/%s/%s/git/refs", owner, repo), createRefBody, nil); err != nil {
			return "", fmt.Errorf("creating branch %s: %w", branch, err)
		}
		createdBranch = true
	} else if branch == "" || expectedHeadSHA == "" {
		return "", fmt.Errorf("existing SFL %s pull request is missing its branch or head SHA", operation)
	}

	cleanupBranch := func() error {
		if !createdBranch {
			return nil
		}
		deletePath := fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", owner, repo, branch)
		return restClient.Delete(deletePath, nil)
	}

	matches, deletions, err := deploymentFilesState(restClient, owner, repo, branch, fileMap)
	if err != nil {
		_ = cleanupBranch()
		return "", err
	}
	if matches {
		if existingPR.URL != "" {
			if err := updateDeploymentPRTitle(restClient, owner, repo, existingPR.Number, headline); err != nil {
				return "", err
			}
			fmt.Fprintf(w, "  Existing SFL %s pull request is already up to date: %s\n", operation, existingPR.URL)
			return existingPR.URL, nil
		}
		if err := cleanupBranch(); err != nil {
			return "", fmt.Errorf("cleaning up unexpectedly current deployment branch: %w", err)
		}
		return "", fmt.Errorf("deployment branch unexpectedly matches after default branch drift check")
	}

	fileChanges := map[string]any{
		"additions": buildFileAdditions(fileMap),
	}
	if len(deletions) > 0 {
		fileChanges["deletions"] = deletions
	}
	input := map[string]any{
		"branch": map[string]string{
			"repositoryNameWithOwner": owner + "/" + repo,
			"branchName":              branch,
		},
		"message": map[string]string{
			"headline": headline,
			"body":     body,
		},
		"fileChanges":     fileChanges,
		"expectedHeadOid": expectedHeadSHA,
	}
	mutation := `mutation CreateSFLCommit($input: CreateCommitOnBranchInput!) {
		createCommitOnBranch(input: $input) {
			commit { oid }
		}
	}`
	var mutationResponse struct {
		CreateCommitOnBranch struct {
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"createCommitOnBranch"`
	}
	graphQLClient, err := newGraphQLClient()
	if err != nil {
		_ = cleanupBranch()
		return "", fmt.Errorf("creating GitHub GraphQL client: %w", err)
	}
	if err := graphQLClient.Do(mutation, map[string]any{"input": input}, &mutationResponse); err != nil {
		cleanupErr := cleanupBranch()
		if cleanupErr != nil {
			return "", fmt.Errorf("creating signed deployment commit: %w (also failed to remove branch %s: %v)", err, branch, cleanupErr)
		}
		return "", fmt.Errorf("creating signed deployment commit: %w", err)
	}
	if mutationResponse.CreateCommitOnBranch.Commit.OID == "" {
		_ = cleanupBranch()
		return "", fmt.Errorf("creating signed deployment commit: GitHub returned an empty commit OID")
	}

	if existingPR.URL != "" {
		if err := updateDeploymentPRTitle(restClient, owner, repo, existingPR.Number, headline); err != nil {
			return "", err
		}
		fmt.Fprintf(w, "  Updated existing SFL %s pull request with %d files: %s\n", operation, len(fileMap), existingPR.URL)
		return existingPR.URL, nil
	}

	prBody, err := jsonBody(map[string]any{
		"title": headline,
		"head":  branch,
		"base":  baseBranch,
		"body":  "Created by `gh sfl " + operation + " --pr`. Merge this pull request to apply the SFL file changes.",
	})
	if err != nil {
		_ = cleanupBranch()
		return "", fmt.Errorf("encoding pull request: %w", err)
	}
	var pull struct {
		URL string `json:"html_url"`
	}
	if err := restClient.Post(fmt.Sprintf("repos/%s/%s/pulls", owner, repo), prBody, &pull); err != nil {
		cleanupErr := cleanupBranch()
		if cleanupErr != nil {
			return "", fmt.Errorf("opening pull request: %w (also failed to remove branch %s: %v)", err, branch, cleanupErr)
		}
		return "", fmt.Errorf("opening pull request: %w", err)
	}
	if pull.URL == "" {
		return "", fmt.Errorf("opening pull request: GitHub returned an empty URL")
	}

	fmt.Fprintf(w, "  Opened pull request with %d files: %s\n", len(fileMap), pull.URL)
	return pull.URL, nil
}

// updateAgentPRBranches merges main into all open PRs labeled sfl-pr
// so they pick up updated reactor/dispatcher code from the sync.
func updateAgentPRBranches(owner, repo string, w io.Writer) {
	prs, err := fetchPRsByLabel(owner, repo, "sfl-pr")
	if err != nil {
		fmt.Fprintf(w, "    ⚠ Could not list PRs: %v\n", err)
		return
	}
	if len(prs) == 0 {
		fmt.Fprintf(w, "    No open sfl-pr branches to update.\n")
		return
	}
	for _, pr := range prs {
		_, _, mergeErr := gh.Exec(
			"api", fmt.Sprintf("repos/%s/%s/pulls/%d/update-branch", owner, repo, pr.Number),
			"--method", "PUT",
			"-f", "update_method=merge",
		)
		if mergeErr != nil {
			fmt.Fprintf(w, "    #%d — ⚠ merge failed (may already be up to date)\n", pr.Number)
		} else {
			fmt.Fprintf(w, "    #%d — ✓ merged main into branch\n", pr.Number)
		}
	}
}
