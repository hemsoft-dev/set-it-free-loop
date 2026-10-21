package reviewer

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

const EvaluationPacketBytes = 64 << 10
const EvaluationOutputBytes = 32 << 10

// Version 2 replaces the owner-only label format. The corpus and plan hashes
// still bind the complete serialized artifacts, including all label revisions.
type EvaluationCorpus struct {
	Version       int
	RepositoryID  int64
	FrozenAt      time.Time
	LabelEvidence string
	Cases         []EvaluationCase
}

func (c EvaluationCorpus) Hash() string { return Digest(c) }
func (c EvaluationCorpus) Validate() error {
	if c.Version != 2 || c.RepositoryID != RepositoryID || c.FrozenAt.IsZero() || strings.TrimSpace(c.LabelEvidence) == "" || len(c.Cases) < 20 || len(c.Cases) > 100 {
		return errors.New("requires an evidence-labeled frozen v2 corpus of 20 to 100 cases")
	}
	clean, material := 0, 0
	seen, snapshots := map[string]bool{}, map[string]bool{}
	for _, test := range c.Cases {
		if test.ID == "" || seen[test.ID] {
			return errors.New("missing/duplicate case")
		}
		seen[test.ID] = true
		if snapshots[Digest(test.Snapshot)] || !digestPattern.MatchString(test.PacketHash) || test.SerializedPacketBytes <= 0 || test.SerializedPacketBytes > EvaluationPacketBytes {
			return errors.New("duplicate snapshot or invalid frozen packet metadata")
		}
		snapshots[Digest(test.Snapshot)] = true
		if err := test.Snapshot.Validate(); err != nil {
			return err
		}
		if err := test.ValidateLabels(c.FrozenAt); err != nil {
			return fmt.Errorf("case %s: %w", test.ID, err)
		}
		if test.Label.Clean {
			clean++
		}
		for _, d := range test.Label.Defects {
			if d.Material {
				material++
			}
		}
	}
	if material == 0 {
		return errors.New("requires at least one material-defect case")
	}
	if clean < 10 {
		return errors.New("requires at least ten evidence-labeled clean controls")
	}
	return nil
}

type EvaluationArm string

const (
	ArmAdversarial   EvaluationArm = "adversarial"
	ArmIndependent   EvaluationArm = "independent-pair"
	ArmCodex         EvaluationArm = "codex-only"
	ArmClaude        EvaluationArm = "claude-only"
	ArmCodexControl  EvaluationArm = "codex-self-critique"
	ArmClaudeControl EvaluationArm = "claude-self-critique"
)

func EvaluationArms() []EvaluationArm {
	return []EvaluationArm{ArmAdversarial, ArmIndependent, ArmCodex, ArmClaude, ArmCodexControl, ArmClaudeControl}
}
func (a EvaluationArm) Sessions() int {
	switch a {
	case ArmAdversarial, ArmCodexControl, ArmClaudeControl:
		return 4
	case ArmIndependent:
		return 2
	case ArmCodex, ArmClaude:
		return 1
	}
	return 0
}
func (a EvaluationArm) Schedule() []Provider {
	switch a {
	case ArmAdversarial:
		return []Provider{Codex, Claude, Codex, Claude}
	case ArmIndependent:
		return []Provider{Codex, Claude}
	case ArmCodex:
		return []Provider{Codex}
	case ArmClaude:
		return []Provider{Claude}
	case ArmCodexControl:
		return []Provider{Codex, Codex, Codex, Codex}
	case ArmClaudeControl:
		return []Provider{Claude, Claude, Claude, Claude}
	}
	return nil
}

type EvaluationPlan struct {
	Version                            int
	CorpusHash, TrustedRelease, Policy string
	Config                             Config
	CreatedAt                          time.Time
	Repeats                            int
}

func (p EvaluationPlan) Hash() string { return Digest(p) }
func (p EvaluationPlan) Validate(c EvaluationCorpus) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if p.Version != 1 || p.CorpusHash != c.Hash() || !objectID.MatchString(p.TrustedRelease) || p.Policy != PolicyVersion || !p.CreatedAt.After(c.FrozenAt) || p.Repeats < 1 || p.Repeats > 3 {
		return errors.New("invalid frozen evaluation plan")
	}
	if err := p.Config.Validate(); err != nil {
		return err
	}
	if p.Config.Limits.InputBytes != DefaultLimits().InputBytes || p.Config.Limits.OutputBytes > EvaluationOutputBytes {
		return errors.New("freeze a common output cap no larger than 32 KiB with 256 KiB input for every arm")
	}
	if p.Config.Mode != Adversarial || p.Config.PeerRounds != 1 {
		return errors.New("evaluation uses default four-session dual protocol")
	}
	return nil
}

