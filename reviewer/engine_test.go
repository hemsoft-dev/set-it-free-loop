package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type reviewFunc func(context.Context, Session) (Report, error)

func (f reviewFunc) Review(c context.Context, s Session) (Report, error) { return f(c, s) }

func testJob() Job {
	return Job{ID: "job-1", RequesterID: OwnerID, Generation: 1, Snapshot: testSnapshot(), Config: DefaultConfig(), ConfigVersion: 1, Release: strings.Repeat("d", 40), Packet: "same immutable evidence"}
}

func protocolReport(s Session) Report {
	r := fakeReport(s)
	if s.Round == 1 {
		r.Findings = []Finding{testFinding(s.Identity.Provider)}
		return r
	}
	focus := map[string]bool{}
	for _, id := range s.FocusIDs {
		focus[id] = true
	}
	for _, peer := range s.PeerReports {
		if peer.Provider == s.Identity.Provider {
			if s.Round == 3 {
				for _, f := range peer.Findings {
					if focus[f.ID] {
						r.Judgments = append(r.Judgments, Judgment{FindingID: f.ID, Verdict: "respond", Reason: "Source-grounded response to received objection", Evidence: []Anchor{f.Anchor}})
					}
				}
			}
			continue
		}
		for _, f := range peer.Findings {
			if s.Round == 3 && !focus[f.ID] {
				continue
			}
			r.Judgments = append(r.Judgments, Judgment{FindingID: f.ID, Verdict: "confirm", Reason: "Source supports this allegation", Evidence: []Anchor{f.Anchor}})
		}
	}
	return r
}

func testEngine(t *testing.T, call reviewFunc) Engine {
	t.Helper()
	return Engine{Reviewers: map[Provider]Reviewer{Codex: call, Claude: call}, Reader: func(s Snapshot) (*Reader, error) {
		return NewReader(&fixtureStore{files: map[string]Object{"source.go": {"100644", []byte("original source")}}}, s, []string{"source.go"}, DefaultLimits())
	}}
}

func TestDefaultAdversarialPhasesAndDeterministicPublication(t *testing.T) {
	var calls []Session
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		calls = append(calls, s)
		if s.Round == 1 && len(s.PeerReports) != 0 {
			t.Error("R1 saw peer output")
		}
		if s.Round == 2 && len(s.PeerReports) != 2 {
			t.Error("R2 did not receive both frozen R1 reports")
		}
		return protocolReport(s), nil
	})
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "dual_complete" || len(calls) != 4 || out.ConsumedSessions != 4 || len(out.Ledger) != 2 {
		t.Fatalf("%+v", out)
	}
	for _, entry := range out.Ledger {
		if entry.Status != "corroborated" || len(entry.History) != 1 || len(entry.RelatedIDs) != 1 {
			t.Fatalf("lost evidence: %+v", entry)
		}
	}
	doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Inline) != 1 || !strings.Contains(doc.Summary, "codex:one") || !strings.Contains(doc.Summary, "claude:one") {
		t.Fatal("duplicate output or lost provenance", doc)
	}
	again, err := e.Run(context.Background(), testJob(), out.Checkpoint())
	if err != nil || again.ExecutedSessions != 0 {
		t.Fatal("completed resume repeated inference", err)
	}
	second, err := Render(again, map[string]map[int]bool{"source.go": {1: true}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(doc)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("identical evidence changed rendered bytes")
	}
	outside, _ := Render(out, nil)
	if len(outside.Inline) != 0 || !strings.Contains(outside.Summary, "Possible defect") {
		t.Fatal("non-diff finding disappeared")
	}
}

func TestExplicitModesCompleteWithoutPeerConfirmation(t *testing.T) {
	for _, mode := range []Mode{IndependentPair, CodexOnly, ClaudeOnly} {
		t.Run(string(mode), func(t *testing.T) {
			job := testJob()
			job.Config.Mode = mode
			count := 0
			e := testEngine(t, func(_ context.Context, s Session) (Report, error) { count++; return protocolReport(s), nil })
			out, err := e.Run(context.Background(), job, Checkpoint{})
			if err != nil {
				t.Fatal(err)
			}
			if count != job.Config.MaxSessions() || strings.Contains(out.State, "incomplete") {
				t.Fatal("selected mode treated as failed dual run")
			}
			for _, entry := range out.Ledger {
				if entry.Status != "unverified" {
					t.Fatal("independent allegation promoted without peer examination")
				}
			}
			doc, err := Render(out, map[string]map[int]bool{"source.go": {1: true}})
			if err != nil || len(doc.Inline) != 0 {
				t.Fatal("unverified inline finding", err)
			}
		})
	}
}

