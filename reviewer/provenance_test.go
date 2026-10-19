package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testFinding(p Provider) Finding {
	return Finding{ID: string(p) + ":one", Title: "Possible defect", Body: "Source-grounded allegation", Severity: 1, Anchor: Anchor{"source.go", 1, "original source", testSnapshot().Head}}
}

func peerSessions(t *testing.T) (Session, []Report) {
	t.Helper()
	c := testSession(Codex)
	h := testSession(Claude)
	cr, hr := fakeReport(c), fakeReport(h)
	cr.Findings = []Finding{testFinding(Codex)}
	hr.Findings = []Finding{testFinding(Claude)}
	peers := []Report{cr, hr}
	s, err := NewSession(c.JobID, c.Snapshot, c.ConfigHash, Codex, 2, c.Packet, peers, nil, c.Limits)
	if err != nil {
		t.Fatal(err)
	}
	return s, peers
}

func TestFrozenPeerProvenanceBeforeLaterRounds(t *testing.T) {
	s, peers := peerSessions(t)
	for _, edit := range []func(*Report){
		func(r *Report) { r.SnapshotHash = Digest("wrong snapshot") },
		func(r *Report) { r.ConfigHash = Digest("wrong config") },
		func(r *Report) { r.JobID = "other-job" },
		func(r *Report) { r.PacketHash = Digest("other packet") },
		func(r *Report) { r.Round = 2 },
	} {
		local := s
		local.PeerReports = append([]Report(nil), peers...)
		edit(&local.PeerReports[0])
		if local.Validate() == nil {
			t.Fatal("accepted stale or cross-job peer report")
		}
	}
	for _, packet := range []string{"", " \n\t"} {
		local := testSession(Codex)
		local.Packet = packet
		local.PacketHash = Digest(packet)
		if local.Validate() == nil {
			t.Fatal("accepted empty evidence packet")
		}
	}
	s.PeerReports = s.PeerReports[:1]
	if s.Validate() == nil {
		t.Fatal("accepted missing peer")
	}
}

func TestJudgmentCoverageAndRetractionReferences(t *testing.T) {
	s, _ := peerSessions(t)
	base := fakeReport(s)
	if ValidateReport(base, s) == nil {
		t.Fatal("accepted unexamined peer finding")
	}
	base.Judgments = []Judgment{{FindingID: "claude:one", Verdict: "confirm", Reason: "Source supports it", Evidence: []Anchor{testFinding(Claude).Anchor}}}
	base.Retractions = []string{"codex:one"}
	if err := ValidateReport(base, s); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Report){
		func(r *Report) { r.Judgments[0].FindingID = "claude:missing" },
		func(r *Report) { r.Judgments[0].Verdict = "approved" },
		func(r *Report) { r.Judgments[0].Evidence = []Anchor{} },
		func(r *Report) { r.Judgments[0].Evidence[0].Revision = strings.Repeat("f", 40) },
		func(r *Report) { r.Retractions = []string{"claude:one"} },
		func(r *Report) { r.Coverage = []Coverage{{Path: "../secret", Reason: "omitted"}} },
	} {
		data, _ := json.Marshal(base)
		copy, err := DecodeStrict[Report](data, s.Limits.OutputBytes)
		if err != nil {
			t.Fatal(err)
		}
		edit(&copy)
		if ValidateReport(copy, s) == nil {
			t.Fatal("accepted malformed peer disposition")
		}
	}
}

func TestAdapterChecksSourceInsteadOfTrustingModelAnchors(t *testing.T) {
	s := testSession(Codex)
	report := fakeReport(s)
	report.Findings = []Finding{testFinding(Codex)}
	makeAdapter := func(r Report) *Adapter {
		data, _ := json.Marshal(r)
		f := &fakeDriver{inspect: s.Identity, actual: s.Identity, data: data}
		reader, err := NewReader(&fixtureStore{files: map[string]Object{"source.go": {"100644", []byte("original source")}}}, s.Snapshot, []string{"source.go", "missing.go"}, s.Limits)
		if err != nil {
			t.Fatal(err)
		}
		return &Adapter{Provider: Codex, Driver: f, AuthDirectory: t.TempDir(), Reader: reader}
	}
	if _, err := makeAdapter(report).Review(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Anchor{{"source.go", 1, "invented quote", s.Snapshot.Head}, {"missing.go", 1, "invented file", s.Snapshot.Head}} {
		local := report
		local.Findings = append([]Finding(nil), report.Findings...)
		local.Findings[0].Anchor = a
		if _, err := makeAdapter(local).Review(context.Background(), s); err == nil {
			t.Fatal("accepted hallucinated source")
		}
	}
	a := makeAdapter(report)
	a.Reader = nil
	if _, err := a.Review(context.Background(), s); err == nil {
		t.Fatal("accepted finding without source verifier")
	}
	// A structurally valid but false peer quote must fail before spending a phase.
	ps, peers := peerSessions(t)
	peers[0].Findings = append([]Finding(nil), peers[0].Findings...)
	peers[0].Findings[0].Anchor.Quote = "invented peer quote"
	ps.PeerReports = peers
	a = makeAdapter(report)
	f := a.Driver.(*fakeDriver)
	if _, err := a.Review(context.Background(), ps); err == nil || f.calls != 0 {
		t.Fatal("unverified peer evidence reached provider")
	}
}
