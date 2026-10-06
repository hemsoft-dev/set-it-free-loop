package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/mattn/go-runewidth"
)

type fakeREST struct {
	openPRURL                 string
	openPRPage                int
	openPRBranch              string
	openPRBase                string
	manifestContent           string
	fileContents              map[string]string
	base64LineBreaks          bool
	commitSHA                 string
	repositoryDefaultBranch   string
	actionsEnabled            bool
	actionsAllowed            string
	githubOwnedActionsAllowed bool
	actionVariablePages       [][]string
	actionSecretPages         [][]string
	rulesetPages              [][]repositoryRuleset
	rulesetDetails            map[int64]repositoryRuleset
	rulesetDetailSequences    map[int64][]repositoryRuleset
	rulesetDetailReads        map[int64]int
	deleteErrors              map[string]error
	deleteApplies             map[string]bool
	postErrors                map[string]error
	variableExists            bool
	variableGetError          error
	labelExists               map[string]bool
	labelGetErrors            map[string]error
	gets                      []string
	posts                     []string
	puts                      []string
	patches                   []string
	deletes                   []string
}

func (f *fakeREST) Get(path string, response interface{}) error {
	f.gets = append(f.gets, path)
	switch {
	case strings.HasSuffix(path, "/actions/permissions/selected-actions"):
		return decodeTestResponse(response, map[string]any{
			"github_owned_allowed": f.githubOwnedActionsAllowed,
		})
	case strings.HasSuffix(path, "/actions/permissions"):
		return decodeTestResponse(response, map[string]any{
			"enabled":         f.actionsEnabled,
			"allowed_actions": f.actionsAllowed,
		})
	case strings.Contains(path, "/actions/variables?"):
		page := 0
		if strings.Contains(path, "page=2") {
			page = 1
		}
		var names []string
		if page < len(f.actionVariablePages) {
			names = f.actionVariablePages[page]
		}
		variables := make([]map[string]string, 0, len(names))
		for _, name := range names {
			variables = append(variables, map[string]string{"name": name})
		}
		return decodeTestResponse(response, map[string]any{"variables": variables})
	case strings.Contains(path, "/actions/secrets?"):
		page := 0
		if strings.Contains(path, "page=2") {
			page = 1
		}
		var names []string
		if page < len(f.actionSecretPages) {
			names = f.actionSecretPages[page]
		}
		secrets := make([]map[string]string, 0, len(names))
		for _, name := range names {
			secrets = append(secrets, map[string]string{"name": name})
		}
		return decodeTestResponse(response, map[string]any{"secrets": secrets})
	case strings.Contains(path, "/labels/"):
		if err := f.labelGetErrors[path]; err != nil {
			return err
		}
		if !f.labelExists[path] {
			return &api.HTTPError{StatusCode: http.StatusNotFound}
		}
		return decodeTestResponse(response, map[string]string{"name": filepath.Base(path)})
	case strings.Contains(path, "/actions/variables/"):
		if f.variableGetError != nil {
			return f.variableGetError
		}
		if !f.variableExists {
			return &api.HTTPError{StatusCode: http.StatusNotFound}
		}
		return decodeTestResponse(response, map[string]string{"name": "SFL_ENABLED"})
	case strings.Contains(path, "/rulesets?"):
		page := 0
		if strings.Contains(path, "page=2") {
			page = 1
		}
		if page < len(f.rulesetPages) {
			return decodeTestResponse(response, f.rulesetPages[page])
		}
		return decodeTestResponse(response, []repositoryRuleset{})
	case strings.Contains(path, "/rulesets/"):
		for id, sequence := range f.rulesetDetailSequences {
			if strings.HasSuffix(path, fmt.Sprintf("/rulesets/%d", id)) && len(sequence) > 0 {
				if f.rulesetDetailReads == nil {
					f.rulesetDetailReads = make(map[int64]int)
				}
				index := min(f.rulesetDetailReads[id], len(sequence)-1)
				f.rulesetDetailReads[id]++
				return decodeTestResponse(response, sequence[index])
			}
		}
		for id, ruleset := range f.rulesetDetails {
			if strings.HasSuffix(path, fmt.Sprintf("/rulesets/%d", id)) {
				return decodeTestResponse(response, ruleset)
			}
		}
		return &api.HTTPError{StatusCode: 404}
	case strings.Contains(path, "/contents/"):
		filePath := strings.SplitN(strings.SplitN(path, "/contents/", 2)[1], "?", 2)[0]
		content, ok := f.fileContents[filePath]
		if !ok && filePath == ".sfl/sfl.json" && f.manifestContent != "" {
			content, ok = f.manifestContent, true
		}
		if !ok {
			return &api.HTTPError{StatusCode: 404}
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		if f.base64LineBreaks {
			encoded = strings.Join(splitEvery(encoded, 24), "\r\n")
		}
		return decodeTestResponse(response, map[string]string{
			"content":  encoded,
			"encoding": "base64",
		})
	case strings.Contains(path, "/pulls?"):
		if f.openPRURL == "" {
			return nil
		}
		page := 1
		if strings.Contains(path, "page=2") {
			page = 2
		}
		targetPage := f.openPRPage
		if targetPage == 0 {
			targetPage = 1
		}
		if page < targetPage {
			pulls := make([]map[string]any, 100)
			for i := range pulls {
				pulls[i] = map[string]any{
					"html_url": fmt.Sprintf("https://github.test/pull/%d", i),
					"head":     map[string]string{"ref": fmt.Sprintf("feature-%d", i)},
				}
			}

			return decodeTestResponse(response, pulls)
		}
		branch := f.openPRBranch
		if branch == "" {
			branch = "sfl/sync-existing"
		}
		base := f.openPRBase
		if base == "" {
			base = "main"
		}
		return decodeTestResponse(response, []map[string]any{{
			"number":   42,
			"html_url": f.openPRURL,
			"head": map[string]any{
				"ref":  branch,
				"sha":  "existing-head-sha",
				"repo": map[string]string{"full_name": "owner/repo"},
			},
			"base": map[string]string{"ref": base},
		}})
	case strings.Contains(path, "/commits?"):
		sha := f.commitSHA
		if sha == "" {
			sha = "source-commit-sha"
		}
		return decodeTestResponse(response, []map[string]string{{"sha": sha}})
	case strings.Contains(path, "/git/ref/heads/"):
		return decodeTestResponse(response, map[string]any{
			"object": map[string]string{"sha": "base-sha"},
		})
	case strings.HasPrefix(path, "repos/") && strings.Count(path, "/") == 2:
		return decodeTestResponse(response, map[string]string{
			"default_branch": f.repositoryDefaultBranch,
		})
	default:
		return fmt.Errorf("unexpected GET %s", path)
	}
}

func (f *fakeREST) GetWithETag(path string, response interface{}) (string, error) {
	if err := f.Get(path, response); err != nil {
		return "", err
	}
	return `"test-etag"`, nil
}

func (f *fakeREST) Post(path string, body io.Reader, response interface{}) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.posts = append(f.posts, path+"\n"+string(data))
	if err := f.postErrors[path]; err != nil {
		return err
	}
	if strings.HasSuffix(path, "/pulls") {
		return decodeTestResponse(response, map[string]string{"html_url": "https://github.test/pull/1"})
	}
	return nil
}

func (f *fakeREST) Put(path string, body io.Reader, response interface{}) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.puts = append(f.puts, path+"\n"+string(data))
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	return decodeTestResponse(response, map[string]any{
		"id":   42,
		"name": payload["name"],
	})
}

func (f *fakeREST) Patch(path string, body io.Reader, _ interface{}) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.patches = append(f.patches, path+"\n"+string(data))
	return nil
}

func (f *fakeREST) Delete(path string, _ interface{}) error {
	f.deletes = append(f.deletes, path)
	if f.deleteApplies[path] {
		for id := range f.rulesetDetails {
			if strings.HasSuffix(path, fmt.Sprintf("/rulesets/%d", id)) {
				delete(f.rulesetDetails, id)
			}
		}
	}
	if err := f.deleteErrors[path]; err != nil {
		return err
	}
	return nil
}

func TestConditionalRESTClientReadsEntityTags(t *testing.T) {
	const etag = `"ruleset-v1"`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", request.Method)
		}
		writer.Header().Set("ETag", etag)
		fmt.Fprint(writer, `{"id":42,"name":"Reviewer gate"}`)
	}))
	defer server.Close()

	client := &conditionalRESTClient{http: server.Client(), baseURL: server.URL + "/"}
	var ruleset repositoryRuleset
	gotETag, err := client.GetWithETag("rulesets/42", &ruleset)
	if err != nil || gotETag != etag || ruleset.ID != 42 {
		t.Fatalf("GetWithETag() = %q, %+v, %v", gotETag, ruleset, err)
	}
}

func TestNewConditionalRESTClientUsesResolvedHost(t *testing.T) {
	client, err := newConditionalRESTClient(api.ClientOptions{
		Host:      "github.example.com",
		AuthToken: "test-token",
	})
	if err != nil {
		t.Fatalf("newConditionalRESTClient() unexpected error: %v", err)
	}
	conditional := client.(*conditionalRESTClient)
	if conditional.baseURL != "https://github.example.com/api/v3/" {
		t.Fatalf("conditional base URL = %q", conditional.baseURL)
	}
}

func splitEvery(value string, size int) []string {
	var chunks []string
	for len(value) > size {
		chunks = append(chunks, value[:size])
		value = value[size:]
	}
	return append(chunks, value)
}

type fakeGraphQL struct {
	query     string
	variables map[string]interface{}
}

func (f *fakeGraphQL) Do(query string, variables map[string]interface{}, response interface{}) error {
	f.query = query
	f.variables = variables
	return decodeTestResponse(response, map[string]any{
		"createCommitOnBranch": map[string]any{
			"commit": map[string]string{"oid": "commit-oid"},
		},
	})
}

func decodeTestResponse(response, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, response)
}

func installDeploymentFakes(t *testing.T, rest restAPI, graphQL graphQLAPI) {
	t.Helper()
	oldREST := newRESTClient
	oldGraphQL := newGraphQLClient
	oldNow := deploymentNow
	newRESTClient = func() (restAPI, error) { return rest, nil }
	newGraphQLClient = func() (graphQLAPI, error) { return graphQL, nil }
	deploymentNow = func() time.Time {
		return time.Date(2026, time.August, 9, 12, 34, 56, 0, time.UTC)
	}
	t.Cleanup(func() {
		newRESTClient = oldREST
		newGraphQLClient = oldGraphQL
		deploymentNow = oldNow
	})
}

func TestParseInitOptionsPR(t *testing.T) {
	opts, err := parseInitOptions([]string{"--repo", "owner/repo", "--pr"}, io.Discard)
	if err != nil {
		t.Fatalf("parseInitOptions() unexpected error: %v", err)
	}
	if !opts.pr {
		t.Error("parseInitOptions() pr = false, want true")
	}
}

func TestParseInitOptionsDefaultsToReviewerPullRequest(t *testing.T) {
	opts, err := parseInitOptions([]string{"--repo", "owner/repo"}, io.Discard)
	if err != nil {
		t.Fatalf("parseInitOptions() unexpected error: %v", err)
	}
	if opts.tier != "reviewer" {
		t.Errorf("parseInitOptions() tier = %q, want reviewer", opts.tier)
	}
	if !opts.pr {
		t.Error("parseInitOptions() pr = false, want true")
	}
	if opts.sourceRef != "" {
		t.Errorf("parseInitOptions() sourceRef = %q, want latest release", opts.sourceRef)
	}
	if opts.tierExplicit {
		t.Error("parseInitOptions() tierExplicit = true without --tier")
	}
}

