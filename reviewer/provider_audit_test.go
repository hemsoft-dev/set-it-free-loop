package reviewer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func actualProviderEvidence(t *testing.T, s Session, failed bool) *ProviderEvidence {
	t.Helper()
	p := egressTestPolicy(s.Identity.Provider)
	if failed {
		p.Limits.RequestBytes = 1
	}
	b, err := NewProviderEgress(context.Background(), s, p, egressRoundTrip(func(*http.Request) (*http.Response, error) {
		if failed {
			t.Error("oversized request reached transport")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	q := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
	q.Host = "provider.example"
	q.ContentLength = -1
	b.ServeHTTP(httptest.NewRecorder(), q)
	a, err := b.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &ProviderEvidence{p, a}
}

func TestStoppedProviderEvidencePreservesFailureAndRejectsRebinding(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		for _, failed := range []bool{false, true} {
			s := testSession(provider)
			e := actualProviderEvidence(t, s, failed)
			if err := e.validateStoppedCaptured(provider, s.Limits, Digest(s)); err != nil {
				t.Fatal(provider, failed, err)
			}
			if completed := e.Access.ValidateCompleted(s, e.Policy); (completed != nil) != failed {
				t.Fatal("failed receipt became a successful report", completed)
			}
			changed := cloneProviderEvidence(e)
			changed.Access.SessionHash = Digest("other")
			if changed.validateStoppedCaptured(provider, s.Limits, Digest(s)) == nil {
				t.Fatal("stale session accepted")
			}
			changed = cloneProviderEvidence(e)
			changed.Access.Stopped = false
			if !errors.Is(changed.validateStoppedCaptured(provider, s.Limits, Digest(s)), ErrProviderEgressPending) {
				t.Fatal("unstopped receipt accepted")
			}
			changed = cloneProviderEvidence(e)
			changed.Policy.Routes[0].Authority = "127.0.0.1"
			changed.Access.PolicyHash = Digest(changed.Policy)
			if changed.validateStoppedCaptured(provider, s.Limits, Digest(s)) == nil {
				t.Fatal("forbidden recovery policy accepted")
			}
			if e.Policy.Routes[0].Authority != "provider.example" {
				t.Fatal("clone aliased captured routes")
			}
		}
	}
}

func TestProviderAttemptRecoveryIsIdempotentAndKeepsChargedLeaseOnWriteFailure(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	var captured *ProviderEvidence
	engine := testEngine(t, func(call context.Context, session Session) (Report, error) {
		captured = actualProviderEvidence(t, session, false)
		state, err := s.Snapshot(call)
		if err != nil {
			t.Fatal(err)
		}
		j := state.Jobs[job.ID]
		if _, err = s.db.Exec(`CREATE TRIGGER provider_audit_fault BEFORE UPDATE ON state BEGIN SELECT RAISE(FAIL,'provider_audit_fault'); END`); err != nil {
			t.Fatal(err)
		}
		err = s.RecordStoppedProviderEvidence(call, job.ID, j.LeaseToken, j.AttemptID, captured)
		if err == nil {
			t.Fatal("failed transaction accepted")
		}
		return Report{}, errors.Join(ErrProviderAuditPending, err)
	})
	out, err := s.RunStopped(ctx, job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if !errors.Is(err, ErrProviderAuditPending) || len(out.Ledger) != 0 {
		t.Fatal("audit failure did not remain pending", out, err)
	}
	state, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := state.Jobs[job.ID]
	if j.LeaseToken == "" || !j.Outstanding || j.Checkpoint.ConsumedSessions != 1 || len(j.ProviderAttempts) != 0 {
		t.Fatal("lost charged recovery attempt", j)
	}
	if _, err = s.db.Exec("DROP TRIGGER provider_audit_fault"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordStoppedProviderEvidence(ctx, job.ID, j.LeaseToken, "wrong", captured); !errors.Is(err, ErrFenced) {
		t.Fatal("wrong attempt accepted", err)
	}
	for i := 0; i < 2; i++ {
		if err = s.RecordStoppedProviderEvidence(ctx, job.ID, j.LeaseToken, j.AttemptID, captured); err != nil {
			t.Fatal(err)
		}
	}
	changed := cloneProviderEvidence(captured)
	changed.Access.Failed = true
	if err = s.RecordStoppedProviderEvidence(ctx, job.ID, j.LeaseToken, j.AttemptID, changed); !errors.Is(err, ErrFenced) {
		t.Fatal("replacement evidence accepted", err)
	}
	if err = s.ReleaseStopped(ctx, job.ID, j.LeaseToken, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	state, err = s.Snapshot(ctx)
	if err != nil || len(state.Jobs[job.ID].ProviderAttempts) != 1 || state.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 || state.Jobs[job.ID].LeaseToken != "" {
		t.Fatal("recovery replayed/refunded or lost receipt", state, err)
	}
}

// Denial counters must agree with the recorded outcome: a completed receipt
// carries none, and a stopped receipt's counters are in range and failed.
func TestProviderEvidenceValidatesDenialCounters(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		s := testSession(provider)
		clean := actualProviderEvidence(t, s, false)
		for _, mutate := range []func(*ProviderAccess){
			func(a *ProviderAccess) { a.ResponseDenials = 1 },
			func(a *ProviderAccess) { a.RouteDenials = 1 },
		} {
			changed := cloneProviderEvidence(clean)
			mutate(&changed.Access)
			if changed.Access.ValidateCompleted(s, changed.Policy) == nil {
				t.Fatal("completed receipt with a denial accepted", provider, changed.Access)
			}
			if changed.validateStoppedCaptured(provider, s.Limits, Digest(s)) == nil {
				t.Fatal("unfailed stopped receipt with a denial accepted", provider, changed.Access)
			}
			changed.Access.Failed = true
			if err := changed.validateStoppedCaptured(provider, s.Limits, Digest(s)); err != nil {
				t.Fatal("consistent failed receipt with a denial rejected", provider, err)
			}
		}
		for _, mutate := range []func(*ProviderAccess){
			func(a *ProviderAccess) { a.RouteDenials = -1 },
			func(a *ProviderAccess) { a.ResponseDenials = -1 },
			func(a *ProviderAccess) { a.ResponseDenials = a.Forwarded + 1 },
		} {
			changed := cloneProviderEvidence(clean)
			changed.Access.Failed = true
			mutate(&changed.Access)
			if changed.validateStoppedCaptured(provider, s.Limits, Digest(s)) == nil {
				t.Fatal("out-of-range denial counter accepted", provider, changed.Access)
			}
		}
	}
}
