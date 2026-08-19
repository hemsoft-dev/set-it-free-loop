package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type reviewREST struct {
	comments             []reviewTriggerComment
	reactions            []reviewCommentReaction
	statuses             []reviewRequestStatus
	reactionGets         int
	clearReaction        bool
	checkRuns            []map[string]any
	checkRunPages        map[int][]map[string]any
	headRepo             string
	headRepoUnavailable  bool
	baseRef              string
	defaultBranch        string
	observerBranch       string
	observerState        string
	updatedAt            string
	workflowRunResponses [][]map[string]any
	workflowRunGets      int
	posts                int
	statusPosts          int
	statusPostErr        error
	statusBody           map[string]string
	postBody             string
	deletes              int
	deletePath           string
	deleteErr            error
}

func (f *reviewREST) Get(path string, response interface{}) error {
	switch {
	case strings.Contains(path, "/contents/.github/workflows/sfl-pr-review-auto.yml"):
		observerBranch := f.observerBranch
		if observerBranch == "" {
			observerBranch = f.defaultBranch
		}
		if observerBranch == "" {
			observerBranch = "main"
		}
		yamlBranch := strings.ReplaceAll(observerBranch, "'", "''")
		content := fmt.Sprintf("name: SFL Codex Review Observer\non:\n  push:\n    branches: ['%s']\nenv:\n  SFL_REVIEW_BASE_BRANCH: '%s'\nif: github.event.sender.id == 199175422\n", yamlBranch, yamlBranch)
		return decodeTestResponse(response, map[string]any{
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			"encoding": "base64",
		})
	case strings.Contains(path, "/actions/workflows/sfl-pr-review-auto.yml/runs?"):
		f.workflowRunGets++
		if len(f.workflowRunResponses) == 0 {
			return decodeTestResponse(response, map[string]any{"workflow_runs": []map[string]any{}})
		}
		index := f.workflowRunGets - 1
		if index >= len(f.workflowRunResponses) {
			index = len(f.workflowRunResponses) - 1
		}
		return decodeTestResponse(response, map[string]any{"workflow_runs": f.workflowRunResponses[index]})
	case strings.Contains(path, "/actions/workflows/sfl-pr-review-auto.yml"):
		state := f.observerState
		if state == "" {
			state = "active"
		}
		return decodeTestResponse(response, map[string]any{"state": state})
	case strings.Contains(path, "/actions/variables/SFL_ENABLED"):
		return decodeTestResponse(response, map[string]any{"value": "true"})
	case path == "repos/HemSoft/consumer":
		defaultBranch := f.defaultBranch
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		return decodeTestResponse(response, map[string]any{"default_branch": defaultBranch})
	case strings.Contains(path, "/check-runs?"):
		for page, runs := range f.checkRunPages {
			if strings.Contains(path, fmt.Sprintf("page=%d", page)) {
				return decodeTestResponse(response, map[string]any{"check_runs": runs})
			}
		}
		return decodeTestResponse(response, map[string]any{"check_runs": f.checkRuns})
	case strings.Contains(path, "/statuses?"):
		return decodeTestResponse(response, f.statuses)
	case strings.Contains(path, "/pulls/"):
		var headRepo any = map[string]string{"full_name": f.headRepo}
		if f.headRepo == "" {
			headRepo = map[string]string{"full_name": "HemSoft/consumer"}
		}
		if f.headRepoUnavailable {
			headRepo = nil
		}
		baseRef := f.baseRef
		if baseRef == "" {
			baseRef = "main"
		}
		return decodeTestResponse(response, map[string]any{
			"base":       map[string]any{"sha": strings.Repeat("a", 40), "ref": baseRef, "repo": map[string]string{"full_name": "HemSoft/consumer"}},
			"head":       map[string]any{"sha": strings.Repeat("b", 40), "repo": headRepo},
			"state":      "open",
			"updated_at": f.updatedAt,
		})
	case strings.Contains(path, "/comments?"):
		return decodeTestResponse(response, f.comments)
	case strings.Contains(path, "/reactions?"):
		f.reactionGets++
		if f.clearReaction && f.reactionGets > 1 {
			return decodeTestResponse(response, []reviewCommentReaction{})
		}
		return decodeTestResponse(response, f.reactions)
	default:
		return fmt.Errorf("unexpected GET %s", path)
	}
}

