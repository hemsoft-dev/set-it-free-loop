//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wedgeFixture is a started service whose store holds one Codex attempt left
// leased the way Close during a native session leaves it: a stopped,
// captured fake-manager unit with a stopped audit receipt, recorded stopped
// provider evidence, missing source audit and (by default) an exported,
// unreturned auth lease. Fake manager and kernel only; no VM or provider.
type wedgeFixture struct {
	service *gitReviewerService
	store   *Store
	job     Job
	phase   *nativeVMHostPhase
	manager *fakeContainmentManager
	auth    *AuthStore
	attempt string
	opened  atomic.Int32
}

// auth: "exported" (lost return), "returned" (durable return, not committed),
// "unexported" (leased, never exported) or "none" (never leased).
func newWedgeFixture(t *testing.T, auth string) *wedgeFixture {
	t.Helper()
	return newWedgeFixtureLaunches(t, auth, false)
}

// newWedgeFixtureLaunches with prior first leaves an earlier attempt's
// complete-looking stopped launch for the same session and evidence under the
// same provider root, as a resumed job's retry would.
func newWedgeFixtureLaunches(t *testing.T, auth string, prior bool) *wedgeFixture {
	t.Helper()
	ctx := context.Background()
	o, _, _, _, _ := gitServiceFixture(t)
	f := &wedgeFixture{store: o.Credentials.Journal.store}
	mustCapture(t, f.store, nil, controlTime.Add(2*time.Minute))
	if err := f.store.RecordRequest(ctx, alphaScope(), native(1, controlTime.Add(time.Minute)), true); err != nil {
		t.Fatal(err)
	}
	job, err := f.store.Admit(ctx, alphaScope(), 1, o.Release, "evidence", controlTime.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	f.job = job
	engine := testEngine(t, nil)
	calls := 0
	reviewer := attemptReviewFunc(func(ctx context.Context, s Session, attempt string) (Report, *SourceAccess, *ProviderEvidence, error) {
		calls++
		if calls > 1 {
			t.Error("wedged attempt was followed by another session")
			return Report{}, nil, nil, ErrLiveBlocked
		}
		systemd, manager, _ := systemdSupervisorFixture(t)
		systemd.payloadKind = "native-vm"
		supervisor := &NativeVMSupervisor{systemd}
		if prior {
			old := wedgeNativeLaunch(t, supervisor, manager, s, strings.Repeat("c", 32))
			old.attempt = strings.Repeat("e", 32)
			if err := old.supervisor.AbortAndWait(ctx, old.request.Binding); err != nil {
				t.Fatal(err)
			}
			if err := old.persistAudit(ctx, nil, nil, wedgeEvidence(t, ctx, s, old.request.Policy), errors.Join(context.Canceled, ErrSourceAuditPending)); err != nil {
				t.Fatal(err)
			}
		}
		p := wedgeNativeLaunch(t, supervisor, manager, s, strings.Repeat("a", 32))
		p.attempt = attempt
		f.phase = p
		f.manager = nil
		f.auth = authTestStore(t, s.Identity.Provider)
		if auth == "none" {
			if err := p.supervisor.AbortAndWait(ctx, p.request.Binding); err != nil {
				t.Fatal(err)
			}
			evidence := wedgeEvidence(t, ctx, s, p.request.Policy)
			failure := errors.Join(context.Canceled, ErrSourceAuditPending)
			if err := p.persistAudit(ctx, nil, nil, evidence, failure); err != nil {
				t.Fatal(err)
			}
			return Report{}, nil, evidence, errors.Join(PhaseError{State: "cancelled"}, failure)
		}
		profile, err := f.auth.LeaseProfile(ctx, p.request.Binding)
		if err != nil {
			t.Fatal(err)
		}
		p.profile = profile
		if auth != "unexported" {
			seed, err := profile.ExportAuth(ctx, authExportCheck(func(context.Context, AuthBinding) error { return nil }))
			if err != nil {
				t.Fatal(err)
			}
			p.exported = true
			info, _ := seed.Info()
			seed.Close()
			if auth == "returned" {
				if err = p.supervisor.AbortAndWait(ctx, p.request.Binding); err != nil {
					t.Fatal(err)
				}
				returned, err := NewAuthReturn(info, []byte("synthetic-wedge-return"))
				if err != nil || profile.ImportAuthReturn(ctx, returned, p.supervisor) != nil {
					t.Fatal("stage durable return", err)
				}
			}
		}
		if err = p.supervisor.AbortAndWait(ctx, p.request.Binding); err != nil {
			t.Fatal(err)
		}
		evidence := wedgeEvidence(t, ctx, s, p.request.Policy)
		failure := errors.Join(context.Canceled, ErrAuthRecoveryRequired, ErrSourceAuditPending)
		if err = p.persistAudit(ctx, nil, nil, evidence, failure); err != nil {
			t.Fatal(err)
		}
		profile.Close()
		return Report{}, nil, evidence, errors.Join(PhaseError{State: "cancelled"}, failure)
	})
	engine.Reviewers = map[Provider]Reviewer{Codex: reviewer, Claude: reviewer}
	_, runErr := f.store.RunStopped(ctx, job.ID, engine, func() time.Time { return controlTime.Add(4 * time.Minute) })
	d, err := f.store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[job.ID]
	if !errors.Is(runErr, ErrSourceAuditPending) || j.LeaseToken == "" || !j.Outstanding || len(j.ProviderAttempts) != 1 || j.State != "running" {
		t.Fatal("fixture did not wedge the attempt", runErr, j.State)
	}
	f.attempt = j.AttemptID
	var checks, providerCalls atomic.Int32
	codex := gitServiceNativeOptions(Codex, &checks, &providerCalls)
	s := f.phase.supervisor.supervisor
	codex.Root, codex.Stub, codex.StubHash, codex.Auth = s.root, s.stub, s.stubHash, f.auth
	o.Runtime = nil
	o.NativeReleases = map[string]map[Provider]NativeVMOptions{o.Release: {Codex: codex, Claude: gitServiceNativeOptions(Claude, &checks, &providerCalls)}}
	service, err := newGitReviewerService(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close(context.Background()) })
	// Recovery reopens the original root as a restarted process would.
	service.openNative = func(_ context.Context, options NativeVMOptions) (*NativeVMSupervisor, error) {
		f.opened.Add(1)
		if options.Root != s.root || options.StubHash != s.stubHash {
			t.Error("recovery opened another provider root")
		}
		reopened := newSystemdSupervisor(options.Root, options.Stub, options.StubHash, s.manager, s.kernel)
		reopened.payloadKind = "native-vm"
		return &NativeVMSupervisor{reopened}, nil
	}
	f.service = service
	return f
}

