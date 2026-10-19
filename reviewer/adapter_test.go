package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testSnapshot() Snapshot {
	return Snapshot{RepositoryID, RepositoryID, 1, strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)}
}
func testSession(p Provider) Session {
	s, err := NewSession("job-1", testSnapshot(), Digest("config"), p, 1, "same immutable evidence", nil, nil, DefaultLimits())
	if err != nil {
		panic(err)
	}
	return s
}

type fakeDriver struct {
	inspect    Identity
	actual     Identity
	data       []byte
	err        error
	inspectErr error
	calls      int
	delay      bool
}

func (f *fakeDriver) Inspect(context.Context, Invocation) (Identity, error) {
	return f.inspect, f.inspectErr
}
func (f *fakeDriver) Execute(ctx context.Context, _ Invocation) ([]byte, Identity, error) {
	f.calls++
	if f.delay {
		<-ctx.Done()
		return nil, f.actual, ctx.Err()
	}
	return f.data, f.actual, f.err
}
func fakeReport(s Session) Report {
	return Report{JobID: s.JobID, PolicyVersion: s.PolicyVersion, PacketHash: s.PacketHash, Provider: s.Identity.Provider, Round: s.Round, SnapshotHash: Digest(s.Snapshot), ConfigHash: s.ConfigHash, Findings: []Finding{}, Judgments: []Judgment{}, Retractions: []string{}, Coverage: []Coverage{}}
}

func TestBothAdaptersContractAndNoAmbientCredentials(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		t.Run(string(p), func(t *testing.T) {
			s := testSession(p)
			data, _ := json.Marshal(fakeReport(s))
			f := &fakeDriver{inspect: s.Identity, actual: s.Identity, data: data}
			a := Adapter{Provider: p, Driver: f, AuthDirectory: t.TempDir()}
			t.Setenv("GH_TOKEN", "github-canary")
			t.Setenv("OPENAI_API_KEY", "api-canary")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-canary")
			inv, err := a.Invocation(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(inv.Environment, "\n"), "canary") {
				t.Fatal("inherited credential")
			}
			if _, err := a.Invocation(s, map[string]string{"GH_TOKEN": "canary"}); err == nil {
				t.Fatal("accepted ambient credential")
			}
			r, err := a.Review(context.Background(), s)
			if err != nil || r.Provider != p || f.calls != 1 {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestAuthPrecheckRejectsBeforeProviderWork(t *testing.T) {
	s := testSession(Codex)
	wrong := s.Identity
	wrong.AuthMode = "api_key"
	f := &fakeDriver{inspect: wrong, actual: s.Identity}
	a := Adapter{Provider: Codex, Driver: f, AuthDirectory: t.TempDir()}
	if _, err := a.Review(context.Background(), s); err == nil || f.calls != 0 {
		t.Fatal("auth precheck did not fence execution")
	}
	f.inspect = s.Identity
	f.actual = wrong
	if _, err := a.Review(context.Background(), s); err == nil {
		t.Fatal("accepted post-execution auth drift")
	}
}

func TestAdapterFailsClosedAndBoundsOutput(t *testing.T) {
	s := testSession(Claude)
	data, _ := json.Marshal(fakeReport(s))
	for _, tc := range []struct {
		name  string
		data  []byte
		err   error
		delay bool
	}{
		{"quota", nil, errors.New("quota_wait"), false},
		{"malformed", []byte(`{"provider":"claude","unknown":true}`), nil, false},
		{"trailing", append(append([]byte(nil), data...), []byte(` {}`)...), nil, false},
		{"oversize", make([]byte, s.Limits.OutputBytes+1), nil, false},
		{"deadline", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := s
			if tc.delay {
				local.Limits.Timeout = time.Millisecond
			}
			f := &fakeDriver{inspect: s.Identity, actual: s.Identity, data: tc.data, err: tc.err, delay: tc.delay}
			if _, err := (Adapter{Provider: Claude, Driver: f, AuthDirectory: t.TempDir()}).Review(context.Background(), local); err == nil {
				t.Fatal("accepted incomplete provider phase")
			}
		})
	}
	for _, d := range []Driver{nil, DisabledLiveDriver{}} {
		if _, err := (Adapter{Provider: Claude, Driver: d, AuthDirectory: t.TempDir()}).Review(context.Background(), s); !errors.Is(err, ErrLiveBlocked) {
			t.Fatal(err)
		}
	}
}

func TestJobAndReportProvenance(t *testing.T) {
	s := testSession(Codex)
	for _, edit := range []func(*Session){
		func(s *Session) { s.Snapshot.RepositoryID++ },
		func(s *Session) { s.Snapshot.Head = "main" },
		func(s *Session) { s.Identity.Model = "anything" },
		func(s *Session) { s.ConfigHash = strings.Repeat("x", 64) },
		func(s *Session) { s.PeerReports = []Report{{Provider: Claude}} },
		func(s *Session) { s.Limits.ReaderBytes = 0 },
	} {
		local := s
		edit(&local)
		if local.Validate() == nil {
			t.Fatal("accepted invalid contract")
		}
	}
	r := fakeReport(s)
	r.Provider = Claude
	if ValidateReport(r, s) == nil {
		t.Fatal("accepted wrong provider")
	}
	r = fakeReport(s)
	r.Findings = []Finding{{ID: "codex:one", Title: "title", Body: "body", Severity: 1, Anchor: Anchor{"a.go", 1, "line", s.Snapshot.Head}}}
	if err := ValidateReport(r, s); err != nil {
		t.Fatal(err)
	}
	r.Findings = append(r.Findings, r.Findings[0])
	if ValidateReport(r, s) == nil {
		t.Fatal("duplicate finding ID accepted")
	}
}
