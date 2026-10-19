package reviewer

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite explicitly requested synthetic render fixture")

func TestReviewMarkdownGolden(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil })
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "adversarial-review.golden.md")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(doc.Summary), 0644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(golden) != doc.Summary {
		t.Fatal("rendered Markdown differs from reviewed golden fixture")
	}
}

func TestRendererNeutralizesUntrustedContent(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		r := protocolReport(s)
		if s.Round == 1 {
			r.Findings[0].Title = "@HemSoft <img src=https://evil.example/x> ![](https://evil.example/image.png)"
			r.Findings[0].Body = "[click](javascript:alert(1)) www.evil.example <script>bad()</script>"
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
	for _, bad := range []string{"@HemSoft", "<img", "<script>", "https://evil.example", "www.evil.example", "[click](javascript"} {
		if strings.Contains(doc.Summary, bad) {
			t.Fatal("unsafe Markdown survived", bad)
		}
	}
	if !strings.Contains(doc.Summary, "https://github.com/hemsoft-dev/set-it-free-loop/blob/") {
		t.Fatal("trusted source links lost")
	}
}

func TestCheckpointDoesNotAliasReportsAndRejectsOrphanPhases(t *testing.T) {
	calls := 0
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) { calls++; return protocolReport(s), nil })
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := out.Checkpoint()
	checkpoint.Artifacts[0].Report.Findings[0].Body = "mutated"
	if out.Artifacts[0].Report.Findings[0].Body == "mutated" {
		t.Fatal("checkpoint rewrote captured evidence")
	}
	checkpoint = out.Checkpoint()
	checkpoint.Artifacts = checkpoint.Artifacts[2:3]
	checkpoint.ConsumedSessions = 1
	before := calls
	if _, err := e.Run(context.Background(), testJob(), checkpoint); err == nil || calls != before {
		t.Fatal("orphan phase consumed provider budget")
	}
}

func TestRendererRedactsRecognizableCredentialStrings(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	if strings.Contains(safeText("Credential "+token), token) {
		t.Fatal("credential-shaped value survived rendering")
	}
}