// attemptReviewFunc receives the durable attempt ID like NativeVMReviewer.
type attemptReviewFunc func(context.Context, Session, string) (Report, *SourceAccess, *ProviderEvidence, error)

func (f attemptReviewFunc) ReviewAudited(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
	return f(ctx, s, "")
}
func (f attemptReviewFunc) reviewAttemptAudited(ctx context.Context, s Session, attempt string) (Report, *SourceAccess, *ProviderEvidence, error) {
	return f(ctx, s, attempt)
}
func (f attemptReviewFunc) Review(context.Context, Session) (Report, error) {
	return Report{}, errors.New("unguarded reviewer entrypoint")
}

func (f *wedgeFixture) request() ProviderRecoveryRequest {
	return ProviderRecoveryRequest{JobID: f.job.ID, AttemptID: f.attempt, LaunchID: f.phase.request.Binding.LaunchID, RetireUnreturnedAuth: true}
}

func (f *wedgeFixture) launchDir() string {
	return filepath.Join(f.phase.supervisor.supervisor.root, f.phase.request.Binding.LaunchID)
}

// requireWedged asserts nothing durable changed: same lease and attempt,
// no recovery receipt and the auth lease still held in its journal stage.
func (f *wedgeFixture) requireWedged(t *testing.T, token, stage string) {
	t.Helper()
	d, err := f.store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[f.job.ID]
	if j.LeaseToken != token || !j.Outstanding || j.AttemptID != f.attempt || j.State != "running" || len(j.ProviderRecoveries) != 0 || j.Checkpoint.ConsumedSessions != 1 {
		t.Fatal("refused recovery changed the durable job", j.State)
	}
	raw, err := os.ReadFile(filepath.Join(f.auth.directory, "lease.json"))
	var journal authJournal
	if err != nil || json.Unmarshal(raw, &journal) != nil || journal.TransferStage != stage || journal.Binding != f.phase.request.Binding {
		t.Fatal("refused recovery changed the auth fence", err)
	}
	if _, err = os.Stat(filepath.Join(f.auth.directory, "state.json")); err != nil {
		t.Fatal("refused recovery removed canonical auth", err)
	}
}

