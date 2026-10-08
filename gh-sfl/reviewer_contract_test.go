package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type observerResult struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

func readContractFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}

func normalizeLineEndings(content []byte) string {
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}

func TestCodexObserverCanonicalAndStagedMatch(t *testing.T) {
	root := filepath.Join("..")
	canonicalBytes := readContractFile(t, filepath.Join(root, "deployment", "infrastructure", "sfl-pr-review-auto.yml"))
	staged := readContractFile(t, filepath.Join(root, ".github", "workflows", "sfl-pr-review-auto.yml"))
	if normalizeLineEndings(canonicalBytes) != normalizeLineEndings(staged) {
		t.Fatal("staged Codex observer differs from canonical deployment source")
	}
	canonical := string(canonicalBytes)

	for _, required := range []string{
		"name: SFL Codex Review Observer",
		"format('SFL Codex comment #{0} request {1}', github.event.issue.number, github.event.comment.id)",
		"types: [opened, reopened, edited, synchronize]",
		"invalidate-base-advance:",
		"github.event.sender.id == 199175422",
		"Fork pull requests are unsupported",
		"SFL reviews require the default branch",
		"appId = 1144995",
		"appSlug = \"chatgpt-codex-connector\"",
		"appOwner = \"openai\"",
		"name: \"SFL Reviewer Gate Runner\"",
		"check.app.id === 15368",
		"Codex artifact is stale",
		"Codex reported review findings",
		"SFL Codex review invalidated",
		"requestMatchesCurrentBase",
		"invalidate-review-request:",
		"vars.SFL_ENABLED != 'false'",
		"SFL Codex Review Request Registry",
		"const terminalRequestIdentity = `:request:${commentId}:at:`",
		"registrations.has(comment.id)",
		"openPullCount",
		"if: github.event_name == 'push'",
	} {
		if !strings.Contains(canonical, required) {
			t.Errorf("Codex observer is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"OPENROUTER_API_KEY",
		"openrouter.ai",
		"moonshotai/kimi",
		"sfl-pr-review.lock.yml",
		"SFL_APP_PRIVATE_KEY",
	} {
		if strings.Contains(canonical, forbidden) {
			t.Errorf("Codex observer retained forbidden legacy reviewer text %q", forbidden)
		}
	}
	for _, required := range []string{
		`status: "in_progress"`,
		"const confirmedState = await publicationState();",
		"const postSuccessState = await publicationState();",
		"await failPublishedSuccess(publicationChangeReason(postSuccessState));",
	} {
		if !strings.Contains(canonical, required) {
			t.Errorf("Codex observer is missing two-phase publication contract %q", required)
		}
	}
}

func TestCodexObserverClassificationFixtures(t *testing.T) {
	const head = "a47ba21b6916090642092f03c8b77b24bf658a46"
	codexUser := map[string]any{
		"id":    199175422,
		"login": "chatgpt-codex-connector[bot]",
	}
	codexApp := map[string]any{
		"id":    1144995,
		"slug":  "chatgpt-codex-connector",
		"owner": map[string]any{"login": "openai"},
	}

	tests := []struct {
		name       string
		input      map[string]any
		wantAction string
	}{
		{
			name: "clean current-head comment passes",
			input: map[string]any{
				"eventName":   "issue_comment",
				"currentHead": head,
				"resolvedSha": head,
				"inlineCount": 0,
				"artifact": map[string]any{
					"user":                     userCopy(codexUser),
					"performed_via_github_app": codexApp,
					"body":                     "Codex Review: Didn't find any major issues. Keep it up!\n\n**Reviewed commit:** `a47ba21b69`",
				},
			},
			wantAction: "success",
		},
		{
			name: "current-head finding review fails",
			input: map[string]any{
				"eventName":    "pull_request_review",
				"currentHead":  head,
				"resolvedSha":  head,
				"reviewCommit": head,
				"inlineCount":  1,
				"artifact": map[string]any{
					"user":  userCopy(codexUser),
					"state": "COMMENTED",
					"body":  "Here are some automated review suggestions.\n\n**Reviewed commit:** `a47ba21b69`",
				},
			},
			wantAction: "failure",
		},
		{
			name: "stale clean result is ignored",
			input: map[string]any{
				"eventName":   "issue_comment",
				"currentHead": head,
				"resolvedSha": "048484a6ced8de230284f03c7214d76f76987318",
				"inlineCount": 0,
				"artifact": map[string]any{
					"user":                     userCopy(codexUser),
					"performed_via_github_app": codexApp,
					"body":                     "Codex Review: Didn't find any major issues.\n\n**Reviewed commit:** `048484a6ce`",
				},
			},
			wantAction: "ignore",
		},
		{
			name: "spoofed comment is ignored",
			input: map[string]any{
				"eventName":   "issue_comment",
				"currentHead": head,
				"resolvedSha": head,
				"inlineCount": 0,
				"artifact": map[string]any{
					"user":                     map[string]any{"id": 42, "login": "chatgpt-codex-connector[bot]"},
					"performed_via_github_app": codexApp,
					"body":                     "Codex Review: Didn't find any major issues.\n\n**Reviewed commit:** `a47ba21b69`",
				},
			},
			wantAction: "ignore",
		},
		{
			name: "malformed comment without a commit binding is ignored",
			input: map[string]any{
				"eventName":   "issue_comment",
				"currentHead": head,
				"resolvedSha": "",
				"inlineCount": 0,
				"artifact": map[string]any{
					"user":                     userCopy(codexUser),
					"performed_via_github_app": codexApp,
					"body":                     "Codex Review returned an unexpected response.",
				},
			},
			wantAction: "ignore",
		},
		{
			name: "malformed current-head review fails closed",
			input: map[string]any{
				"eventName":    "pull_request_review",
				"currentHead":  head,
				"resolvedSha":  "",
				"reviewCommit": head,
				"inlineCount":  0,
				"artifact": map[string]any{
					"user":  userCopy(codexUser),
					"state": "COMMENTED",
					"body":  "Codex returned an unexpected review response.",
				},
			},
			wantAction: "failure",
		},
		{
			name: "stale malformed review is ignored",
			input: map[string]any{
				"eventName":    "pull_request_review",
				"currentHead":  head,
				"resolvedSha":  "",
				"reviewCommit": "048484a6ced8de230284f03c7214d76f76987318",
				"inlineCount":  1,
				"artifact": map[string]any{
					"user":  userCopy(codexUser),
					"state": "COMMENTED",
					"body":  "Here are some automated review suggestions without a commit marker.",
				},
			},
			wantAction: "ignore",
		},
		{
			name: "current-head result for another base is ignored",
			input: map[string]any{
				"eventName":                 "pull_request_review",
				"currentHead":               head,
				"resolvedSha":               head,
				"reviewCommit":              head,
				"requestMatchesCurrentBase": false,
				"inlineCount":               0,
				"artifact": map[string]any{
					"user":  userCopy(codexUser),
					"state": "COMMENTED",
					"body":  "Codex Review: Didn't find any major issues.",
				},
			},
			wantAction: "ignore",
		},
		{
			name: "shared current head fails closed",
			input: map[string]any{
				"eventName":     "issue_comment",
				"currentHead":   head,
				"resolvedSha":   head,
				"openPullCount": 2,
				"inlineCount":   0,
				"artifact": map[string]any{
					"user":                     userCopy(codexUser),
					"performed_via_github_app": codexApp,
					"body":                     "Codex Review: Didn't find any major issues.",
				},
			},
			wantAction: "failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := runObserverFixture(t, tc.input)
			if result.Action != tc.wantAction {
				t.Fatalf("observer action = %q (%s), want %q", result.Action, result.Reason, tc.wantAction)
			}
		})
	}
}

func TestCodexObserverActiveInvalidationFixtures(t *testing.T) {
	tests := []struct {
		name       string
		run        map[string]any
		pullNumber int
		want       bool
	}{
		{
			name:       "active push can invalidate every reviewed pull",
			run:        map[string]any{"event": "push", "status": "in_progress"},
			pullNumber: 42,
			want:       true,
		},
		{
			name: "matching active pull context can invalidate",
			run: map[string]any{
				"event":         "pull_request_target",
				"status":        "queued",
				"pull_requests": []map[string]any{{"number": 42}},
			},
			pullNumber: 42,
			want:       true,
		},
		{
			name: "other pull context is irrelevant",
			run: map[string]any{
				"event":         "pull_request_target",
				"status":        "waiting",
				"pull_requests": []map[string]any{{"number": 41}},
			},
			pullNumber: 42,
			want:       false,
		},
		{
			name:       "completed push is no longer active",
			run:        map[string]any{"event": "push", "status": "completed"},
			pullNumber: 42,
			want:       false,
		},
		{
			name: "active owner request invalidation blocks observation",
			run: map[string]any{
				"event": "issue_comment", "status": "in_progress",
				"actor":         map[string]any{"login": "HemSoft"},
				"display_title": "SFL Codex review request #42",
			},
			pullNumber: 42,
			want:       true,
		},
		{
			name: "owner request for another pull is irrelevant",
			run: map[string]any{
				"event": "issue_comment", "status": "in_progress",
				"actor":         map[string]any{"login": "HemSoft"},
				"display_title": "SFL Codex review request #41",
			},
			pullNumber: 42,
			want:       false,
		},
		{
			name: "ordinary owner comment is not an invalidation",
			run: map[string]any{
				"event": "issue_comment", "status": "queued",
				"actor":         map[string]any{"login": "HemSoft"},
				"display_title": "SFL issue_comment event",
			},
			pullNumber: 42,
			want:       false,
		},
		{
			name: "Codex result comment is not a request invalidation",
			run: map[string]any{
				"event": "issue_comment", "status": "queued",
				"actor": map[string]any{"login": "chatgpt-codex-connector[bot]"},
			},
			pullNumber: 42,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runActiveInvalidationFixture(t, tc.run, tc.pullNumber); got != tc.want {
				t.Fatalf("isActiveInvalidationRun() = %t, want %t", got, tc.want)
			}
		})
	}
}

