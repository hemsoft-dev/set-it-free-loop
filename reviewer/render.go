package reviewer

import (
	"fmt"
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

// htmlEscaper uses named entities only. Numeric entities such as &#39; would
// put an ASCII '#' followed by digits back into the published text.
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")

// issueKeyReference matches GitHub's GH-<number> issue key unless the key is glued
// to a preceding ASCII letter or digit, where GitHub cannot match it either.
var issueKeyReference = regexp.MustCompile(`(^|[^A-Za-z0-9])([Gg][Hh])-([0-9])`)

// commitRunJoiner splits commit-shaped hexadecimal runs. U+2060 WORD JOINER is
// invisible and does not allow a line break.
const commitRunJoiner = "\u2060"

// commitRunChunk is one less than the shortest commit SHA GitHub autolinks.
const commitRunChunk = 6

func asciiAlphanumeric(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func hexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' || c >= 'a' && c <= 'f'
}

// neutralizeReferences removes GitHub reference autolinks from model text. The
// caller has already replaced '#' and '@' with fullwidth signs, which covers
// #N, owner/repo#N, owner@SHA and owner/repo@SHA. This rewrites the rest:
//   - GH-N (any case) not preceded by an ASCII letter or digit becomes GH－N
//     with a fullwidth hyphen-minus.
//   - Every maximal run of at least 7 ASCII hexadecimal digits (any case) that
//     is not preceded or followed by an ASCII letter or digit gets a word
//     joiner after each 6 digits, so no piece is long enough to be a SHA.
//
// Runs glued to other ASCII letters or digits stay unchanged because GitHub's
// word-boundary match cannot select them.
func neutralizeReferences(s string) string {
	s = issueKeyReference.ReplaceAllString(s, "${1}${2}－${3}")
	var b strings.Builder
	for i := 0; i < len(s); {
		if !hexDigit(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := i
		for end < len(s) && hexDigit(s[end]) {
			end++
		}
		run := s[i:end]
		glued := i > 0 && asciiAlphanumeric(s[i-1]) || end < len(s) && asciiAlphanumeric(s[end])
		for len(run) > commitRunChunk && !glued {
			b.WriteString(run[:commitRunChunk] + commitRunJoiner)
			run = run[commitRunChunk:]
		}
		b.WriteString(run)
		i = end
	}
	return b.String()
}

// safeText renders untrusted model text as inert Markdown. It cannot produce
// mentions, issue or commit reference autolinks, links, images or HTML.
func safeText(s string) string {
	s = credentialText.ReplaceAllString(s, "[redacted credential]")
	s = externalLink.ReplaceAllString(s, "[external link omitted]")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '@':
			return '＠'
		case r == '#':
			return '＃'
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, s)
	return markdownEscaper.Replace(htmlEscaper.Replace(neutralizeReferences(s)))
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
	if snapshotEvidencePacket(out.Job.Packet) {
		return Document{}, ErrFenced
	}
	return renderOutcome(out, diffLines)
}

func renderOutcome(out Outcome, diffLines map[string]map[int]bool) (Document, error) {
	doc := Document{Inline: []InlineComment{}}
	if err := out.Job.Validate(); err != nil {
		return doc, err
	}
	complete := out.State == "dual_complete" || out.State == "single_complete"
	var b strings.Builder
	title := "SFL advisory review"
	if !complete {
		title = "SFL advisory review status"
	}
	fmt.Fprintf(&b, "## %s\n\nMode: `%s`.\n\n", title, out.Job.Config.Mode)
	b.WriteString(triggerDescription(out.Job.Origin))
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
	if !complete {
		fmt.Fprintf(&b, "%s No substantive partial findings are published. Completed phases: %d of %d.\n", safeText(statusBudgetCause(out.Job, out.State, out.ConsumedSessions, out.Artifacts)), len(out.Artifacts), out.Job.Config.MaxSessions())
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
	ledger := renderGroups(out.Ledger)
	omitted := 0
	for _, status := range []string{"corroborated", "disputed", "unverified", "retracted"} {
		var section strings.Builder
		seen := map[string]bool{}
		for _, entry := range ledger {
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
			if len(entry.WithdrawnIDs) > 0 {
				text += "Withdrawn duplicate IDs: " + safeText(strings.Join(entry.WithdrawnIDs, ", ")) + ".\n"
			}
			if len(entry.RelatedIDs) > 0 {
				text += "Related allegation IDs: " + safeText(strings.Join(entry.RelatedIDs, ", ")) + ".\n"
			}
			history := entry.History
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
			if b.Len()+section.Len()+len(text) > 48<<10 {
				omitted++
				continue
			}
			section.WriteString("### " + text + "\n")
			if status == "corroborated" && diffLines[f.Anchor.Path][f.Anchor.Line] && len(doc.Inline) < 40 && len(text) < (16<<10)-64 {
				doc.Inline = append(doc.Inline, InlineComment{f.Anchor.Path, f.Anchor.Line, "Model-assessed peer support.\n\n" + text})
			}
		}
		if section.Len() > 0 {
			label := map[string]string{"corroborated": "Peer-supported findings", "disputed": "Disputed findings", "unverified": "Unverified findings", "retracted": "Retracted allegations"}[status]
			b.WriteString("## " + label + "\n\n" + section.String())
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "Publication limit: %d finding groups omitted from this summary; retained in the durable ledger. This review is not exhaustive.\n\n", omitted)
	}
	b.WriteString("## Coverage\n\n")
	coverageCount := 0
	for _, artifact := range out.Artifacts {
		for _, c := range append(append([]Coverage(nil), artifact.Report.Coverage...), artifact.Coverage...) {
			line := fmt.Sprintf("- %s R%d: %s %s\n", artifact.Report.Provider, artifact.Report.Round, safeText(c.Path), safeText(c.Reason))
			if b.Len()+len(line) < 60<<10 {
				b.WriteString(line)
			} else {
				omitted++
				continue
			}
			coverageCount++
		}
	}
	if coverageCount == 0 {
		b.WriteString("No explicit omissions reported. Source-reader limits and inspected evidence still bound this review.\n")
	}
	if omitted > 0 {
		b.WriteString("\nSome retained evidence exceeded publication limits. See the durable job record for complete findings and coverage.\n")
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

// statusCause names why a job has no review. It is fixed controller text keyed
// by the durable job state and the captured configuration, never model output,
// and the caller still passes it through safeText. Cancellation and supersession
// have their own leading word; every other non-complete state is INCOMPLETE.
func statusCause(job Job, state string) string {
	switch state {
	case "quota_wait":
		return fmt.Sprintf("INCOMPLETE: the daily review quota is exhausted. This job waits for quota and resumes by itself until its %s quota wait expires.", job.Config.QuotaWait)
	case "auth_failed":
		return "INCOMPLETE: provider authentication is unavailable. This job stays stopped until an operator repairs authentication and explicitly resumes it."
	case "provider_unavailable":
		return "INCOMPLETE: a selected provider is unavailable. This job stays stopped until an operator explicitly resumes it."
	case "invalid_output":
		return "INCOMPLETE: a provider returned output that failed validation. This job ended without a review."
	case "incomplete_expired":
		return "INCOMPLETE: this job expired before every required phase completed. It ended without a review."
	case "capture_refused":
		cause := "this snapshot could not be captured"
		switch captureRefusalReason(job) {
		case CaptureMissingHistory:
			cause = "the commit history of this snapshot is missing from the trusted object store, so its changes could not be captured"
		case CaptureAmbiguousHistory:
			cause = "the merge base of this snapshot is ambiguous or differs from the recorded merge base, so its changes could not be captured"
		case CaptureInventoryLimit:
			cause = fmt.Sprintf("this snapshot changes more than %d files, over the capture inventory limit, so its changes could not be captured", snapshotFileLimit)
		}
		return "INCOMPLETE: " + cause + ". No source was reviewed and this request ended without a review. A new head or a fresh authorized request is captured again."
	case "cancelled":
		return "CANCELLED: this review request was cancelled before the job completed. A new review needs a fresh authorized request."
	case "stale":
		return "SUPERSEDED: a newer review request or head replaced this job before it completed. This status does not describe or fulfill the newer request."
	}
	return "INCOMPLETE: " + state + "."
}

// statusBudgetCause is statusCause, except that a job whose remaining session
// budget can no longer finish its schedule says so: it cannot resume, and an
// expired job names the charged sessions that produced no result.
func statusBudgetCause(job Job, state string, consumed int, artifacts []Artifact) string {
	if sessionBudgetFits(job, Checkpoint{ConsumedSessions: consumed, Artifacts: artifacts}) {
		return statusCause(job, state)
	}
	short := fmt.Sprintf("%d of its %d captured sessions are charged, %d of them without a result, so the remaining sessions cannot complete every required phase.", consumed, job.Config.MaxSessions(), consumed-len(artifacts))
	switch state {
	case "incomplete_expired":
		return "INCOMPLETE: the captured session budget is exhausted. " + short + " It ended without a review and starts no further session."
	case "auth_failed":
		return "INCOMPLETE: provider authentication is unavailable. " + short + " This job cannot be resumed and ended without a review."
	case "provider_unavailable":
		return "INCOMPLETE: a selected provider is unavailable. " + short + " This job cannot be resumed and ended without a review."
	}
	return statusCause(job, state)
}

// Exact duplicate allegations share one conservative status and all history.
// An unresolved objection to either ID blocks inline publication for the group.
func renderGroups(entries []Entry) []Entry {
	groups := []Entry{}
	indexes := map[string]int{}
	for _, e := range entries {
		key := Digest(struct {
			Title, Body string
			Anchor      Anchor
		}{e.Finding.Title, e.Finding.Body, e.Finding.Anchor})
		if i, ok := indexes[key]; ok {
			g := &groups[i]
			g.History = append(g.History, e.History...)
			if e.Status == "retracted" {
				g.WithdrawnIDs = append(g.WithdrawnIDs, e.Finding.ID)
			}
			g.RelatedIDs = append(g.RelatedIDs, e.Finding.ID)
			if e.Status != "retracted" {
				if g.Status == "retracted" {
					g.RelatedIDs = append(g.RelatedIDs, g.Finding.ID)
					g.Finding = e.Finding
					g.Origin = e.Origin
					g.RaisedRound = e.RaisedRound
					g.Status = e.Status
					g.SeverityMin = e.SeverityMin
					g.SeverityMax = e.SeverityMax
				}
				if e.SeverityMin < g.SeverityMin {
					g.SeverityMin = e.SeverityMin
				}
				if e.SeverityMax > g.SeverityMax {
					g.SeverityMax = e.SeverityMax
				}
				if e.Status == "disputed" || g.SeverityMin != g.SeverityMax {
					g.Status = "disputed"
				} else if e.Status == "unverified" && g.Status != "disputed" {
					g.Status = "unverified"
				}
			}
		} else {
			if e.Status == "retracted" {
				e.WithdrawnIDs = append([]string{}, e.Finding.ID)
			}
			indexes[key] = len(groups)
			e.History = append([]Assessment{}, e.History...)
			e.RelatedIDs = append([]string{}, e.RelatedIDs...)
			groups = append(groups, e)
		}
	}
	return groups
}
