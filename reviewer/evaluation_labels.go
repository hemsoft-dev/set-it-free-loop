package reviewer

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Labeler identifies the actor that inspected independent evidence. Agent
// labels require the actual agent, provider, model and session, not reviewer
// output attribution. Only Franz can use kind "owner" or record an override.
type Labeler struct {
	Kind, Agent, Provider, Model, Session string
	HumanID                               int64
}
type EvidenceLines struct {
	Revision, Path string
	Start, End     int
}

// EvaluationEvidence is a primary source, never an evaluated review, comment,
// model response or paraphrase of one. At records the source event's time.
// Lines identify the candidate lines affected by a defect, on head or base.
type EvaluationEvidence struct {
	Kind, URL, Commit, Description string
	At                             time.Time
	Lines                          []EvidenceLines
}
type LabelProvenance struct {
	Labeler       Labeler
	At            time.Time
	Evidence      []EvaluationEvidence
	Supersedes    string
	OwnerOverride bool
}
type SurvivalWindow struct {
	Days            int
	From, Through   time.Time
	HistoryRevision string
	Searches        []string
	Limitations     string
}
type DefectLabel struct {
	ID, Rationale string
	Material      bool
	Provenance    LabelProvenance
}
type EvaluationCaseLabel struct {
	Clean      bool
	Rationale  string
	Provenance LabelProvenance
	Survival   []SurvivalWindow
	Defects    []DefectLabel
}
type EvaluationCase struct {
	PacketHash            string
	SerializedPacketBytes int
	ID                    string
	Snapshot              Snapshot
	MergedAt              time.Time
	Label                 EvaluationCaseLabel
	// Oldest first. Each revision binds the complete previous label digest.
	LabelHistory []EvaluationCaseLabel
}

var primaryEvidencePath = regexp.MustCompile(`^/hemsoft-dev/set-it-free-loop/(commit/[a-f0-9]{40,64}|pull/[1-9][0-9]*|issues/[1-9][0-9]*|blob/[a-f0-9]{40,64}/[^?#]+)$`)
var sourceLineFragment = regexp.MustCompile(`^L[1-9][0-9]*(?:-L[1-9][0-9]*)?$`)

func (p LabelProvenance) validate(earliest, latest time.Time) error {
	l := p.Labeler
	for _, value := range []string{l.Agent, l.Provider, l.Model, l.Session} {
		if strings.TrimSpace(value) == "" {
			return errors.New("label requires agent, provider, model and session identity")
		}
	}
	if (l.Kind != "agent" || l.HumanID != 0) && (l.Kind != "owner" || l.HumanID != OwnerID) {
		return errors.New("labeler must be an identified agent or Franz")
	}
	manualSession := strings.HasPrefix(l.Session, "manual-") && strings.TrimSpace(strings.TrimPrefix(l.Session, "manual-")) != ""
	if l.Kind == "owner" {
		if l.Agent != "Franz" || l.Provider != "human" || l.Model != "none" || !manualSession {
			return errors.New("owner provenance requires Franz, human/none and an identified manual- session")
		}
	} else if strings.EqualFold(l.Agent, "Franz") || strings.EqualFold(l.Provider, "human") || strings.EqualFold(l.Model, "none") || strings.HasPrefix(strings.ToLower(l.Session), "manual-") {
		return errors.New("agent provenance cannot claim owner identity fields")
	}
	if p.OwnerOverride && l.Kind != "owner" {
		return errors.New("only Franz can override labels")
	}
	if p.At.IsZero() || p.At.Before(earliest) || p.At.After(latest) || len(p.Evidence) == 0 {
		return errors.New("label requires a bounded timestamp and independent primary evidence")
	}
	for _, e := range p.Evidence {
		switch e.Kind {
		case "fix", "revert", "bug", "history", "source":
		default:
			return errors.New("review outputs and unknown evidence kinds cannot label the corpus")
		}
		u, err := url.Parse(e.URL)
		if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.RawPath != "" || !primaryEvidencePath.MatchString(u.Path) || (u.Fragment != "" && !sourceLineFragment.MatchString(u.Fragment)) {
			return errors.New("evidence must link primary repository source, commit, PR or issue; review/comment links are forbidden")
		}
		if strings.TrimSpace(e.Description) == "" || e.At.IsZero() || e.At.After(p.At) || !objectID.MatchString(e.Commit) {
			return errors.New("evidence requires source time, immutable revision and description")
		}
		// A commit/blob link must agree with the captured immutable revision.
		parts := strings.Split(u.Path, "/")
		if (parts[3] == "commit" || parts[3] == "blob") && parts[4] != e.Commit {
			return errors.New("evidence link revision differs from captured revision")
		}
		if e.Kind == "bug" && parts[3] != "issues" {
			return errors.New("bug evidence must link a bug/regression issue")
		}
		if (e.Kind == "fix" || e.Kind == "revert") && parts[3] != "commit" && parts[3] != "pull" {
			return errors.New("fix/revert evidence must link a later commit or PR")
		}
		for _, lines := range e.Lines {
			if !objectID.MatchString(lines.Revision) || !validPath(lines.Path) || lines.Start < 1 || lines.End < lines.Start {
				return errors.New("invalid evidence line range")
			}
		}
	}
	return nil
}