func userCopy(user map[string]any) map[string]any {
	return map[string]any{"id": user["id"], "login": user["login"]}
}

func runObserverFixture(t *testing.T, input map[string]any) observerResult {
	t.Helper()
	if _, ok := input["requestMatchesCurrentBase"]; !ok {
		input["requestMatchesCurrentBase"] = true
	}
	if _, ok := input["openPullCount"]; !ok {
		input["openPullCount"] = 1
	}
	workflow := string(readContractFile(t, filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml")))
	startMarker := "// BEGIN TESTABLE CODEX OBSERVER"
	endMarker := "// END TESTABLE CODEX OBSERVER"
	start := strings.Index(workflow, startMarker)
	end := strings.Index(workflow, endMarker)
	if start < 0 || end <= start {
		t.Fatal("could not locate testable Codex observer block")
	}
	source := workflow[start+len(startMarker) : end]
	source += "\nconsole.log(JSON.stringify(classifyCodexArtifact(JSON.parse(process.argv[2]))));\n"

	temp := filepath.Join(t.TempDir(), "observer.js")
	if err := os.WriteFile(temp, []byte(source), 0o600); err != nil {
		t.Fatalf("write observer fixture: %v", err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal observer fixture: %v", err)
	}
	output, err := exec.Command("node", temp, string(payload)).CombinedOutput()
	if err != nil {
		t.Fatalf("run observer fixture: %v\n%s", err, output)
	}
	var result observerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("parse observer result %q: %v", output, err)
	}
	return result
}

func runActiveInvalidationFixture(t *testing.T, run map[string]any, pullNumber int) bool {
	t.Helper()
	workflow := string(readContractFile(t, filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml")))
	startMarker := "// BEGIN TESTABLE CODEX OBSERVER"
	endMarker := "// END TESTABLE CODEX OBSERVER"
	start := strings.Index(workflow, startMarker)
	end := strings.Index(workflow, endMarker)
	if start < 0 || end <= start {
		t.Fatal("could not locate testable Codex observer block")
	}
	source := workflow[start+len(startMarker) : end]
	source += "\nconst fixture = JSON.parse(process.argv[2]); console.log(JSON.stringify(isActiveInvalidationRun(fixture.run, fixture.pullNumber, \"HemSoft\")));\n"

	temp := filepath.Join(t.TempDir(), "active-invalidation.js")
	if err := os.WriteFile(temp, []byte(source), 0o600); err != nil {
		t.Fatalf("write active invalidation fixture: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"run": run, "pullNumber": pullNumber})
	if err != nil {
		t.Fatalf("marshal active invalidation fixture: %v", err)
	}
	output, err := exec.Command("node", temp, string(payload)).CombinedOutput()
	if err != nil {
		t.Fatalf("run active invalidation fixture: %v\n%s", err, output)
	}
	var result bool
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("parse active invalidation result %q: %v", output, err)
	}
	return result
}

// The fleet fixture executes the production guard rather than trusting only
// the artifact classifier, which cannot establish a native artifact's base.
func TestCodexObserverRejectsUnregisteredOldBaseCompletion(t *testing.T) {
	content := readContractFile(t, filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml"))
	capture, err := json.Marshal(map[string]any{
		"repository_id": 1, "repository": "hemsoft-dev/fixture", "revision_sha": strings.Repeat("a", 40),
		"path": ".github/workflows/sfl-pr-review-auto.yml", "content": string(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "workflow.json")
	output := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(input, capture, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{filepath.Join("..", "deployment", "tests", "run-org-observer-fixtures.cjs"),
		"--workflow", input, "--repository-id", "1", "--repository", "hemsoft-dev/fixture",
		"--deployment-sha", strings.Repeat("a", 40), "--release-version", "fixture", "--revision", strings.Repeat("a", 40),
		"--scenario", "unregistered_base_context", "--output", output}
	result, err := exec.Command("node", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("context provenance fixture: %v\n%s", err, result)
	}
	var receipt struct {
		Passed   bool   `json:"passed"`
		Scenario string `json:"scenario"`
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Passed || receipt.Scenario != "unregistered_base_context" {
		t.Fatalf("unexpected fixture receipt: %s", raw)
	}
}