func TestParseInitOptionsDirectDisablesPullRequest(t *testing.T) {
	opts, err := parseInitOptions([]string{"--repo", "owner/repo", "--direct"}, io.Discard)
	if err != nil {
		t.Fatalf("parseInitOptions() unexpected error: %v", err)
	}
	if opts.pr {
		t.Error("parseInitOptions() pr = true with --direct")
	}
}

func TestParseInitOptionsRegistersAndDeduplicatesAddons(t *testing.T) {
	opts, err := parseInitOptions([]string{"--tier", "minimal", "--addon", "pr-review", "--addon", "pr-review"}, io.Discard)
	if err != nil {
		t.Fatalf("parseInitOptions() unexpected error: %v", err)
	}
	if !slices.Equal(opts.addons, []string{"pr-review"}) {
		t.Fatalf("parseInitOptions() addons = %v, want [pr-review]", opts.addons)
	}
	if names := knownAddonNames(); !slices.Equal(names, []string{"pr-review"}) {
		t.Fatalf("knownAddonNames() = %v, want [pr-review]", names)
	}
}

func TestAddonCompilationGuidanceOnlyAppliesToMarkdownSource(t *testing.T) {
	for _, test := range []struct {
		name      string
		workflows []string
		want      bool
	}{
		{name: "standard Actions YAML", workflows: []string{"sfl-pr-review-auto.yml"}, want: false},
		{name: "Markdown source", workflows: []string{"repo-audit.md"}, want: true},
		{name: "precompiled Markdown", workflows: []string{"repo-audit.md", "repo-audit.lock.yml"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := addonNeedsCompilation(test.workflows); got != test.want {
				t.Fatalf("addonNeedsCompilation(%v) = %v, want %v", test.workflows, got, test.want)
			}
		})
	}
}

func TestReviewerAddonRequiresSubscriptionBackedSource(t *testing.T) {
	if _, err := prepareWorkflowSource(
		"sfl-pr-review-auto.yml",
		"name: SFL PR Review Auto Trigger",
		strings.Repeat("a", 40),
		"HemSoft/consumer",
		"main",
	); err == nil || !strings.Contains(err.Error(), "predates") ||
		!strings.Contains(err.Error(), strings.Repeat("a", 40)) {
		t.Fatalf("legacy reviewer source error = %v", err)
	}

	content := reviewerSourcePlaceholder + "\nname: SFL Codex Review Observer\non:\n  push:\n" + reviewerPushBranchPlaceholder + "\nenv:\n" + reviewerBaseBranchPlaceholder + "\nif: github.event.sender.id == 199175422\n"
	prepared, err := prepareWorkflowSource("sfl-pr-review-auto.yml", content, strings.Repeat("b", 40), "HemSoft/consumer", "release/next")
	if err != nil {
		t.Fatalf("subscription-backed reviewer source rejected: %v", err)
	}
	if strings.Contains(prepared, "@main") || !strings.Contains(prepared, "@"+strings.Repeat("b", 40)) ||
		!strings.HasPrefix(prepared, "# Deployed from:") || !strings.Contains(prepared, "branches: ['release/next']") ||
		!strings.Contains(prepared, "SFL_REVIEW_BASE_BRANCH: 'release/next'") {
		t.Fatalf("prepared reviewer source lacks immutable provenance: %q", prepared)
	}
	if _, err := prepareWorkflowSource("repo-audit.md", "legacy content", strings.Repeat("a", 40), "HemSoft/consumer", "main"); err != nil {
		t.Fatalf("non-reviewer add-on source rejected: %v", err)
	}
}

func TestReviewerSourceValidationCoversEveryDeploymentPath(t *testing.T) {
	for _, file := range []string{"init.go", "sync.go", "add.go"} {
		source := string(readContractFile(t, file))
		if !strings.Contains(source, "prepareWorkflowSource(wf, content,") {
			t.Fatalf("%s does not validate the selected reviewer source", file)
		}
	}
}

func TestParseAddOptionsDefaultsToPullRequest(t *testing.T) {
	opts, err := parseAddOptions([]string{"pr-review"}, io.Discard)
	if err != nil {
		t.Fatalf("parseAddOptions() unexpected error: %v", err)
	}
	if !opts.pr {
		t.Fatal("parseAddOptions() pr = false, want PR-first default")
	}
	opts, err = parseAddOptions([]string{"--direct", "pr-review"}, io.Discard)
	if err != nil || opts.pr {
		t.Fatalf("parseAddOptions(--direct) opts=%+v err=%v", opts, err)
	}
}

func TestSyncPreservesAuditFieldsOnlyForCurrentCanonicalDeployment(t *testing.T) {
	release := deploymentRelease{Version: "2.0.0", SHA: strings.Repeat("a", 40)}
	current := &sflManifest{MotherRepo: motherRepoOwner + "/" + motherRepoName, Version: release.Version, SourceSHA: release.SHA, Tier: "reviewer"}
	if !shouldPreserveSyncAudit(current, release, "reviewer") {
		t.Fatal("current canonical deployment did not preserve audit fields")
	}
	for _, stale := range []*sflManifest{
		{Version: "1.9.0", SourceSHA: release.SHA, Tier: "reviewer"},
		{Version: release.Version, SourceSHA: strings.Repeat("b", 40), Tier: "reviewer"},
		{Version: release.Version, SourceSHA: release.SHA, Tier: "review"},
	} {
		if shouldPreserveSyncAudit(stale, release, "reviewer") {
			t.Errorf("stale deployment unexpectedly preserved audit fields: %+v", stale)
		}
	}
}

func TestGeneratedLabelsMatchCanonicalDeploymentLabels(t *testing.T) {
	raw := readContractFile(t, filepath.Join("..", "deployment", "governance", "labels.json"))
	var definitions []labelDef
	if err := json.Unmarshal(raw, &definitions); err != nil {
		t.Fatalf("parse canonical labels: %v", err)
	}
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	if !slices.Equal(sflLabels, names) {
		t.Fatalf("generated labels are stale:\n generated=%v\n canonical=%v", sflLabels, names)
	}
}

func TestVersionUpdateHintIsCrossPlatform(t *testing.T) {
	oldVersion := version
	oldLatest := fetchLatestReleaseFunc
	version = "1.0.0"
	fetchLatestReleaseFunc = func(string, string) (string, error) { return "v2.0.0", nil }
	t.Cleanup(func() {
		version = oldVersion
		fetchLatestReleaseFunc = oldLatest
	})

	var output bytes.Buffer
	if err := runVersion(&output); err != nil {
		t.Fatalf("runVersion() unexpected error: %v", err)
	}
	if strings.Contains(output.String(), `.\\`) || !strings.Contains(output.String(), "rerun its installer") {
		t.Fatalf("runVersion() emitted platform-specific update guidance: %q", output.String())
	}
}

func TestAddUsesPinnedSourceAndMergesEnginePolicy(t *testing.T) {
	source := string(readContractFile(t, "add.go"))
	for _, required := range []string{
		"fullCommitSHAPattern.MatchString(manifest.SourceSHA)",
		"srcPath, manifest.SourceSHA",
		"renderHemSoftWorkflow(wf, content, manifest.Version)",
		"workflowsIncludeReviewer(workflows)",
		"assertReviewerRolloutReady(owner, repo, stdout)",
		"mergeHemSoftEnginePolicyManifest(",
		`deployViaPullRequest(owner, repo, defaultBranch, "add", fileMap, commitMsg, false, stdout)`,
	} {
		if !strings.Contains(source, required) {
			t.Errorf("add.go missing pinned add-on contract %q", required)
		}
	}
	if strings.Contains(source, `srcPath, ""`) {
		t.Error("add.go still reads add-on workflow sources from the mutable default branch")
	}

	existing := &sflEnginePolicyManifest{
		DefaultProfile: "old-default",
		Workflows:      []sflEngineWorkflowProfile{{Name: "repo-audit", Profile: "old"}},
	}
	additional := &sflEnginePolicyManifest{
		DefaultProfile: "new-default",
		Workflows: []sflEngineWorkflowProfile{
			{Name: "repo-audit", Profile: "updated"},
			{Name: "sfl-pr-review", Profile: "reviewer"},
		},
	}
	merged := mergeHemSoftEnginePolicyManifest(existing, additional)
	if merged.DefaultProfile != "new-default" || len(merged.Workflows) != 2 {
		t.Fatalf("merged engine policy = %+v", merged)
	}
	if merged.Workflows[0].Name != "repo-audit" || merged.Workflows[0].Profile != "updated" ||
		merged.Workflows[1].Name != "sfl-pr-review" {
		t.Fatalf("merged engine policy workflows = %+v", merged.Workflows)
	}
}

