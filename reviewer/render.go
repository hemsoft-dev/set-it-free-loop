package reviewer

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}
type Document struct {
	Summary string          `json:"summary"`
	Inline  []InlineComment `json:"inline"`
}

var credentialText = regexp.MustCompile(`(?i)\b(?:gh[pousr]_|github_pat_|sk-(?:proj-|ant-)?|xox[baprs]-)[a-z0-9_-]{16,}|\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
var externalLink = regexp.MustCompile(`(?i)(?:https?://|ftp://|www\.)[^\s]+`)
var markdownEscaper = strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "!", "\\!", "#", "\\#", "|", "\\|", "~", "\\~")

func safeText(s string) string {
	s = credentialText.ReplaceAllString(s, "[redacted credential]")
	s = externalLink.ReplaceAllString(s, "[external link omitted]")
	s = strings.Map(func(r rune) rune {
		if r == '@' {
			return '＠'
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return markdownEscaper.Replace(html.EscapeString(s))
}

func sourceURL(snapshot Snapshot, a Anchor) string {
	segments := strings.Split(a.Path, "/")
	for i, p := range segments {
		segments[i] = url.PathEscape(p)
	}
	return "https://github.com/hemsoft-dev/set-it-free-loop/blob/" + snapshot.Head + "/" + strings.Join(segments, "/") + fmt.Sprintf("#L%d", a.Line)
}

// Render uses only a trusted diff-line map for inline eligibility. Source-valid
// findings outside the diff stay in the summary. Incomplete jobs never expose
// substantive partial findings, even if prior phases succeeded.
func Render(out Outcome, diffLines map[string]map[int]bool) (Document, error) {
	doc := Document{Inline: []InlineComment{}}
	if err := out.Job.Validate(); err != nil {
		return doc, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## SFL advisory review\n\nMode: `%s`.\n\n", out.Job.Config.Mode)
	b.WriteString("<details>\n<summary>Reviewed revisions and configuration</summary>\n\n")
	fmt.Fprintf(&b, "Head: `%s`. Base: `%s`. Merge base: `%s`.\n\nConfiguration: `%s`, version %d. Trusted release: `%s`.\n\n", out.Job.Snapshot.Head, out.Job.Snapshot.Base, out.Job.Snapshot.MergeBase, out.Job.Config.Hash(), out.Job.ConfigVersion, out.Job.Release)
	b.WriteString("| Provider | Model | CLI version | Authentication |\n| --- | --- | --- | --- |\n")
	for _, p := range out.Job.Config.Providers() {
		i := out.Job.Config.Codex
		if p == Claude {
			i = out.Job.Config.Claude
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", i.Provider, i.Model, i.ClientVersion, i.AuthMode)
	}
	b.WriteString("\n</details>\n\n")
	if out.State != "dual_complete" && out.State != "single_complete" {
		fmt.Fprintf(&b, "INCOMPLETE: %s. No substantive partial findings are published. Completed phases: %d of %d.\n", safeText(out.State), len(out.Artifacts), out.Job.Config.MaxSessions())
		doc.Summary = b.String()
		return doc, nil
	}
	if len(out.Artifacts) != out.Job.Config.MaxSessions() {
		return doc, fmt.Errorf("completion is missing required phases")
	}
	if (len(out.Job.Config.Providers()) == 1 && out.State != "single_complete") || (len(out.Job.Config.Providers()) == 2 && out.State != "dual_complete") {
		return doc, fmt.Errorf("completion does not match selected mode")
	}
	fmt.Fprintf(&b, "Completed %d required phases. Peer support is model-assessed, not proof of a defect. Proposed tests were not executed.\n\n", len(out.Artifacts))
	if len(out.Ledger) == 0 {
		b.WriteString("No findings reported within the stated coverage. This does not establish that omitted or unreadable code is clean.\n\n")
	}
	for _, status := range []string{"corroborated", "disputed", "unverified", "retracted"} {
		var section strings.Builder
		seen := map[string]bool{}
		for _, entry := range out.Ledger {
			if entry.Status != status {
				continue
			}
			if !validAnchor(entry.Finding.Anchor, out.Job.Snapshot) {
				return doc, fmt.Errorf("invalid ledger anchor")
			}
			if !entry.Origin.Valid() || entry.RaisedRound < 1 || entry.RaisedRound > 3 || entry.SeverityMin < 0 || entry.SeverityMax > 3 || entry.SeverityMin > entry.SeverityMax {
				return doc, fmt.Errorf("invalid ledger provenance or severity")
			}
			f := entry.Finding
			fingerprint := Digest(struct {
				Title, Body string
				Anchor      Anchor
				Status      string
			}{f.Title, f.Body, f.Anchor, status})
			if seen[fingerprint] {
				continue
			}
			seen[fingerprint] = true
			text := fmt.Sprintf("P%d %s\n\n%s\n\nSource: [%s:%d](%s). Finding `%s`, %s R%d.\n", entry.SeverityMin, safeText(f.Title), safeText(f.Body), safeText(f.Anchor.Path), f.Anchor.Line, sourceURL(out.Job.Snapshot, f.Anchor), safeText(f.ID), entry.Origin, entry.RaisedRound)
			if entry.SeverityMin != entry.SeverityMax {
				text += fmt.Sprintf("Severity disagreement: P%d through P%d.\n", entry.SeverityMin, entry.SeverityMax)
			}
			if len(entry.RelatedIDs) > 0 {
				text += "Related allegation IDs: " + safeText(strings.Join(entry.RelatedIDs, ", ")) + ".\n"
			}
			var history []Assessment
			for _, related := range out.Ledger {
				if related.Status == status && related.Finding.Title == f.Title && related.Finding.Body == f.Body && related.Finding.Anchor == f.Anchor {
					history = append(history, related.History...)
				}
			}
			for _, assessment := range history {
				if !assessment.Provider.Valid() {
					return doc, fmt.Errorf("invalid assessment identity")
				}
				text += fmt.Sprintf("\n%s R%d, %s: %s\n", assessment.Provider, assessment.Round, safeText(assessment.Judgment.Verdict), safeText(assessment.Judgment.Reason))
				for _, a := range assessment.Judgment.Evidence {
					if !validAnchor(a, out.Job.Snapshot) {
						return doc, fmt.Errorf("invalid assessment source anchor")
					}
					text += fmt.Sprintf("Evidence: [%s:%d](%s).\n", safeText(a.Path), a.Line, sourceURL(out.Job.Snapshot, a))
				}
			}
			section.WriteString("### " + text + "\n")
			if status == "corroborated" && diffLines[f.Anchor.Path][f.Anchor.Line] {
				doc.Inline = append(doc.Inline, InlineComment{f.Anchor.Path, f.Anchor.Line, "Model-assessed peer support.\n\n" + text})
			}
		}
		if section.Len() > 0 {
			label := map[string]string{"corroborated": "Peer-supported findings", "disputed": "Disputed findings", "unverified": "Unverified findings", "retracted": "Retracted allegations"}[status]
			b.WriteString("## " + label + "\n\n" + section.String())
		}
	}
	b.WriteString("## Coverage\n\n")
	coverageCount := 0
	for _, artifact := range out.Artifacts {
		for _, c := range append(append([]Coverage(nil), artifact.Report.Coverage...), artifact.Coverage...) {
			fmt.Fprintf(&b, "- %s R%d: %s %s\n", artifact.Report.Provider, artifact.Report.Round, safeText(c.Path), safeText(c.Reason))
			coverageCount++
		}
	}
	if coverageCount == 0 {
		b.WriteString("No explicit omissions reported. Source-reader limits and inspected evidence still bound this review.\n")
	}
	doc.Summary = b.String()
	if len(doc.Summary) > 64<<10 || len(doc.Inline) > 40 {
		return Document{}, fmt.Errorf("rendered review exceeds publication bounds")
	}
	for _, comment := range doc.Inline {
		if len(comment.Body) > 16<<10 {
			return Document{}, fmt.Errorf("inline body exceeds publication bound")
		}
	}
	return doc, nil
}