func validateLabelRevision(p LabelProvenance, previousHash string, previousAt time.Time) error {
	if p.Supersedes != previousHash || (previousHash != "" && !p.At.After(previousAt)) {
		return errors.New("label revision must preserve and hash its earlier label with a later timestamp")
	}
	if p.OwnerOverride != (previousHash != "" && p.Labeler.Kind == "owner") {
		return errors.New("owner revisions must explicitly record override status")
	}
	return nil
}

// ValidateLabels also works on draft cases before packet materialization. It
// does not assert that linked evidence exists or proves causality. The retained
// source audit must independently check those claims before corpus freeze.
func (c EvaluationCase) ValidateLabels(frozenAt time.Time) error {
	if c.MergedAt.IsZero() || c.MergedAt.After(frozenAt) {
		return errors.New("missing candidate merge time")
	}
	revisions := append(append([]EvaluationCaseLabel{}, c.LabelHistory...), c.Label)
	previousHash := ""
	previousAt := time.Time{}
	for _, label := range revisions {
		p := label.Provenance
		if err := p.validate(c.MergedAt, frozenAt); err != nil {
			return err
		}
		if err := validateLabelRevision(p, previousHash, previousAt); err != nil {
			return err
		}
		if strings.TrimSpace(label.Rationale) == "" {
			return errors.New("label requires rationale")
		}
		if label.Clean {
			if len(label.Survival) != 1 {
				return errors.New("clean label requires exactly one survival window")
			}
			w := label.Survival[0]
			if len(label.Defects) != 0 || w.Days < 1 || w.Days > 3650 || !w.From.Equal(c.MergedAt) || w.Through.Before(w.From.Add(time.Duration(w.Days)*24*time.Hour)) || w.Through.After(p.At) || !objectID.MatchString(w.HistoryRevision) || len(w.Searches) == 0 || strings.TrimSpace(w.Limitations) == "" {
				return errors.New("clean label requires a stated post-merge survival window, searched revision, searches and limitations")
			}
			history := false
			for _, e := range p.Evidence {
				if e.Kind == "history" && e.Commit == w.HistoryRevision && !e.At.Before(w.Through) {
					history = true
				}
			}
			if !history {
				return errors.New("clean window lacks linked history evidence")
			}
			for _, search := range w.Searches {
				if strings.TrimSpace(search) == "" {
					return errors.New("empty clean evidence search")
				}
			}
		} else {
			if len(label.Survival) != 0 || len(label.Defects) == 0 {
				return errors.New("defect case requires defects and no clean survival claim")
			}
			seen := map[string]bool{}
			for _, d := range label.Defects {
				if d.ID == "" || seen[d.ID] || strings.TrimSpace(d.Rationale) == "" {
					return errors.New("invalid/duplicate defect label")
				}
				seen[d.ID] = true
				if err := d.Provenance.validate(c.MergedAt, frozenAt); err != nil {
					return err
				}
				if d.Provenance.Labeler != p.Labeler || !d.Provenance.At.Equal(p.At) || d.Provenance.Supersedes != p.Supersedes || d.Provenance.OwnerOverride != p.OwnerOverride {
					return errors.New("defect must retain its containing label revision's actor, time and override provenance")
				}
				later := false
				for _, e := range d.Provenance.Evidence {
					if e.Kind != "fix" && e.Kind != "revert" && e.Kind != "bug" {
						continue
					}
					if !e.At.After(c.MergedAt) || len(e.Lines) == 0 || (e.Kind != "bug" && (e.Commit == c.Snapshot.Head || e.Commit == c.Snapshot.Base)) {
						return errors.New("defect evidence must be later than the candidate and identify affected lines")
					}
					for _, lines := range e.Lines {
						if lines.Revision != c.Snapshot.Head && lines.Revision != c.Snapshot.Base {
							return errors.New("defect lines must identify the candidate head or base")
						}
					}
					later = true
				}
				if !later {
					return errors.New("defect needs an independent later fix, revert or bug/regression issue")
				}
			}
		}
		previousHash, previousAt = Digest(label), p.At
	}
	return nil
}

func (l FindingLabel) validateAssessments(completedAt, assessedAt time.Time, known map[string]bool) error {
	revisions := append(append([]FindingAssessment{}, l.History...), l.Assessment)
	previousHash := ""
	previousAt := time.Time{}
	for _, a := range revisions {
		if !a.Provenance.At.After(completedAt) {
			return errors.New("finding assessment must be strictly after run completion")
		}
		if a.Actionable && len(a.DefectIDs) == 0 {
			return errors.New("actionable finding requires ground-truth match")
		}
		if !a.Actionable && len(a.DefectIDs) != 0 {
			return errors.New("non-actionable finding cannot claim recall credit")
		}
		seen := map[string]bool{}
		for _, id := range a.DefectIDs {
			if !known[id] || seen[id] {
				return errors.New("historical/current assessment has unknown or duplicate defect match")
			}
			seen[id] = true
		}
		if strings.TrimSpace(a.Rationale) == "" {
			return errors.New("actionability label requires rationale")
		}
		if err := a.Provenance.validate(completedAt, assessedAt); err != nil {
			return fmt.Errorf("finding label: %w", err)
		}
		if err := validateLabelRevision(a.Provenance, previousHash, previousAt); err != nil {
			return err
		}
		previousHash, previousAt = Digest(a), a.Provenance.At
	}
	return nil
}
