package reviewer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func copyEvaluationCase(t *testing.T, c EvaluationCase) EvaluationCase {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out EvaluationCase
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestAgentCorpusLabelsRequireIndependentTimedEvidence(t *testing.T) {
	c := syntheticCorpus()
	tests := []struct {
		name   string
		index  int
		mutate func(*EvaluationCase)
	}{
		{"missing-model", 0, func(c *EvaluationCase) { c.Label.Provenance.Labeler.Model = "" }},
		{"missing-session", 0, func(c *EvaluationCase) { c.Label.Provenance.Labeler.Session = "" }},
		{"invented-human", 0, func(c *EvaluationCase) { c.Label.Provenance.Labeler.HumanID = OwnerID }},
		{"forged-override", 0, func(c *EvaluationCase) { c.Label.Provenance.OwnerOverride = true }},
		{"before-window", 0, func(c *EvaluationCase) { c.Label.Provenance.At = c.MergedAt }},
		{"short-window", 0, func(c *EvaluationCase) { c.Label.Survival[0].Through = c.MergedAt.Add(24 * time.Hour) }},
		{"no-search", 0, func(c *EvaluationCase) { c.Label.Survival[0].Searches = nil }},
		{"no-history", 0, func(c *EvaluationCase) { c.Label.Survival[0].HistoryRevision = strings.Repeat("f", 40) }},
		{"review-url", 0, func(c *EvaluationCase) {
			c.Label.Provenance.Evidence[0].URL = "https://github.com/hemsoft-dev/set-it-free-loop/pull/1#pullrequestreview-123"
		}},
		{"comment-url", 0, func(c *EvaluationCase) {
			c.Label.Provenance.Evidence[0].URL = "https://github.com/hemsoft-dev/set-it-free-loop/issues/1#issuecomment-123"
		}},
		{"review-kind", 0, func(c *EvaluationCase) { c.Label.Provenance.Evidence[0].Kind = "codex-review" }},
		{"wrong-revision", 0, func(c *EvaluationCase) { c.Label.Provenance.Evidence[0].Commit = strings.Repeat("e", 40) }},
		{"not-later", 10, func(c *EvaluationCase) { c.Label.Defects[0].Provenance.Evidence[0].At = c.MergedAt }},
		{"no-affected-lines", 10, func(c *EvaluationCase) { c.Label.Defects[0].Provenance.Evidence[0].Lines = nil }},
		{"other-pr-lines", 10, func(c *EvaluationCase) {
			c.Label.Defects[0].Provenance.Evidence[0].Lines[0].Revision = strings.Repeat("e", 40)
		}},
		{"source-is-not-fix", 10, func(c *EvaluationCase) { c.Label.Defects[0].Provenance.Evidence[0].Kind = "source" }},
		{"defect-actor", 10, func(c *EvaluationCase) { c.Label.Defects[0].Provenance.Labeler.Session = "other-session" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := copyEvaluationCase(t, c.Cases[tt.index])
			tt.mutate(&test)
			if err := test.ValidateLabels(c.FrozenAt); err == nil {
				t.Fatal("accepted invalid agent label")
			}
		})
	}
	if err := c.Validate(); err != nil {
		t.Fatal("valid agent labels rejected", err)
	}
}
func TestAgentRevisionsAndOwnerOverridesPreserveHistory(t *testing.T) {
	c := syntheticCorpus()
	test := copyEvaluationCase(t, c.Cases[0])
	original := test.Label
	test.LabelHistory = append(test.LabelHistory, original)
	test.Label.Provenance.At = test.Label.Provenance.At.Add(time.Minute)
	test.Label.Provenance.Supersedes = Digest(original)
	test.Label.Rationale = "agent revised its independent evidence interpretation"
	if err := test.ValidateLabels(c.FrozenAt); err != nil {
		t.Fatal(err)
	}
	agentRevision := test.Label
	test.LabelHistory = append(test.LabelHistory, agentRevision)
	test.Label.Provenance.At = test.Label.Provenance.At.Add(time.Minute)
	test.Label.Provenance.Supersedes = Digest(agentRevision)
	test.Label.Provenance.Labeler = Labeler{Kind: "owner", HumanID: OwnerID, Agent: "Franz", Provider: "human", Model: "none", Session: "manual-override-1"}
	test.Label.Provenance.OwnerOverride = true
	test.Label.Rationale = "Franz override with retained evidence"
	if err := test.ValidateLabels(c.FrozenAt); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.Cases = append([]EvaluationCase{}, c.Cases...)
	changed.Cases[0] = test
	if changed.Hash() == c.Hash() {
		t.Fatal("label history did not change corpus hash")
	}
	if err := syntheticPlan(c).Validate(changed); err == nil {
		t.Fatal("old plan accepted revised corpus")
	}
	for _, mutate := range []func(*EvaluationCase){
		func(c *EvaluationCase) { c.Label.Provenance.OwnerOverride = false },
		func(c *EvaluationCase) { c.LabelHistory[0].Rationale = "rewritten history" },
		func(c *EvaluationCase) { c.LabelHistory = nil },
	} {
		bad := copyEvaluationCase(t, test)
		mutate(&bad)
		if err := bad.ValidateLabels(c.FrozenAt); err == nil {
			t.Fatal("accepted erased or tampered override history")
		}
	}
}
func TestAgentFindingAssessmentNeedsIndependentPostRunEvidence(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	runs := syntheticResults(c, p)
	runs[60].Labels[0].Assessment.Provenance.At = runs[60].StartedAt
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("pre-run assessment accepted")
	}
	runs = syntheticResults(c, p)
	runs[60].Labels[0].Assessment.Provenance.Evidence[0].URL = "https://github.com/hemsoft-dev/set-it-free-loop/pull/11#discussion_r1"
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("review comment used as actionability evidence")
	}
}

