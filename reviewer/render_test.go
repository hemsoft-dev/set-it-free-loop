package reviewer

import (
	"context"
	"flag"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

func TestExactDuplicateObjectionBlocksInline(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		r := protocolReport(s)
		if s.Round == 2 && s.Identity.Provider == Claude {
			r.Judgments[0].Verdict = "reject"
			r.Judgments[0].Reason = "Unresolved objection"
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
	if len(doc.Inline) != 0 || !strings.Contains(doc.Summary, "Unresolved objection") || strings.Contains(doc.Summary, "## Peer-supported findings") {
		t.Fatal("duplicate hid objection", doc)
	}
}

func TestPublicationBoundsRetainOversizeLedger(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil })
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	seed := out.Ledger[0]
	out.Ledger = nil
	for i := 0; i < 100; i++ {
		entry := seed
		entry.Finding.Title = strings.Repeat("large finding ", 300) + string(rune('A'+i))
		out.Ledger = append(out.Ledger, entry)
	}
	doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
	if err != nil || len(doc.Summary) > 64<<10 || len(doc.Inline) > 40 || !strings.Contains(doc.Summary, "Publication limit") || len(out.Ledger) != 100 {
		t.Fatal("valid ledger cannot publish bounded document", err)
	}
}

func TestWithdrawnDuplicateCannotRepresentActiveGroup(t *testing.T) {
	first := Entry{Finding: testFinding(Codex), Origin: Codex, RaisedRound: 1, Status: "retracted", SeverityMin: 2, SeverityMax: 2}
	active := first
	active.Finding.ID = "claude-active"
	active.Origin = Claude
	active.RaisedRound = 2
	active.Status = "corroborated"
	groups := renderGroups([]Entry{first, active})
	if len(groups) != 1 || groups[0].Finding.ID != active.Finding.ID || groups[0].Origin != Claude || groups[0].RaisedRound != 2 || len(groups[0].WithdrawnIDs) != 1 || groups[0].WithdrawnIDs[0] != first.Finding.ID {
		t.Fatal("withdrawn provenance survived as active", groups)
	}
}

func TestEscapedCoverageCannotExceedPublicationLimit(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil })
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	out.Artifacts[0].Coverage = []Coverage{}
	for i := 0; i < 256; i++ {
		out.Artifacts[0].Coverage = append(out.Artifacts[0].Coverage, Coverage{Path: strings.Repeat("'", 512), Reason: strings.Repeat("'", 1024)})
	}
	doc, err := Render(out, nil)
	if err != nil || len(doc.Summary) > 64<<10 || !strings.Contains(doc.Summary, "exceeded publication limits") {
		t.Fatal("escaped coverage overflow", err)
	}
}

// hostileReferences covers GitHub's autolinked reference grammar: issue and
// pull request numbers, GH- keys, cross-repository numbers, cross-repository
// and user commit references, bare commit SHAs and compare ranges, plus forms
// that only become references after Markdown or HTML escaping is undone.
var hostileReferences = []string{
	"#262", "(#7)", "[#8]", "fixes #9.", "GH-262", "gh-12", "(GH-3)", "_GH-4", `\GH-15`,
	"hemsoft-dev/other#5", "owner/repo#6", "owner#10",
	"owner/repo@deadbeef1", "owner@1a2b3c4", "hemsoft-dev/set-it-free-loop@a3f665b",
	"a3f665b", "A3F665B", "0123456789abcdef0123456789abcdef01234567", "a3f665b...f0a5fa0",
	"deadbeef", "1234567", "sha:a3f665b,", "deadbeef\x01cafe1234",
	`\#11`, "&#35;12", "&num;13", "&#x23;14", "it's", `say "x"`,
	"x https://github.com/hemsoft-dev/other/issues/1", "a3f665bhttps://evil.example",
}

