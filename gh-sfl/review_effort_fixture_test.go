package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewEffortResolutionFixtures(t *testing.T) {
	bash := reviewerFixtureBash(t)
	workflows := []string{"sfl-pr-review-auto.yml"}

	for _, workflow := range workflows {
		t.Run(workflow, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("..", "deployment", "infrastructure", workflow))
			if err != nil {
				t.Fatalf("read %s: %v", workflow, err)
			}
			text := strings.ReplaceAll(string(source), "\r\n", "\n")
			const begin = "# BEGIN TESTABLE REVIEW EFFORT"
			const end = "# END TESTABLE REVIEW EFFORT"
			start := strings.Index(text, begin)
			finish := strings.Index(text, end)
			if start < 0 || finish <= start {
				t.Fatalf("%s review effort helper block is missing", workflow)
			}
			helper := text[start+len(begin) : finish]

			script := `#!/usr/bin/env bash
set -euo pipefail
` + helper + `
resolve_review_effort "$(cat)"
`
			scriptPath := filepath.Join(t.TempDir(), "review-effort.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
				t.Fatalf("write review effort fixture: %v", err)
			}
			scriptArgument := filepath.ToSlash(scriptPath)
			if volume := filepath.VolumeName(scriptPath); volume != "" {
				scriptArgument = "/" +
					strings.ToLower(strings.TrimSuffix(volume, ":")) +
					strings.TrimPrefix(scriptArgument, filepath.ToSlash(volume))
			}

			for _, fixture := range []struct {
				name   string
				labels string
				want   string
			}{
				{name: "no label defaults low", want: "low"},
				{name: "low", labels: "sfl-effort:low", want: "low"},
				{name: "medium", labels: "sfl-effort:medium", want: "medium"},
				{name: "high", labels: "sfl-effort:high", want: "high"},
				{
					name:   "highest effort wins",
					labels: "sfl-effort:low\nsfl-effort:high\nsfl-effort:medium",
					want:   "high",
				},
				{name: "unrelated labels default low", labels: "sfl-review\nrisk:high", want: "low"},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					cmd := exec.Command(bash, scriptArgument)
					cmd.Stdin = strings.NewReader(fixture.labels)
					output, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("execute review effort fixture: %v\n%s", err, output)
					}
					if got := strings.TrimSpace(string(output)); got != fixture.want {
						t.Fatalf("effort=%q, want %q", got, fixture.want)
					}
				})
			}
		})
	}
}