func TestLaterBugIssueCanLabelAnUnfixedCandidate(t *testing.T) {
	c := syntheticCorpus()
	test := copyEvaluationCase(t, c.Cases[10])
	evidence := &test.Label.Defects[0].Provenance.Evidence[0]
	evidence.Kind = "bug"
	evidence.URL = "https://github.com/hemsoft-dev/set-it-free-loop/issues/123"
	evidence.Commit = test.Snapshot.Head
	if err := test.ValidateLabels(c.FrozenAt); err != nil {
		t.Fatal("a later bug does not require an invented fix commit", err)
	}
}

func TestRetainedEvaluationLabelsRemainAuditableAndUnfrozen(t *testing.T) {
	raw, err := os.ReadFile("testdata/evaluation/labels.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeStrict[EvaluationCorpus](raw, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) != 20 {
		t.Fatal("candidate set changed")
	}
	clean, defects := 0, 0
	for _, test := range c.Cases {
		if err := test.ValidateLabels(c.FrozenAt); err != nil {
			t.Fatal(test.ID, err)
		}
		if test.Label.Clean {
			clean++
		} else {
			defects++
		}
		if test.PacketHash != "" || test.SerializedPacketBytes != 0 {
			t.Fatal("draft claims materialized packet")
		}
	}
	if clean != 14 || defects != 6 {
		t.Fatal("retained label count changed without source audit")
	}
	if err := c.Validate(); err == nil {
		t.Fatal("unmaterialized draft became scorable")
	}
}

func TestRetainedEvaluationManifestMatchesLabels(t *testing.T) {
	raw, err := os.ReadFile("testdata/evaluation-candidates.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Candidates []struct {
			Number                 int
			HeadRefOid, BaseRefOid string
			Label                  EvaluationCaseLabel
		}
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("testdata/evaluation/labels.json")
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := DecodeStrict[EvaluationCorpus](raw, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Candidates) != len(corpus.Cases) {
		t.Fatal("selection and corpus differ")
	}
	for i, candidate := range manifest.Candidates {
		test := corpus.Cases[i]
		if candidate.Number != test.Snapshot.PR || candidate.HeadRefOid != test.Snapshot.Head || candidate.BaseRefOid != test.Snapshot.Base || Digest(candidate.Label) != Digest(test.Label) {
			t.Fatal("manifest drift at", candidate.Number)
		}
	}
}