var (
	autolinkIssue    = regexp.MustCompile(`#[0-9]`)
	autolinkKey      = regexp.MustCompile(`(?i)\bgh-[0-9]`)
	autolinkCommit   = regexp.MustCompile(`\b[0-9A-Fa-f]{7,40}\b`)
	markdownDecoding = regexp.MustCompile(`\\[!-/:-@\[-` + "`" + `{-~]|&(?:#[0-9]{1,7}|#[xX][0-9A-Fa-f]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)
)

// githubText approximates the text GitHub's reference filter sees after
// CommonMark resolves backslash escapes and entity references in one pass.
func githubText(markdown string) string {
	return markdownDecoding.ReplaceAllStringFunc(markdown, func(m string) string {
		if m[0] == '\\' {
			return m[1:]
		}
		return html.UnescapeString(m)
	})
}

// autolinkCandidates reports text GitHub could autolink, checking both the raw
// Markdown (code spans are literal) and its decoded text. Trusted snapshot SHAs
// are the only allowed commit-shaped runs.
func autolinkCandidates(markdown string, trusted ...string) []string {
	var found []string
	for _, text := range []string{markdown, githubText(markdown)} {
		found = append(found, autolinkIssue.FindAllString(text, -1)...)
		found = append(found, autolinkKey.FindAllString(text, -1)...)
		if strings.Contains(text, "@") {
			found = append(found, "@")
		}
		for _, m := range autolinkCommit.FindAllString(text, -1) {
			if !slices.Contains(trusted, m) {
				found = append(found, m)
			}
		}
	}
	return found
}

func TestSafeTextNeutralizesReferenceAutolinks(t *testing.T) {
	for _, in := range hostileReferences {
		for _, text := range []string{in, "before " + in + " after", "`" + in + "`"} {
			if found := autolinkCandidates(safeText(text)); len(found) != 0 {
				t.Errorf("reference autolink survived %q -> %q: %q", text, safeText(text), found)
			}
		}
	}
	for in, want := range map[string]string{
		"#262":           "＃262",
		"owner/repo#6":   "owner/repo＃6",
		"gh-12 and GH-3": "gh－12 and GH－3",
		"a3f665b":        "a3f665\u2060b",
		"0123456789abcdef0123456789abcdef01234567": "012345\u20606789ab\u2060cdef01\u2060234567\u206089abcd\u2060ef0123\u20604567",
		"it's":                      "it&apos;s",
		"high-5 deadbeefs x1234567": "high-5 deadbeefs x1234567",
		"123456 abcdef build 2026":  "123456 abcdef build 2026",
	} {
		if got := safeText(in); got != want {
			t.Errorf("safeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRendererNeutralizesReferenceAutolinksEverywhere(t *testing.T) {
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil })
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	hostile := strings.Join(hostileReferences, " ")
	entry := &out.Ledger[0]
	entry.Finding.ID = hostile
	entry.Finding.Title = hostile
	entry.Finding.Body = hostile
	entry.RelatedIDs = append(entry.RelatedIDs, hostile)
	for i := range entry.History {
		entry.History[i].Judgment.Verdict = hostile
		entry.History[i].Judgment.Reason = hostile
	}
	out.Artifacts[0].Coverage = []Coverage{{Path: hostile, Reason: hostile}}
	doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := out.Job.Snapshot
	trusted := []string{snapshot.Head, snapshot.Base, snapshot.MergeBase, out.Job.Release}
	if found := autolinkCandidates(doc.Summary, trusted...); len(found) != 0 {
		t.Fatalf("model text produced reference autolinks in summary: %q", found)
	}
	if len(doc.Inline) == 0 {
		t.Fatal("hostile corroborated finding lost its inline candidate")
	}
	for _, comment := range doc.Inline {
		if found := autolinkCandidates(comment.Body, trusted...); len(found) != 0 {
			t.Fatalf("model text produced reference autolinks inline: %q", found)
		}
	}
	link := "[source.go:1](https://github.com/hemsoft-dev/set-it-free-loop/blob/" + snapshot.Head + "/source.go#L1)"
	revisions := "Head: `" + snapshot.Head + "`. Base: `" + snapshot.Base + "`. Merge base: `" + snapshot.MergeBase + "`."
	if !strings.Contains(doc.Summary, "Source: "+link) || !strings.Contains(doc.Summary, "Evidence: "+link) || !strings.Contains(doc.Summary, revisions) || !strings.Contains(doc.Inline[0].Body, link) {
		t.Fatal("trusted snapshot links or revisions changed", doc.Summary)
	}
}
