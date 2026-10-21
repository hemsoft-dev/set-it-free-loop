//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This exercises the real supervisor journal with a fake containment manager,
// not a physical VM qualification.
func evaluationNativePhase(t *testing.T, capability Session) *nativeVMHostPhase {
	t.Helper()
	q := nativeRequestFixture(t, capability.Identity.Provider)
	q.Session = capability
	q.Binding.SessionHash = Digest(capability)
	q.Binding.LaunchID = strings.Repeat("a", 32)
	if !q.valid() {
		t.Fatal("invalid evaluation native request")
	}
	s, manager, _ := systemdSupervisorFixture(t)
	s.payloadKind = "native-vm"
	supervisor := &NativeVMSupervisor{s}
	manager.observation.MemoryMax = 3 << 30
	manager.observation.TasksMax = 128
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
func evaluationNativeReturnedProfile(t *testing.T, p *nativeVMHostPhase) (*AuthStore, *ProviderEvidence) {
	t.Helper()
	ctx := context.Background()
	store := authTestStore(t, p.request.Session.Identity.Provider)
	profile, err := store.LeaseProfile(ctx, p.request.Binding)
	if err != nil {
		t.Fatal(err)
	}
	p.profile = profile
	t.Cleanup(func() { profile.Close() })
	seed, err := profile.ExportAuth(ctx, authExportCheck(func(context.Context, AuthBinding) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	info, err := seed.Info()
	seed.Close()
	if err != nil {
		t.Fatal(err)
	}
	returned, err := NewAuthReturn(info, []byte("synthetic-evaluation-native-return"))
	if err != nil {
		t.Fatal(err)
	}
	p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "invalid_output", SourceAudit: nativeHostAuditFixture(t, p.request)}
	if err = p.supervisor.AbortAndWait(ctx, p.request.Binding); err != nil {
		t.Fatal(err)
	}
	if err = profile.ImportAuthReturn(ctx, returned, p.supervisor); err != nil {
		t.Fatal(err)
	}
	egress, err := NewProviderEgress(ctx, p.request.Session, p.request.Policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("recovery invoked transport")
		return nil, ErrLiveBlocked
	}))
	if err != nil {
		t.Fatal(err)
	}
	access, err := egress.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return store, &ProviderEvidence{p.request.Policy, access}
}
func TestNativeVMEvaluationStoppedEvidenceSurvivesProfileDisposal(t *testing.T) {
	ctx := context.Background()
	p := evaluationNativePhase(t, testSession(Codex))
	store, provider := evaluationNativeReturnedProfile(t, p)
	if _, err := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("missing intent qualified", err)
	}
	if err := p.persistReturnedAuthIntent(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if err := p.profile.Complete(ctx, p.supervisor); err != nil {
		t.Fatal(err)
	}
	if err := p.profile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.directory, "lease.json")); !os.IsNotExist(err) {
		t.Fatal("profile not disposed", err)
	}
	proof, err := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session)
	if err != nil || proof.ReturnedAuthHash != authHash([]byte("synthetic-evaluation-native-return")) || proof.Source == nil || Digest(proof.Provider) != Digest(provider) {
		t.Fatal("durable proof lost", proof, err)
	}
	dir := filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID)
	intentPath := filepath.Join(dir, "auth-return-intent.json")
	original, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(original, []byte("synthetic-evaluation-native-return")) {
		t.Fatal("credentials in containment receipt")
	}
	var intent nativeVMAuthReturnIntent
	if json.Unmarshal(original, &intent) != nil {
		t.Fatal("invalid intent")
	}
	intent.Provider.Access.Stopped = false
	mutated, _ := json.Marshal(intent)
	for _, raw := range [][]byte{append(original, ' '), mutated, []byte(`{"Version":1,"unknown":true}`)} {
		if os.WriteFile(intentPath, raw, 0600) != nil {
			t.Fatal("cannot inject receipt mutation")
		}
		if _, err = p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); !errors.Is(err, ErrEvaluationRecovery) {
			t.Fatal("mutated receipt qualified", err)
		}
	}
	if os.WriteFile(intentPath, original, 0600) != nil {
		t.Fatal("restore receipt")
	}
	sourcePath := filepath.Join(dir, "source-access.json")
	if os.Remove(sourcePath) != nil || os.Symlink(intentPath, sourcePath) != nil {
		t.Fatal("inject symlink")
	}
	if _, err = p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("symlink evidence accepted", err)
	}
}
func TestNativeVMEvaluationIntentWriteFailureKeepsReturnedLease(t *testing.T) {
	ctx := context.Background()
	p := evaluationNativePhase(t, testSession(Codex))
	store, provider := evaluationNativeReturnedProfile(t, p)
	path := filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID, "auth-return-intent.json")
	if os.Mkdir(path, 0700) != nil {
		t.Fatal("inject intent write fault")
	}
	if err := p.persistReturnedAuthIntent(ctx, provider); !errors.Is(err, ErrProviderAuditPending) {
		t.Fatal("lost intent accepted", err)
	}
	if err := p.profile.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverProfile(ctx, p.request.Binding)
	if err != nil {
		t.Fatal("returned lease lost", err)
	}
	defer recovered.Close()
	if recovered.journal.TransferStage != "returned" {
		t.Fatal("returned state discarded")
	}
	if _, err = p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("failed intent qualified", err)
	}
}
func TestEvaluationRunnerRetiresDisposedNativeReturnWithoutReplay(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	_, err := f.journal.store.db.Exec(`CREATE TRIGGER native_result_fault BEFORE UPDATE ON evaluation_execution WHEN instr(CAST(new.data AS TEXT),'"State":"complete"')>0 BEGIN SELECT RAISE(ABORT,'synthetic lost native completion'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal(err)
	}
	if _, err = f.journal.store.db.Exec(`DROP TRIGGER native_result_fault`); err != nil {
		t.Fatal(err)
	}
	record, err := f.journal.LoadExecution(context.Background(), f.corpus.Cases[0].ID, ArmAdversarial, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := evaluationNativePhase(t, *record.Attempts[0].Review)
	store, provider := evaluationNativeReturnedProfile(t, p)
	if err = p.persistReturnedAuthIntent(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err = p.profile.Complete(context.Background(), p.supervisor); err != nil {
		t.Fatal(err)
	}
	if err = p.profile.Close(); err != nil {
		t.Fatal(err)
	}
	proof, err := p.supervisor.StoppedAttemptEvidence(context.Background(), p.request.Binding, p.request.Session)
	if err != nil {
		t.Fatal(err)
	}
	options := EvaluationStoppedRecovery{Binding: p.request.Binding, Auth: store, Stopped: p.supervisor, Source: proof.Source, Evidence: proof.Provider, Check: func(context.Context, EvaluationPlan) error { return nil }}
	if err = f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options); err != nil {
		t.Fatal(err)
	}
	retired, err := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if err != nil || retired.Recovery == nil || retired.Recovery.NativeIntentHash != proof.IntentHash || retired.Recovery.VerifiedAt.IsZero() || retired.Sessions() != 1 || len(retired.Ledger) != 0 || f.calls != 1 {
		t.Fatal("native retirement replayed/refunded or lost proof", retired, err)
	}
}

func TestNativeVMEvaluationHostPersistsIntentBeforeProfileDisposal(t *testing.T) {
	for _, control := range []bool{false, true} {
		for _, failure := range []string{"none", "intent-write", "malformed-source", "wrong-provider-session"} {
			t.Run(fmt.Sprintf("control-%t/failure-%s", control, failure), func(t *testing.T) {
				ctx := context.Background()
				capability := testSession(Codex)
				var input *ControlSession
				if control {
					value := controlInputFixture(Codex)
					input = &value
					var err error
					capability, err = ControlCapabilitySession(value)
					if err != nil {
						t.Fatal(err)
					}
				}
				p := evaluationNativePhase(t, capability)
				if control {
					p.request.Version = 2
					p.request.Task = "evaluation-control-v1"
					p.request.Control = input
					if !p.request.valid() {
						t.Fatal("invalid control request")
					}
				}
				channels, err := openNativeVMChannels()
				if err != nil {
					t.Fatal(err)
				}
				p.channels = channels
				t.Cleanup(func() { channels.Close() })
				store := authTestStore(t, Codex)
				p.profile, err = store.LeaseProfile(ctx, p.request.Binding)
				if err != nil {
					t.Fatal(err)
				}
				seed, err := p.profile.ExportAuth(ctx, authExportCheck(func(context.Context, AuthBinding) error { return nil }))
				if err != nil {
					t.Fatal(err)
				}
				info, err := seed.Info()
				seed.Close()
				if err != nil {
					t.Fatal(err)
				}
				returned, err := NewAuthReturn(info, []byte("synthetic-host-intent-return"))
				if err != nil {
					t.Fatal(err)
				}
				p.exported = true
				p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "invalid_output", AuthReturn: authWire(t, returned), SourceAudit: nativeHostAuditFixture(t, p.request)}
				if failure == "malformed-source" {
					p.finished.SourceAudit = []byte("not a captured source audit")
				}
				guest, host := vmSocketPair(t)
				defer guest.Close()
				bridgeCapability := capability
				if failure == "wrong-provider-session" {
					if strings.HasPrefix(bridgeCapability.JobID, "control_") {
						bridgeCapability.JobID = "control_" + Digest("different provider session")
					} else {
						bridgeCapability.JobID = "different_provider_session"
					}
				}
				p.bridge, err = startNativeProviderBridge(ctx, bridgeCapability, p.request.Policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
					t.Fatal("invalid-output retirement reached upstream")
					return nil, ErrLiveBlocked
				}), host)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "intent-write" {
					path := filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID, "auth-return-intent.json")
					if os.Mkdir(path, 0700) != nil {
						t.Fatal("inject host intent failure")
					}
				}
				result, _, _, err := p.finishTask(ctx, nativeVMTaskReport{}, PhaseError{State: "invalid_output"})
				if err == nil || result.review.JobID != "" || result.control != nil {
					t.Fatal("invalid output exposed report", err)
				}
				if failure != "none" {
					pending := ErrProviderAuditPending
					if failure == "malformed-source" {
						pending = ErrSourceAuditPending
						for _, name := range []string{"auth-return-intent.json", "source-access.json", "native-request.json"} {
							if _, statErr := os.Stat(filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID, name)); !os.IsNotExist(statErr) {
								t.Fatal("malformed source persisted a recovery receipt", name, statErr)
							}
						}
					}
					if p.authReturned || !errors.Is(err, ErrAuthRecoveryRequired) || !errors.Is(err, pending) {
						t.Fatal("host disposed profile after failed intent", p.authReturned, err)
					}
					recovered, recoverErr := store.RecoverProfile(ctx, p.request.Binding)
					if recoverErr != nil {
						t.Fatal("host lost returned lease", recoverErr)
					}
					defer recovered.Close()
					if recovered.journal.TransferStage != "returned" {
						t.Fatal("host did not retain matching stopped return")
					}
				} else {
					proof, proofErr := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, capability)
					if !p.authReturned || proofErr != nil || proof.ReturnedAuthHash != authHash([]byte("synthetic-host-intent-return")) {
						t.Fatal("host disposed profile without durable proof", proofErr)
					}
				}
			})
		}
	}
}

func TestNativeVMEvaluationRecoveryRejectsChangedNativeRequestHash(t *testing.T) {
	ctx := context.Background()
	p := evaluationNativePhase(t, testSession(Codex))
	_, provider := evaluationNativeReturnedProfile(t, p)
	if Digest(p.request) == Digest(p.request.Session) {
		t.Fatal("native request and capability are different contracts")
	}
	if err := p.persistReturnedAuthIntent(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if err := p.profile.Complete(ctx, p.supervisor); err != nil {
		t.Fatal(err)
	}
	if _, err := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); err != nil {
		t.Fatal("actual request hash did not qualify", err)
	}
	path := filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID, "auth-return-intent.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var intent nativeVMAuthReturnIntent
	if json.Unmarshal(raw, &intent) != nil {
		t.Fatal("invalid intent fixture")
	}
	originalIntent := append([]byte{}, raw...)
	intent.RequestHash = Digest("different original native request")
	raw, _ = json.Marshal(intent)
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("cannot mutate request hash")
	}
	if _, err := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("unbound native request hash qualified recovery", err)
	}
	if os.WriteFile(path, originalIntent, 0600) != nil {
		t.Fatal("restore original intent")
	}
	requestPath := filepath.Join(filepath.Dir(path), "native-request.json")
	originalRequest, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	changedRequest := p.request
	changedRequest.Paths = []string{"different.go"}
	changedRaw, _ := json.Marshal(changedRequest)
	for _, mutation := range []struct {
		name string
		raw  []byte
	}{
		{"missing", nil}, {"noncanonical", append(append([]byte{}, originalRequest...), ' ')},
		{"changed-paths", changedRaw}, {"oversized", bytes.Repeat([]byte{' '}, (4<<20)+1)},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if err := os.Remove(requestPath); err != nil {
				t.Fatal(err)
			}
			if mutation.raw != nil && os.WriteFile(requestPath, mutation.raw, 0600) != nil {
				t.Fatal("write request mutation")
			}
			if _, err := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); !errors.Is(err, ErrEvaluationRecovery) {
				t.Fatal("invalid retained request qualified recovery", err)
			}
			if os.WriteFile(requestPath, originalRequest, 0600) != nil {
				t.Fatal("restore retained request")
			}
		})
	}
	if _, err := p.supervisor.StoppedAttemptEvidence(ctx, p.request.Binding, p.request.Session); err != nil {
		t.Fatal("restored original request did not qualify", err)
	}
}