func TestOwnerLabelProvenanceCannotUseAgentFields(t *testing.T) {
	base := syntheticCorpus().Cases[0]
	base.Label.Provenance.Labeler = Labeler{Kind: "owner", Agent: "Franz", Provider: "human", Model: "none", Session: "manual-test", HumanID: OwnerID}
	for _, mutate := range []func(*Labeler){
		func(l *Labeler) { l.Agent = "/root" },
		func(l *Labeler) { l.Provider = "OpenAI" },
		func(l *Labeler) { l.Model = "gpt-6.1-sol" },
		func(l *Labeler) { l.Session = "agent-session" },
		func(l *Labeler) { l.Session = "manual-" },
	} {
		test := copyEvaluationCase(t, base)
		mutate(&test.Label.Provenance.Labeler)
		if err := test.ValidateLabels(controlTime); err == nil {
			t.Fatal("agent fields accepted as Franz provenance", test.Label.Provenance.Labeler)
		}
	}
	if err := base.ValidateLabels(controlTime); err != nil {
		t.Fatal("valid owner rejected", err)
	}
	for _, mutate := range []func(*Labeler){
		func(l *Labeler) { l.Agent = "Franz" }, func(l *Labeler) { l.Provider = "human" },
		func(l *Labeler) { l.Model = "none" }, func(l *Labeler) { l.Session = "manual-override" },
	} {
		test := copyEvaluationCase(t, syntheticCorpus().Cases[0])
		mutate(&test.Label.Provenance.Labeler)
		if err := test.ValidateLabels(controlTime); err == nil {
			t.Fatal("owner-style agent provenance accepted")
		}
	}
}
func TestFindingAssessmentsMustBeStrictlyAfterRunCompletion(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	runs := syntheticResults(c, p)
	run := &runs[60]
	run.Labels[0].Assessment.Provenance = syntheticLabelProvenance(run.StartedAt.Add(run.Latency))
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("assessment at completion boundary accepted")
	}
	runs = syntheticResults(c, p)
	runs[0].LabelledAt = runs[0].StartedAt.Add(runs[0].Latency)
	if _, err := ScoreEvaluation(c, p, runs); err == nil {
		t.Fatal("batch assessment at completion boundary accepted")
	}
}
func TestFindingAssessmentHistoryValidatesEachJudgment(t *testing.T) {
	c := syntheticCorpus()
	p := syntheticPlan(c)
	for _, mutate := range []func(*FindingAssessment){
		func(a *FindingAssessment) { a.Actionable = false },
		func(a *FindingAssessment) { a.DefectIDs = []string{} },
		func(a *FindingAssessment) { a.DefectIDs = []string{"unknown"} },
		func(a *FindingAssessment) { a.DefectIDs = []string{"d1", "d1"} },
	} {
		runs := syntheticResults(c, p)
		run := &runs[60]
		label := &run.Labels[0]
		old := FindingAssessment{Actionable: true, DefectIDs: []string{"d1"}, Rationale: "prior assessment", Provenance: syntheticLabelProvenance(run.StartedAt.Add(2 * run.Latency))}
		mutate(&old)
		label.History = []FindingAssessment{old}
		label.Assessment.Provenance.Supersedes = Digest(old)
		if _, err := ScoreEvaluation(c, p, runs); err == nil {
			t.Fatal("invalid historical judgment accepted", old)
		}
	}
	// Valid revisions preserve a superseded actionable judgment after withdrawal.
	runs := syntheticResults(c, p)
	run := &runs[60]
	label := &run.Labels[0]
	old := FindingAssessment{Actionable: true, DefectIDs: []string{"d1"}, Rationale: "prior assessment", Provenance: syntheticLabelProvenance(run.StartedAt.Add(2 * run.Latency))}
	label.History = []FindingAssessment{old}
	label.Assessment.Provenance.Supersedes = Digest(old)
	label.Assessment.Actionable = false
	label.Assessment.DefectIDs = []string{}
	run.FindingStatuses["f1"] = "retracted"
	run.Concessions = 1
	if _, err := ScoreEvaluation(c, p, runs); err != nil {
		t.Fatal("valid withdrawal revision rejected", err)
	}
}
