package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

type reviewREST struct {
	comments []reviewTriggerComment
	headRepo string
	posts    int
	postBody string
}

func (f *reviewREST) Get(path string, response interface{}) error {
	switch {
	case strings.Contains(path, "/contents/.github/workflows/sfl-pr-review-auto.yml"):
		content := "name: SFL Codex Review Observer\nif: github.event.sender.id == 199175422\n"
		return decodeTestResponse(response, map[string]any{
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			"encoding": "base64",
		})
	case strings.Contains(path, "/actions/variables/SFL_ENABLED"):
		return decodeTestResponse(response, map[string]any{"value": "true"})
	case strings.Contains(path, "/pulls/"):
		headRepo := f.headRepo
		if headRepo == "" {
			headRepo = "HemSoft/consumer"
		}
		return decodeTestResponse(response, map[string]any{
			"base":  map[string]any{"sha": strings.Repeat("a", 40), "repo": map[string]string{"full_name": "HemSoft/consumer"}},
			"head":  map[string]any{"sha": strings.Repeat("b", 40), "repo": map[string]string{"full_name": headRepo}},
			"state": "open",
		})
	case strings.Contains(path, "/comments?"):
		return decodeTestResponse(response, f.comments)
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
	wantMarker := codexReviewMarker(strings.Repeat("b", 40))
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

func TestRunReviewDeduplicatesCurrentHeadRequest(t *testing.T) {
	head := strings.Repeat("b", 40)
	rest := &reviewREST{comments: []reviewTriggerComment{{
		Body:    codexReviewCommand + "\n\n" + codexReviewMarker(head),
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
	rest := &reviewREST{comments: []reviewTriggerComment{{
		Body:    codexReviewCommand + "\n\n" + codexReviewMarker(head),
		HTMLURL: "https://github.test/existing",
		User: struct {
			Login string `json:"login"`
		}{Login: "HemSoft"},
	}}}
	installReviewFakes(t, rest)

	var stdout bytes.Buffer
	if err := runReview([]string{"--repo", "HemSoft/consumer", "--retry", "94"}, &stdout, io.Discard); err != nil {
		t.Fatalf("runReview() unexpected error: %v", err)
	}
	if rest.posts != 1 {
		t.Fatalf("Codex retry posts = %d, want 1", rest.posts)
	}
	if rest.postBody != codexReviewCommand+"\n\n"+codexReviewMarker(head) {
		t.Fatalf("posted retry body = %q", rest.postBody)
	}
}

func installReviewFakes(t *testing.T, rest restAPI) {
	t.Helper()
	oldREST := newRESTClient
	oldGHExec := ghExec
	newRESTClient = func() (restAPI, error) { return rest, nil }
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		if strings.Join(args, " ") != "api user --jq .login" {
			return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("unexpected gh call: %v", args)
		}
		return *bytes.NewBufferString("HemSoft\n"), bytes.Buffer{}, nil
	}
	t.Cleanup(func() {
		newRESTClient = oldREST
		ghExec = oldGHExec
	})
}
