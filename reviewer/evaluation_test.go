package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These generated labels exercise the scoring arithmetic only. They are not
// human labels or evaluation evidence for any real pull request.
func syntheticCorpus() EvaluationCorpus {
	c := EvaluationCorpus{Version: 1, RepositoryID: RepositoryID, FrozenAt: controlTime, HumanID: OwnerID, LabelEvidence: "synthetic unit-test attestation only", Cases: []EvaluationCase{}}
	for i := 0; i < 20; i++ {
		snapshot := testSnapshot()
		snapshot.PR = i + 1
		test := EvaluationCase{ID: fmt.Sprintf("synthetic-%d", i), Snapshot: snapshot, Clean: i < 10, HumanID: OwnerID, Rationale: "synthetic unit-test label", Evidence: "synthetic evidence", Defects: []DefectLabel{}}
		if !test.Clean {
			test.Defects = append(test.Defects, DefectLabel{"d1", "synthetic defect", "synthetic source", true})
		}
		c.Cases = append(c.Cases, test)
	}
	return c
}
func syntheticPlan(c EvaluationCorpus) EvaluationPlan {
	return EvaluationPlan{Version: 1, CorpusHash: c.Hash(), TrustedRelease: strings.Repeat("d", 40), Policy: PolicyVersion, Config: DefaultConfig(), CreatedAt: c.FrozenAt.Add(time.Minute), Repeats: 1}
}
func syntheticResults(c EvaluationCorpus, p EvaluationPlan) []EvaluationResult {
	results := []EvaluationResult{}
	for _, test := range c.Cases {
		for _, arm := range EvaluationArms() {
			r := EvaluationResult{CaseID: test.ID, Arm: arm, CorpusHash: c.Hash(), PlanHash: p.Hash(), ReportHash: strings.Repeat("f", 64), Snapshot: test.Snapshot, StartedAt: p.CreatedAt.Add(time.Minute), State: "complete", Sessions: arm.Sessions(), Providers: arm.Schedule(), FindingIDs: []string{}, Labels: []FindingLabel{}, LabelledAt: p.CreatedAt.Add(time.Hour), Latency: time.Minute, Queue: time.Second}
			if !test.Clean {
				r.FindingIDs = []string{"f1"}
				r.Labels = []FindingLabel{{"f1", true, []string{"d1"}, OwnerID, "synthetic actionability only"}}
			}
			results = append(results, r)
		}
	}
	return results
}
func TestEvaluationDenominatorsAndNaturalBudgets(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	results := syntheticResults(c, p)
	score, err := ScoreEvaluation(c, p, results)
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range score.Arms {
		if arm.Precision.Numerator != 10 || arm.Precision.Denominator != 10 || arm.Recall.Numerator != 10 || arm.Recall.Denominator != 10 || arm.CleanSuccess.Numerator != 10 || arm.CleanSuccess.Denominator != 10 || arm.Completed != 20 || arm.UsageUnknown != 20 || arm.LatencyP95 != time.Minute || *arm.Precision.WilsonLow >= .9 {
			t.Fatal("bad arithmetic/uncertainty", arm)
		}
	}
	if ArmAdversarial.Sessions() != 4 || ArmIndependent.Sessions() != 2 || ArmCodex.Sessions() != 1 || ArmClaude.Sessions() != 1 || ArmCodexControl.Sessions() != 4 || ArmClaudeControl.Sessions() != 4 {
		t.Fatal("production budgets changed")
	}
	if p.Order(0, 0)[0] != ArmAdversarial || p.Order(1, 0)[0] != ArmIndependent {
		t.Fatal("deterministic ordering changed")
	}
	if !strings.HasPrefix(score.Rollout, "stop:") {
		t.Fatal("synthetic score authorized rollout")
	}
}
func TestEvaluationFailuresStayInRecallAndCleanDenominators(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	results := syntheticResults(c, p)
	for i := range results {
		r := &results[i]
		if r.Arm == ArmAdversarial && (r.CaseID == "synthetic-0" || r.CaseID == "synthetic-10") {
			r.State = "quota_wait"
			r.FindingIDs = []string{}
			r.Labels = []FindingLabel{}
			r.Sessions = 0
			r.Providers = []Provider{}
		}
	}
	score, err := ScoreEvaluation(c, p, results)
	if err != nil {
		t.Fatal(err)
	}
	arm := score.Arms[0]
	if arm.Recall.Numerator != 9 || arm.Recall.Denominator != 10 || arm.CleanSuccess.Numerator != 9 || arm.CleanSuccess.Denominator != 10 || arm.QuotaWaits != 2 || arm.Failures != 2 {
		t.Fatal("failed cases excluded", arm)
	}
	score, err = ScoreEvaluation(c, p, nil)
	if err != nil || score.Arms[0].Missing != 20 || score.Arms[0].Precision.Rate != nil || score.Arms[0].Recall.Numerator != 0 {
		t.Fatal("missing runs looked clean", score, err)
	}
}
func TestEvaluationRejectsUnlabeledAndChangedEvidence(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	results := syntheticResults(c, p)
	tests := []func(*EvaluationCorpus, *EvaluationPlan, []EvaluationResult){
		func(c *EvaluationCorpus, p *EvaluationPlan, r []EvaluationResult) { c.HumanID = 0 },
		func(c *EvaluationCorpus, p *EvaluationPlan, r []EvaluationResult) { p.CorpusHash = Digest("changed") },
		func(c *EvaluationCorpus, p *EvaluationPlan, r []EvaluationResult) { r[0].StartedAt = p.CreatedAt },
		func(c *EvaluationCorpus, p *EvaluationPlan, r []EvaluationResult) { r[0].Providers[0] = Claude },
		func(c *EvaluationCorpus, p *EvaluationPlan, r []EvaluationResult) { r[60].Labels[0].HumanID = 44 },
		func(c *EvaluationCorpus, p *EvaluationPlan, r []EvaluationResult) {
			r[60].Labels[0].DefectIDs = []string{"d1", "d1"}
		},
	}
	for i, mutate := range tests {
		raw, _ := json.Marshal(results)
		var copy []EvaluationResult
		json.Unmarshal(raw, &copy)
		cc := c
		pp := p
		mutate(&cc, &pp, copy)
		if _, err := ScoreEvaluation(cc, pp, copy); err == nil {
			t.Fatal("accepted changed evidence", i)
		}
	}
}
func TestEvaluationArtifactsDecodeStrictTimestamps(t *testing.T) {
	c := syntheticCorpus()
	raw, _ := json.Marshal(c)
	decoded, err := DecodeStrict[EvaluationCorpus](raw, 1<<20)
	if err != nil || decoded.Hash() != c.Hash() {
		t.Fatal("corpus CLI timestamp decoding", err)
	}
	p := syntheticPlan(c)
	runs := syntheticResults(c, p)
	raw, _ = json.Marshal(runs)
	if _, err = DecodeStrict[[]EvaluationResult](raw, 1<<20); err != nil {
		t.Fatal(err)
	}
	raw = []byte(`{"at":null}`)
	if _, err = DecodeStrict[struct {
		At time.Time `json:"at"`
	}](raw, 1024); err == nil {
		t.Fatal("null timestamp accepted")
	}
}