func (f *reviewREST) GetWithETag(string, interface{}) (string, error) {
	return "", fmt.Errorf("unexpected GetWithETag")
}

func (f *reviewREST) Post(path string, body io.Reader, response interface{}) error {
	if strings.Contains(path, "/statuses/") {
		f.statusPosts++
		if err := json.NewDecoder(body).Decode(&f.statusBody); err != nil {
			return err
		}
		return f.statusPostErr
	}
	if !strings.HasSuffix(path, "/issues/94/comments") {
		return fmt.Errorf("unexpected POST %s", path)
	}
	f.posts++
	var payload map[string]string
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return err
	}
	f.postBody = payload["body"]
	return decodeTestResponse(response, map[string]any{
		"id":       999,
		"html_url": "https://github.test/HemSoft/consumer/pull/94#issuecomment-999",
	})
}

func (f *reviewREST) Put(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected PUT")
}
func (f *reviewREST) Patch(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected PATCH")
}
func (f *reviewREST) Delete(path string, _ interface{}) error {
	f.deletes++
	f.deletePath = path
	return f.deleteErr
}

func TestParseReviewOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantPR  int
		wantErr string
	}{
		{name: "positional PR", args: []string{"94"}, wantPR: 94},
		{name: "hash prefix", args: []string{"#94"}, wantPR: 94},
		{name: "PR flag", args: []string{"--pr", "94"}, wantPR: 94},
		{name: "repo and PR flags", args: []string{"--repo", "owner/repo", "--pr", "94"}, wantPR: 94},
		{name: "retry", args: []string{"--retry", "94"}, wantPR: 94},
		{name: "missing number", args: []string{}, wantErr: "required"},
		{name: "duplicate number", args: []string{"--pr", "94", "94"}, wantErr: "exactly once"},
		{name: "two numbers", args: []string{"1", "2"}, wantErr: "exactly once"},
		{name: "not a number", args: []string{"abc"}, wantErr: "invalid pull request number"},
		{name: "zero", args: []string{"0"}, wantErr: "invalid pull request number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseReviewOptions(tc.args, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseReviewOptions(%v) error = %v, want %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReviewOptions(%v) unexpected error: %v", tc.args, err)
			}
			if opts.pr != tc.wantPR {
				t.Errorf("parseReviewOptions(%v) pr = %d, want %d", tc.args, opts.pr, tc.wantPR)
			}
		})
	}
}

