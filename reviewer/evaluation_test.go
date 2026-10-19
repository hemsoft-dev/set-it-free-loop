package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		test := EvaluationCase{PacketHash: Digest("synthetic immutable evidence"), SerializedPacketBytes: 30, ID: fmt.Sprintf("synthetic-%d", i), Snapshot: snapshot, Clean: i < 10, HumanID: OwnerID, Rationale: "synthetic unit-test label", Evidence: "synthetic evidence", Defects: []DefectLabel{}}
		if !test.Clean {
			test.Defects = append(test.Defects, DefectLabel{"d1", "synthetic defect", "synthetic source", true})
		}
		c.Cases = append(c.Cases, test)
	}
	return c
}
func syntheticPlan(c EvaluationCorpus) EvaluationPlan {
	return EvaluationPlan{Version: 1, CorpusHash: c.Hash(), TrustedRelease: strings.Repeat("d", 40), Policy: PolicyVersion, Config: func() Config { cfg := DefaultConfig(); cfg.Limits.OutputBytes = EvaluationOutputBytes; return cfg }(), CreatedAt: c.FrozenAt.Add(time.Minute), Repeats: 1}
}
func syntheticResults(c EvaluationCorpus, p EvaluationPlan) []EvaluationResult {
	results := []EvaluationResult{}
	for caseIndex, test := range c.Cases {
		for position, arm := range p.Order(caseIndex, 0) {
			started := p.CreatedAt.Add(time.Duration(1+caseIndex*6+position) * 2 * time.Minute)
			r := EvaluationResult{FindingStatuses: map[string]string{}, PacketHash: test.PacketHash, OutputBytesLimit: p.Config.Limits.OutputBytes, CaseID: test.ID, Arm: arm, CorpusHash: c.Hash(), PlanHash: p.Hash(), ReportHash: strings.Repeat("f", 64), Snapshot: test.Snapshot, StartedAt: started, State: "complete", Sessions: arm.Sessions(), Providers: arm.Schedule(), FindingIDs: []string{}, Labels: []FindingLabel{}, LabelledAt: started.Add(time.Hour), Latency: time.Minute, Queue: time.Second}
			if !test.Clean {
				r.FindingIDs = []string{"f1"}
				r.FindingStatuses["f1"] = "unverified"
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
			r.FindingStatuses = map[string]string{}
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
		job.Config.Limits.OutputBytes = EvaluationOutputBytes
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
	job.Config.Limits.OutputBytes = EvaluationOutputBytes
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

func TestScoreCommandReadsRetainedFiles(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	runs := syntheticResults(c, p)
	dir := t.TempDir()
	args := []string{"run", "./cmd/evalscore"}
	for _, item := range []struct {
		name  string
		value any
	}{{"corpus.json", c}, {"plan.json", p}, {"results.json", runs}} {
		raw, err := json.Marshal(item.value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, item.name)
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
	}
	command := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), args...)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("offline CLI failed", err, string(raw))
	}
	var score EvaluationScore
	if err = json.Unmarshal(raw, &score); err != nil {
		t.Fatal(err)
	}
	if len(score.Arms) != 6 || len(score.Comparisons) != 5 || !score.Arms[0].ProvisionalGoalsMet || score.Comparisons[0].PrecisionDelta == nil || *score.Comparisons[0].PrecisionDelta != 0 {
		t.Fatal("wrong CLI score", score)
	}
}

func TestFrozenCorpusRejectsNoMaterialAndDuplicateSnapshots(t *testing.T) {
	c := syntheticCorpus()
	for i := range c.Cases {
		for j := range c.Cases[i].Defects {
			c.Cases[i].Defects[j].Material = false
		}
	}
	if err := c.Validate(); err == nil {
		t.Fatal("recall-free corpus accepted")
	}
	c = syntheticCorpus()
	c.Cases[1].Snapshot = c.Cases[0].Snapshot
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate snapshot overweighted corpus")
	}
}
func TestEvaluationRejectsCrossArmReorderingAndPartialCounters(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	runs := syntheticResults(c, p)
	runs[0].StartedAt, runs[1].StartedAt = runs[1].StartedAt, runs[0].StartedAt
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("frozen order not enforced")
	}
	runs = syntheticResults(c, p)
	runs[0].State = "quota_wait"
	runs[0].Sessions = 0
	runs[0].Providers = []Provider{}
	runs[0].Corroborated = 1
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("failed run fabricated ledger counters")
	}
	runs = syntheticResults(c, p)
	for i := range runs {
		if runs[i].Arm == ArmCodexControl {
			runs[i].Corroborated = 1
			break
		}
	}
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("self-confirmation became peer support")
	}
}
func TestCommonFrozenOutputCapAcrossAllArms(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	p.Config.Limits.OutputBytes = DefaultLimits().OutputBytes
	if err := p.Validate(c); err == nil {
		t.Fatal("unequal effective output limits permitted")
	}
	p = syntheticPlan(c)
	for _, mode := range []Mode{Adversarial, IndependentPair, CodexOnly, ClaudeOnly} {
		job := testJob()
		job.Config = p.Config
		job.Config.Mode = mode
		job.Packet = strings.Repeat("x", EvaluationPacketBytes-2)
		limits, err := protocolLimits(job)
		if err != nil || limits.OutputBytes != p.Config.Limits.OutputBytes {
			t.Fatal("production arm cap changed", mode, err, limits)
		}
	}
	job := testJob()
	job.Config = p.Config
	job.Config.Mode = CodexOnly
	engine := testEngine(t, nil)
	_, err := RunSelfCritique(context.Background(), job, Codex, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
		if s.Limits.OutputBytes != p.Config.Limits.OutputBytes {
			t.Fatal("self-control cap differs")
		}
		return fakeControl(s), nil
	}), engine.Reader)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RunSelfCritique(context.Background(), job, Codex, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
		r := fakeControl(s)
		r.InputHash = "forged"
		return r, nil
	}), engine.Reader)
	if err == nil || out.State != "invalid_output" {
		t.Fatal("invalid control output misclassified", out, err)
	}
}