func TestTrimTextPreservesUTF8AndDisplayWidth(t *testing.T) {
	got := trimText("🚀 deployment ready", 10)
	if !utf8.ValidString(got) {
		t.Fatalf("trimText() returned invalid UTF-8: %q", got)
	}
	if width := runewidth.StringWidth(got); width > 10 {
		t.Fatalf("trimText() display width = %d, want <= 10: %q", width, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("trimText() = %q, want ellipsis suffix", got)
	}
}

func TestParseToggleOptionsRejectsInvalidPositionalRepositories(t *testing.T) {
	if _, err := parseToggleOptions("start", []string{"repo-only"}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "OWNER/REPO") {
		t.Fatalf("invalid positional repository error = %v", err)
	}
	if _, err := parseToggleOptions("start", []string{"owner/repo", "extra"}, io.Discard); err == nil {
		t.Fatal("multiple positional repositories unexpectedly accepted")
	}
}

func TestToggleHelpIsSuccessful(t *testing.T) {
	for _, run := range []func([]string, io.Writer, io.Writer) error{runStart, runStop} {
		if err := run([]string{"--help"}, io.Discard, io.Discard); err != nil {
			t.Fatalf("toggle --help returned error: %v", err)
		}
	}
}

func TestEnsureRepoVariableOnlyCreatesOnNotFound(t *testing.T) {
	for _, test := range []struct {
		name      string
		client    *fakeREST
		wantPatch int
		wantPost  int
		wantError bool
	}{
		{name: "update existing", client: &fakeREST{variableExists: true}, wantPatch: 1},
		{name: "create missing", client: &fakeREST{}, wantPost: 1},
		{name: "preserve permission error", client: &fakeREST{variableGetError: &api.HTTPError{StatusCode: http.StatusForbidden}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ensureRepoVariableWithClient(test.client, "owner", "repo", "SFL_ENABLED", "true")
			if (err != nil) != test.wantError {
				t.Fatalf("ensureRepoVariableWithClient() error = %v, wantError=%v", err, test.wantError)
			}
			if len(test.client.patches) != test.wantPatch || len(test.client.posts) != test.wantPost {
				t.Fatalf("patches=%v posts=%v", test.client.patches, test.client.posts)
			}
		})
	}
}

func TestEnsureLabelsOnlyCreatesOnNotFound(t *testing.T) {
	label := labelDef{Name: "risk:high", Color: "e11d48", Description: "High risk"}
	path := "repos/owner/repo/labels/risk%3Ahigh"

	for _, test := range []struct {
		name      string
		client    *fakeREST
		wantPost  int
		wantPatch int
		wantError bool
	}{
		{name: "create missing", client: &fakeREST{}, wantPost: 1},
		{name: "update existing", client: &fakeREST{labelExists: map[string]bool{path: true}}, wantPatch: 1},
		{name: "preserve server error", client: &fakeREST{labelGetErrors: map[string]error{path: &api.HTTPError{StatusCode: http.StatusInternalServerError}}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, updated, err := ensureLabelsWithClient(test.client, "owner", "repo", []labelDef{label})
			if (err != nil) != test.wantError {
				t.Fatalf("ensureLabelsWithClient() error = %v, wantError=%v", err, test.wantError)
			}
			if len(test.client.posts) != test.wantPost || len(test.client.patches) != test.wantPatch {
				t.Fatalf("created=%d updated=%d posts=%v patches=%v", created, updated, test.client.posts, test.client.patches)
			}
		})
	}
}

func TestRemoveLabelsReportsPartialFailureAndIgnoresMissing(t *testing.T) {
	failedPath := "repos/owner/repo/labels/risk%3Ahigh"
	missingPath := "repos/owner/repo/labels/missing"
	client := &fakeREST{deleteErrors: map[string]error{
		failedPath:  &api.HTTPError{StatusCode: http.StatusForbidden},
		missingPath: &api.HTTPError{StatusCode: http.StatusNotFound},
	}}
	var output bytes.Buffer
	removed, err := removeLabelsWithClient(client, "owner", "repo", []string{"risk:low", "risk:high", "missing"}, &output)
	if removed != 1 || err == nil || !strings.Contains(err.Error(), "risk:high") {
		t.Fatalf("removeLabelsWithClient() removed=%d err=%v", removed, err)
	}
	if !strings.Contains(output.String(), "✓ risk:low") || strings.Contains(output.String(), "risk:high") {
		t.Fatalf("removeLabelsWithClient() output = %q", output.String())
	}
}

func TestInitRequiresExplicitReviewerMigrationFromLegacyTier(t *testing.T) {
	opts := initOptions{repo: "owner/repo", tier: "reviewer"}
	for _, tier := range []string{"full", "", "unknown"} {
		existing := &sflManifest{Tier: tier}
		if err := validateInitTierTransition(existing, opts, "owner/repo"); err == nil ||
			!strings.Contains(err.Error(), "--tier reviewer") {
			t.Fatalf("validateInitTierTransition(tier=%q) = %v, want explicit migration error", tier, err)
		}
	}
	opts.tierExplicit = true
	if err := validateInitTierTransition(&sflManifest{Tier: "full"}, opts, "owner/repo"); err != nil {
		t.Fatalf("explicit reviewer migration rejected: %v", err)
	}
	if err := validateInitTierTransition(&sflManifest{Tier: "review"}, opts, "owner/repo"); err != nil {
		t.Fatalf("legacy review tier was not recognized as reviewer: %v", err)
	}
	if err := validateInitTierTransition(&sflManifest{Tier: "full"}, initOptions{tier: "reviewer"}, "HemSoft/consumer"); err == nil ||
		!strings.Contains(err.Error(), "HemSoft/consumer") {
		t.Fatalf("tier transition error does not identify resolved target: %v", err)
	}
}

func TestInstalledManifestWorkflowResolutionFailsClosed(t *testing.T) {
	reviewer := tierWorkflows["reviewer"]
	for _, tier := range []string{"reviewer", "review"} {
		got, err := workflowsForInstalledManifest(&sflManifest{Tier: tier})
		if err != nil {
			t.Fatalf("workflowsForInstalledManifest(tier=%q): %v", tier, err)
		}
		if !slices.Equal(got, reviewer) {
			t.Errorf("workflowsForInstalledManifest(tier=%q) = %v, want %v", tier, got, reviewer)
		}
	}

	custom, err := workflowsForInstalledManifest(&sflManifest{
		Tier:       "custom",
		Components: []string{"repo-audit", "sfl-pr-review", "sfl-pr-review-auto"},
	})
	if err != nil {
		t.Fatalf("custom workflow resolution: %v", err)
	}
	for _, want := range []string{"repo-audit.md", "sfl-pr-review-auto.yml"} {
		if !slices.Contains(custom, want) {
			t.Errorf("custom workflows missing %q: %v", want, custom)
		}
	}

	for _, manifest := range []*sflManifest{
		{Tier: "unknown"},
		{Tier: "custom", Components: []string{"not-a-managed-workflow"}},
	} {
		if _, err := workflowsForInstalledManifest(manifest); err == nil {
			t.Errorf("workflowsForInstalledManifest(%+v) unexpectedly succeeded", manifest)
		}
	}
}

func TestNormalizeManifestForSyncRemovesRetiredReviewerComponents(t *testing.T) {
	for _, tier := range []string{"review", "reviewer"} {
		t.Run(tier, func(t *testing.T) {
			manifest := &sflManifest{
				Tier:       tier,
				Components: []string{"sfl-pr-review", "sfl-pr-review-auto", "sfl-pr-review-recovery"},
			}
			normalizeManifestForSync(manifest, canonicalDeploymentTier(manifest.Tier))
			if manifest.Tier != "reviewer" {
				t.Fatalf("normalized tier = %q, want reviewer", manifest.Tier)
			}
			if !slices.Equal(manifest.Components, tierComponents["reviewer"]) {
				t.Fatalf("normalized components = %v, want %v", manifest.Components, tierComponents["reviewer"])
			}
		})
	}

	full := &sflManifest{Tier: "full", Components: []string{"sfl-pr-review", "sfl-pr-review-recovery"}}
	normalizeManifestForSync(full, canonicalDeploymentTier(full.Tier))
	if !slices.Equal(full.Components, []string{"sfl-pr-review-auto"}) {
		t.Fatalf("full components did not migrate to the observer component: %v", full.Components)
	}

	custom := &sflManifest{Tier: "custom", Components: []string{"repo-audit", "sfl-pr-review", "sfl-pr-review-recovery"}}
	normalizeManifestForSync(custom, canonicalDeploymentTier(custom.Tier))
	if !slices.Equal(custom.Components, []string{"repo-audit", "sfl-pr-review-auto"}) {
		t.Fatalf("custom components = %v, want legacy reviewer migrated to observer", custom.Components)
	}
}

func TestSyncChecksManagedFilesWhenSourceRevisionIsCurrent(t *testing.T) {
	source := string(readContractFile(t, "sync.go"))
	if !strings.Contains(source, "verifying managed files for drift") {
		t.Error("sync does not report drift verification for a current source revision")
	}
	blockStart := strings.Index(source, "if manifest.SourceSHA == latestSHA {")
	if blockStart < 0 {
		t.Fatal("sync does not branch on a current source revision")
	}
	blockEnd := strings.Index(source[blockStart:], "\n\t}")
	if blockEnd < 0 {
		t.Fatal("could not find the end of the current source revision branch")
	}
	if strings.Contains(source[blockStart:blockStart+blockEnd], "return nil") {
		t.Error("sync still returns before checking managed file drift")
	}
}

func TestSyncReportsNonFatalLabelSourceFailures(t *testing.T) {
	source := string(readContractFile(t, "sync.go"))
	for _, required := range []string{
		"label sync skipped; could not fetch labels",
		"label sync skipped; labels are invalid JSON",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("sync.go does not report %q", required)
		}
	}
}

func TestReviewerTierContainsOnlyReviewerPackage(t *testing.T) {
	want := []string{
		"sfl-pr-review-auto.yml",
	}
	if !slices.Equal(tierWorkflows["reviewer"], want) {
		t.Errorf("reviewer workflows = %v, want %v", tierWorkflows["reviewer"], want)
	}
	if slices.Contains(tierComponents["reviewer"], "labels") ||
		slices.Contains(tierComponents["reviewer"], "governance") ||
		slices.Contains(tierComponents["reviewer"], "sfl-dispatcher") {
		t.Errorf("reviewer components include unrelated SFL suite components: %v", tierComponents["reviewer"])
	}
}

func TestReviewerHealthUsesManifestPackageAndVersionMarkers(t *testing.T) {
	manifest := &sflManifest{
		Tier:    "reviewer",
		Version: "6.5.1",
	}
	want := []string{
		"sfl-pr-review-auto.yml",
	}
	if got := expectedWorkflowFiles(manifest); !slices.Equal(got, want) {
		t.Errorf("expectedWorkflowFiles() = %v, want %v", got, want)
	}
	source := "SFL_CLI_VERSION: " + sflVersionPlaceholder
	if got := renderWorkflow(source, manifest.Version); !strings.Contains(got, "6.5.1") {
		t.Errorf("renderWorkflow() did not stamp manifest version: %q", got)
	}
}

func TestReviewerHealthCoversEveryTierContainingReviewer(t *testing.T) {
	custom := &sflManifest{
		Tier:       "custom",
		Components: []string{"labels", "sfl-pr-review", "sfl-pr-review-auto"},
	}
	for name, manifest := range map[string]*sflManifest{
		"reviewer": {Tier: "reviewer"},
		"full":     {Tier: "full"},
		"custom":   custom,
	} {
		t.Run(name, func(t *testing.T) {
			if !manifestIncludesReviewer(manifest) {
				t.Fatalf("manifestIncludesReviewer(%s) = false", name)
			}
			want := []string{
				"sfl-pr-review-auto.yml",
			}
			if got := expectedReviewerWorkflowFiles(manifest); !slices.Equal(got, want) {
				t.Fatalf("expectedReviewerWorkflowFiles(%s) = %v, want %v", name, got, want)
			}
		})
	}
	if manifestIncludesReviewer(&sflManifest{Tier: "minimal"}) {
		t.Fatal("minimal tier unexpectedly includes reviewer health")
	}
	addonManifest := &sflManifest{Tier: "standard", Addons: []string{"pr-review"}}
	if !manifestIncludesReviewer(addonManifest) {
		t.Fatal("pr-review add-on did not enable reviewer health")
	}
	wantAddon := []string{
		"sfl-pr-review-auto.yml",
	}
	if got := expectedReviewerWorkflowFiles(addonManifest); !slices.Equal(got, wantAddon) {
		t.Fatalf("reviewer add-on files = %v, want %v", got, wantAddon)
	}
}