func (f *wedgeFixture) token(t *testing.T) string {
	t.Helper()
	d, err := f.store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d.Jobs[f.job.ID].LeaseToken
}

func TestProviderRecoveryRetiresLostAuthAndReleasesChargedLease(t *testing.T) {
	ctx := context.Background()
	f := newWedgeFixture(t, "exported")
	if w := f.service.controller.selectWork(DurableState{Jobs: map[string]*DurableJob{f.job.ID: {LeaseToken: "held"}}}, nil); w != nil {
		t.Fatal("leased state admitted work")
	}
	candidates, err := f.service.ProviderRecoveryCandidates(ctx)
	if err != nil || len(candidates) != 1 || candidates[0].JobID != f.job.ID || candidates[0].AttemptID != f.attempt || !candidates[0].ProviderEvidenceRecorded || len(candidates[0].Launches) != 1 || candidates[0].Launches[0] != f.phase.request.Binding.LaunchID {
		t.Fatal("candidate view does not identify the wedged attempt", candidates, err)
	}
	result, err := f.service.RecoverStoppedProvider(ctx, f.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.Auth != ProviderAuthRetired || result.State != "auth_failed" || result.Provider != Codex || result.Round != 1 || result.AttemptID != f.attempt {
		t.Fatal("unexpected recovery result", result)
	}
	d, err := f.store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[f.job.ID]
	if j.LeaseToken != "" || j.Outstanding || j.State != "auth_failed" || j.Checkpoint.ConsumedSessions != 1 || len(j.Checkpoint.Artifacts) != 0 || len(j.SourceAttempts) != 0 || len(j.ChargeDays) != 1 {
		t.Fatal("recovered lease was not released as a charged, incomplete attempt", j.State, j.Checkpoint.ConsumedSessions)
	}
	if len(j.ProviderRecoveries) != 1 || j.ProviderRecoveries[0].Auth != ProviderAuthRetired || j.ProviderRecoveries[0].LaunchID != f.phase.request.Binding.LaunchID || j.ProviderRecoveries[0].EvidenceHash != Digest(cloneProviderEvidence(j.ProviderAttempts[0].Evidence)) {
		t.Fatal("recovery receipt missing or unbound", j.ProviderRecoveries)
	}
	for _, name := range []string{"lease.json", "state.json", "profile", "pending.json"} {
		if _, err := os.Stat(filepath.Join(f.auth.directory, name)); !os.IsNotExist(err) {
			t.Fatal("lost exported auth was not retired", name, err)
		}
	}
	// The retired seed is never reused: the next lease has no auth to read.
	if _, err := f.auth.LeaseProfile(ctx, authTestBinding()); err == nil {
		t.Fatal("retired auth could still be leased")
	}
	// Only an explicit resume can requeue the charged job; nothing reviewed it.
	if err = f.store.ResumeUnavailable(ctx, f.job.ID, controlTime.Add(5*time.Minute)); err != nil {
		t.Fatal("documented resume path refused the recovered job", err)
	}
	if _, err = f.service.RecoverStoppedProvider(ctx, f.request()); !errors.Is(err, ErrFenced) {
		t.Fatal("repeated recovery was accepted", err)
	}
	if err = f.service.Close(ctx); err != nil {
		t.Fatal("service could not close after recovery", err)
	}
}

func TestProviderRecoveryFinishesDurableReturnAndUnexportedLease(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ mode, auth, state, canonical string }{
		{"returned", ProviderAuthReturned, "provider_unavailable", "synthetic-wedge-return"},
		{"unexported", ProviderAuthUnexported, "provider_unavailable", "synthetic-seed"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			f := newWedgeFixture(t, c.mode)
			request := f.request()
			request.RetireUnreturnedAuth = false
			result, err := f.service.RecoverStoppedProvider(ctx, request)
			if err != nil || result.Auth != c.auth || result.State != c.state {
				t.Fatal("recovery did not settle auth by the stopped-worker rules", result, err)
			}
			raw, err := os.ReadFile(filepath.Join(f.auth.directory, "state.json"))
			if err != nil || string(raw) != c.canonical {
				t.Fatal("canonical auth is not the expected stopped state", err)
			}
			if _, err = os.Stat(filepath.Join(f.auth.directory, "lease.json")); !os.IsNotExist(err) {
				t.Fatal("auth lease remains", err)
			}
			d, _ := f.store.Snapshot(ctx)
			if j := d.Jobs[f.job.ID]; j.LeaseToken != "" || j.Outstanding || j.State != c.state || j.Checkpoint.ConsumedSessions != 1 {
				t.Fatal("lease not released as provider_unavailable", j.State)
			}
		})
	}
}

