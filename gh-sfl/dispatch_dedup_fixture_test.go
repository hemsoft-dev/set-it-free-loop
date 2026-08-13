package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The dispatch dedup filter in sfl-pr-review-auto.yml decides whether an
// automatic trigger dispatches a review. These fixtures execute the shipped
// jq against representative workflow-runs payloads.
func TestDispatchDedupFixtures(t *testing.T) {
	bash := reviewerFixtureBash(t)

	source, err := os.ReadFile(
		filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml"),
	)
	if err != nil {
		t.Fatalf("read review auto trigger workflow: %v", err)
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	const begin = "# BEGIN TESTABLE DISPATCH DEDUP"
	const end = "# END TESTABLE DISPATCH DEDUP"
	start := strings.Index(text, begin)
	finish := strings.Index(text, end)
	if start < 0 || finish <= start {
		t.Fatal("dispatch dedup helper block is missing")
	}
	helper := text[start+len(begin) : finish]

	script := `#!/usr/bin/env bash
set -euo pipefail
` + helper + `
if [ "${4:-false}" = "true" ]; then
  produce_large_input() {
    local i
    for ((i = 0; i < 131072; i++)); do
      printf x
    done
  }
  produce_large_input | find_reusable_review_run "$1" "$3"
else
  find_reusable_review_run "$1" "$3"
fi
`
	scriptPath := filepath.Join(t.TempDir(), "dispatch-dedup.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write dispatch dedup fixture: %v", err)
	}
	scriptArgument := filepath.ToSlash(scriptPath)
	if volume := filepath.VolumeName(scriptPath); volume != "" {
		scriptArgument = "/" +
			strings.ToLower(strings.TrimSuffix(volume, ":")) +
			strings.TrimPrefix(scriptArgument, filepath.ToSlash(volume))
	}

	const expectedTitle = "SFL PR Review #42 aaa111:bbb222 retry="
	for _, fixture := range []struct {
		name   string
		action string
		label  string
		pipe   string
		input  string
		want   string
	}{
		{
			name:   "matching active run suppresses dispatch",
			action: "synchronize",
			label:  "false",
			pipe:   "false",
			input: `{"total_count":2,"workflow_runs":[
  {"id":101,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=0"},
  {"id":102,"display_title":"SFL PR Review #99 ccc333:ddd444 retry=0"}
]}`,
			want: "101",
		},
		{
			name:   "unrelated runs allow dispatch",
			action: "opened",
			label:  "false",
			pipe:   "false",
			input: `{"total_count":2,"workflow_runs":[
  {"id":101,"display_title":"SFL PR Review #99 ccc333:ddd444 retry=0"},
  {"id":102,"display_title":"SFL PR Review #41 eee555:fff666 retry=1"}
]}`,
			want: "",
		},
		{
			name:   "empty page allows dispatch",
			action: "ready_for_review",
			label:  "false",
			pipe:   "false",
			input:  `{"total_count":0,"workflow_runs":[]}`,
			want:   "",
		},
		{
			name:   "same PR but different head allows dispatch",
			action: "synchronize",
			label:  "false",
			pipe:   "false",
			input:  `{"total_count":1,"workflow_runs":[{"id":101,"display_title":"SFL PR Review #42 aaa111:ccc999 retry=0"}]}`,
			want:   "",
		},
		{
			name:   "live explicit request bypasses active run",
			action: "labeled",
			label:  "true",
			pipe:   "false",
			input:  `{"total_count":1,"workflow_runs":[{"id":101,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=0"}]}`,
			want:   "",
		},
		{
			name:   "stale labeled event uses active run",
			action: "labeled",
			label:  "false",
			pipe:   "false",
			input:  `{"total_count":1,"workflow_runs":[{"id":101,"display_title":"SFL PR Review #42 aaa111:bbb222 retry=0"}]}`,
			want:   "101",
		},
		{
			name:   "live request drains producer under pipefail",
			action: "labeled",
			label:  "true",
			pipe:   "true",
			want:   "",
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cmd := exec.Command(
				bash,
				scriptArgument,
				expectedTitle,
				fixture.action,
				fixture.label,
				fixture.pipe,
			)
			cmd.Stdin = strings.NewReader(fixture.input)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute dispatch dedup fixture: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != fixture.want {
				t.Fatalf("run=%q, want %q", got, fixture.want)
			}
		})
	}
}