func TestClassifyReviewerGate(t *testing.T) {
	rule := func(ruleType string, parameters map[string]any) repositoryRuleset {
		ruleset := repositoryRuleset{Name: "SFL gate", Enforcement: "active"}
		ruleset.Conditions.RefName.Include = []string{"~DEFAULT_BRANCH"}
		ruleset.Rules = append(ruleset.Rules, struct {
			Type       string         `json:"type"`
			Parameters map[string]any `json:"parameters"`
		}{Type: ruleType, Parameters: parameters})
		return ruleset
	}

	reviewerWorkflow := func(repositoryID int64, ref string) map[string]any {
		return map[string]any{
			"workflows": []any{map[string]any{
				"path":          ".github/workflows/sfl-pr-review-auto.yml",
				"ref":           ref,
				"repository_id": repositoryID,
			}},
		}
	}
	mode, name := classifyReviewerGate(
		[]repositoryRuleset{rule("workflows", reviewerWorkflow(123, "refs/heads/main"))},
		"main",
		123,
	)
	if mode != "stale-required-workflow" || name != "SFL gate" {
		t.Errorf("classifyReviewerGate() = %q, %q", mode, name)
	}
	combined := rule("workflows", reviewerWorkflow(123, "refs/heads/main"))
	combined.Rules = append(combined.Rules, struct {
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}{
		Type: "required_status_checks",
		Parameters: map[string]any{
			"strict_required_status_checks_policy": true,
			"required_status_checks": []any{map[string]any{
				"context":        reviewerGateCheckContext,
				"integration_id": 15368,
			}},
		},
	})
	mode, _ = classifyReviewerGate([]repositoryRuleset{combined}, "main", 123)
	if mode != "required-workflow" {
		t.Errorf("combined classifyReviewerGate() = %q", mode)
	}
	if !hasReviewerFreshnessInterlock(combined) {
		t.Error("combined reviewer gate is missing its freshness interlock")
	}
	multipleStatusRules := combined
	multipleStatusRules.Rules = nil
	multipleStatusRules.Rules = append(multipleStatusRules.Rules, struct {
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}{
		Type: "required_status_checks",
		Parameters: map[string]any{
			"strict_required_status_checks_policy": false,
			"required_status_checks":               []any{map[string]any{"context": "CI"}},
		},
	})
	multipleStatusRules.Rules = append(multipleStatusRules.Rules, combined.Rules...)
	if !hasReviewerFreshnessInterlock(multipleStatusRules) {
		t.Error("freshness detection stopped at an unrelated status-check rule")
	}
	combinedWithExistingCI := combined
	combinedWithExistingCI.Rules = append(combinedWithExistingCI.Rules[:0:0], combined.Rules...)
	combinedWithExistingCI.Rules[1].Parameters = map[string]any{
		"strict_required_status_checks_policy": true,
		"required_status_checks": []any{
			map[string]any{
				"context":        reviewerGateCheckContext,
				"integration_id": 15368,
			},
			map[string]any{"context": "Existing CI"},
		},
	}
	if !hasReviewerFreshnessInterlock(combinedWithExistingCI) {
		t.Error("combined gate with an additional check lost reviewer freshness")
	}

	mode, _ = classifyReviewerGate([]repositoryRuleset{rule("required_status_checks", map[string]any{
		"strict_required_status_checks_policy": true,
		"required_status_checks": []any{map[string]any{
			"context":        legacyReviewerGateCheckContext,
			"integration_id": githubActionsAppID,
		}},
	})}, "main", 123)
	if mode != "stale-status-check" {
		t.Errorf("stale status-check classifyReviewerGate() = %q", mode)
	}

	mode, _ = classifyReviewerGate([]repositoryRuleset{rule("required_status_checks", map[string]any{
		"strict_required_status_checks_policy": true,
		"required_status_checks": []any{map[string]any{
			"context":        reviewerGateCheckContext,
			"integration_id": 15368,
		}},
	})}, "main", 123)
	if mode != "required-status-check" {
		t.Errorf("strict status-check classifyReviewerGate() = %q", mode)
	}

	mode, _ = classifyReviewerGate(nil, "main", 123)
	if mode != "advisory" {
		t.Errorf("empty classifyReviewerGate() = %q", mode)
	}

	inactive := rule("workflows", reviewerWorkflow(123, "refs/heads/main"))
	inactive.Enforcement = "evaluate"
	mode, _ = classifyReviewerGate([]repositoryRuleset{inactive}, "main", 123)
	if mode != "advisory" {
		t.Errorf("evaluate-only classifyReviewerGate() = %q", mode)
	}
	wrongBranch := rule("workflows", reviewerWorkflow(123, "refs/heads/main"))
	wrongBranch.Conditions.RefName.Include = []string{"refs/heads/release"}
	mode, _ = classifyReviewerGate([]repositoryRuleset{wrongBranch}, "main", 123)
	if mode != "advisory" {
		t.Errorf("wrong-branch classifyReviewerGate() = %q", mode)
	}
	excluded := rule("workflows", reviewerWorkflow(123, "refs/heads/main"))
	excluded.Conditions.RefName.Include = []string{"~ALL"}
	excluded.Conditions.RefName.Exclude = []string{"refs/heads/main"}
	mode, _ = classifyReviewerGate([]repositoryRuleset{excluded}, "main", 123)
	if mode != "advisory" {
		t.Errorf("excluded-default-branch classifyReviewerGate() = %q", mode)
	}
	for _, mismatch := range []map[string]any{
		reviewerWorkflow(999, "refs/heads/main"),
		reviewerWorkflow(123, "refs/heads/release"),
	} {
		mode, _ = classifyReviewerGate(
			[]repositoryRuleset{rule("workflows", mismatch)},
			"main",
			123,
		)
		if mode != "advisory" {
			t.Errorf("wrong workflow source classifyReviewerGate() = %q for %v", mode, mismatch)
		}
	}
}

func TestRemoveReviewerGatesForUninstallDeletesDedicatedRulesets(t *testing.T) {
	required := strictReviewerStatusRuleset(21, "Required reviewer")
	client := &fakeREST{rulesetDetails: map[int64]repositoryRuleset{21: required}}
	if err := removeReviewerGatesForUninstall(
		client,
		"owner",
		"repo",
		[]repositoryRuleset{required},
		io.Discard,
	); err != nil {
		t.Fatalf("removeReviewerGatesForUninstall() unexpected error: %v", err)
	}
	if !slices.Equal(client.deletes, []string{"repos/owner/repo/rulesets/21"}) {
		t.Errorf("uninstall ruleset deletions = %v", client.deletes)
	}
}

func TestRemoveReviewerGatesTreatsDeleteErrorWithNotFoundAsSuccess(t *testing.T) {
	required := strictReviewerStatusRuleset(22, "Required reviewer")
	client := &fakeREST{
		deleteErrors: map[string]error{
			"repos/owner/repo/rulesets/22": errors.New("delete failed"),
		},
		deleteApplies: map[string]bool{
			"repos/owner/repo/rulesets/22": true,
		},
		rulesetDetails: map[int64]repositoryRuleset{22: required},
	}

	err := removeReviewerGatesForUninstall(
		client,
		"owner",
		"repo",
		[]repositoryRuleset{required},
		io.Discard,
	)
	if err != nil {
		t.Fatalf("removeReviewerGatesForUninstall() unexpected error = %v", err)
	}
	if len(client.posts) != 0 {
		t.Errorf("unexpected ruleset restores = %v", client.posts)
	}
}

func TestRemoveReviewerGatesDoesNotDuplicateFailedDeletion(t *testing.T) {
	first := strictReviewerStatusRuleset(21, "First reviewer")
	second := strictReviewerStatusRuleset(22, "Second reviewer")
	client := &fakeREST{
		deleteErrors: map[string]error{
			"repos/owner/repo/rulesets/22": errors.New("delete failed"),
		},
		rulesetDetails: map[int64]repositoryRuleset{
			21: first,
			22: second,
		},
	}

	err := removeReviewerGatesForUninstall(
		client,
		"owner",
		"repo",
		[]repositoryRuleset{first, second},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("removeReviewerGatesForUninstall() error = %v", err)
	}
	if len(client.posts) != 1 ||
		!strings.Contains(client.posts[0], `"name":"First reviewer"`) {
		t.Errorf("restored ruleset payloads = %v", client.posts)
	}
}

func TestRestoreReviewerGatesPreservesDedicatedRulesetPayload(t *testing.T) {
	required := strictReviewerStatusRuleset(21, "Required reviewer")
	client := &fakeREST{}

	if err := restoreReviewerGates(
		client,
		"owner",
		"repo",
		[]repositoryRuleset{required},
		io.Discard,
	); err != nil {
		t.Fatalf("restoreReviewerGatesForUninstall() unexpected error: %v", err)
	}
	if len(client.posts) != 1 {
		t.Fatalf("ruleset restores = %v", client.posts)
	}
	for _, want := range []string{
		`"name":"Required reviewer"`,
		`"target":"branch"`,
		`"enforcement":"active"`,
		`"include":["~DEFAULT_BRANCH"]`,
		`"type":"required_status_checks"`,
		`"integration_id":15368`,
	} {
		if !strings.Contains(client.posts[0], want) {
			t.Errorf("restored ruleset payload missing %q: %s", want, client.posts[0])
		}
	}
}

func legacyReviewerRuleset(id int64, name string) repositoryRuleset {
	ruleset := repositoryRuleset{
		ID:          id,
		Name:        name,
		Target:      "branch",
		SourceType:  "Repository",
		Enforcement: "active",
		ETag:        `"test-etag"`,
	}
	ruleset.Conditions.RefName.Include = []string{"~DEFAULT_BRANCH"}
	ruleset.Rules = append(ruleset.Rules, struct {
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}{
		Type: "required_status_checks",
		Parameters: map[string]any{
			"required_status_checks": []any{map[string]any{"context": "SFL Reviewer Approval"}},
		},
	})
	return ruleset
}

func strictReviewerStatusRuleset(id int64, name string) repositoryRuleset {
	ruleset := legacyReviewerRuleset(id, name)
	ruleset.Rules[0].Parameters = mergeReviewerFreshnessRule(ruleset.Rules[0].Parameters)
	return ruleset
}

func TestInspectReviewerRolloutAcceptsHealthyPrivateConsumer(t *testing.T) {
	client := &fakeREST{
		repositoryDefaultBranch: "main",
		actionsEnabled:          true,
		actionsAllowed:          "all",
		actionVariablePages:     [][]string{{"SFL_APP_CLIENT_ID"}},
		actionSecretPages:       [][]string{{"SFL_APP_PRIVATE_KEY", "OPENROUTER_API_KEY"}},
	}
	health, err := inspectReviewerRolloutWithClient(client, "owner", "repo")
	if err != nil {
		t.Fatalf("inspectReviewerRolloutWithClient() unexpected error: %v", err)
	}
	if issues := health.issues(); len(issues) != 0 {
		t.Fatalf("healthy rollout issues = %v", issues)
	}
	if len(client.posts)+len(client.puts)+len(client.patches)+len(client.deletes) != 0 {
		t.Fatalf("read-only preflight mutated repository: %+v", client)
	}
}

func TestInspectReviewerRolloutReportsEveryDistinctFault(t *testing.T) {
	client := &fakeREST{
		actionsAllowed:      "local_only",
		actionVariablePages: [][]string{{}},
		actionSecretPages:   [][]string{{}},
	}
	health, err := inspectReviewerRolloutWithClient(client, "owner", "repo")
	if err != nil {
		t.Fatalf("inspectReviewerRolloutWithClient() unexpected error: %v", err)
	}
	joined := strings.Join(health.issues(), "\n")
	for _, want := range []string{
		"repository has no default branch",
		"GitHub Actions is disabled",
		"Actions policy allows only local actions",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rollout issues missing %q:\n%s", want, joined)
		}
	}
}

func TestReviewerPreflightDoesNotReadCredentialMetadata(t *testing.T) {
	client := &fakeREST{
		repositoryDefaultBranch: "main",
		actionsEnabled:          true,
		actionsAllowed:          "all",
	}
	health, err := inspectReviewerRolloutWithClient(client, "owner", "repo")
	if err != nil || len(health.issues()) != 0 {
		t.Fatalf("paginated rollout health = %+v, %v", health, err)
	}
	joinedRequests := strings.Join(client.gets, "\n")
	if strings.Contains(joinedRequests, "/actions/variables") || strings.Contains(joinedRequests, "/actions/secrets") {
		t.Errorf("subscription-backed reviewer inspected API credentials: %v", client.gets)
	}
}