// A resumed job's retry has the same session hash and stopped evidence as
// its earlier attempt, so only the attempt ID binds a launch to the lease.
func TestProviderRecoveryRefusesAnotherAttemptsLaunch(t *testing.T) {
	ctx := context.Background()
	f := newWedgeFixtureLaunches(t, "none", true)
	token := f.token(t)
	request := f.request()
	request.LaunchID = strings.Repeat("c", 32)
	// The fake kernel proves either launch's original cgroup empty, as a real
	// kernel does once a stopped unit's cgroup is gone.
	kernel := f.phase.supervisor.supervisor.manager.(*fakeContainmentManager).kernel
	current := kernel.group
	kernel.group.Path = "/user.slice/user-1000.slice/sfl-alpha-" + request.LaunchID + ".service"
	result, err := f.service.RecoverStoppedProvider(ctx, request)
	if !errors.Is(err, ErrProviderRecovery) {
		t.Fatal("recovery accepted an earlier attempt's launch", result, err)
	}
	kernel.group = current
	d, _ := f.store.Snapshot(ctx)
	if j := d.Jobs[f.job.ID]; j.LeaseToken != token || !j.Outstanding || j.AttemptID != f.attempt || j.State != "running" || len(j.ProviderRecoveries) != 0 {
		t.Fatal("refused recovery changed the durable job", j.State)
	}
	candidates, err := f.service.ProviderRecoveryCandidates(ctx)
	if err != nil || len(candidates) != 1 || len(candidates[0].Launches) != 1 || candidates[0].Launches[0] != f.phase.request.Binding.LaunchID {
		t.Fatal("candidate view offers another attempt's launch", candidates, err)
	}
	result, err = f.service.RecoverStoppedProvider(ctx, f.request())
	if err != nil || result.Auth != ProviderAuthNone || result.State != "provider_unavailable" || result.LaunchID != f.phase.request.Binding.LaunchID {
		t.Fatal("recovery refused the attempt's own launch", result, err)
	}
}

