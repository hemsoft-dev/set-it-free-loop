package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewRecoveryPolicyFixtures(t *testing.T) {
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
			t.Fatal("bash is required to execute review recovery fixtures")
		}
	}

	source, err := os.ReadFile(
		filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-recovery.yml"),
	)
	if err != nil {
		t.Fatalf("read review recovery workflow: %v", err)
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	const begin = "# BEGIN TESTABLE REVIEW RECOVERY POLICY"
	const end = "# END TESTABLE REVIEW RECOVERY POLICY"
	start := strings.Index(text, begin)
	finish := strings.Index(text, end)
	if start < 0 || finish <= start {
		t.Fatal("review recovery policy helper block is missing")
	}
	helper := text[start+len(begin) : finish]

	script := `#!/usr/bin/env bash
set -u
` + helper + `
status=0
review_count=$(reconcile_review_count "$2" "$3")
decision=$(decide_review_recovery "$1" "$review_count" "$4" "$5" "$6" "$7" "$8" "$9") || status=$?
printf '%s:%s\n' "$decision" "$status"
`
	scriptPath := filepath.Join(t.TempDir(), "review-recovery.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write review recovery fixture: %v", err)
	}
	scriptArgument := filepath.ToSlash(scriptPath)
	if volume := filepath.VolumeName(scriptPath); volume != "" {
		scriptArgument = "/" +
			strings.ToLower(strings.TrimSuffix(volume, ":")) +
			strings.TrimPrefix(scriptArgument, filepath.ToSlash(volume))
	}

	head := strings.Repeat("a", 40)
	otherHead := strings.Repeat("b", 40)
	base := strings.Repeat("c", 40)
	otherBase := strings.Repeat("d", 40)
	for _, fixture := range []struct {
		name   string
		args   []string
		output string
	}{
		{
			name:   "valid review output",
			args:   []string{"0", "1", "1", "0", head, head, base, base, "true"},
			output: "complete:0",
		},
		{
			name:   "published review recovers missing manifest",
			args:   []string{"0", "0", "1", "0", head, head, base, base, "true"},
			output: "complete:0",
		},
		{
			name:   "first missing output retries",
			args:   []string{"0", "0", "0", "0", head, head, base, base, "true"},
			output: "retry:0",
		},
		{
			name:   "second missing output fails closed",
			args:   []string{"0", "0", "0", "1", head, head, base, base, "true"},
			output: "exhausted:1",
		},
		{
			name:   "explicit incomplete output does not retry",
			args:   []string{"1", "0", "0", "0", head, head, base, base, "true"},
			output: "reported:0",
		},
		{
			name:   "partial durable output retries",
			args:   []string{"0", "0", "0", "0", head, head, base, base, "true"},
			output: "retry:0",
		},
		{
			name:   "changed head suppresses stale retry",
			args:   []string{"0", "0", "0", "0", head, otherHead, base, base, "true"},
			output: "stale:0",
		},
		{
			name:   "changed base suppresses stale retry",
			args:   []string{"0", "0", "0", "0", head, head, base, otherBase, "true"},
			output: "stale:0",
		},
		{
			name:   "ineligible pull request suppresses retry",
			args:   []string{"0", "0", "0", "0", head, head, base, base, "false"},
			output: "stale:0",
		},
		{
			name:   "multiple reviews fail closed",
			args:   []string{"0", "0", "2", "0", head, head, base, base, "true"},
			output: "invalid:1",
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			args := append([]string{scriptArgument}, fixture.args...)
			cmd := exec.Command(bash, args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute review recovery fixture: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != fixture.output {
				t.Fatalf("decision=%q, want %q", got, fixture.output)
			}
		})
	}
}

func TestNewerRunSuppressionFixtures(t *testing.T) {
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
			t.Fatal("bash is required to execute review recovery fixtures")
		}
	}

	source, err := os.ReadFile(
		filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-recovery.yml"),
	)
	if err != nil {
		t.Fatalf("read review recovery workflow: %v", err)
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	const begin = "# BEGIN TESTABLE REVIEW RECOVERY POLICY"
	const end = "# END TESTABLE REVIEW RECOVERY POLICY"
	start := strings.Index(text, begin)
	finish := strings.Index(text, end)
	if start < 0 || finish <= start {
		t.Fatal("review recovery policy helper block is missing")
	}
	helper := text[start+len(begin) : finish]

	script := `#!/usr/bin/env bash
set -u
` + helper + `
newer_run_suppresses_retry "$1" "$2" "$3" "$4" "${5:-}"
`
	scriptPath := filepath.Join(t.TempDir(), "newer-run-suppression.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write newer run suppression fixture: %v", err)
	}
	scriptArgument := filepath.ToSlash(scriptPath)
	if volume := filepath.VolumeName(scriptPath); volume != "" {
		scriptArgument = "/" +
			strings.ToLower(strings.TrimSuffix(volume, ":")) +
			strings.TrimPrefix(scriptArgument, filepath.ToSlash(volume))
	}

	for _, fixture := range []struct {
		name   string
		args   []string
		output string
	}{
		{
			name:   "completed successful provenance match suppresses",
			args:   []string{"completed", "true", "true", "false", "success"},
			output: "suppress",
		},
		{
			name:   "completed failed provenance match suppresses",
			args:   []string{"completed", "true", "true", "false", "failure"},
			output: "suppress",
		},
		{
			name:   "in-flight provenance match suppresses",
			args:   []string{"in_progress", "true", "true", "false"},
			output: "suppress",
		},
		{
			name:   "in-flight unavailable provenance with title match suppresses",
			args:   []string{"in_progress", "false", "false", "true"},
			output: "suppress",
		},
		{
			name:   "queued unavailable provenance with title match suppresses",
			args:   []string{"queued", "false", "false", "true"},
			output: "suppress",
		},
		{
			name:   "in-flight unavailable provenance without title match allows",
			args:   []string{"in_progress", "false", "false", "false"},
			output: "allow",
		},
		{
			name:   "completed unavailable provenance with title match allows",
			args:   []string{"completed", "false", "false", "true"},
			output: "allow",
		},
		{
			name:   "cancelled unavailable provenance with title match allows",
			args:   []string{"completed", "false", "false", "true", "cancelled"},
			output: "allow",
		},
		{
			name:   "cancelled matching provenance allows",
			args:   []string{"completed", "true", "true", "true", "cancelled"},
			output: "allow",
		},
		{
			name:   "cancelled unavailable provenance without title match allows",
			args:   []string{"completed", "false", "false", "false", "cancelled"},
			output: "allow",
		},
		{
			name:   "completed run with unavailable provenance allows",
			args:   []string{"completed", "false", "false", "false"},
			output: "allow",
		},
		{
			name:   "completed run with provenance mismatch allows",
			args:   []string{"completed", "true", "false", "false"},
			output: "allow",
		},
		{
			name:   "in-flight run with provenance mismatch allows",
			args:   []string{"in_progress", "true", "false", "false"},
			output: "allow",
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			args := append([]string{scriptArgument}, fixture.args...)
			cmd := exec.Command(bash, args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute newer run suppression fixture: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != fixture.output {
				t.Fatalf("suppression=%q, want %q", got, fixture.output)
			}
		})
	}
}