func TestReviewerActionsSelectedPolicyRequiresGitHubOwnedActions(t *testing.T) {
	cases := map[string]struct {
		githubOwned bool
		wantHealthy bool
	}{
		"GitHub owned":          {githubOwned: true, wantHealthy: true},
		"GitHub owned disabled": {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := &fakeREST{
				githubOwnedActionsAllowed: tc.githubOwned,
			}
			issues, err := reviewerActionsPolicyIssues(client, "owner", "repo", "selected")
			if err != nil {
				t.Fatalf("reviewerActionsPolicyIssues() unexpected error: %v", err)
			}
			if tc.wantHealthy && len(issues) != 0 {
				t.Fatalf("healthy selected policy issues = %v", issues)
			}
			if !tc.wantHealthy && !slices.Contains(issues, "selected Actions policy must allow GitHub-owned actions for the reviewer") {
				t.Fatalf("blocked selected policy issues = %v", issues)
			}
		})
	}
}

func TestAssertReviewerRolloutReadyFailsBeforeAnyWrite(t *testing.T) {
	client := &fakeREST{
		repositoryDefaultBranch: "main",
		actionsEnabled:          false,
		actionsAllowed:          "all",
		actionVariablePages:     [][]string{{}},
		actionSecretPages:       [][]string{{}},
	}
	oldClient := newRESTClient
	newRESTClient = func() (restAPI, error) { return client, nil }
	t.Cleanup(func() { newRESTClient = oldClient })

	err := assertReviewerRolloutReady("owner", "repo", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "GitHub Actions is disabled") {
		t.Fatalf("assertReviewerRolloutReady() error = %v", err)
	}
	if len(client.posts)+len(client.puts)+len(client.patches)+len(client.deletes) != 0 {
		t.Fatalf("failed preflight mutated repository: %+v", client)
	}
}

func TestAssertReviewerRolloutReadyDoesNotClaimAppVerification(t *testing.T) {
	client := &fakeREST{
		repositoryDefaultBranch: "main",
		actionsEnabled:          true,
		actionsAllowed:          "all",
		actionVariablePages:     [][]string{{"SFL_APP_CLIENT_ID"}},
		actionSecretPages:       [][]string{{"OPENROUTER_API_KEY", "SFL_APP_PRIVATE_KEY"}},
	}
	oldClient := newRESTClient
	newRESTClient = func() (restAPI, error) { return client, nil }
	t.Cleanup(func() { newRESTClient = oldClient })

	var output bytes.Buffer
	if err := assertReviewerRolloutReady("owner", "repo", &output); err != nil {
		t.Fatalf("assertReviewerRolloutReady() unexpected error: %v", err)
	}
	if strings.Contains(output.String(), "App, variables, and secrets ready") {
		t.Fatalf("preflight falsely reported App verification:\n%s", output.String())
	}
	for _, want := range []string{
		"default branch main and Actions policy ready",
		"verify with a current-head review after deployment",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("preflight output missing %q:\n%s", want, output.String())
		}
	}
}

func TestPrintReviewerPrerequisitesSeparatesCredentialFaults(t *testing.T) {
	health := reviewerRolloutHealth{
		DefaultBranch:    "main",
		ActionsEnabled:   false,
		ActionsIssues:    []string{"Actions policy allows only local actions; reviewer requires pinned GitHub-owned actions"},
		AppNotice:        "App scope requires App-authenticated credential bootstrap verification",
		MissingVariables: []string{"SFL_APP_CLIENT_ID"},
		MissingSecrets:   []string{"OPENROUTER_API_KEY", "SFL_APP_PRIVATE_KEY"},
	}
	var output bytes.Buffer
	printReviewerPrerequisites(&output, newTableStyler(&output, false), health, nil)
	for _, want := range []string{
		"Codex App:",
		"App scope requires App-authenticated credential bootstrap verification",
		"Reviewer prerequisites:",
		"Default branch: main",
		"GitHub Actions disabled",
		"Actions policy allows only local actions",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "Selected-repository installation uses the reviewer permission contract") {
		t.Fatalf("status falsely reported App verification:\n%s", output.String())
	}
}

func TestWorkflowsIncludeReviewer(t *testing.T) {
	if !workflowsIncludeReviewer(tierWorkflows["reviewer"]) ||
		!workflowsIncludeReviewer(tierWorkflows["full"]) ||
		!workflowsIncludeReviewer(tierWorkflows["standard"], addonWorkflowFiles([]string{"pr-review"})) ||
		workflowsIncludeReviewer(tierWorkflows["minimal"]) {
		t.Fatal("workflowsIncludeReviewer() did not follow tier definitions")
	}
}

func TestDryRunSyncSkipsReviewerPreflight(t *testing.T) {
	reviewer := tierWorkflows["reviewer"]
	if shouldPreflightReviewerSync(true, reviewer) {
		t.Fatal("dry-run sync unexpectedly enforced reviewer rollout prerequisites")
	}
	if !shouldPreflightReviewerSync(false, reviewer) {
		t.Fatal("write-capable sync skipped reviewer rollout prerequisites")
	}
	if shouldPreflightReviewerSync(false, tierWorkflows["minimal"]) {
		t.Fatal("non-reviewer sync unexpectedly enforced reviewer rollout prerequisites")
	}
}

func TestForceUninstallRemovesEveryManagedDeploymentPath(t *testing.T) {
	files, err := uninstallFiles(nil, true)
	if err != nil {
		t.Fatalf("uninstallFiles(force): %v", err)
	}
	for _, want := range []string{
		".github/workflows/sfl-pr-review-auto.yml",
		".sfl/governance/policy.md",
		".sfl/sfl-config.yml",
		".sfl/sfl.json",
	} {
		if !slices.Contains(files, want) {
			t.Errorf("force uninstall files missing %q: %v", want, files)
		}
	}
	for _, managed := range managedDeploymentPaths() {
		if !slices.Contains(files, managed) {
			t.Errorf("force uninstall files missing managed path %q", managed)
		}
	}
}

func TestRemoveExistingManagedFilesReportsRemovalFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "managed.yml")
	if err := os.WriteFile(path, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("locked")
	removed, err := removeExistingManagedFiles(root, []string{"managed.yml"}, func(string) error {
		return wantErr
	}, io.Discard)
	if removed != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("removeExistingManagedFiles() = (%d, %v), want (0, locked error)", removed, err)
	}
}

func TestParseUninstallOptionsSeparatesConfirmationFromManifestFallback(t *testing.T) {
	opts, err := parseUninstallOptions([]string{"--force", "--allow-manifest-fallback"}, io.Discard)
	if err != nil {
		t.Fatalf("parseUninstallOptions() unexpected error: %v", err)
	}
	if !opts.force || !opts.allowManifestFallback {
		t.Fatalf("parseUninstallOptions() = %+v", opts)
	}
	opts, err = parseUninstallOptions([]string{"--force"}, io.Discard)
	if err != nil || !opts.force || opts.allowManifestFallback {
		t.Fatalf("--force unexpectedly enables manifest fallback: opts=%+v err=%v", opts, err)
	}
}

func TestUninstallUsesManifestTierWithoutExpandingUnknownTiers(t *testing.T) {
	legacyReview, err := uninstallFiles(&sflManifest{Tier: "review"}, false)
	if err != nil {
		t.Fatalf("uninstallFiles(review): %v", err)
	}
	if !slices.Contains(legacyReview, ".github/workflows/sfl-pr-review-auto.yml") {
		t.Errorf("legacy review uninstall is missing reviewer workflow: %v", legacyReview)
	}
	if slices.Contains(legacyReview, ".github/workflows/pr-fixer.md") {
		t.Errorf("legacy review uninstall expanded to full tier: %v", legacyReview)
	}
	if _, err := uninstallFiles(&sflManifest{Tier: "unknown"}, false); err == nil {
		t.Error("unknown uninstall tier unexpectedly succeeded")
	}
}

func TestPartialDeploymentDoesNotDeleteUnspecifiedManagedPaths(t *testing.T) {
	desired := map[string]string{
		".github/workflows/policy-manager.md": "content",
		".sfl/sfl.json":                       "{}",
	}
	if paths := obsoleteManagedPaths(desired, false); len(paths) != 0 {
		t.Errorf("partial deployment obsolete paths = %v", paths)
	}
	reconciled := obsoleteManagedPaths(desired, true)
	if !slices.Contains(reconciled, ".sfl/governance/policy.md") {
		t.Errorf("full reconciliation did not identify stale governance: %v", reconciled)
	}
}

func TestUninstallCommitReachedRemote(t *testing.T) {
	var commands [][]string
	runGit := func(args ...string) (string, error) {
		commands = append(commands, slices.Clone(args))
		return "", nil
	}
	reached, err := uninstallCommitReachedRemote(runGit, "main", "commit-sha")
	if err != nil || !reached {
		t.Fatalf("uninstallCommitReachedRemote() reached=%v err=%v", reached, err)
	}
	want := [][]string{
		{"fetch", "origin", "main"},
		{"merge-base", "--is-ancestor", "commit-sha", "FETCH_HEAD"},
	}
	if !slices.EqualFunc(commands, want, slices.Equal[[]string]) {
		t.Errorf("git commands = %v, want %v", commands, want)
	}
}

