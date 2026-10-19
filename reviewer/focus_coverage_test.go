package reviewer

import (
	"context"
	"encoding/json"
	"testing"
)

func thirdRoundFixture(t *testing.T) (Session, []Report) {
	t.Helper()
	s, peers := peerSessions(t)
	all := append([]Report(nil), peers...)
	for _, p := range []Provider{Codex, Claude} {
		phase, err := NewSession(s.JobID, s.Snapshot, s.ConfigHash, p, 2, s.Packet, peers, nil, s.Limits)
		if err != nil {
			t.Fatal(err)
		}
		r := fakeReport(phase)
		peer := Claude
		if p == Claude {
			peer = Codex
		}
		r.Judgments = []Judgment{{FindingID: string(peer) + ":one", Verdict: "reject", Reason: "Source-grounded disagreement", Evidence: []Anchor{testFinding(peer).Anchor}}}
		all = append(all, r)
	}
	third, err := NewSession(s.JobID, s.Snapshot, s.ConfigHash, Codex, 3, s.Packet, all, []string{"codex:one", "claude:one"}, s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	return third, all
}

func TestEveryFocusedOwnFindingRequiresResponseOrRetraction(t *testing.T) {
	s, _ := thirdRoundFixture(t)
	r := fakeReport(s)
	r.Judgments = []Judgment{{FindingID: "claude:one", Verdict: "confirm", Reason: "Peer confirmed", Evidence: []Anchor{testFinding(Claude).Anchor}}}
	if ValidateReport(r, s) == nil {
		t.Fatal("own focused allegation silently ignored")
	}
	r.Retractions = []string{"codex:one"}
	if err := ValidateReport(r, s); err != nil {
		t.Fatal(err)
	}
	r.Retractions = []string{}
	r.Judgments = append(r.Judgments, Judgment{FindingID: "codex:one", Verdict: "respond", Reason: "Reply to received objection", Evidence: []Anchor{testFinding(Codex).Anchor}})
	if err := ValidateReport(r, s); err != nil {
		t.Fatal(err)
	}
	r.Retractions = []string{"codex:one"}
	if ValidateReport(r, s) == nil {
		t.Fatal("ambiguous simultaneous response/retraction")
	}
}

func TestWithdrawnFindingCannotReturnToFocus(t *testing.T) {
	s, all := thirdRoundFixture(t)
	all[2].Retractions = []string{"codex:one"}
	if _, err := NewSession(s.JobID, s.Snapshot, s.ConfigHash, Codex, 3, s.Packet, all, []string{"codex:one"}, s.Limits); err == nil {
		t.Fatal("withdrawn allegation reactivated in focus")
	}
}

func TestReaderOmissionsCannotDisappearFromAcceptedReport(t *testing.T) {
	s := testSession(Codex)
	reader, err := NewReader(&fixtureStore{files: map[string]Object{}}, s.Snapshot, []string{"missing.go"}, s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	reader.Read(context.Background(), s.Snapshot.Head, "missing.go")
	reader.Read(context.Background(), s.Snapshot.Head, "../secret")
	data, _ := json.Marshal(fakeReport(s))
	driver := &fakeDriver{inspect: s.Identity, actual: s.Identity, data: data}
	a := Adapter{Provider: Codex, Driver: driver, AuthDirectory: t.TempDir(), Reader: reader}
	r, err := a.Review(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Coverage) != 2 || r.Coverage[0].Path != "missing.go" || r.Coverage[1].Path != "" {
		t.Fatalf("authoritative omissions lost: %+v", r.Coverage)
	}
}

func TestCitationBatchReadsOneObjectOnce(t *testing.T) {
	s := testSnapshot()
	limits := DefaultLimits()
	limits.ReaderCalls = 1
	store := &fixtureStore{files: map[string]Object{"source.go": {"100644", []byte("original source")}}}
	reader, err := NewReader(store, s, []string{"source.go"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	var anchors []Anchor
	for i := 0; i < 100; i++ {
		anchors = append(anchors, testFinding(Codex).Anchor)
	}
	if err := reader.VerifyAll(context.Background(), anchors); err != nil || store.calls != 1 {
		t.Fatal("redundant validation exhausted reader budget", err)
	}
}