func TestQuotaWaitResumesOnlySameCapturedJob(t *testing.T) {
	blocked := true
	calls := 0
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		calls++
		if blocked && s.Identity.Provider == Claude {
			return Report{}, PhaseError{State: "quota_wait", BeforeWork: true}
		}
		return protocolReport(s), nil
	})
	job := testJob()
	out, err := e.Run(context.Background(), job, Checkpoint{})
	if err == nil || out.State != "quota_wait" || len(out.Artifacts) != 1 || out.ConsumedSessions != 1 {
		t.Fatalf("wrong quota state: %+v %v", out, err)
	}
	doc, _ := Render(out, nil)
	if strings.Contains(doc.Summary, "Possible defect") || len(doc.Inline) > 0 || !strings.Contains(doc.Summary, "INCOMPLETE") {
		t.Fatal("substantive partial publication")
	}
	for _, change := range []func(*Job){func(j *Job) { j.Snapshot.Head = strings.Repeat("e", 40) }, func(j *Job) { j.ConfigVersion++ }, func(j *Job) { j.Packet = "other packet" }, func(j *Job) { j.Generation++ }} {
		other := job
		change(&other)
		before := calls
		if _, err := e.Run(context.Background(), other, out.Checkpoint()); err == nil || calls != before {
			t.Fatal("changed job resumed or spent quota")
		}
	}
	blocked = false
	resumed, err := e.Run(context.Background(), job, out.Checkpoint())
	if err != nil || resumed.ExecutedSessions != 3 || resumed.ConsumedSessions != 4 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
}

func TestFailedSessionsCannotExceedBudgetAcrossResume(t *testing.T) {
	fail := true
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		if fail && s.Identity.Provider == Claude {
			return Report{}, errors.New("connection lost after work may have begun")
		}
		return protocolReport(s), nil
	})
	out, _ := e.Run(context.Background(), testJob(), Checkpoint{})
	if out.ConsumedSessions != 2 {
		t.Fatal("uncertain session was refunded")
	}
	fail = false
	next, err := e.Run(context.Background(), testJob(), out.Checkpoint())
	if err == nil || next.State != "incomplete_expired" || next.ConsumedSessions != 4 {
		t.Fatal("session ceiling bypassed", err)
	}
}

func TestDisputesLateFindingsAndOptionalPairedRound(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(map[bool]string{false: "four", true: "six"}[extra], func(t *testing.T) {
			job := testJob()
			if extra {
				job.Config.PeerRounds = 2
			}
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
			out, err := e.Run(context.Background(), job, Checkpoint{})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Artifacts) != job.Config.MaxSessions() {
				t.Fatal("unbounded or missing paired phase")
			}
			for _, entry := range out.Ledger {
				if extra {
					if entry.Status != "corroborated" {
						t.Fatal("R3 peer confirmation failed", entry)
					}
				} else if entry.RaisedRound == 2 && entry.Status != "unverified" {
					t.Fatal("late R2 finding promoted without R3")
				} else if entry.RaisedRound == 1 && entry.Status != "disputed" {
					t.Fatal("objection lost")
				}
			}
		})
	}
}

func TestInvalidEvidenceAndMissingProviderNeverComplete(t *testing.T) {
	for _, bad := range []string{"missing-provider", "source", "peer-disposition", "deadline"} {
		t.Run(bad, func(t *testing.T) {
			job := testJob()
			if bad == "deadline" {
				job.Config.Limits.Timeout = time.Millisecond
			}
			e := testEngine(t, func(ctx context.Context, s Session) (Report, error) {
				if bad == "deadline" {
					<-ctx.Done()
					return Report{}, ctx.Err()
				}
				r := protocolReport(s)
				if bad == "source" && s.Round == 1 {
					r.Findings[0].Anchor.Quote = "invented"
				}
				if bad == "peer-disposition" && s.Round == 2 {
					r.Judgments = []Judgment{}
				}
				return r, nil
			})
			if bad == "missing-provider" {
				delete(e.Reviewers, Claude)
			}
			out, err := e.Run(context.Background(), job, Checkpoint{})
			if err == nil || out.State == "dual_complete" || len(out.Ledger) > 0 {
				t.Fatal("incomplete phase became successful review")
			}
		})
	}
}

func TestUnresolvedRejectionSurvivesLaterInabilityToVerify(t *testing.T) {
	job := testJob()
	job.Config.PeerRounds = 2
	e := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		r := protocolReport(s)
		if s.Round == 2 {
			for i := range r.Judgments {
				r.Judgments[i].Verdict = "reject"
			}
		}
		if s.Round == 3 {
			for i := range r.Judgments {
				if r.Judgments[i].Verdict != "respond" {
					r.Judgments[i].Verdict = "cannot_verify"
					r.Judgments[i].Evidence = []Anchor{}
				}
			}
		}
		return r, nil
	})
	out, err := e.Run(context.Background(), job, Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range out.Ledger {
		if entry.Status != "disputed" {
			t.Fatal("unresolved source-grounded objection disappeared")
		}
	}
}