func TestUninstallCommitNotOnRemote(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if err := os.WriteFile(first, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	notAncestorErr := exec.Command("git", "diff", "--quiet", "--no-index", first, second).Run()
	if notAncestorErr == nil {
		t.Fatal("expected git diff to return exit code 1")
	}

	runGit := func(args ...string) (string, error) {
		if args[0] == "merge-base" {
			return "", notAncestorErr
		}
		return "", nil
	}
	reached, err := uninstallCommitReachedRemote(runGit, "main", "commit-sha")
	if err != nil || reached {
		t.Fatalf("uninstallCommitReachedRemote() reached=%v err=%v", reached, err)
	}
}

func TestGetRepositoryRulesetsPaginates(t *testing.T) {
	firstPage := make([]repositoryRuleset, 100)
	details := make(map[int64]repositoryRuleset, 101)
	for index := range firstPage {
		firstPage[index].ID = int64(index + 1)
		details[firstPage[index].ID] = firstPage[index]
	}
	last := repositoryRuleset{ID: 101, Name: "last"}
	details[last.ID] = last
	client := &fakeREST{
		rulesetPages:   [][]repositoryRuleset{firstPage, {last}},
		rulesetDetails: details,
	}
	rulesets, err := getRepositoryRulesets(client, "owner", "repo")
	if err != nil {
		t.Fatalf("getRepositoryRulesets() unexpected error: %v", err)
	}
	if len(rulesets) != 101 || rulesets[100].Name != "last" {
		t.Errorf("getRepositoryRulesets() returned %d rulesets", len(rulesets))
	}
	if !slices.Contains(client.gets, "repos/owner/repo/rulesets?includes_parents=true&per_page=100&page=2") {
		t.Errorf("ruleset requests did not include page 2: %v", client.gets)
	}
}

func TestRepositoryReviewerGatePayloadRequiresStrictActionsCheck(t *testing.T) {
	payload := repositoryReviewerGatePayload("repo")
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal reviewer ruleset: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"include":["~DEFAULT_BRANCH"]`,
		`"name":"Require SFL Reviewer Gate Runner (repo)"`,
		`"type":"required_status_checks"`,
		`"strict_required_status_checks_policy":true`,
		`"context":"SFL Reviewer Gate Runner"`,
		`"integration_id":15368`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reviewer ruleset payload missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, `"type":"workflows"`) || strings.Contains(text, `"repository_id"`) {
		t.Errorf("repository reviewer gate contains organization-only fields: %s", text)
	}
}

func TestUpdateRepositoryReviewerGateUsesConditionalRepositoryEndpoint(t *testing.T) {
	ruleset := legacyReviewerRuleset(42, "Existing reviewer gate")
	ruleset.BypassActors = []any{map[string]any{"actor_id": 7}}
	ruleset.Conditions.RefName.Include = []string{"~ALL"}
	ruleset.Conditions.RefName.Exclude = []string{"refs/heads/archive"}
	desired := strictReviewerStatusRuleset(42, ruleset.Name)
	desired.BypassActors = ruleset.BypassActors
	desired.Conditions.RefName = ruleset.Conditions.RefName
	client := &fakeREST{rulesetDetailSequences: map[int64][]repositoryRuleset{
		42: {ruleset, ruleset, desired},
	}}
	name, err := updateRepositoryReviewerGate(client, "owner", "repo", "main", ruleset)
	if err != nil {
		t.Fatalf("updateRepositoryReviewerGate() unexpected error: %v", err)
	}
	if name != "Existing reviewer gate" ||
		len(client.puts) != 1 ||
		!strings.HasPrefix(client.puts[0], "repos/owner/repo/rulesets/42\n") ||
		!strings.Contains(client.puts[0], `"strict_required_status_checks_policy":true`) ||
		!strings.Contains(client.puts[0], `"include":["~ALL"]`) ||
		!strings.Contains(client.puts[0], `"exclude":["refs/heads/archive"]`) ||
		!strings.Contains(client.puts[0], `"actor_id":7`) ||
		!strings.Contains(client.puts[0], `"name":"Existing reviewer gate"`) ||
		!strings.Contains(client.puts[0], `"context":"SFL Reviewer Gate Runner"`) ||
		strings.Contains(client.puts[0], `"context":"SFL Reviewer Approval"`) ||
		strings.Count(client.puts[0], `"type":"required_status_checks"`) != 1 {
		t.Errorf("repository ruleset update = %q, requests %v", name, client.puts)
	}
}

func TestPrepareReviewerGatesRejectsSharedRuleAlongsideDedicatedRule(t *testing.T) {
	shared := strictReviewerStatusRuleset(20, "Shared reviewer")
	shared.Rules = append(shared.Rules, struct {
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}{Type: "required_signatures", Parameters: map[string]any{}})
	dedicated := strictReviewerStatusRuleset(21, "Dedicated reviewer")
	client := &fakeREST{rulesetDetails: map[int64]repositoryRuleset{
		20: shared,
		21: dedicated,
	}}
	_, err := prepareReviewerGatesForUninstall(
		client,
		"owner",
		"repo",
		"main",
		nil,
		[]repositoryRuleset{shared, dedicated},
	)
	if err == nil || !strings.Contains(err.Error(), "contains unrelated requirements") {
		t.Fatalf("prepareReviewerGatesForUninstall() error = %v", err)
	}
	if len(client.deletes) != 0 {
		t.Errorf("prepareReviewerGatesForUninstall() deleted rulesets: %v", client.deletes)
	}
}

func TestPrepareReviewerGatesRefetchesLegacyGateEntityTag(t *testing.T) {
	legacy := legacyReviewerRuleset(22, "Legacy reviewer")
	summary := legacy
	summary.ETag = ""
	client := &fakeREST{rulesetDetails: map[int64]repositoryRuleset{
		legacy.ID: legacy,
	}}
	gates, err := prepareReviewerGatesForUninstall(
		client,
		"owner",
		"repo",
		"main",
		nil,
		[]repositoryRuleset{summary},
	)
	if err != nil {
		t.Fatalf("prepareReviewerGatesForUninstall() unexpected error: %v", err)
	}
	if len(gates) != 1 || gates[0].ETag != `"test-etag"` {
		t.Fatalf("prepared legacy gates = %+v", gates)
	}
	if !slices.Contains(client.gets, "repos/owner/repo/rulesets/22") {
		t.Fatalf("legacy ruleset detail requests = %v", client.gets)
	}
}

func TestParseSyncOptionsPR(t *testing.T) {
	opts, err := parseSyncOptions([]string{"--repo", "owner/repo", "--pr", "--source-ref", "v6.5.0"}, io.Discard)
	if err != nil {
		t.Fatalf("parseSyncOptions() unexpected error: %v", err)
	}
	if !opts.pr {
		t.Error("parseSyncOptions() pr = false, want true")
	}
	if opts.sourceRef != "v6.5.0" {
		t.Errorf("parseSyncOptions() sourceRef = %q, want v6.5.0", opts.sourceRef)
	}
}

func TestParseSyncOptionsDefaultsToPullRequest(t *testing.T) {
	opts, err := parseSyncOptions([]string{"--repo", "owner/repo"}, io.Discard)
	if err != nil {
		t.Fatalf("parseSyncOptions() unexpected error: %v", err)
	}
	if !opts.pr {
		t.Error("parseSyncOptions() pr = false, want pull-request-first default")
	}
}

func TestParseSyncOptionsDirectAndDryRun(t *testing.T) {
	opts, err := parseSyncOptions([]string{"--repo", "owner/repo", "--direct", "--dry-run"}, io.Discard)
	if err != nil {
		t.Fatalf("parseSyncOptions() unexpected error: %v", err)
	}
	if opts.pr {
		t.Error("parseSyncOptions() pr = true with --direct")
	}
	if !opts.dryRun {
		t.Error("parseSyncOptions() dryRun = false with --dry-run")
	}
}

func TestMutatingCommandsRejectProtectedRepositoryBeforeAccess(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func([]string, io.Writer, io.Writer) error
		args func(string) []string
	}{
		{name: "init", run: runInit, args: func(target string) []string { return []string{"--repo", target} }},
		{name: "sync", run: runSync, args: func(target string) []string { return []string{"--repo", target} }},
		{name: "gate", run: runGate, args: func(target string) []string { return []string{"--repo", target} }},
		{name: "add", run: runAdd, args: func(target string) []string { return []string{"--repo", target, "policy-manager"} }},
		{name: "uninstall", run: runUninstall, args: func(target string) []string { return []string{"--repo", target, "--force"} }},
		{name: "start", run: runStart, args: func(target string) []string { return []string{"--repo", target} }},
		{name: "stop", run: runStop, args: func(target string) []string { return []string{"--repo", target} }},
	} {
		t.Run(command.name, func(t *testing.T) {
			target := "HemSoft/set-it-free-loop"
			err := command.run(command.args(target), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "is protected") {
				t.Fatalf("%s returned %v, want protected repository rejection", command.name, err)
			}
		})
	}
}

func TestInitAndSyncPinEverySourceReadToSynchronizedRelease(t *testing.T) {
	initSource := string(readContractFile(t, "init.go"))
	for _, fragment := range []string{
		`fetchLatestReleaseFunc(motherRepoOwner, motherRepoName)`,
		`getCommitSHA(motherRepoOwner, motherRepoName, ref)`,
		`fetchFileRaw(motherRepoOwner, motherRepoName, "VERSION", sha)`,
		`fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, release.SHA)`,
		`fetchFileRaw(motherRepoOwner, motherRepoName, gf, release.SHA)`,
	} {
		if !strings.Contains(initSource, fragment) {
			t.Errorf("init.go missing release-pinned source read %q", fragment)
		}
	}
	if strings.Contains(initSource, "checkGhAwInstalled(stdout)") {
		t.Error("init.go requires gh-aw even though reviewer lock file is precompiled")
	}

	syncSource := string(readContractFile(t, "sync.go"))
	for _, fragment := range []string{
		`resolveDeploymentRelease(opts.sourceRef)`,
		`sourceRef := release.SHA`,
		`fetchFileRaw(motherRepoOwner, motherRepoName, srcPath, sourceRef)`,
		`fetchFileRaw(motherRepoOwner, motherRepoName, gf, sourceRef)`,
		`fetchFileRaw(motherRepoOwner, motherRepoName, "deployment/governance/labels.json", sourceRef)`,
	} {
		if !strings.Contains(syncSource, fragment) {
			t.Errorf("sync.go missing release-pinned source read %q", fragment)
		}
	}
}

func TestManifestUsesCanonicalSchemaAndReadsLegacyFields(t *testing.T) {
	legacy := []byte(`{
		"version": "6.4.1",
		"tier": "full",
		"motherRepo": "HemSoft/set-it-free-loop",
		"sourceSHA": "0123456789012345678901234567890123456789",
		"deployedAt": "2026-08-08T10:00:00Z",
		"deployedBy": "legacy",
		"components": ["sfl-pr-review"]
	}`)
	var manifest sflManifest
	if err := json.Unmarshal(legacy, &manifest); err != nil {
		t.Fatalf("unmarshal legacy manifest: %v", err)
	}
	if manifest.MotherRepo != motherRepoOwner+"/"+motherRepoName {
		t.Errorf("legacy source = %q", manifest.MotherRepo)
	}
	if manifest.SourceSHA != "0123456789012345678901234567890123456789" {
		t.Errorf("legacy source SHA = %q", manifest.SourceSHA)
	}

	canonical, err := marshalManifest(&manifest)
	if err != nil {
		t.Fatalf("marshal canonical manifest: %v", err)
	}
	if !strings.Contains(canonical, `"source": "HemSoft/set-it-free-loop"`) ||
		!strings.Contains(canonical, `"sourceSha": "0123456789012345678901234567890123456789"`) {
		t.Errorf("canonical manifest missing source fields:\n%s", canonical)
	}
	if strings.Contains(canonical, `"motherRepo"`) || strings.Contains(canonical, `"sourceSHA"`) {
		t.Errorf("canonical manifest retained legacy fields:\n%s", canonical)
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	schemaPath := filepath.Join("..", "deployment", "sfl-manifest.schema.json")
	if err := json.Unmarshal(readContractFile(t, schemaPath), &schema); err != nil {
		t.Fatalf("parse manifest schema: %v", err)
	}
	for _, property := range []string{"source", "sourceSha", "deployedBy", "addons"} {
		if _, ok := schema.Properties[property]; !ok {
			t.Errorf("manifest schema missing canonical property %q", property)
		}
	}
	var tierProperty struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["tier"], &tierProperty); err != nil {
		t.Fatalf("parse manifest tier schema: %v", err)
	}
	for _, tier := range []string{"reviewer", "review", "minimal", "standard", "full", "custom"} {
		if !slices.Contains(tierProperty.Enum, tier) {
			t.Errorf("manifest tier schema missing supported tier %q: %v", tier, tierProperty.Enum)
		}
	}
}

func TestRenderWorkflowPinsSFLVersion(t *testing.T) {
	got := renderWorkflow("SFL_CLI_VERSION: "+sflVersionPlaceholder, "6.5.0")
	if got != "SFL_CLI_VERSION: 6.5.0" {
		t.Errorf("renderWorkflow() = %q", got)
	}
}

func TestSourceReadClientUsesSeparateTokenOnlyForCentralRepositories(t *testing.T) {
	t.Setenv("SFL_SOURCE_TOKEN", "source-read-token")
	oldSourceClient := newSourceRESTClient
	sourceClient := &fakeREST{
		fileContents: map[string]string{"VERSION": "6.5.0\n"},
	}
	var receivedToken string
	newSourceRESTClient = func(token string) (restAPI, error) {
		receivedToken = token
		return sourceClient, nil
	}
	t.Cleanup(func() {
		newSourceRESTClient = oldSourceClient
	})

	client, ok, err := sourceReadClient(motherRepoOwner, motherRepoName)
	if err != nil {
		t.Fatalf("sourceReadClient() unexpected error: %v", err)
	}
	if !ok || client != sourceClient {
		t.Fatal("sourceReadClient() did not select the source client")
	}
	if receivedToken != "source-read-token" {
		t.Errorf("sourceReadClient() token = %q", receivedToken)
	}
	content, err := fetchFileRaw(motherRepoOwner, motherRepoName, "VERSION", "")
	if err != nil {
		t.Fatalf("fetchFileRaw() unexpected error: %v", err)
	}
	if content != "6.5.0\n" {
		t.Errorf("fetchFileRaw() = %q", content)
	}

	if _, ok, err := sourceReadClient("consumer", "repo"); err != nil || ok {
		t.Errorf("sourceReadClient() selected source credentials for consumer repository: ok=%v err=%v", ok, err)
	}
	if _, ok, err := sourceReadClient(motherRepoOwner, extensionName); err != nil || ok {
		t.Errorf("sourceReadClient() selected a separate extension repository: ok=%v err=%v", ok, err)
	}
}

func TestGetCommitSHAPinsConfiguredSourceRef(t *testing.T) {
	t.Setenv("SFL_SOURCE_TOKEN", "source-read-token")
	oldSourceClient := newSourceRESTClient
	sourceClient := &fakeREST{commitSHA: "tagged-commit-sha"}
	newSourceRESTClient = func(string) (restAPI, error) {
		return sourceClient, nil
	}
	t.Cleanup(func() {
		newSourceRESTClient = oldSourceClient
	})

	got, err := getCommitSHA(motherRepoOwner, motherRepoName, "v6.5.0")
	if err != nil {
		t.Fatalf("getCommitSHA() unexpected error: %v", err)
	}
	if got != "tagged-commit-sha" {
		t.Errorf("getCommitSHA() = %q, want tagged-commit-sha", got)
	}
	if len(sourceClient.gets) != 1 ||
		!strings.Contains(sourceClient.gets[0], "/commits?per_page=1&sha=v6.5.0") {
		t.Errorf("getCommitSHA() requested %v, want immutable tag ref", sourceClient.gets)
	}
}

func TestResolveDeploymentReleaseUsesLatestTagAndPinnedVersion(t *testing.T) {
	t.Setenv("SFL_SOURCE_TOKEN", "source-read-token")
	oldLatest := fetchLatestReleaseFunc
	oldSourceClient := newSourceRESTClient
	sourceClient := &fakeREST{
		commitSHA:    "0123456789012345678901234567890123456789",
		fileContents: map[string]string{"VERSION": "6.5.1\n"},
	}
	fetchLatestReleaseFunc = func(owner, repo string) (string, error) {
		if owner != motherRepoOwner || repo != motherRepoName {
			t.Fatalf("latest release requested from %s/%s", owner, repo)
		}
		return "v6.5.1", nil
	}
	newSourceRESTClient = func(string) (restAPI, error) {
		return sourceClient, nil
	}
	t.Cleanup(func() {
		fetchLatestReleaseFunc = oldLatest
		newSourceRESTClient = oldSourceClient
	})

	release, err := resolveDeploymentRelease("")
	if err != nil {
		t.Fatalf("resolveDeploymentRelease() unexpected error: %v", err)
	}
	if release.Ref != "v6.5.1" || release.Version != "6.5.1" ||
		release.SHA != "0123456789012345678901234567890123456789" {
		t.Errorf("resolveDeploymentRelease() = %+v", release)
	}
	if len(sourceClient.gets) != 2 ||
		!strings.Contains(sourceClient.gets[0], "sha=v6.5.1") ||
		!strings.Contains(sourceClient.gets[1], "contents/VERSION?ref="+release.SHA) {
		t.Errorf("release source requests = %v", sourceClient.gets)
	}
}

func TestSemanticVersionPatternAcceptsPrereleaseAndRejectsMalformedVersions(t *testing.T) {
	valid := []string{"0.0.0", "2.0.0", "2.1.0-rc.1", "2.1.0-alpha-beta+build-meta.7"}
	for _, version := range valid {
		if !semanticVersionPattern.MatchString(version) {
			t.Errorf("semanticVersionPattern rejected %q", version)
		}
	}
	invalid := []string{
		"v2.0.0", "2.0", "2.0.0-", "2.0.0+", "2.0.0 rc.1",
		"01.0.0", "1.01.0", "1.0.01", "1.0.0-rc.01",
	}
	for _, version := range invalid {
		if semanticVersionPattern.MatchString(version) {
			t.Errorf("semanticVersionPattern accepted %q", version)
		}
	}
}

func TestResolveDeploymentReleaseAcceptsExplicitPrerelease(t *testing.T) {
	t.Setenv("SFL_SOURCE_TOKEN", "source-read-token")
	oldSourceClient := newSourceRESTClient
	sourceClient := &fakeREST{
		commitSHA:    "0123456789012345678901234567890123456789",
		fileContents: map[string]string{"VERSION": "2.1.0-rc.1\n"},
	}
	newSourceRESTClient = func(string) (restAPI, error) { return sourceClient, nil }
	t.Cleanup(func() { newSourceRESTClient = oldSourceClient })

	release, err := resolveDeploymentRelease("v2.1.0-rc.1")
	if err != nil {
		t.Fatalf("resolveDeploymentRelease() prerelease error: %v", err)
	}
	if release.Ref != "v2.1.0-rc.1" || release.Version != "2.1.0-rc.1" {
		t.Errorf("resolveDeploymentRelease() = %+v", release)
	}
}

func TestDeploymentBranchName(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 34, 56, 123456789, time.FixedZone("local", -4*60*60))
	got := deploymentBranchName("Sync SFL!", now)
	want := "sfl/sync-sfl-20260809-163456.123456789"
	if got != want {
		t.Errorf("deploymentBranchName() = %q, want %q", got, want)
	}
}

func TestBuildFileAdditionsSortsAndEncodes(t *testing.T) {
	got := buildFileAdditions(map[string]string{
		"z.yml": "last",
		"a.yml": "first",
	})
	if len(got) != 2 {
		t.Fatalf("buildFileAdditions() returned %d items, want 2", len(got))
	}
	if got[0].Path != "a.yml" || got[1].Path != "z.yml" {
		t.Fatalf("buildFileAdditions() paths = %q, %q, want sorted paths", got[0].Path, got[1].Path)
	}
	decoded, err := base64.StdEncoding.DecodeString(got[0].Contents)
	if err != nil {
		t.Fatalf("decoding addition: %v", err)
	}
	if string(decoded) != "first" {
		t.Errorf("decoded contents = %q, want %q", decoded, "first")
	}
}

func TestSplitCommitMessage(t *testing.T) {
	headline, body := splitCommitMessage("chore: sync SFL\n\nGenerated by gh-sfl")
	if headline != "chore: sync SFL" {
		t.Errorf("headline = %q", headline)
	}
	if body != "Generated by gh-sfl" {
		t.Errorf("body = %q", body)
	}

	headline, body = splitCommitMessage(strings.TrimSpace("single line"))
	if headline != "single line" || body != "" {
		t.Errorf("single-line result = %q, %q", headline, body)
	}
}

func TestDeployViaPullRequest(t *testing.T) {
	rest := &fakeREST{}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	var output bytes.Buffer
	got, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"sync",
		map[string]string{".github/workflows/sfl.yml": "workflow"},
		"chore: sync SFL\n\nGenerated by test",
		true,
		&output,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	if got != "https://github.test/pull/1" {
		t.Errorf("deployViaPullRequest() URL = %q", got)
	}
	if len(rest.posts) != 2 {
		t.Fatalf("REST posts = %d, want branch and pull request", len(rest.posts))
	}
	if !strings.Contains(rest.posts[0], `"ref":"refs/heads/sfl/sync-20260809-123456.000000000"`) {
		t.Errorf("branch request = %s", rest.posts[0])
	}
	if !strings.Contains(rest.posts[1], `"base":"main"`) || !strings.Contains(rest.posts[1], `"head":"sfl/sync-`) {
		t.Errorf("pull request = %s", rest.posts[1])
	}
	if !strings.Contains(graphQL.query, "createCommitOnBranch") {
		t.Errorf("GraphQL query = %q", graphQL.query)
	}

	input, ok := graphQL.variables["input"].(map[string]any)
	if !ok {
		t.Fatalf("GraphQL input type = %T", graphQL.variables["input"])
	}
	if input["expectedHeadOid"] != "base-sha" {
		t.Errorf("expectedHeadOid = %v", input["expectedHeadOid"])
	}
	changes := input["fileChanges"].(map[string]any)
	additions := changes["additions"].([]fileAddition)
	if len(additions) != 1 {
		t.Fatalf("GraphQL additions = %+v", additions)
	}
	workflowAddition, ok := findAddition(additions, ".github/workflows/sfl.yml")
	if !ok {
		t.Fatalf("GraphQL additions are missing the workflow: %+v", additions)
	}
	if _, ok := findAddition(additions, "CODEOWNERS"); ok {
		t.Fatalf("GraphQL additions unexpectedly overwrite consumer CODEOWNERS: %+v", additions)
	}
	decoded, err := base64.StdEncoding.DecodeString(workflowAddition.Contents)
	if err != nil || string(decoded) != "workflow" {
		t.Errorf("GraphQL addition contents = %q, error = %v", decoded, err)
	}
	if len(rest.deletes) != 0 {
		t.Errorf("unexpected branch cleanup: %v", rest.deletes)
	}
}

