package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewTriggerConcurrencyGroups(t *testing.T) {
	source, err := os.ReadFile(
		filepath.Join("..", "deployment", "infrastructure", "sfl-pr-review-auto.yml"),
	)
	if err != nil {
		t.Fatalf("read review auto trigger workflow: %v", err)
	}

	const group = "group: sfl-pr-review-auto-${{ github.event.pull_request.number || github.event.issue.number }}-${{ github.event.pull_request.head.sha || github.event.comment.id }}-${{ github.event_name }}-${{ github.event.action }}-${{ github.event.action == 'labeled' && github.event.label.name == 'sfl-review' && 'explicit' || 'standard' }}"
	if !strings.Contains(normalizeLineEndings(source), group) {
		t.Fatal("automatic reviewer does not isolate required runs, callbacks, heads, and explicit labels")
	}

	groupFor := func(pr, head, event, action, label string) string {
		suffix := "standard"
		if action == "labeled" && label == "sfl-review" {
			suffix = "explicit"
		}
		return fmt.Sprintf("sfl-pr-review-auto-%s-%s-%s-%s-%s", pr, head, event, action, suffix)
	}

	required := groupFor("78", "head-a", "pull_request_target", "synchronize", "")
	distinct := map[string]string{
		"prior head":      groupFor("78", "head-b", "pull_request_target", "synchronize", ""),
		"review callback": groupFor("78", "head-a", "pull_request_review", "submitted", ""),
		"edited event":    groupFor("78", "head-a", "pull_request_target", "edited", ""),
		"explicit label":  groupFor("78", "head-a", "pull_request_target", "labeled", "sfl-review"),
		"ordinary label":  groupFor("78", "head-a", "pull_request_target", "labeled", "sfl-effort:high"),
	}
	for name, candidate := range distinct {
		if candidate == required {
			t.Fatalf("%s group %q can evict required group %q", name, candidate, required)
		}
	}
	if distinct["explicit label"] == distinct["ordinary label"] {
		t.Fatal("explicit review trigger shares concurrency with ordinary labels")
	}
	if duplicate := groupFor("78", "head-a", "pull_request_target", "synchronize", ""); duplicate != required {
		t.Fatalf("duplicate required group=%q, want %q", duplicate, required)
	}
	commentGroupFor := func(issue, commentID string) string {
		return fmt.Sprintf("sfl-pr-review-auto-%s-%s-issue_comment-created-standard", issue, commentID)
	}
	command := commentGroupFor("78", "1234")
	if duplicate := commentGroupFor("78", "1234"); duplicate != command {
		t.Fatalf("duplicate command group=%q, want %q", duplicate, command)
	}
	if anotherComment := commentGroupFor("78", "1235"); anotherComment == command {
		t.Fatal("distinct comment commands share a concurrency group")
	}
}
