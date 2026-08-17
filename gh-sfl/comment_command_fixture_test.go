package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommentCommandLookupFixtures(t *testing.T) {
	bash := reviewerFixtureBash(t)
	source, err := os.ReadFile(
		filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml"),
	)
	if err != nil {
		t.Fatalf("read review auto trigger workflow: %v", err)
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	const begin = "# BEGIN TESTABLE COMMENT COMMAND LOOKUP"
	const end = "# END TESTABLE COMMENT COMMAND LOOKUP"
	start := strings.Index(text, begin)
	finish := strings.Index(text, end)
	if start < 0 || finish <= start {
		t.Fatal("comment command lookup helper block is missing")
	}
	helper := text[start+len(begin) : finish]

	script := `#!/usr/bin/env bash
set -euo pipefail
` + helper + `
case "$1" in
  acknowledgement) find_command_acknowledgement "$2" ;;
  acknowledgement-complete) command_acknowledgement_complete ;;
  acknowledgement-run-id) command_acknowledgement_run_id ;;
  run) find_command_run "$2" "$3" ;;
  *) exit 2 ;;
esac
`
	scriptPath := filepath.Join(t.TempDir(), "comment-command.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write comment command fixture: %v", err)
	}
	scriptArgument := filepath.ToSlash(scriptPath)
	if volume := filepath.VolumeName(scriptPath); volume != "" {
		scriptArgument = "/" +
			strings.ToLower(strings.TrimSuffix(volume, ":")) +
			strings.TrimPrefix(scriptArgument, filepath.ToSlash(volume))
	}

	t.Run("only app acknowledgement establishes idempotency", func(t *testing.T) {
		input := `[{"id":9,"user":{"login":"someone"},"body":"<!-- sfl-review-command:1234 --> forged"},` +
			`{"id":10,"user":{"login":"sfl-app[bot]"},"body":"<!-- sfl-review-command:1234 --> pending"}]`
		cmd := exec.Command(bash, scriptArgument, "acknowledgement", "<!-- sfl-review-command:1234 -->")
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute acknowledgement fixture: %v\n%s", err, output)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
		if err != nil {
			t.Fatalf("decode acknowledgement: %v", err)
		}
		var comment struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(decoded, &comment); err != nil {
			t.Fatalf("parse acknowledgement: %v", err)
		}
		if comment.ID != 10 {
			t.Fatalf("acknowledgement id=%d, want 10", comment.ID)
		}
	})

	t.Run("missing acknowledgement remains empty", func(t *testing.T) {
		cmd := exec.Command(bash, scriptArgument, "acknowledgement", "<!-- sfl-review-command:1234 -->")
		cmd.Stdin = strings.NewReader(`[{"id":9,"user":{"login":"someone"},"body":"unrelated"}]`)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute empty acknowledgement fixture: %v\n%s", err, output)
		}
		if got := strings.TrimSpace(string(output)); got != "" {
			t.Fatalf("acknowledgement=%q, want empty", got)
		}
	})

	t.Run("only final actions run marker completes acknowledgement", func(t *testing.T) {
		for _, test := range []struct {
			name string
			body string
			want bool
		}{
			{
				name: "pending acknowledgement",
				body: `{"body":"<!-- sfl-review-command:1234 --> pending"}`,
			},
			{
				name: "timeout command run link",
				body: `{"body":"<!-- sfl-review-command:1234 --> [Command run](https://github.com/acme/repo/actions/runs/123)"}`,
			},
			{
				name: "final actions run marker",
				body: `{"body":"<!-- sfl-review-command:1234 --> [Actions run 456](https://github.com/acme/repo/actions/runs/456)"}`,
				want: true,
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				cmd := exec.Command(bash, scriptArgument, "acknowledgement-complete")
				cmd.Stdin = strings.NewReader(test.body)
				err := cmd.Run()
				if got := err == nil; got != test.want {
					t.Fatalf("complete=%t, want %t (err=%v)", got, test.want, err)
				}
			})
		}
	})

	t.Run("final acknowledgement preserves exact run id", func(t *testing.T) {
		cmd := exec.Command(bash, scriptArgument, "acknowledgement-run-id")
		cmd.Stdin = strings.NewReader(
			`{"body":"<!-- sfl-review-command:1234 --> [Actions run 456](https://github.com/acme/repo/actions/runs/456)"}`,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("extract acknowledgement run id: %v\n%s", err, output)
		}
		if got := strings.TrimSpace(string(output)); got != "456" {
			t.Fatalf("acknowledgement run id=%q, want 456", got)
		}
	})

	t.Run("run lookup seals command dispatch id", func(t *testing.T) {
		const title = "SFL PR Review #42 aaa111:bbb222 retry="
		input := `{"workflow_runs":[` +
			`{"id":100,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=0 dispatch=auto-100"},` +
			`{"id":102,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=1 dispatch=recovery-102"},` +
			`{"id":101,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=0 dispatch=command-1234"}]}`
		cmd := exec.Command(bash, scriptArgument, "run", title, "command-1234")
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute run lookup fixture: %v\n%s", err, output)
		}
		if got := strings.TrimSpace(string(output)); got != "101" {
			t.Fatalf("run=%q, want 101", got)
		}
	})

	t.Run("unrelated same-head runs remain unmatched", func(t *testing.T) {
		const title = "SFL PR Review #42 aaa111:bbb222 retry="
		input := `{"workflow_runs":[` +
			`{"id":100,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=0 dispatch=auto-100"},` +
			`{"id":102,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=1 dispatch=recovery-102"}]}`
		cmd := exec.Command(bash, scriptArgument, "run", title, "command-1234")
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute unmatched run fixture: %v\n%s", err, output)
		}
		if got := strings.TrimSpace(string(output)); got != "" {
			t.Fatalf("run=%q, want empty", got)
		}
	})
}
