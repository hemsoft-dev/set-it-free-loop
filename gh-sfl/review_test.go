package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type reviewREST struct {
	comments            []reviewTriggerComment
	reactions           []reviewCommentReaction
	reactionGets        int
	clearReaction       bool
	checkRuns           []map[string]any
	checkRunPages       map[int][]map[string]any
	headRepo            string
	headRepoUnavailable bool
	baseRef             string
	defaultBranch       string
	observerBranch      string
	observerState       string
	posts               int
	postBody            string
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
			"base":  map[string]any{"sha": strings.Repeat("a", 40), "ref": baseRef, "repo": map[string]string{"full_name": "HemSoft/consumer"}},
			"head":  map[string]any{"sha": strings.Repeat("b", 40), "repo": headRepo},
			"state": "open",
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
		"html_url": "https://github.test/HemSoft/consumer/pull/94#issuecomment-1",
	})
}

func (f *reviewREST) Put(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected PUT")
}
func (f *reviewREST) Patch(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected PATCH")
}
func (f *reviewREST) Delete(string, interface{}) error {
	return fmt.Errorf("unexpected DELETE")
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
	wantMarker := codexReviewMarker(strings.Repeat("b", 40), strings.Repeat("a", 40), "none")
	if rest.postBody != codexReviewCommand+"\n\n"+wantMarker {
		t.Fatalf("posted body = %q", rest.postBody)
	}
	if !strings.Contains(stdout.String(), "subscription-backed Codex review") ||
		!strings.Contains(stdout.String(), "#issuecomment-1") {
		t.Fatalf("stdout = %q", stdout.String())
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
	}}, checkRuns: []map[string]any{{
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
		Body:      codexReviewCommand + "\n\n" + codexReviewMarker(head, strings.Repeat("a", 40), "none"),
		HTMLURL:   "https://github.test/existing",
		CreatedAt: "2026-08-19T00:00:00Z",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "still outstanding") {
		t.Fatalf("runReview() outstanding retry error = %v", err)
	}
	if rest.posts != 0 {
		t.Fatalf("outstanding retry posts = %d, want 0", rest.posts)
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
	newRESTClient = func() (restAPI, error) { return rest, nil }
	waitForRetryOrdering = func() {}
	waitForCodexReactionPoll = func() {}
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
	})
}