type controlFunc func(context.Context, ControlSession) (ControlReport, error)

func (f controlFunc) Critique(ctx context.Context, s ControlSession) (ControlReport, error) {
	return f(ctx, s)
}
func fakeControl(s ControlSession) ControlReport {
	r := ControlReport{InputHash: Digest(s), JobHash: Digest(s.Job), Step: s.Step, Findings: []Finding{}, Judgments: []Judgment{}, Retractions: []string{}, Coverage: []Coverage{}}
	if s.Step == 1 {
		r.Findings = []Finding{testFinding(s.Provider)}
	} else {
		for _, previous := range s.History {
			for _, f := range previous.Findings {
				r.Judgments = append(r.Judgments, Judgment{f.ID, "confirm", "synthetic self-check", []Anchor{f.Anchor}})
			}
		}
	}
	return r
}
func TestEqualSessionSelfCritiqueControl(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		job := testJob()
		if provider == Codex {
			job.Config.Mode = CodexOnly
		} else {
			job.Config.Mode = ClaudeOnly
		}
		engine := testEngine(t, nil)
		calls := 0
		out, err := RunSelfCritique(context.Background(), job, provider, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
			calls++
			if s.Step != calls || len(s.History) != calls-1 || s.Provider != provider {
				t.Fatal("control changed lineage/provider")
			}
			return fakeControl(s), nil
		}), engine.Reader)
		if err != nil || calls != 4 || out.ConsumedSessions != 4 || out.State != "complete" || len(out.Ledger) != 1 || out.Ledger[0].Status != "unverified" || len(out.Ledger[0].History) != 3 {
			t.Fatal("four-session control", out, err)
		}
	}
}
func TestSelfCritiqueCannotReturnPartialOrForgeSupport(t *testing.T) {
	job := testJob()
	job.Config.Mode = CodexOnly
	engine := testEngine(t, nil)
	out, err := RunSelfCritique(context.Background(), job, Codex, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
		if s.Step == 3 {
			return ControlReport{}, errors.New("provider failure")
		}
		return fakeControl(s), nil
	}), engine.Reader)
	if err == nil || out.State == "complete" || len(out.Ledger) != 0 || out.ConsumedSessions != 3 || len(out.Reports) != 2 {
		t.Fatal("partial control looked complete", out, err)
	}
	_, err = RunSelfCritique(context.Background(), job, Codex, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
		r := fakeControl(s)
		if s.Step == 2 {
			r.Judgments = nil
		}
		return r, nil
	}), engine.Reader)
	if err == nil {
		t.Fatal("unexamined self-finding accepted")
	}
}
