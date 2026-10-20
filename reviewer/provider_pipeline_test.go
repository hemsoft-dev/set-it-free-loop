package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type auditedReviewFunc func(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error)

func (f auditedReviewFunc) ReviewAudited(c context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
	return f(c, s)
}
func (f auditedReviewFunc) Review(context.Context, Session) (Report, error) {
	return Report{}, errors.New("unguarded reviewer entrypoint")
}

func httpProviderEvidence(t *testing.T, ctx context.Context, s Session, physical *atomic.Int32) *ProviderEvidence {
	t.Helper()
	p := egressTestPolicy(s.Identity.Provider)
	p.Limits.Requests, p.Limits.InferenceRequests = 2, 2
	p.Limits.RefreshRequests, p.Limits.MetadataRequests, p.Limits.NegotiationRequests = 0, 0, 0
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		physical.Add(1)
		if q.Header.Get("Authorization") != "Bearer synthetic-provider-token-canary" {
			t.Error("fake auth changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	guard, err := NewProviderEgress(ctx, s, p, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		copy := q.Clone(q.Context())
		u := *q.URL
		u.Host = target.Host
		copy.URL = &u
		return backend.Client().Transport.RoundTrip(copy)
	}))
	if err != nil {
		t.Fatal(err)
	}
	intake := httptest.NewServer(guard)
	defer intake.Close()
	for i := 0; i < 2; i++ {
		q, err := http.NewRequestWithContext(ctx, "POST", intake.URL+"/infer", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		q.Host = "provider.example"
		q.Header.Set("Authorization", "Bearer synthetic-provider-token-canary")
		response, err := intake.Client().Do(q)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatal(response.StatusCode, err)
		}
	}
	intake.Close()
	stop, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a, err := guard.Close(stop)
	if err != nil {
		t.Fatal(err)
	}
	return &ProviderEvidence{p, a}
}