func TestFinalSelfCritiqueCannotIntroduceUnexaminedAllegations(t *testing.T) {
	job := testJob()
	job.Config.Mode = CodexOnly
	job.Config.Limits.OutputBytes = EvaluationOutputBytes
	engine := testEngine(t, nil)
	out, err := RunSelfCritique(context.Background(), job, Codex, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
		r := fakeControl(s)
		if s.Step == 4 {
			finding := testFinding(Codex)
			finding.ID = "codex-late"
			r.Findings = []Finding{finding}
		}
		return r, nil
	}), engine.Reader)
	if err == nil || out.State != "invalid_output" || len(out.Ledger) != 0 || out.ConsumedSessions != 4 {
		t.Fatal("late unexamined control finding accepted", out, err)
	}
}
func TestEvaluationCountersFollowLedgerAndRetainTurnUsage(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	runs := syntheticResults(c, p)
	runs[0].Corroborated = 1
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("empty ledger fabricated corroboration")
	}
	runs = syntheticResults(c, p)
	for i := range runs {
		runs[i].TurnsKnown = true
		runs[i].Turns = runs[i].Sessions * 2
	}
	score, err := ScoreEvaluation(c, p, runs)
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range score.Arms {
		if arm.Turns != 20*arm.Arm.Sessions()*2 || arm.TurnsUnknown != 0 {
			t.Fatal("turn totals lost", arm)
		}
	}
	// Withdrawn IDs remain auditable but do not inflate active precision counts.
	runs = syntheticResults(c, p)
	for i := range runs {
		if runs[i].CaseID == "synthetic-10" && runs[i].Arm == ArmAdversarial {
			runs[i].FindingStatuses["f1"] = "retracted"
			runs[i].Concessions = 1
			runs[i].Labels[0].Actionable = false
			runs[i].Labels[0].DefectIDs = []string{}
		}
	}
	score, err = ScoreEvaluation(c, p, runs)
	if err != nil || score.Arms[0].Precision.Denominator != 9 || score.Arms[0].Concessions != 1 || score.Arms[0].TurnsUnknown != 20 {
		t.Fatal("withdrawals or unknown turn usage scored incorrectly", score, err)
	}
}

func TestR1OnlyArmsCannotFabricateCritiquesOrWithdrawals(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	for _, arm := range []EvaluationArm{ArmIndependent, ArmCodex, ArmClaude} {
		for _, status := range []string{"disputed", "retracted"} {
			runs := syntheticResults(c, p)
			for i := range runs {
				if runs[i].CaseID == "synthetic-10" && runs[i].Arm == arm {
					runs[i].FindingStatuses["f1"] = status
					if status == "disputed" {
						runs[i].Disputed = 1
					} else {
						runs[i].Concessions = 1
						runs[i].Labels[0].Actionable = false
						runs[i].Labels[0].DefectIDs = []string{}
					}
				}
			}
			if _, err := ScoreEvaluation(c, p, runs); err == nil {
				t.Fatal("R1-only arm fabricated critique", arm, status)
			}
		}
	}
}