// Order rotates all six arms by case index and repeat, before collecting results.
// Production modes keep natural budgets. Single-provider controls each use four
// sessions: an initial pass followed by three bounded self-critiques.
func (p EvaluationPlan) Order(caseIndex, repeat int) []EvaluationArm {
	arms := EvaluationArms()
	offset := (caseIndex + repeat) % len(arms)
	return append(append([]EvaluationArm{}, arms[offset:]...), arms[:offset]...)
}

type FindingAssessment struct {
	Actionable bool
	DefectIDs  []string
	Rationale  string
	Provenance LabelProvenance
}
type FindingLabel struct {
	FindingID  string
	Assessment FindingAssessment
	History    []FindingAssessment
}
type EvaluationResult struct {
	FindingStatuses                     map[string]string
	Turns                               int
	TurnsKnown                          bool
	PacketHash                          string
	OutputBytesLimit                    int
	CaseID                              string
	Arm                                 EvaluationArm
	Repeat                              int
	CorpusHash, PlanHash, ReportHash    string
	Snapshot                            Snapshot
	StartedAt                           time.Time
	State                               string
	Sessions                            int
	Providers                           []Provider
	FindingIDs                          []string
	Labels                              []FindingLabel
	LabelledAt                          time.Time
	InputTokens, OutputTokens           int64
	UsageKnown                          bool
	Latency, Queue                      time.Duration
	Corroborated, Disputed, Concessions int
}
type Fraction struct {
	Numerator, Denominator int
	Rate                   *float64
	WilsonLow, WilsonHigh  *float64
}

func fraction(n, d int) Fraction {
	f := Fraction{Numerator: n, Denominator: d}
	if d == 0 {
		return f
	}
	rate := float64(n) / float64(d)
	f.Rate = &rate
	z := 1.959963984540054
	den := 1 + z*z/float64(d)
	center := (rate + z*z/(2*float64(d))) / den
	half := z * math.Sqrt(rate*(1-rate)/float64(d)+z*z/(4*float64(d*d))) / den
	lo, hi := math.Max(0, center-half), math.Min(1, center+half)
	f.WilsonLow = &lo
	f.WilsonHigh = &hi
	return f
}

type ArmScore struct {
	Turns, TurnsUnknown                                    int
	ProvisionalGoalsMet                                    bool
	Arm                                                    EvaluationArm
	Precision, Recall, CleanSuccess                        Fraction
	Completed, Failures, QuotaWaits, UsageUnknown, Missing int
	InputTokens, OutputTokens                              int64
	Corroborated, Disputed, Concessions                    int
	LatencyP50, LatencyP95, QueueP50, QueueP95             time.Duration
}
type ArmComparison struct {
	Against                                 EvaluationArm
	PrecisionDelta, RecallDelta, CleanDelta *float64
	Interpretation                          string
}

type EvaluationScore struct {
	Comparisons          []ArmComparison
	CorpusHash, PlanHash string
	Arms                 []ArmScore
	Rollout              string
	Caveat               string
}

func quantile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	copy := append([]time.Duration{}, values...)
	sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
	i := int(math.Ceil(p*float64(len(copy)))) - 1
	if i < 0 {
		i = 0
	}
	return copy[i]
}

