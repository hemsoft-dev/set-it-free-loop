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

	t.Run("run lookup seals title and command time", func(t *testing.T) {
		const title = "SFL PR Review #42 aaa111:bbb222 retry="
		input := `{"workflow_runs":[` +
			`{"id":100,"created_at":"2026-08-17T01:59:59Z","display_title":"SFL PR Review #42 aaa111:bbb222 retry=0 dispatch=old"},` +
			`{"id":102,"created_at":"2026-08-17T02:00:02Z","display_title":"SFL PR Review #99 aaa111:bbb222 retry=0 dispatch=other"},` +
			`{"id":101,"created_at":"2026-08-17T02:00:01Z","display_title":"SFL PR Review #42 aaa111:bbb222 retry=0 dispatch=command"}]}`
		cmd := exec.Command(bash, scriptArgument, "run", "2026-08-17T02:00:00Z", title)
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute run lookup fixture: %v\n%s", err, output)
		}
		if got := strings.TrimSpace(string(output)); got != "101" {
			t.Fatalf("run=%q, want 101", got)
		}
	})
}