func TestDeployViaPullRequestAddDoesNotDeleteInstalledWorkflows(t *testing.T) {
	installedWorkflow := ".github/workflows/sfl-dispatcher.yml"
	rest := &fakeREST{
		fileContents: map[string]string{
			installedWorkflow: "installed tier workflow",
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	_, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"add",
		map[string]string{
			".github/workflows/sfl-pr-review.yml": "add-on workflow",
			".sfl/sfl.json":                       `{"tier":"standard","addons":["pr-review"]}`,
		},
		"chore: add SFL add-on",
		false,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}

	input := graphQL.variables["input"].(map[string]any)
	fileChanges := input["fileChanges"].(map[string]any)
	if deletions, ok := fileChanges["deletions"]; ok {
		t.Errorf("add pull request unexpectedly reconciled installed workflows: %+v", deletions)
	}
}

func findAddition(additions []fileAddition, path string) (fileAddition, bool) {
	for _, addition := range additions {
		if addition.Path == path {
			return addition, true
		}
	}
	return fileAddition{}, false
}

func TestDeployViaPullRequestUpdatesOpenPR(t *testing.T) {
	rest := &fakeREST{
		openPRURL:  "https://github.test/pull/existing",
		openPRPage: 2,
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	got, err := deployViaPullRequest("owner", "repo", "main", "sync", map[string]string{"file": "content"}, "sync", true, io.Discard)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	if got != rest.openPRURL {
		t.Errorf("deployViaPullRequest() URL = %q, want %q", got, rest.openPRURL)
	}
	if len(rest.posts) != 0 {
		t.Errorf("existing PR caused REST writes: %d", len(rest.posts))
	}
	if len(rest.patches) != 1 ||
		!strings.Contains(rest.patches[0], "repos/owner/repo/pulls/42") ||
		!strings.Contains(rest.patches[0], `"title":"sync"`) {
		t.Errorf("existing PR title update = %#v", rest.patches)
	}
	if !strings.Contains(graphQL.query, "createCommitOnBranch") {
		t.Errorf("existing PR branch was not updated: query=%q", graphQL.query)
	}
	input := graphQL.variables["input"].(map[string]any)
	if input["expectedHeadOid"] != "existing-head-sha" {
		t.Errorf("expectedHeadOid = %v, want existing-head-sha", input["expectedHeadOid"])
	}
	branch := input["branch"].(map[string]string)
	if branch["branchName"] != "sfl/sync-existing" {
		t.Errorf("branchName = %q, want existing deployment branch", branch["branchName"])
	}
}

func TestDeployViaPullRequestIgnoresOpenPRForOldBase(t *testing.T) {
	rest := &fakeREST{
		openPRURL:  "https://github.test/pull/existing",
		openPRBase: "legacy",
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	got, err := deployViaPullRequest("owner", "repo", "main", "sync", map[string]string{"file": "content"}, "sync", true, io.Discard)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	if got != "https://github.test/pull/1" {
		t.Errorf("deployViaPullRequest() URL = %q, want a new pull request", got)
	}
	if len(rest.posts) != 2 {
		t.Fatalf("REST posts = %d, want a new branch and pull request", len(rest.posts))
	}
	input := graphQL.variables["input"].(map[string]any)
	branch := input["branch"].(map[string]string)
	if branch["branchName"] == "sfl/sync-existing" {
		t.Error("deployment reused a pull request targeting the old base branch")
	}
}

func TestDeployViaPullRequestSkipsCurrentOpenPR(t *testing.T) {
	currentManifest := `{
		"version": "6.5.0",
		"tier": "reviewer",
		"source": "HemSoft/set-it-free-loop",
		"deployedAt": "2026-08-08T10:00:00Z",
		"deployedBy": "previous-user",
		"sourceSha": "source-sha",
		"components": ["sfl-pr-review"]
	}`
	desiredManifest := `{
		"version": "6.5.0",
		"tier": "reviewer",
		"source": "HemSoft/set-it-free-loop",
		"deployedAt": "2026-08-09T10:00:00Z",
		"deployedBy": "scheduled-sync",
		"sourceSha": "source-sha",
		"components": ["sfl-pr-review"]
	}`
	rest := &fakeREST{
		openPRURL:       "https://github.test/pull/existing",
		manifestContent: currentManifest,
		fileContents: map[string]string{
			"file": "content",
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	var output bytes.Buffer
	got, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"sync",
		map[string]string{
			".sfl/sfl.json": desiredManifest,
			"file":          "content",
		},
		"sync",
		true,
		&output,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	if got != rest.openPRURL {
		t.Errorf("deployViaPullRequest() URL = %q, want %q", got, rest.openPRURL)
	}
	if graphQL.query != "" || len(rest.posts) != 0 || len(rest.patches) != 1 {
		t.Errorf("current PR caused unexpected writes: query=%q posts=%d patches=%d", graphQL.query, len(rest.posts), len(rest.patches))
	}
	if !strings.Contains(rest.patches[0], `"title":"sync"`) {
		t.Errorf("current PR title update = %s", rest.patches[0])
	}
	if !strings.Contains(output.String(), "already up to date") {
		t.Errorf("output = %q, want already up to date", output.String())
	}
}

func TestDeployViaPullRequestSkipsCurrentDefaultBranch(t *testing.T) {
	reviewerWorkflow := string(readContractFile(t, filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml")))
	currentManifest := `{
		"version": "6.5.0",
		"tier": "reviewer",
		"source": "HemSoft/set-it-free-loop",
		"deployedAt": "2026-08-08T10:00:00Z",
		"deployedBy": "previous-user",
		"sourceSha": "source-sha",
		"components": ["sfl-pr-review"]
	}`
	desiredManifest := strings.ReplaceAll(
		strings.ReplaceAll(currentManifest, "2026-08-08T10:00:00Z", "2026-08-09T10:00:00Z"),
		"previous-user",
		"scheduled-sync",
	)
	manifestMatches, err := deploymentManifestContentsMatch([]byte(currentManifest), desiredManifest)
	if err != nil || !manifestMatches {
		t.Fatalf("deployment manifests should match after audit-field changes: matches=%v err=%v", manifestMatches, err)
	}
	rest := &fakeREST{
		manifestContent: currentManifest,
		fileContents: map[string]string{
			".github/workflows/sfl-pr-review-auto.yml": reviewerWorkflow,
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	var output bytes.Buffer
	got, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"init",
		map[string]string{
			".sfl/sfl.json": desiredManifest,
			".github/workflows/sfl-pr-review-auto.yml": reviewerWorkflow,
		},
		"init",
		true,
		&output,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	if got != "" || graphQL.query != "" {
		t.Errorf("no-op deployment returned URL %q or created commit %q", got, graphQL.query)
	}
	if len(rest.posts) != 0 || len(rest.deletes) != 0 {
		t.Errorf("no-op deployment created branch writes posts=%v deletes=%v", rest.posts, rest.deletes)
	}
	if !strings.Contains(output.String(), "no pull request needed") {
		t.Errorf("output = %q", output.String())
	}
}

func TestDeploymentFileContentAcceptsWrappedBase64(t *testing.T) {
	rest := &fakeREST{
		base64LineBreaks: true,
		fileContents: map[string]string{
			".github/workflows/sfl.yml": strings.Repeat("workflow content\n", 8),
		},
	}
	got, exists, err := deploymentFileContent(rest, "owner", "repo", "branch", ".github/workflows/sfl.yml")
	if err != nil {
		t.Fatalf("deploymentFileContent() unexpected error: %v", err)
	}
	if !exists || string(got) != rest.fileContents[".github/workflows/sfl.yml"] {
		t.Errorf("deploymentFileContent() = %q, %v", got, exists)
	}
}

func TestDeployViaPullRequestRepairsDriftOutsideManifest(t *testing.T) {
	manifest := `{
		"version": "6.5.0",
		"tier": "full",
		"motherRepo": "relias-engineering/set-it-free-loop",
		"sourceSHA": "source-sha"
	}`
	rest := &fakeREST{
		openPRURL:       "https://github.test/pull/existing",
		manifestContent: manifest,
		fileContents: map[string]string{
			".github/workflows/sfl.yml": "edited",
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	_, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"sync",
		map[string]string{
			".sfl/sfl.json":             manifest,
			".github/workflows/sfl.yml": "expected",
		},
		"sync",
		true,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	if !strings.Contains(graphQL.query, "createCommitOnBranch") {
		t.Error("drift outside the manifest did not update the existing pull request")
	}
}

func TestDeploymentManifestContentsMatchRepairsMalformedCurrent(t *testing.T) {
	desired := `{
		"version": "6.5.0",
		"tier": "minimal",
		"motherRepo": "relias-engineering/set-it-free-loop",
		"sourceSHA": "source-sha"
	}`
	matches, err := deploymentManifestContentsMatch([]byte(`{"version":`), desired)
	if err != nil {
		t.Fatalf("deploymentManifestContentsMatch() unexpected error: %v", err)
	}
	if matches {
		t.Error("deploymentManifestContentsMatch() accepted malformed current manifest")
	}
}

func TestDeployViaPullRequestRemovesStaleManagedWorkflows(t *testing.T) {
	manifest := `{
		"version": "6.5.0",
		"tier": "minimal",
		"motherRepo": "relias-engineering/set-it-free-loop",
		"sourceSHA": "source-sha",
		"components": ["labels", "governance", "sfl-dispatcher"]
	}`
	rest := &fakeREST{
		openPRURL:       "https://github.test/pull/existing",
		openPRBranch:    "sfl/init-existing",
		manifestContent: manifest,
		fileContents: map[string]string{
			".github/workflows/sfl-dispatcher.yml": "dispatcher",
			".github/workflows/sfl-pr-review.md":   "stale reviewer",
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	_, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"init",
		map[string]string{
			".sfl/sfl.json":                        manifest,
			".github/workflows/sfl-dispatcher.yml": "dispatcher",
		},
		"deploy minimal",
		true,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	input := graphQL.variables["input"].(map[string]any)
	changes := input["fileChanges"].(map[string]any)
	deletions := changes["deletions"].([]fileDeletion)
	if len(deletions) != 1 || deletions[0].Path != ".github/workflows/sfl-pr-review.md" {
		t.Errorf("GraphQL deletions = %+v, want stale reviewer workflow", deletions)
	}
}

func TestDeployViaPullRequestRemovesLegacyGovernanceFromReviewerInstall(t *testing.T) {
	reviewerWorkflow := string(readContractFile(t, filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml")))
	manifest := `{
		"version": "6.5.1",
		"tier": "reviewer",
		"source": "HemSoft/set-it-free-loop",
		"sourceSha": "source-sha",
		"components": ["sfl-pr-review", "sfl-pr-review-auto"]
	}`
	governancePath := ".sfl/governance/policy.md"
	rest := &fakeREST{
		openPRURL:       "https://github.test/pull/existing",
		openPRBranch:    "sfl/init-existing",
		manifestContent: manifest,
		fileContents: map[string]string{
			".github/workflows/sfl-pr-review-auto.yml": reviewerWorkflow,
			governancePath: "legacy policy",
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	_, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"init",
		map[string]string{
			".sfl/sfl.json": manifest,
			".github/workflows/sfl-pr-review-auto.yml": reviewerWorkflow,
		},
		"deploy reviewer",
		true,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	input := graphQL.variables["input"].(map[string]any)
	deletions := input["fileChanges"].(map[string]any)["deletions"].([]fileDeletion)
	if !slices.ContainsFunc(deletions, func(item fileDeletion) bool {
		return item.Path == governancePath
	}) {
		t.Errorf("deletions = %+v, want %s", deletions, governancePath)
	}
}

func TestDeployViaPullRequestRemovesRetiredWorkflowTombstone(t *testing.T) {
	manifest := `{
		"version": "6.5.0",
		"tier": "minimal",
		"motherRepo": "relias-engineering/set-it-free-loop",
		"sourceSHA": "source-sha"
	}`
	retiredPath := ".github/workflows/sfl-copilot-review-bridge.yml"
	rest := &fakeREST{
		openPRURL:       "https://github.test/pull/existing",
		manifestContent: manifest,
		fileContents: map[string]string{
			".github/workflows/sfl-dispatcher.yml": "dispatcher",
			retiredPath:                            "obsolete workflow",
		},
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)

	_, err := deployViaPullRequest(
		"owner",
		"repo",
		"main",
		"sync",
		map[string]string{
			".sfl/sfl.json":                        manifest,
			".github/workflows/sfl-dispatcher.yml": "dispatcher",
		},
		"sync",
		true,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("deployViaPullRequest() unexpected error: %v", err)
	}
	input := graphQL.variables["input"].(map[string]any)
	deletions := input["fileChanges"].(map[string]any)["deletions"].([]fileDeletion)
	if len(deletions) != 1 || deletions[0].Path != retiredPath {
		t.Errorf("GraphQL deletions = %+v, want retired workflow tombstone", deletions)
	}
}