func TestRunReviewPostsOneHeadBoundCodexRequest(t *testing.T) {
	rest := &reviewREST{}
	installReviewFakes(t, rest)

	var stdout bytes.Buffer
	if err := runReview([]string{"--repo", "HemSoft/consumer", "--pr", "94"}, &stdout, io.Discard); err != nil {
		t.Fatalf("runReview() unexpected error: %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("Codex request posts = %d, want 1", rest.posts)
	}
	if rest.statusPosts != 1 ||
		rest.statusBody["context"] != codexReviewRequestRegistryContext ||
		rest.statusBody["target_url"] != "https://github.test/HemSoft/consumer/pull/94#issuecomment-999" {
		t.Fatalf("request registry status = posts %d, body %#v", rest.statusPosts, rest.statusBody)
	}
	wantMarker := codexReviewMarker(strings.Repeat("b", 40), strings.Repeat("a", 40), "none")
	if rest.postBody != codexReviewCommand+"\n\n"+wantMarker {
		t.Fatalf("posted body = %q", rest.postBody)
	}
	if !strings.Contains(stdout.String(), "subscription-backed Codex review") ||
		!strings.Contains(stdout.String(), "#issuecomment-999") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunReviewDeletesRequestWhenRegistrationFails(t *testing.T) {
	rest := &reviewREST{statusPostErr: errors.New("status unavailable")}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--pr", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "deleted unregistered request comment 999") {
		t.Fatalf("runReview() registration error = %v", err)
	}
	if rest.deletes != 1 || rest.deletePath != "repos/HemSoft/consumer/issues/comments/999" {
		t.Fatalf("request cleanup = deletes %d, path %q", rest.deletes, rest.deletePath)
	}
}

func TestRunReviewReportsRequestCleanupFailure(t *testing.T) {
	rest := &reviewREST{
		statusPostErr: errors.New("status unavailable"),
		deleteErr:     errors.New("delete unavailable"),
	}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--pr", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "status unavailable") ||
		!strings.Contains(err.Error(), "deleting unregistered request comment 999: delete unavailable") {
		t.Fatalf("runReview() cleanup error = %v", err)
	}
}

func TestRunReviewWaitsForCurrentInvalidationBeforeReadingContext(t *testing.T) {
	updatedAt := "2026-08-19T00:00:00Z"
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	run := func(status string) map[string]any {
		return map[string]any{
			"id":         77,
			"event":      "pull_request_target",
			"status":     status,
			"created_at": updatedAt,
			"pull_requests": []map[string]any{{
				"number": 94,
				"head":   map[string]string{"sha": head},
				"base":   map[string]string{"sha": base},
			}},
		}
	}
	contextToken := "sfl-codex-review:pull-context:at:1787097600000:77"
	rest := &reviewREST{
		updatedAt: updatedAt,
		workflowRunResponses: [][]map[string]any{
			{},
			{},
			{},
			{},
			{},
			{run("queued")},
			{},
		},
		checkRuns: []map[string]any{{
			"id":          5,
			"external_id": contextToken,
			"app":         map[string]any{"id": 15368},
		}},
	}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() invalidation barrier error = %v", err)
	}
	if rest.workflowRunGets != 15 {
		t.Fatalf("workflow run GETs = %d, want 15", rest.workflowRunGets)
	}
	wantMarker := codexReviewMarker(head, base, contextToken)
	if !strings.Contains(rest.postBody, wantMarker) {
		t.Fatalf("posted body = %q, want current context marker %q", rest.postBody, wantMarker)
	}
}

func TestReviewWorkflowRunAppliesOnlyToExactContext(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	run := reviewWorkflowRun{
		Event:     "pull_request_target",
		Status:    "in_progress",
		CreatedAt: "2026-08-19T00:00:00Z",
	}
	run.PullRequests = append(run.PullRequests, struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	}{Number: 94})
	run.PullRequests[0].Head.SHA = head
	run.PullRequests[0].Base.SHA = base

	if !reviewWorkflowRunApplies(run, 94, "main", head, base) {
		t.Fatal("exact pull-request invalidation did not apply")
	}
	if reviewWorkflowRunApplies(run, 95, "main", head, base) {
		t.Fatal("another pull request invalidation applied")
	}
	run.Event = "push"
	run.HeadBranch = "main"
	run.HeadSHA = base
	if !reviewWorkflowRunApplies(run, 94, "main", head, base) {
		t.Fatal("exact default-branch push invalidation did not apply")
	}
	run.Status = "completed"
	if reviewWorkflowRunApplies(run, 94, "main", head, base) {
		t.Fatal("completed invalidation run remained active")
	}
}

func TestFetchActiveReviewWorkflowRunsPaginatesEveryStatus(t *testing.T) {
	firstPage := make([]map[string]any, 100)
	for index := range firstPage {
		firstPage[index] = map[string]any{"id": index + 1, "status": "requested"}
	}
	rest := &reviewREST{workflowRunResponses: [][]map[string]any{
		firstPage,
		{{"id": 101, "status": "requested"}},
		{},
	}}

	runs, err := fetchActiveReviewWorkflowRuns(rest, "HemSoft", "consumer")
	if err != nil {
		t.Fatalf("fetchActiveReviewWorkflowRuns() error = %v", err)
	}
	if len(runs) != 101 {
		t.Fatalf("active workflow runs = %d, want 101", len(runs))
	}
	if rest.workflowRunGets != 6 {
		t.Fatalf("workflow run GETs = %d, want 6", rest.workflowRunGets)
	}
}