func auditedFixture(t *testing.T) (*Store, Outcome, *fakeRemote, Engine, *atomic.Int32) {
	t.Helper()
	store, job, remote := admittedFixture(t)
	engine := testEngine(t, nil)
	physical := &atomic.Int32{}
	reviewer := auditedReviewFunc(func(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
		reader, err := engine.Reader(s.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		broker, err := newSourceBroker(ctx, s, reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = broker.Read(ctx, s.Snapshot.Head, "source.go"); err != nil {
			t.Fatal(err)
		}
		evidence := httpProviderEvidence(t, ctx, s, physical)
		return protocolReport(s), broker.seal(), evidence, nil
	})
	engine.Reviewers = map[Provider]Reviewer{Codex: reviewer, Claude: reviewer}
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if err != nil {
		t.Fatal("audited run failed", out.State, err)
	}
	return store, out, remote, engine, physical
}

func TestPrivateProviderEvidenceFourPhasesDurableRecoveryAndPublication(t *testing.T) {
	store, out, remote, engine, physical := auditedFixture(t)
	if out.State != "dual_complete" || out.ConsumedSessions != 4 || physical.Load() != 8 {
		t.Fatal("HTTP counts confused with sessions", out.State, out.ConsumedSessions, physical.Load())
	}
	d, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[out.Job.ID]
	if len(j.ProviderAttempts) != 4 || len(j.SourceAttempts) != 4 || j.Outstanding || j.LeaseToken != "" {
		t.Fatal("private evidence missing or lease retained", j)
	}
	for i, a := range out.Artifacts {
		if !a.ProviderEvidenceRequired || a.ProviderEvidence == nil || a.ProviderEvidence.Access.InferenceRequests != 2 || a.SourceAccess == nil || a.SourceAccess.ToolCalls != 1 || j.ProviderAttempts[i].ID != j.SourceAttempts[i].ID {
			t.Fatal("phase audits conflated", i, a)
		}
	}
	recovered, err := store.RecoverCompleted(context.Background(), out.Job.ID, engine.Reader)
	if err != nil || Digest(recovered.Checkpoint()) != Digest(out.Checkpoint()) || physical.Load() != 8 {
		t.Fatal("completion recovery replayed work or lost evidence", err, physical.Load())
	}
	publisher := Publisher{Store: store, Remote: remote}
	intent, err := publisher.Prepare(context.Background(), recovered, nil)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(intent.Document)
	for _, private := range []string{"synthetic-provider-token-canary", "provider.example", "policy_hash", "provider_evidence"} {
		if strings.Contains(string(public), private) {
			t.Fatal("private audit leaked to review", private)
		}
	}
	if err = publisher.Dispatch(context.Background(), intent.ID); err != nil || remote.posts != 1 {
		t.Fatal("fake publication failed", err, remote.posts)
	}
}

func TestPrivateProviderEvidenceCheckpointAndPublisherCannotDropProof(t *testing.T) {
	for _, kind := range []string{"missing", "stale", "policy", "unrecorded"} {
		t.Run(kind, func(t *testing.T) {
			store, out, remote, engine, physical := auditedFixture(t)
			checkpoint := out.Checkpoint()
			switch kind {
			case "missing":
				checkpoint.Artifacts[0].ProviderEvidence = nil
			case "stale":
				checkpoint.Artifacts[0].ProviderEvidence.Access.SessionHash = Digest("other")
			case "policy":
				checkpoint.Artifacts[0].ProviderEvidence.Policy.Headers[0] = "Cookie"
			case "unrecorded":
				if err := store.update(context.Background(), func(d *DurableState) error { d.Jobs[out.Job.ID].ProviderAttempts = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "unrecorded" {
				if out.Artifacts[0].ProviderEvidence == nil || out.Artifacts[0].ProviderEvidence.Policy.Headers[0] == "Cookie" || out.Artifacts[0].ProviderEvidence.Access.SessionHash == Digest("other") {
					t.Fatal("checkpoint aliased private evidence")
				}
				failed, err := (Engine{Reader: engine.Reader}).Run(context.Background(), out.Job, checkpoint)
				if err == nil || failed.State != "invalid_output" || failed.ExecutedSessions != 0 || physical.Load() != 8 {
					t.Fatal("invalid cached receipt accepted or replayed", failed.State, err)
				}
			} else {
				if _, err := (Publisher{Store: store, Remote: remote}).Prepare(context.Background(), out, nil); !errors.Is(err, ErrProviderAuditPending) || remote.posts != 0 {
					t.Fatal("unpersisted receipt published", err, remote.posts)
				}
			}
		})
	}
}

func TestRequiredProviderEvidenceFailureRetainsChargeAndRecoveryIdentity(t *testing.T) {
	for _, kind := range []string{"missing", "unstopped", "stale", "failed", "before-work-after-request", "before-work-unstopped", "before-work-stale", "before-work-malformed"} {
		t.Run(kind, func(t *testing.T) {
			store, job, _ := admittedFixture(t)
			engine := testEngine(t, nil)
			reviewer := auditedReviewFunc(func(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
				e := actualProviderEvidence(t, s, kind == "failed")
				switch kind {
				case "missing":
					e = nil
				case "unstopped":
					e.Access.Stopped = false
				case "stale":
					e.Access.SessionHash = Digest("other")
				case "before-work-after-request", "before-work-unstopped", "before-work-stale", "before-work-malformed":
					switch kind {
					case "before-work-unstopped":
						e.Access.Stopped = false
					case "before-work-stale":
						e.Access.SessionHash = Digest("other")
					case "before-work-malformed":
						e.Access.PolicyHash = Digest("other")
					}
					return Report{}, nil, e, PhaseError{State: "quota_wait", BeforeWork: true}
				}
				return protocolReport(s), nil, e, nil
			})
			engine.Reviewers = map[Provider]Reviewer{Codex: reviewer, Claude: reviewer}
			out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
			if err == nil || out.ConsumedSessions != 1 || len(out.Ledger) != 0 {
				t.Fatal("failed audited phase became free or complete", out, err)
			}
			d, readErr := store.Snapshot(context.Background())
			if readErr != nil {
				t.Fatal(readErr)
			}
			j := d.Jobs[job.ID]
			if j.Checkpoint.ConsumedSessions != 1 || len(j.Checkpoint.Artifacts) != 0 {
				t.Fatal("attempt charge or report checkpoint incorrect", j)
			}
			pending := kind == "missing" || kind == "unstopped" || kind == "stale" || kind == "before-work-unstopped" || kind == "before-work-stale" || kind == "before-work-malformed"
			if pending {
				if !errors.Is(err, ErrProviderAuditPending) || j.LeaseToken == "" || !j.Outstanding {
					t.Fatal("unknown audit released lease", j, err)
				}
				if err = store.ReleaseStopped(context.Background(), job.ID, j.LeaseToken, "provider_unavailable"); !errors.Is(err, ErrProviderAuditPending) {
					t.Fatal("unrecorded required attempt released", err)
				}
			} else if j.LeaseToken != "" || j.Outstanding || len(j.ProviderAttempts) != 1 || out.FailedProviderEvidence == nil {
				t.Fatal("stopped failure audit lost", j, out.FailedProviderEvidence)
			}
		})
	}
}

func TestModelReportCannotSupplyPrivateProviderEvidence(t *testing.T) {
	s := testSession(Codex)
	raw, _ := json.Marshal(fakeReport(s))
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body)
	body["provider_evidence"] = json.RawMessage(`{"access":{"stopped":true}}`)
	raw, _ = json.Marshal(body)
	if _, err := DecodeStrict[Report](raw, s.Limits.OutputBytes); err == nil {
		t.Fatal("model supplied private evidence")
	}
}

func TestPrivateProviderEvidenceSurvivesStorageFailureWithoutReplayOrRefund(t *testing.T) {
	store, job, _ := admittedFixture(t)
	engine := testEngine(t, nil)
	calls := 0
	reviewer := auditedReviewFunc(func(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
		calls++
		e := actualProviderEvidence(t, s, false)
		if _, err := store.db.Exec(`CREATE TRIGGER private_provider_fault BEFORE UPDATE ON state BEGIN SELECT RAISE(FAIL,'private_provider_fault'); END`); err != nil {
			t.Fatal(err)
		}
		return protocolReport(s), nil, e, nil
	})
	engine.Reviewers = map[Provider]Reviewer{Codex: reviewer, Claude: reviewer}
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if !errors.Is(err, ErrProviderAuditPending) || out.FailedProviderEvidence == nil || out.ConsumedSessions != 1 {
		t.Fatal("private write failure lost evidence/charge", out, err)
	}
	d, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[job.ID]
	if j.LeaseToken == "" || !j.Outstanding || j.Checkpoint.ConsumedSessions != 1 || len(j.ProviderAttempts) != 0 {
		t.Fatal("recovery state lost", j)
	}
	if _, err = store.db.Exec("DROP TRIGGER private_provider_fault"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = store.RecordStoppedProviderEvidence(context.Background(), job.ID, j.LeaseToken, j.AttemptID, out.FailedProviderEvidence); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.RefundStoppedBeforeWork(context.Background(), job.ID, j.LeaseToken, j.AttemptProvider, j.AttemptRound); !errors.Is(err, ErrFenced) {
		t.Fatal("recorded provider work was refunded", err)
	}
	if err = store.ReleaseStopped(context.Background(), job.ID, j.LeaseToken, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	d, err = store.Snapshot(context.Background())
	if err != nil || calls != 1 || len(d.Jobs[job.ID].ProviderAttempts) != 1 || d.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 {
		t.Fatal("recovery replayed/refunded or lost proof", calls, err)
	}
}

func TestAuditedBeforeWorkFailureWithoutAnyReceiptCanRefund(t *testing.T) {
	store, job, _ := admittedFixture(t)
	engine := testEngine(t, nil)
	reviewer := auditedReviewFunc(func(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error) {
		return Report{}, nil, nil, PhaseError{State: "quota_wait", BeforeWork: true}
	})
	engine.Reviewers = map[Provider]Reviewer{Codex: reviewer, Claude: reviewer}
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if err == nil || out.State != "quota_wait" || out.ConsumedSessions != 0 {
		t.Fatal(out, err)
	}
	d, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[job.ID]
	if j.Outstanding || j.LeaseToken != "" || j.Checkpoint.ConsumedSessions != 0 || len(j.ProviderAttempts) != 0 {
		t.Fatal("trusted no-work failure could not refund", j)
	}
}

func TestPrivateAttemptHistoryPreventsStrippingArtifactAuditFields(t *testing.T) {
	store, out, remote, engine, _ := auditedFixture(t)
	for i := range out.Artifacts {
		out.Artifacts[i].ProviderEvidence = nil
		out.Artifacts[i].ProviderEvidenceRequired = false
	}
	if err := store.update(context.Background(), func(d *DurableState) error {
		d.Jobs[out.Job.ID].Checkpoint = out.Checkpoint()
		d.Jobs[out.Job.ID].ProviderAttempts = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Publisher{Store: store, Remote: remote}).Prepare(context.Background(), out, nil); !errors.Is(err, ErrProviderAuditPending) {
		t.Fatal("stripped private fields allowed publication", err)
	}
	if _, err := store.RecoverCompleted(context.Background(), out.Job.ID, engine.Reader); !errors.Is(err, ErrProviderAuditPending) {
		t.Fatal("stripped cache allowed recovery", err)
	}
}

func TestPreparedReviewRechecksPrivateEvidenceBeforeAnyPost(t *testing.T) {
	store, out, remote, _, physical := auditedFixture(t)
	publisher := Publisher{Store: store, Remote: remote}
	intent, err := publisher.Prepare(context.Background(), out, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	retained := d.Jobs[out.Job.ID].ProviderAttempts
	if err = store.update(context.Background(), func(d *DurableState) error { d.Jobs[out.Job.ID].ProviderAttempts = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = publisher.Dispatch(context.Background(), intent.ID); !errors.Is(err, ErrProviderAuditPending) || remote.posts != 0 {
		t.Fatal("POST used missing private evidence", err, remote.posts)
	}
	d, err = store.Snapshot(context.Background())
	if err != nil || d.Writes[intent.ID].State != "prepared" {
		t.Fatal("blocked intent was dispatched or discarded", err)
	}
	if err = store.update(context.Background(), func(d *DurableState) error { d.Jobs[out.Job.ID].ProviderAttempts = retained; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = publisher.Dispatch(context.Background(), intent.ID); err != nil || remote.posts != 1 || physical.Load() != 8 {
		t.Fatal("receipt restoration caused replay or failed publication", err, remote.posts, physical.Load())
	}
}

func TestAuditedProviderCannotResumeThroughLegacyReviewer(t *testing.T) {
	store, job, _ := admittedFixture(t)
	audited := auditedReviewFunc(func(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error) {
		return Report{}, nil, nil, PhaseError{State: "quota_wait", BeforeWork: true}
	})
	engine := testEngine(t, nil)
	engine.Reviewers = map[Provider]Reviewer{Codex: audited, Claude: audited}
	_, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if err == nil {
		t.Fatal("expected before-work quota failure")
	}
	legacyCalls := 0
	legacy := testEngine(t, func(_ context.Context, s Session) (Report, error) { legacyCalls++; return protocolReport(s), nil })
	out, err := store.RunStopped(context.Background(), job.ID, legacy, func() time.Time { return controlTime.Add(5 * time.Minute) })
	if !errors.Is(err, ErrProviderAuditPending) || legacyCalls != 0 || out.State == "dual_complete" {
		t.Fatal("audited provider downgraded to legacy execution", out.State, err, legacyCalls)
	}
	d, readErr := store.Snapshot(context.Background())
	if readErr != nil || d.Jobs[job.ID].LeaseToken != "" || d.Jobs[job.ID].Outstanding {
		t.Fatal("pre-reservation rejection stranded lease", readErr)
	}
	// A separate PR must still be claimable: leases gate the entire queue.
	scope := alphaScope()
	scope.PR = 2
	request := native(2, controlTime.Add(6*time.Minute))
	request.Snapshot.PR = 2
	now := controlTime.Add(7 * time.Minute)
	if err = store.Discover(context.Background(), Discovery{Scope: scope, Requests: []NativeRequest{request}, Through: now, Complete: true}); err != nil {
		t.Fatal(err)
	}
	other, err := store.Admit(context.Background(), scope, request.ID, job.Release, "evidence", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(context.Background(), other.ID, now); err != nil {
		t.Fatal("downgrade blocked other PR", err)
	}
}

func TestKnownAuditedReviewerRejectsStrippedCacheWithoutReplay(t *testing.T) {
	_, out, _, engine, physical := auditedFixture(t)
	checkpoint := out.Checkpoint()
	checkpoint.Artifacts[0].ProviderEvidenceRequired = false
	checkpoint.Artifacts[0].ProviderEvidence = nil
	failed, err := engine.Run(context.Background(), out.Job, checkpoint)
	if err == nil || failed.State != "invalid_output" || failed.ExecutedSessions != 0 || physical.Load() != 8 {
		t.Fatal("audited cache silently downgraded", failed.State, err, physical.Load())
	}
}

func TestAuditedSelectionSurvivesDailyQuotaRollback(t *testing.T) {
	store, job, _ := admittedFixture(t)
	ctx := context.Background()
	firstDay := controlTime.Add(4 * time.Minute)
	partial := testEngine(t, func(_ context.Context, s Session) (Report, error) {
		if s.Identity.Provider == Claude {
			return Report{}, PhaseError{State: "quota_wait", BeforeWork: true}
		}
		return protocolReport(s), nil
	})
	if out, err := store.RunStopped(ctx, job.ID, partial, func() time.Time { return firstDay }); err == nil || out.ConsumedSessions != 1 {
		t.Fatal("partial fixture", out.State, err)
	}
	nextDay := controlTime.Add(24*time.Hour + 2*time.Minute)
	for i := 2; i <= 5; i++ {
		scope := alphaScope()
		scope.PR = i
		request := native(int64(i), controlTime.Add(24*time.Hour))
		request.Snapshot.PR = i
		if err := store.Discover(ctx, Discovery{Scope: scope, Requests: []NativeRequest{request}, Through: nextDay, Complete: true}); err != nil {
			t.Fatal(err)
		}
		other, err := store.Admit(ctx, scope, request.ID, job.Release, "evidence", nextDay)
		if err != nil {
			t.Fatal(err)
		}
		successful := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil })
		if _, err = store.RunStopped(ctx, other.ID, successful, func() time.Time { return nextDay }); err != nil {
			t.Fatal(err)
		}
	}
	auditedCalls := 0
	audited := auditedReviewFunc(func(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error) {
		auditedCalls++
		return Report{}, nil, nil, errors.New("quota should prevent work")
	})
	engine := testEngine(t, nil)
	engine.Reviewers = map[Provider]Reviewer{Codex: partial.Reviewers[Codex], Claude: audited}
	out, err := store.RunStopped(ctx, job.ID, engine, func() time.Time { return nextDay })
	if err == nil || out.State != "quota_wait" || out.ConsumedSessions != 1 || auditedCalls != 0 {
		t.Fatal("quota charged or launched work", out.State, err, auditedCalls)
	}
	d, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[job.ID]
	if len(j.AuditedProviders) != 1 || j.AuditedProviders[0] != Claude || j.Outstanding || j.LeaseToken != "" {
		t.Fatal("quota rollback erased audited selection", j)
	}
	// Make quota available without changing the captured provider selection.
	if err = store.update(ctx, func(d *DurableState) error {
		for id, other := range d.Jobs {
			if id != job.ID {
				other.ChargeDays = nil
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	legacyCalls := 0
	legacy := testEngine(t, func(_ context.Context, s Session) (Report, error) { legacyCalls++; return protocolReport(s), nil })
	out, err = store.RunStopped(ctx, job.ID, legacy, func() time.Time { return nextDay.Add(time.Minute) })
	if !errors.Is(err, ErrProviderAuditPending) || legacyCalls != 0 || out.State == "dual_complete" {
		t.Fatal("quota retry downgraded audited provider", out.State, err, legacyCalls)
	}
}

func TestPreparedReviewRequiresExactCompletedCheckpointAtDispatch(t *testing.T) {
	for _, kind := range []string{"empty", "truncated", "duplicated", "state", "consumed"} {
		t.Run(kind, func(t *testing.T) {
			store, out, remote, _, physical := auditedFixture(t)
			publisher := Publisher{Store: store, Remote: remote}
			intent, err := publisher.Prepare(context.Background(), out, nil)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := saved.Jobs[out.Job.ID].Checkpoint
			if err = store.update(context.Background(), func(d *DurableState) error {
				j := d.Jobs[out.Job.ID]
				switch kind {
				case "empty":
					j.Checkpoint.Artifacts = nil
				case "truncated":
					j.Checkpoint.Artifacts = j.Checkpoint.Artifacts[:3]
				case "duplicated":
					j.Checkpoint.Artifacts[3] = j.Checkpoint.Artifacts[2]
				case "state":
					j.State = "invalid_output"
				case "consumed":
					j.Checkpoint.ConsumedSessions--
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err = publisher.Dispatch(context.Background(), intent.ID); err == nil || remote.posts != 0 {
				t.Fatal("altered checkpoint published", kind, err)
			}
			current, err := store.Snapshot(context.Background())
			if err != nil || current.Writes[intent.ID].State != "prepared" {
				t.Fatal("blocked intent lost", err)
			}
			if err = store.update(context.Background(), func(d *DurableState) error {
				j := d.Jobs[out.Job.ID]
				j.Checkpoint = checkpoint
				j.State = out.State
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err = publisher.Dispatch(context.Background(), intent.ID); err != nil || remote.posts != 1 || physical.Load() != 8 {
				t.Fatal("restored completion failed or replayed", err, remote.posts, physical.Load())
			}
		})
	}
}