// A retry after an unexported lease's Complete was interrupted past its
// commit point must still report that no auth was exported or returned.
func TestProviderRecoveryKeepsUnexportedAcrossInterruptedCommit(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"committing", "committed"} {
		t.Run(stage, func(t *testing.T) {
			f := newWedgeFixture(t, "unexported")
			profile, err := f.auth.RecoverProfile(ctx, f.phase.request.Binding)
			if err != nil {
				t.Fatal(err)
			}
			r := profile.held.root
			next, err := readAuth(r, profile.store.authRelative(), false)
			if err == nil {
				err = writeAuth(r, "pending.json", next)
			}
			j := profile.journal
			j.Stage, j.NextHash = "committing", authHash(next)
			if err == nil {
				err = writeAuthJournal(r, j)
			}
			if err == nil && stage == "committed" {
				j.Stage = "committed"
				if err = writeAuth(r, "state.json", next); err == nil {
					err = writeAuthJournal(r, j)
				}
			}
			if err != nil || profile.Close() != nil {
				t.Fatal("stage interrupted commit", err)
			}
			request := f.request()
			request.RetireUnreturnedAuth = false
			result, err := f.service.RecoverStoppedProvider(ctx, request)
			if err != nil || result.Auth != ProviderAuthUnexported || result.State != "provider_unavailable" {
				t.Fatal("interrupted unexported commit reported as another auth history", result, err)
			}
			d, _ := f.store.Snapshot(ctx)
			if recoveries := d.Jobs[f.job.ID].ProviderRecoveries; len(recoveries) != 1 || recoveries[0].Auth != ProviderAuthUnexported {
				t.Fatal("recovery receipt misstates the auth history", recoveries)
			}
			if raw, err := os.ReadFile(filepath.Join(f.auth.directory, "state.json")); err != nil || string(raw) != "synthetic-seed" {
				t.Fatal("canonical auth is not the unchanged seed", err)
			}
			if _, err = os.Stat(filepath.Join(f.auth.directory, "lease.json")); !os.IsNotExist(err) {
				t.Fatal("auth lease remains", err)
			}
		})
	}
}

