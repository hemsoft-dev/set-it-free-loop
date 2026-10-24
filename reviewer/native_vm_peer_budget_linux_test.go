//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func frozenPeerBudgetFixture(t *testing.T) (Session, Report, map[string]Object, []string) {
	t.Helper()
	initial := testSession(Codex)
	files := map[string]Object{}
	paths := []string{}
	makeFindings := func(provider Provider, round, count int) []Finding {
		findings := []Finding{}
		for n := 0; n < count; n++ {
			id := fmt.Sprintf("%s:r%df%d", provider, round, n)
			path := fmt.Sprintf("%s_r%d_%d.go", provider, round, n)
			paths = append(paths, path)
			files[path] = Object{Mode: "100644", Data: []byte("x\n")}
			findings = append(findings, Finding{ID: id, Title: "Synthetic", Body: "Synthetic evidence only", Severity: 2, Anchor: Anchor{Path: path, Line: 1, Quote: "x", Revision: initial.Snapshot.Head}})
		}
		return findings
	}
	peers := []Report{}
	for _, provider := range []Provider{Codex, Claude} {
		s := testSession(provider)
		report := fakeReport(s)
		report.Findings = makeFindings(provider, 1, 40)
		if err := ValidateReport(report, s); err != nil {
			t.Fatal(err)
		}
		peers = append(peers, report)
	}
	first := append([]Report{}, peers...)
	for _, provider := range []Provider{Codex, Claude} {
		s, err := NewSession(initial.JobID, initial.Snapshot, initial.ConfigHash, provider, 2, initial.Packet, first, nil, initial.Limits)
		if err != nil {
			t.Fatal(err)
		}
		report := fakeReport(s)
		report.Findings = makeFindings(provider, 2, 9)
		for _, peer := range first {
			if peer.Provider != provider {
				for _, finding := range peer.Findings {
					report.Judgments = append(report.Judgments, Judgment{FindingID: finding.ID, Verdict: "confirm", Reason: "Synthetic source evidence", Evidence: []Anchor{finding.Anchor}})
				}
			}
		}
		if err = ValidateReport(report, s); err != nil {
			t.Fatal(err)
		}
		peers = append(peers, report)
	}
	session, err := NewSession(initial.JobID, initial.Snapshot, initial.ConfigHash, Codex, 3, initial.Packet, peers, nil, initial.Limits)
	if err != nil {
		t.Fatal(err)
	}
	report := fakeReport(session)
	report.Findings = makeFindings(Codex, 3, 9)
	if err = ValidateReport(report, session); err != nil {
		t.Fatal(err)
	}
	return session, report, files, paths
}
func TestFrozenPeerReportsHaveIndependentVerificationBudgets(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "adapter"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			session, output, files, paths := frozenPeerBudgetFixture(t)
			store := &controlCountingStore{store: &fixtureStore{files: files}}
			reader, err := NewReader(store, session.Snapshot, paths, session.Limits)
			if err != nil {
				t.Fatal(err)
			}
			expected := 187
			if native {
				expected = 178
				policy := egressTestPolicy(Codex)
				policy.ResponsePolicy = CodexSourceResponses
				withCodexRequestPolicy(&policy)
				worker, err := NewNativeVMReviewer(NativeVMOptions{Provider: Codex, Root: "/unopened/native-root", Stub: "/unopened/stub", StubHash: strings.Repeat("a", 64), Auth: &AuthStore{provider: Codex, directory: "/unopened/auth"}, Reader: reader, Policy: policy, Transport: egressRoundTrip(func(*http.Request) (*http.Response, error) {
					t.Fatal("preflight reached HTTP")
					return nil, ErrLiveBlocked
				}), Check: func(context.Context, Session) error { return nil }})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, _, err = worker.ReviewAudited(context.Background(), session); err == nil {
					t.Fatal("missing boot assets admitted native work")
				}
			} else {
				raw, _ := json.Marshal(output)
				driver := &fakeDriver{inspect: session.Identity, actual: session.Identity, data: raw}
				adapter := Adapter{Provider: Codex, Driver: driver, AuthDirectory: t.TempDir(), Reader: reader}
				if _, err = adapter.Review(context.Background(), session); err != nil || driver.calls != 1 {
					t.Fatal("valid peer history prevented current work", driver.calls, store.reads, err)
				}
			}
			if store.reads != expected {
				t.Fatal("frozen peer reports shared a verification allowance", store.reads, expected)
			}
			reads, _ := reader.Evidence()
			want := 9
			if native {
				want = 0
			}
			if len(reads) != want {
				t.Fatal("history spent the current reader budget", len(reads), want)
			}
		})
	}
}