func TestRunReviewRejectsForkPullRequest(t *testing.T) {
	rest := &reviewREST{headRepo: "contributor/consumer"}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "same-repository branches only") ||
		!strings.Contains(err.Error(), "contributor/consumer") {
		t.Fatalf("runReview() fork error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("fork review posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewRejectsMissingHeadRepository(t *testing.T) {
	rest := &reviewREST{headRepoUnavailable: true}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no available head repository") {
		t.Fatalf("runReview() missing head repository error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("missing-head review posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewRejectsNonDefaultBaseBranch(t *testing.T) {
	rest := &reviewREST{baseRef: "release/next", defaultBranch: "main"}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "support only the default branch main") ||
		!strings.Contains(err.Error(), "release/next") {
		t.Fatalf("runReview() non-default base error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("non-default-base review posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewRejectsDisabledObserver(t *testing.T) {
	rest := &reviewREST{observerState: "disabled_manually"}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "disabled_manually") ||
		!strings.Contains(err.Error(), "enable sfl-pr-review-auto.yml") {
		t.Fatalf("runReview() disabled observer error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("disabled-observer review posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewRejectsStaleObserverBaseBranch(t *testing.T) {
	rest := &reviewREST{defaultBranch: "main", observerBranch: "trunk"}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "retired or stale") ||
		!strings.Contains(err.Error(), "gh sfl sync") {
		t.Fatalf("runReview() stale observer error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("stale-observer review posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewAcceptsYamlEscapedObserverBaseBranch(t *testing.T) {
	branch := "release/o'brien"
	rest := &reviewREST{defaultBranch: branch, baseRef: branch}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() quoted observer branch error = %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("quoted-observer review posts = %d, want 1", rest.posts)
	}
}

func TestRunReviewRejectsSameHeadRequestedAgainstAnotherBase(t *testing.T) {
	head := strings.Repeat("b", 40)
	rest := &reviewREST{comments: []reviewTriggerComment{{
		Body:    codexReviewCommand + "\n\n" + codexReviewMarker(head, strings.Repeat("c", 40), "none"),
		HTMLURL: "https://github.test/old-base-request",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already requested against another base") ||
		!strings.Contains(err.Error(), "update the pull request branch to a new head") {
		t.Fatalf("runReview() cross-base head reuse error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("cross-base head reuse posts = %d, want 0", rest.posts)
	}
}

func TestFetchReviewContextTokenUsesLatestInvalidation(t *testing.T) {
	rest := &reviewREST{checkRuns: []map[string]any{{
		"id":          2,
		"external_id": "sfl-codex-review:pull-context:at:123:456",
		"app":         map[string]any{"id": 15368},
	}, {
		"id":          1,
		"external_id": "sfl-codex-review:base-advance:at:100:400:94",
		"app":         map[string]any{"id": 15368},
	}}}
	token, err := fetchReviewContextToken(rest, "HemSoft", "consumer", strings.Repeat("b", 40))
	if err != nil {
		t.Fatalf("fetchReviewContextToken() error = %v", err)
	}
	if token != "sfl-codex-review:pull-context:at:123:456" {
		t.Fatalf("context token = %q", token)
	}
}

func TestFetchReviewContextTokenPaginatesPastTerminalHistory(t *testing.T) {
	firstPage := make([]map[string]any, 100)
	for index := range firstPage {
		firstPage[index] = map[string]any{
			"id":          index + 1,
			"external_id": fmt.Sprintf("sfl-codex-review:pull:94:request:%d", index+1),
			"app":         map[string]any{"id": 15368},
		}
	}
	want := "sfl-codex-review:pull-context:at:123:456"
	rest := &reviewREST{checkRunPages: map[int][]map[string]any{
		1: firstPage,
		2: {{
			"id":          101,
			"external_id": want,
			"app":         map[string]any{"id": 15368},
		}},
	}}

	token, err := fetchReviewContextToken(rest, "HemSoft", "consumer", strings.Repeat("b", 40))
	if err != nil {
		t.Fatalf("fetchReviewContextToken() error = %v", err)
	}
	if token != want {
		t.Fatalf("context token = %q, want paginated %q", token, want)
	}
}

func TestRunReviewDeduplicatesCurrentHeadRequest(t *testing.T) {
	head := strings.Repeat("b", 40)
	rest := &reviewREST{comments: []reviewTriggerComment{{
		Body:    codexReviewCommand + "\n\n" + codexReviewMarker(head, strings.Repeat("a", 40), "none"),
		HTMLURL: "https://github.test/existing",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	var stdout bytes.Buffer
	if err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, &stdout, io.Discard); err != nil {
		t.Fatalf("runReview() unexpected error: %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("Codex request posts = %d, want 0", rest.posts)
	}
	if !strings.Contains(stdout.String(), "already requested") ||
		!strings.Contains(stdout.String(), "https://github.test/existing") ||
		!strings.Contains(stdout.String(), "--retry") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunReviewRetryPostsAnotherCurrentHeadRequest(t *testing.T) {
	head := strings.Repeat("b", 40)
	requestTime := "2026-08-19T00:00:00Z"
	requestMillis := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).UnixMilli()
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      codexReviewCommand + "\n\n" + codexReviewMarker(head, strings.Repeat("a", 40), "none"),
		HTMLURL:   "https://github.test/existing",
		CreatedAt: requestTime,
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}, statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}, checkRuns: []map[string]any{{
		"status":      "completed",
		"external_id": fmt.Sprintf("sfl-codex-review:pull:94:base:%s:context:none:request:123:at:%d:artifact:r456", strings.Repeat("a", 40), requestMillis),
		"app":         map[string]any{"id": 15368},
	}}}
	installReviewFakes(t, rest)

	var stdout bytes.Buffer
	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, &stdout, io.Discard); err != nil {
		t.Fatalf("runReview() unexpected error: %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("Codex retry posts = %d, want 1", rest.posts)
	}
	if rest.postBody != codexReviewCommand+"\n\n"+codexReviewMarker(head, strings.Repeat("a", 40), "none") {
		t.Fatalf("posted retry body = %q", rest.postBody)
	}
}

func TestRunReviewRejectsRetryAfterSuccessfulGate(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	requestTime := "2026-08-19T00:00:00Z"
	requestMillis := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).UnixMilli()
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none"),
		HTMLURL:   "https://github.test/existing",
		CreatedAt: requestTime,
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}, statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}, checkRuns: []map[string]any{{
		"status":      "completed",
		"conclusion":  "success",
		"external_id": fmt.Sprintf("sfl-codex-review:pull:94:base:%s:context:none:request:123:at:%d:artifact:r456", base, requestMillis),
		"app":         map[string]any{"id": 15368},
	}}}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already passed") ||
		!strings.Contains(err.Error(), "push a new commit") {
		t.Fatalf("runReview() successful retry error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("successful-gate retry posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewRetryRecoversFromOverlappingRequest(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	firstCreated := "2026-08-19T00:00:00Z"
	secondCreated := "2026-08-19T00:00:01Z"
	firstMillis := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).UnixMilli()
	marker := codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none")
	comment := func(id int64, createdAt string) reviewTriggerComment {
		return reviewTriggerComment{
			ID:        id,
			Body:      marker,
			HTMLURL:   fmt.Sprintf("https://github.test/request/%d", id),
			CreatedAt: createdAt,
			User: struct {
				Login string `json:"login"`
			}{Login: "HemSoft"},
		}
	}
	rest := &reviewREST{
		comments: []reviewTriggerComment{
			comment(123, firstCreated),
			comment(124, secondCreated),
		},
		statuses: []reviewRequestStatus{
			registeredReviewRequestStatus(123),
			registeredReviewRequestStatus(124),
		},
		checkRuns: []map[string]any{{
			"status":       "completed",
			"completed_at": "2026-08-19T00:00:01.500Z",
			"external_id":  fmt.Sprintf("sfl-codex-review:pull:94:base:%s:context:none:request:123:at:%d:artifact:r456", base, firstMillis),
			"app":          map[string]any{"id": 15368},
		}},
	}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() overlapping retry error = %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("overlapping retry posts = %d, want 1", rest.posts)
	}
}

func TestRunReviewRetryRecoversFromSameSecondOverlap(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	firstCreated := "2026-08-19T00:00:00Z"
	secondCreated := "2026-08-19T00:00:01Z"
	firstMillis := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).UnixMilli()
	marker := codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none")
	comment := func(id int64, createdAt string) reviewTriggerComment {
		return reviewTriggerComment{
			ID:        id,
			Body:      marker,
			HTMLURL:   fmt.Sprintf("https://github.test/request/%d", id),
			CreatedAt: createdAt,
			User: struct {
				Login string `json:"login"`
			}{Login: "HemSoft"},
		}
	}
	rest := &reviewREST{
		comments: []reviewTriggerComment{
			comment(123, firstCreated),
			comment(124, secondCreated),
		},
		statuses: []reviewRequestStatus{
			registeredReviewRequestStatus(123),
			registeredReviewRequestStatus(124),
		},
		checkRuns: []map[string]any{{
			"status":       "completed",
			"completed_at": secondCreated,
			"external_id":  fmt.Sprintf("sfl-codex-review:pull:94:base:%s:context:none:request:123:at:%d:artifact:r456", base, firstMillis),
			"app":          map[string]any{"id": 15368},
		}},
	}
	installReviewFakes(t, rest)
	waits := 0
	waitForRetryOrdering = func() { waits++ }

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() same-second overlap error = %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("same-second overlap posts = %d, want 1", rest.posts)
	}
	if waits != 1 {
		t.Fatalf("same-second overlap waits = %d, want 1", waits)
	}
}

func TestRunReviewRetryRejectsOutstandingRequest(t *testing.T) {
	head := strings.Repeat("b", 40)
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      codexReviewCommand + "\n\n" + codexReviewMarker(head, strings.Repeat("a", 40), "none"),
		HTMLURL:   "https://github.test/existing",
		CreatedAt: "2026-08-19T00:00:00Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}, statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "still outstanding") {
		t.Fatalf("runReview() outstanding retry error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("outstanding retry posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewRetryRecoversFromEditedRequest(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none"),
		HTMLURL:   "https://github.test/edited",
		CreatedAt: "2026-08-19T00:00:00Z",
		UpdatedAt: "2026-08-19T00:00:01Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() edited-request retry error = %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("edited-request retry posts = %d, want 1", rest.posts)
	}
	if rest.reactionGets != 60 {
		t.Fatalf("edited-request reaction GETs = %d, want 60", rest.reactionGets)
	}
}

func TestRunReviewRetryRecoversFromSameSecondBodyMutation(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	timestamp := "2026-08-19T00:00:00Z"
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      "please " + codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none"),
		HTMLURL:   "https://github.test/edited-same-second",
		CreatedAt: timestamp,
		UpdatedAt: timestamp,
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}, statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() same-second mutation retry error = %v", err)
	}
	if rest.posts != 1 || rest.reactionGets != 60 {
		t.Fatalf("same-second mutation posts/reaction GETs = %d/%d, want 1/60", rest.posts, rest.reactionGets)
	}
}

func TestRunReviewRejectsEditedRequestAfterSuccessfulGate(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	createdAt := "2026-08-19T00:00:00Z"
	createdMillis := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).UnixMilli()
	rest := &reviewREST{
		comments: []reviewTriggerComment{{
			ID:        123,
			Body:      "please " + codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none"),
			HTMLURL:   "https://github.test/edited-after-success",
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
			User: struct {
				Login string `json:"login"`
			}{Login: "HemSoft"},
		}},
		statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)},
		checkRuns: []map[string]any{{
			"status":       "completed",
			"conclusion":   "success",
			"completed_at": "2026-08-19T00:00:01Z",
			"external_id":  fmt.Sprintf("sfl-codex-review:pull:94:base:%s:context:none:request:123:at:%d:artifact:c456", base, createdMillis),
			"app":          map[string]any{"id": 15368},
		}},
	}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already passed") {
		t.Fatalf("runReview() edited successful request error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("edited successful request posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewAllowsEditedRecoveryAfterContextInvalidation(t *testing.T) {
	head := strings.Repeat("b", 40)
	base := strings.Repeat("a", 40)
	createdAt := "2026-08-19T00:00:00Z"
	createdMillis := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).UnixMilli()
	currentContext := "sfl-codex-review:pull-context:at:1787097600000:77"
	rest := &reviewREST{
		comments: []reviewTriggerComment{{
			ID:        123,
			Body:      codexReviewCommand + "\n\n" + codexReviewMarker(head, base, "none"),
			HTMLURL:   "https://github.test/pre-invalidation-success",
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
			User: struct {
				Login string `json:"login"`
			}{Login: "HemSoft"},
		}},
		statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)},
		checkRuns: []map[string]any{
			{
				"id":          77,
				"status":      "completed",
				"external_id": currentContext,
				"app":         map[string]any{"id": 15368},
			},
			{
				"status":      "completed",
				"conclusion":  "success",
				"external_id": fmt.Sprintf("sfl-codex-review:pull:94:base:%s:context:none:request:123:at:%d:artifact:c456", base, createdMillis),
				"app":         map[string]any{"id": 15368},
			},
		},
	}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() invalidated success recovery error = %v", err)
	}
	if rest.posts != 1 || rest.reactionGets != 60 {
		t.Fatalf("invalidated success posts/reaction GETs = %d/%d, want 1/60", rest.posts, rest.reactionGets)
	}
}

func TestRunReviewRetryRecoversAfterEditedMarkerIsRemoved(t *testing.T) {
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      codexReviewCommand + "\n\nmarker removed",
		HTMLURL:   "https://github.test/edited-without-marker",
		CreatedAt: "2026-08-19T00:00:00Z",
		UpdatedAt: "2026-08-19T00:00:01Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}, statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() marker-removed retry error = %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("marker-removed retry posts = %d, want 1", rest.posts)
	}
	if rest.reactionGets != 60 {
		t.Fatalf("marker-removed reaction GETs = %d, want 60", rest.reactionGets)
	}
}

func TestRunReviewIgnoresPriorHeadRequest(t *testing.T) {
	oldHead := strings.Repeat("c", 40)
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      codexReviewCommand + "\n\n" + codexReviewMarker(oldHead, strings.Repeat("a", 40), "none"),
		HTMLURL:   "https://github.test/old-head",
		CreatedAt: "2026-08-19T00:00:00Z",
		UpdatedAt: "2026-08-19T00:00:00Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() prior-head request error = %v", err)
	}
	if rest.posts != 1 || rest.reactionGets != 0 {
		t.Fatalf("prior-head request posts/reaction GETs = %d/%d, want 1/0", rest.posts, rest.reactionGets)
	}
}

