package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func reviewerFixtureBash(t *testing.T) string {
	t.Helper()
	bash := ""
	if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
		gitBash := filepath.Join(programFiles, "Git", "bin", "bash.exe")
		if _, err := os.Stat(gitBash); err == nil {
			bash = gitBash
		}
	}
	if bash == "" {
		var err error
		bash, err = exec.LookPath("bash")
		if err != nil {
			t.Fatal("bash is required to execute reviewer fixtures")
		}
	}
	return bash
}

func TestAutoGateRunValidationFixtures(t *testing.T) {
	bash := reviewerFixtureBash(t)

	source, err := os.ReadFile(
		filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml"),
	)
	if err != nil {
		t.Fatalf("read automatic review workflow: %v", err)
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	const begin = "# BEGIN TESTABLE AUTO GATE RUN VALIDATION"
	const end = "# END TESTABLE AUTO GATE RUN VALIDATION"
	start := strings.Index(text, begin)
	finish := strings.Index(text, end)
	if start < 0 || finish <= start {
		t.Fatal("automatic gate run validation helper block is missing")
	}
	helper := text[start+len(begin) : finish]

	script := `#!/usr/bin/env bash
set -u
` + helper + `
if printf '%s' "$1" | validate_review_run "$2" "$3" "$4"; then
  echo valid
else
  echo invalid
fi
`
	scriptPath := filepath.Join(t.TempDir(), "auto-gate-run.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write automatic gate run fixture: %v", err)
	}
	scriptArgument := filepath.ToSlash(scriptPath)
	if volume := filepath.VolumeName(scriptPath); volume != "" {
		scriptArgument = "/" +
			strings.ToLower(strings.TrimSuffix(volume, ":")) +
			strings.TrimPrefix(scriptArgument, filepath.ToSlash(volume))
	}

	const validRun = `{
  "path":".github/workflows/sfl-pr-review.lock.yml",
  "event":"workflow_dispatch",
  "head_repository":{"full_name":"acme/repo"},
  "head_branch":"main",
  "display_title":"SFL PR Review #69 base:head retry=0",
  "status":"completed",
  "conclusion":"success"
}`
	for _, fixture := range []struct {
		name   string
		input  string
		output string
	}{
		{
			name:   "accepts exact trusted reviewer run",
			input:  validRun,
			output: "valid",
		},
		{
			name:   "rejects another workflow",
			input:  strings.Replace(validRun, "sfl-pr-review.lock.yml", "forged.yml", 1),
			output: "invalid",
		},
		{
			name:   "rejects another repository",
			input:  strings.Replace(validRun, "acme/repo", "acme/other", 1),
			output: "invalid",
		},
		{
			name:   "rejects an untrusted branch",
			input:  strings.Replace(validRun, `"head_branch":"main"`, `"head_branch":"feature"`, 1),
			output: "invalid",
		},
		{
			name:   "rejects another pull request state",
			input:  strings.Replace(validRun, "#69 base:head", "#70 other:head", 1),
			output: "invalid",
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cmd := exec.Command(
				bash,
				scriptArgument,
				fixture.input,
				"acme/repo",
				"main",
				"SFL PR Review #69 base:head retry=",
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute automatic gate run fixture: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != fixture.output {
				t.Fatalf("run validation=%q, want %q", got, fixture.output)
			}
		})
	}
}
