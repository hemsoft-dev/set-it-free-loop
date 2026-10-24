package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkGolden compares rendered Markdown with a reviewed fixture, rewriting it
// only under -update-golden.
func checkGolden(t *testing.T, name, rendered string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(rendered), 0644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(golden) != rendered {
		t.Fatal("rendered Markdown differs from reviewed golden fixture", name)
	}
}

// Each non-complete state renders its own cause with no partial finding, even
// when an earlier phase produced one. Cancelled and superseded jobs have their
// own wording rather than a generic INCOMPLETE.
func TestStatusMarkdownGolden(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil })
	complete, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"quota_wait", "auth_failed", "provider_unavailable", "invalid_output", "incomplete_expired", "cancelled", "stale"} {
		t.Run(state, func(t *testing.T) {
			out := Outcome{Job: complete.Job, State: state, Artifacts: complete.Artifacts[:1]}
			doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Inline) != 0 || strings.Contains(doc.Summary, "Possible defect") || strings.Contains(doc.Summary, "Source-grounded allegation") || !strings.Contains(doc.Summary, "No substantive partial findings are published. Completed phases: 1 of 4.") {
				t.Fatal("status exposed a partial finding or lost its phase count", doc.Summary)
			}
			checkGolden(t, "status-"+state+".golden.md", doc.Summary)
		})
	}
}

// A completed adversarial review with an R1 objection and an R2-raised late
// allegation renders the disputed and unverified sections.
func TestDisputedLateReviewMarkdownGolden(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		r := protocolReport(s)
		if s.Round == 2 {
			for i := range r.Judgments {
				r.Judgments[i].Verdict = "reject"
				r.Judgments[i].Reason = "Concrete source-grounded disagreement"
			}
			late := testFinding(s.Identity.Provider)
			late.ID = string(s.Identity.Provider) + ":late"
			late.Title = "Late allegation"
			r.Findings = append(r.Findings, late)
		}
		return r, nil
	})
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Summary, "## Disputed findings") || !strings.Contains(doc.Summary, "## Unverified findings") || strings.Contains(doc.Summary, "## Peer-supported findings") || len(doc.Inline) != 0 {
		t.Fatal("disputed or late findings rendered as peer-supported", doc.Summary)
	}
	checkGolden(t, "disputed-late-review.golden.md", doc.Summary)
}