func TestProviderRecoveryRefusesWithoutDurableChange(t *testing.T) {
	ctx := context.Background()
	rewriteAudit := func(t *testing.T, f *wedgeFixture, change func(*nativeVMAuditReceipt)) {
		path := filepath.Join(f.launchDir(), "native-audit.json")
		raw, err := os.ReadFile(path)
		var receipt nativeVMAuditReceipt
		if err != nil || json.Unmarshal(raw, &receipt) != nil {
			t.Fatal(err)
		}
		change(&receipt)
		raw, _ = json.Marshal(receipt)
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name   string
		mutate func(*testing.T, *wedgeFixture, *ProviderRecoveryRequest)
		want   error
	}{
		{"wrong-job", func(_ *testing.T, _ *wedgeFixture, q *ProviderRecoveryRequest) { q.JobID = "native-1-2" }, ErrFenced},
		{"wrong-attempt", func(_ *testing.T, _ *wedgeFixture, q *ProviderRecoveryRequest) { q.AttemptID = strings.Repeat("0", 32) }, ErrFenced},
		{"malformed-launch", func(_ *testing.T, _ *wedgeFixture, q *ProviderRecoveryRequest) { q.LaunchID = "../" + q.LaunchID }, ErrFenced},
		{"other-launch", func(_ *testing.T, _ *wedgeFixture, q *ProviderRecoveryRequest) { q.LaunchID = strings.Repeat("b", 32) }, ErrProviderRecovery},
		{"no-retire-consent", func(_ *testing.T, _ *wedgeFixture, q *ProviderRecoveryRequest) { q.RetireUnreturnedAuth = false }, ErrAuthRecoveryRequired},
		{"missing-audit", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			if err := os.Remove(filepath.Join(f.launchDir(), "native-audit.json")); err != nil {
				t.Fatal(err)
			}
		}, ErrProviderRecovery},
		{"audit-claims-complete", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			rewriteAudit(t, f, func(r *nativeVMAuditReceipt) { r.Complete, r.Failure = true, "" })
		}, ErrProviderRecovery},
		{"audit-for-other-attempt", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			rewriteAudit(t, f, func(r *nativeVMAuditReceipt) { r.AttemptID = strings.Repeat("e", 32) })
		}, ErrProviderRecovery},
		{"audit-without-attempt", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			rewriteAudit(t, f, func(r *nativeVMAuditReceipt) { r.Version, r.AttemptID = 1, "" })
		}, ErrProviderRecovery},
		{"audit-evidence-differs", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			rewriteAudit(t, f, func(r *nativeVMAuditReceipt) { r.Provider.Access.ResponseBytes++ })
		}, ErrProviderRecovery},
		{"audit-hides-export", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			rewriteAudit(t, f, func(r *nativeVMAuditReceipt) { r.AuthExported = false })
		}, ErrAuthRecoveryRequired},
		{"unit-not-proven-stopped", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			if err := f.phase.supervisor.supervisor.updateUnit(ctx, f.phase.request.Binding, func(u *unitJournal) error { u.Stopped = false; return nil }); err != nil {
				t.Fatal(err)
			}
		}, ErrProviderRecovery},
		{"fresh-stop-proof-fails", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			// The original cgroup is still populated: no fresh stop proof.
			f.phase.supervisor.supervisor.manager.(*fakeContainmentManager).kernel.populated = true
		}, ErrAuthRecoveryRequired},
		{"unrecorded-provider-evidence", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			if err := f.store.update(ctx, func(d *DurableState) error { d.Jobs[f.job.ID].ProviderAttempts = nil; return nil }); err != nil {
				t.Fatal(err)
			}
		}, ErrProviderRecovery},
		{"auth-lease-for-other-launch", func(t *testing.T, f *wedgeFixture, _ *ProviderRecoveryRequest) {
			path := filepath.Join(f.auth.directory, "lease.json")
			raw, _ := os.ReadFile(path)
			var j authJournal
			if json.Unmarshal(raw, &j) != nil {
				t.Fatal("journal")
			}
			j.Binding = authTestBinding()
			raw, _ = json.Marshal(j)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}, ErrAuthRecoveryRequired},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newWedgeFixture(t, "exported")
			token := f.token(t)
			request := f.request()
			c.mutate(t, f, &request)
			_, err := f.service.RecoverStoppedProvider(ctx, request)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if c.name == "auth-lease-for-other-launch" {
				d, _ := f.store.Snapshot(ctx)
				if j := d.Jobs[f.job.ID]; j.LeaseToken != token || !j.Outstanding || len(j.ProviderRecoveries) != 0 {
					t.Fatal("refused recovery changed the durable job")
				}
				return
			}
			stage := "exported"
			if c.name == "unrecorded-provider-evidence" {
				d, _ := f.store.Snapshot(ctx)
				if j := d.Jobs[f.job.ID]; j.LeaseToken != token || !j.Outstanding || len(j.ProviderRecoveries) != 0 {
					t.Fatal("refused recovery changed the durable job")
				}
				return
			}
			f.requireWedged(t, token, stage)
		})
	}
}

func TestProviderRecoveryRequiresIdleServiceAndOperatorInvocation(t *testing.T) {
	ctx := context.Background()
	f := newWedgeFixture(t, "exported")
	token := f.token(t)
	// Without an operator call nothing changes: Close keeps the fence.
	if err := f.service.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.service.Run(run) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.service.mu.Lock()
		ran := f.service.ran
		f.service.mu.Unlock()
		if ran || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.service.RecoverStoppedProvider(ctx, f.request()); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("recovery ran while the service was running", err)
	}
	if _, err := f.service.ProviderRecoveryCandidates(ctx); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("candidate scan ran while the service was running", err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := f.service.Close(ctx); !errors.Is(err, ErrWorkerStopPending) {
		t.Fatal("Close released an uncertain lease", err)
	}
	cancel()
	<-done
	if _, err := f.service.RecoverStoppedProvider(ctx, f.request()); !errors.Is(err, ErrFenced) && !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("recovery ran on a closing service", err)
	}
	if f.opened.Load() != 0 {
		t.Fatal("refused recovery opened the provider supervisor")
	}
	f.requireWedged(t, token, "exported")
}