func TestRunReviewRetryRecoversAfterEditedCommandIsRemoved(t *testing.T) {
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      "request edited beyond recognition",
		HTMLURL:   "https://github.test/edited-without-command",
		CreatedAt: "2026-08-19T00:00:00Z",
		UpdatedAt: "2026-08-19T00:00:01Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}, statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() command-removed retry error = %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("command-removed retry posts = %d, want 1", rest.posts)
	}
	if rest.reactionGets != 60 {
		t.Fatalf("command-removed reaction GETs = %d, want 60", rest.reactionGets)
	}
}

func TestRunReviewRejectsDeletedRegisteredRequest(t *testing.T) {
	rest := &reviewREST{statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "registered Codex review request comment 123") ||
		!strings.Contains(err.Error(), "was deleted") ||
		!strings.Contains(err.Error(), "push a new commit") {
		t.Fatalf("runReview() deleted request error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("deleted request posts = %d, want 0", rest.posts)
	}
}

func TestRunReviewIgnoresUnregisteredEditedDiscussionComment(t *testing.T) {
	rest := &reviewREST{comments: []reviewTriggerComment{{
		ID:        123,
		Body:      "ordinary edited discussion",
		HTMLURL:   "https://github.test/discussion",
		CreatedAt: "2026-08-19T00:00:00Z",
		UpdatedAt: "2026-08-19T00:00:01Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	if err := runReview([]string{"--repo", "HemSoft/consumer", "94"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runReview() ordinary edited comment error = %v", err)
	}
	if rest.posts != 1 || rest.reactionGets != 0 {
		t.Fatalf("ordinary edited comment posts/reaction GETs = %d/%d, want 1/0", rest.posts, rest.reactionGets)
	}
}

func registeredReviewRequestStatus(commentID int64) reviewRequestStatus {
	status := reviewRequestStatus{
		Context:   codexReviewRequestRegistryContext,
		TargetURL: fmt.Sprintf("https://github.test/HemSoft/consumer/pull/94#issuecomment-%d", commentID),
	}
	status.Creator.Login = "HemSoft"
	return status
}

func TestFindRegisteredCodexRequestIDsMatchesCanonicalURLCase(t *testing.T) {
	rest := &reviewREST{statuses: []reviewRequestStatus{registeredReviewRequestStatus(123)}}

	got, err := findRegisteredCodexRequestIDs(
		rest, "hemsoft", "consumer", 94, strings.Repeat("b", 40),
	)
	if err != nil {
		t.Fatalf("findRegisteredCodexRequestIDs() error = %v", err)
	}
	if !got[123] {
		t.Fatalf("registered IDs = %#v, want 123", got)
	}
}

func TestReviewCommentTimeUsesImmutableCreatedAt(t *testing.T) {
	created := "2026-08-19T00:00:00Z"
	comment := reviewTriggerComment{
		CreatedAt: created,
		UpdatedAt: "2026-08-19T01:00:00Z",
	}

	if got := reviewCommentTime(comment); !got.Equal(time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("reviewCommentTime() = %s, want immutable created_at %s", got, created)
	}
}

func TestWaitForCodexRequestCompletionRecognizesFinishedRequest(t *testing.T) {
	rest := &reviewREST{reactions: []reviewCommentReaction{{Content: "eyes"}}}
	rest.reactions[0].User.ID = 12345

	if err := waitForCodexRequestCompletion(rest, "HemSoft", "consumer", 123, false); err != nil {
		t.Fatalf("waitForCodexRequestCompletion() error = %v", err)
	}
}

func TestWaitForCodexRequestCompletionWaitsForActiveReactionToClear(t *testing.T) {
	rest := &reviewREST{
		reactions:     []reviewCommentReaction{{Content: "eyes"}},
		clearReaction: true,
	}
	rest.reactions[0].User.ID = 199175422
	oldPoll := waitForCodexReactionPoll
	waitForCodexReactionPoll = func() {}
	t.Cleanup(func() { waitForCodexReactionPoll = oldPoll })

	if err := waitForCodexRequestCompletion(rest, "HemSoft", "consumer", 123, true); err != nil {
		t.Fatalf("waitForCodexRequestCompletion() error = %v", err)
	}
	if rest.reactionGets != 2 {
		t.Fatalf("reaction GETs = %d, want 2", rest.reactionGets)
	}
}

func TestWaitForCodexRequestCompletionAllowsMaterializationGrace(t *testing.T) {
	rest := &reviewREST{}
	oldPoll := waitForCodexReactionPoll
	waitForCodexReactionPoll = func() {}
	t.Cleanup(func() { waitForCodexReactionPoll = oldPoll })

	if err := waitForCodexRequestCompletion(rest, "HemSoft", "consumer", 123, true); err != nil {
		t.Fatalf("waitForCodexRequestCompletion() error = %v", err)
	}
	if rest.reactionGets != 60 {
		t.Fatalf("reaction GETs = %d, want 60", rest.reactionGets)
	}
}

func installReviewFakes(t *testing.T, rest restAPI) {
	t.Helper()
	oldREST := newRESTClient
	oldGHExec := ghExec
	oldWaitForRetryOrdering := waitForRetryOrdering
	oldWaitForCodexReactionPoll := waitForCodexReactionPoll
	oldWaitForReviewInvalidationPoll := waitForReviewInvalidationPoll
	newRESTClient = func() (restAPI, error) { return rest, nil }
	waitForRetryOrdering = func() {}
	waitForCodexReactionPoll = func() {}
	waitForReviewInvalidationPoll = func() {}
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		if strings.Join(args, " ") != "api user --jq .login" {
			return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("unexpected gh call: %v", args)
		}
		return *bytes.NewBufferString("HemSoft\n"), bytes.Buffer{}, nil
	}
	t.Cleanup(func() {
		newRESTClient = oldREST
		ghExec = oldGHExec
		waitForRetryOrdering = oldWaitForRetryOrdering
		waitForCodexReactionPoll = oldWaitForCodexReactionPoll
		waitForReviewInvalidationPoll = oldWaitForReviewInvalidationPoll
	})
}