// ScoreEvaluation is offline. Ground truth and actionability must be independently
// supplied by the named owner. Model outputs cannot populate or mutate labels.
// Failures remain in all-case recall and clean-control denominators.
func ScoreEvaluation(c EvaluationCorpus, p EvaluationPlan, results []EvaluationResult) (EvaluationScore, error) {
	if err := p.Validate(c); err != nil {
		return EvaluationScore{}, err
	}
	score := EvaluationScore{CorpusHash: c.Hash(), PlanHash: p.Hash(), Rollout: "stop: pilot and human comparative decision required", Caveat: "Wilson intervals describe binomial proportions only; repeated/correlated cases are not independent. No small-sample result proves general quality. Latency quantiles cover completed runs; failures, quota waits and missing runs are separate."}
	cases := map[string]EvaluationCase{}
	material, clean := 0, 0
	for _, test := range c.Cases {
		cases[test.ID] = test
		if test.Label.Clean {
			clean++
		}
		for _, d := range test.Label.Defects {
			if d.Material {
				material++
			}
		}
	}
	byArm := map[EvaluationArm][]EvaluationResult{}
	seen := map[string]bool{}
	for _, run := range results {
		test, ok := cases[run.CaseID]
		key := fmt.Sprintf("%s/%s/%d", run.CaseID, run.Arm, run.Repeat)
		if !ok || run.Arm.Sessions() == 0 || seen[key] || run.Repeat < 0 || run.Repeat >= p.Repeats || run.CorpusHash != c.Hash() || run.PlanHash != p.Hash() || run.Snapshot != test.Snapshot || run.PacketHash != test.PacketHash || run.OutputBytesLimit != p.Config.Limits.OutputBytes || !run.StartedAt.After(p.CreatedAt) || run.Latency < 0 || run.Queue < 0 || run.InputTokens < 0 || run.OutputTokens < 0 || run.Corroborated < 0 || run.Disputed < 0 || run.Concessions < 0 || run.Turns < 0 || (run.TurnsKnown && run.Turns > p.Config.Limits.MaxTurns*run.Sessions) {
			return score, errors.New("invalid/duplicate run or changed evaluation provenance")
		}
		seen[key] = true
		if run.Sessions < 0 || run.Sessions > run.Arm.Sessions() || len(run.Providers) != run.Sessions {
			return score, errors.New("invalid session accounting")
		}
		schedule := run.Arm.Schedule()
		for i, provider := range run.Providers {
			if provider != schedule[i] {
				return score, errors.New("arm changed provider/order")
			}
		}
		switch run.State {
		case "complete":
			if run.Sessions != run.Arm.Sessions() || !digestPattern.MatchString(run.ReportHash) || !run.LabelledAt.After(run.StartedAt.Add(run.Latency)) {
				return score, errors.New("incomplete run or missing label assessment time")
			}
		case "quota_wait", "auth_failed", "provider_unavailable", "invalid_output", "cancelled", "incomplete_expired":
			if len(run.FindingIDs) != 0 || len(run.Labels) != 0 || run.Corroborated != 0 || run.Disputed != 0 || run.Concessions != 0 {
				return score, errors.New("failed run exposed partial findings")
			}
		default:
			return score, errors.New("unknown evaluation outcome")
		}
		if run.Arm != ArmAdversarial && run.Corroborated != 0 {
			return score, errors.New("arm cannot produce peer corroboration")
		}
		findingPhases := run.Arm.Sessions()
		if run.Arm == ArmCodexControl || run.Arm == ArmClaudeControl {
			findingPhases--
		}
		if len(run.FindingIDs) > p.Config.Limits.MaxFindings*findingPhases {
			return score, errors.New("result exceeds frozen finding budget")
		}
		ids := map[string]bool{}
		for _, id := range run.FindingIDs {
			if id == "" || ids[id] {
				return score, errors.New("duplicate finding ID")
			}
			ids[id] = true
		}
		if len(run.FindingStatuses) != len(ids) {
			return score, errors.New("every retained finding needs one ledger status")
		}
		corroborated, disputed, retracted := 0, 0, 0
		for id, status := range run.FindingStatuses {
			if !ids[id] {
				return score, errors.New("status refers to unknown finding")
			}
			if (run.Arm == ArmIndependent || run.Arm == ArmCodex || run.Arm == ArmClaude) && status != "unverified" {
				return score, errors.New("R1-only arm cannot critique or withdraw findings")
			}
			switch status {
			case "corroborated":
				corroborated++
			case "disputed":
				disputed++
			case "retracted":
				retracted++
			case "unverified":
			default:
				return score, errors.New("invalid retained finding status")
			}
		}
		if run.Corroborated != corroborated || run.Disputed != disputed || run.Concessions != retracted {
			return score, errors.New("ledger counters differ from retained finding statuses")
		}
		if len(run.Labels) != len(ids) {
			return score, errors.New("every reported finding requires evidence-backed actionability judgment")
		}
		labels := map[string]bool{}
		matches := map[string]bool{}
		known := map[string]bool{}
		for _, d := range test.Label.Defects {
			known[d.ID] = true
		}
		for _, label := range run.Labels {
			if !ids[label.FindingID] || labels[label.FindingID] || strings.TrimSpace(label.Assessment.Rationale) == "" {
				return score, errors.New("invalid actionability label")
			}
			if err := label.validateAssessments(run.StartedAt.Add(run.Latency), run.LabelledAt, known); err != nil {
				return score, err
			}
			labels[label.FindingID] = true
			if run.FindingStatuses[label.FindingID] == "retracted" && label.Assessment.Actionable {
				return score, errors.New("withdrawn allegation cannot count as an actionable prediction")
			}
			for _, id := range label.Assessment.DefectIDs {
				if !known[id] || matches[id] {
					return score, errors.New("unknown or duplicate defect match")
				}
				matches[id] = true
			}
		}
		byArm[run.Arm] = append(byArm[run.Arm], run)
	}
	// Validate actual chronology against each frozen case/repeat rotation.
	for caseIndex, test := range c.Cases {
		for repeat := 0; repeat < p.Repeats; repeat++ {
			ranks := map[EvaluationArm]int{}
			for i, arm := range p.Order(caseIndex, repeat) {
				ranks[arm] = i
			}
			group := []EvaluationResult{}
			for _, run := range results {
				if run.CaseID == test.ID && run.Repeat == repeat {
					group = append(group, run)
				}
			}
			sort.Slice(group, func(i, j int) bool { return group[i].StartedAt.Before(group[j].StartedAt) })
			for i := 1; i < len(group); i++ {
				if !group[i].StartedAt.After(group[i-1].StartedAt) || ranks[group[i].Arm] <= ranks[group[i-1].Arm] {
					return score, errors.New("cross-arm execution order differs from frozen rotation")
				}
			}
		}
	}
	for _, arm := range EvaluationArms() {
		a := ArmScore{Arm: arm}
		actionable, predicted, recalled, cleanOK := 0, 0, 0, 0
		var latencies, queues []time.Duration
		runs := byArm[arm]
		a.Missing = len(c.Cases)*p.Repeats - len(runs)
		for _, run := range runs {
			if run.State != "complete" {
				a.Failures++
				if run.State == "quota_wait" {
					a.QuotaWaits++
				}
			} else {
				a.Completed++
				for _, status := range run.FindingStatuses {
					if status != "retracted" {
						predicted++
					}
				}
				test := cases[run.CaseID]
				if test.Label.Clean && len(run.FindingIDs) == 0 {
					cleanOK++
				}
				materialIDs := map[string]bool{}
				for _, d := range test.Label.Defects {
					if d.Material {
						materialIDs[d.ID] = true
					}
				}
				for _, label := range run.Labels {
					if label.Assessment.Actionable {
						actionable++
					}
					for _, id := range label.Assessment.DefectIDs {
						if materialIDs[id] {
							recalled++
						}
					}
				}
				latencies = append(latencies, run.Latency)
				queues = append(queues, run.Queue)
			}
			if !run.TurnsKnown {
				a.TurnsUnknown++
			} else {
				a.Turns += run.Turns
			}
			if !run.UsageKnown {
				a.UsageUnknown++
			} else {
				a.InputTokens += run.InputTokens
				a.OutputTokens += run.OutputTokens
			}
			a.Corroborated += run.Corroborated
			a.Disputed += run.Disputed
			a.Concessions += run.Concessions
		}
		a.Precision = fraction(actionable, predicted)
		a.Recall = fraction(recalled, material*p.Repeats)
		a.CleanSuccess = fraction(cleanOK, clean*p.Repeats)
		a.LatencyP50 = quantile(latencies, .5)
		a.LatencyP95 = quantile(latencies, .95)
		a.QueueP50 = quantile(queues, .5)
		a.QueueP95 = quantile(queues, .95)
		a.ProvisionalGoalsMet = a.Missing == 0 && a.Failures == 0 && a.Precision.Rate != nil && a.Recall.Rate != nil && a.CleanSuccess.Rate != nil && *a.Precision.Rate >= .9 && *a.Recall.Rate >= .7 && *a.CleanSuccess.Rate >= .9
		score.Arms = append(score.Arms, a)
	}
	dual := score.Arms[0]
	delta := func(a, b *float64) *float64 {
		if a == nil || b == nil {
			return nil
		}
		d := *a - *b
		return &d
	}
	for _, other := range score.Arms[1:] {
		score.Comparisons = append(score.Comparisons, ArmComparison{Against: other.Arm, PrecisionDelta: delta(dual.Precision.Rate, other.Precision.Rate), RecallDelta: delta(dual.Recall.Rate, other.Recall.Rate), CleanDelta: delta(dual.CleanSuccess.Rate, other.CleanSuccess.Rate), Interpretation: "Descriptive differences only. Paired human analysis and uncertainty are required; these differences do not establish superiority."})
	}
	return score, nil
}