func TestProviderRecoveryStoreReleaseBindsAttemptAndEvidence(t *testing.T) {
	ctx := context.Background()
	f := newWedgeFixture(t, "exported")
	token := f.token(t)
	d, _ := f.store.Snapshot(ctx)
	evidence := Digest(cloneProviderEvidence(d.Jobs[f.job.ID].ProviderAttempts[0].Evidence))
	good := ProviderRecoveryReceipt{AttemptID: f.attempt, LaunchID: f.phase.request.Binding.LaunchID, Provider: Codex, Round: 1, EvidenceHash: evidence, Auth: ProviderAuthRetired, State: "auth_failed"}
	for name, change := range map[string]func(*ProviderRecoveryReceipt){
		"reviewed-state": func(r *ProviderRecoveryReceipt) { r.State = "dual_complete" },
		"requeue-state":  func(r *ProviderRecoveryReceipt) { r.State = "queued" },
		"attempt":        func(r *ProviderRecoveryReceipt) { r.AttemptID = "other" },
		"provider":       func(r *ProviderRecoveryReceipt) { r.Provider = Claude },
		"round":          func(r *ProviderRecoveryReceipt) { r.Round = 2 },
		"evidence":       func(r *ProviderRecoveryReceipt) { r.EvidenceHash = Digest("other") },
	} {
		receipt := good
		change(&receipt)
		if err := f.store.releaseRecoveredProviderAttempt(ctx, f.job.ID, token, receipt, controlTime); err == nil {
			t.Fatal("store released a mismatched recovery", name)
		}
	}
	if err := f.store.releaseRecoveredProviderAttempt(ctx, f.job.ID, "other-token", good, controlTime); !errors.Is(err, ErrFenced) {
		t.Fatal("store released under another lease nonce", err)
	}
	f.requireWedged(t, token, "exported")
}

// wedgeEvidence is the stopped provider evidence of a session that made no
// provider request.
func wedgeEvidence(t *testing.T, ctx context.Context, s Session, policy EgressPolicy) *ProviderEvidence {
	t.Helper()
	egress, err := NewProviderEgress(ctx, s, policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("wedge fixture reached a provider")
		return nil, ErrLiveBlocked
	}))
	if err != nil {
		t.Fatal(err)
	}
	access, err := egress.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &ProviderEvidence{policy, access}
}

// wedgeNativeLaunch is evaluationNativePhase with the production launch
// binding derivation, so recovery can rederive it from the durable session.
func wedgeNativeLaunch(t *testing.T, supervisor *NativeVMSupervisor, manager *fakeContainmentManager, capability Session, launch string) *nativeVMHostPhase {
	t.Helper()
	q := nativeRequestFixture(t, capability.Identity.Provider)
	q.Session = capability
	q.Binding = nativeLaunchBinding(Digest(capability), launch)
	if !q.valid() {
		t.Fatal("invalid wedge native request")
	}
	s := supervisor.supervisor
	manager.observation.ActiveState = "active"
	manager.observation.MainPID = uint32(os.Getpid())
	manager.kernel.populated = true
	manager.observation.MemoryMax = 3 << 30
	manager.observation.TasksMax = 128
	manager.observation.Description = payloadDescription("native-vm", q.Binding)
	manager.kernel.group.Path = "/user.slice/user-1000.slice/sfl-alpha-" + q.Binding.LaunchID + ".service"
	manager.observation.ControlGroup = manager.kernel.group.Path
	spec := vmSpec(t)
	spec.Binding = q.Binding
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { images.Close() })
	manifest, err := images.NativeManifest(q.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Prepare(manifest); err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Start(context.Background(), q.Binding); err != nil {
		t.Fatal(err)
	}
	unit, err := s.snapshotUnit(context.Background(), q.Binding)
	if err != nil {
		t.Fatal(err)
	}
	gate := s.gate(q.Binding)
	held, err := gate.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := gate.read(held)
	if err == nil {
		record.Phase, record.Identity = "granted", unit.Identity
		err = gate.write(held, record)
	}
	held.close()
	if err != nil {
		t.Fatal(err)
	}
	return &nativeVMHostPhase{request: q, manifest: manifest, supervisor: supervisor, started: true}
}
