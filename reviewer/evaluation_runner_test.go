//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type evaluationControlFunc func(context.Context, ControlSession) (ControlReport, *SourceAccess, *ProviderEvidence, error)

func (f evaluationControlFunc) Critique(ctx context.Context, s ControlSession) (ControlReport, error) {
	r, _, _, err := f(ctx, s)
	return r, err
}
func (f evaluationControlFunc) CritiqueAudited(ctx context.Context, s ControlSession) (ControlReport, *SourceAccess, *ProviderEvidence, error) {
	return f(ctx, s)
}
func evaluationTestAccess(s Session) *SourceAccess {
	return &SourceAccess{Version: 1, SessionHash: Digest(s), Reads: []ReadRecord{}, Coverage: []Coverage{}}
}

type evaluationRunnerFixture struct {
	journal                   *EvaluationJournal
	options                   EvaluationRunnerOptions
	corpus                    EvaluationCorpus
	plan                      EvaluationPlan
	path                      string
	calls, sources, factories int
	order                     []EvaluationArm
	inputs                    []string
	fail                      func(int, Session) error
}

func newEvaluationRunnerFixture(t *testing.T) *evaluationRunnerFixture {
	t.Helper()
	corpus := syntheticCorpus()
	for i := range corpus.Cases {
		corpus.Cases[i].Label.Rationale = "SECRET_GROUND_TRUTH_DO_NOT_SEND"
	}
	plan := syntheticPlan(corpus)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "evaluation.db")
	journal, err := OpenEvaluationJournal(path, alphaEnrollment(), corpus, plan)
	if err != nil {
		t.Fatal(err)
	}
	f := &evaluationRunnerFixture{journal: journal, corpus: corpus, plan: plan, path: path, order: []EvaluationArm{}, inputs: []string{}}
	f.options = EvaluationRunnerOptions{Journal: journal, Check: func(context.Context, EvaluationPlan) error { return nil }, Source: func(_ context.Context, request EvaluationSourceRequest) (EvaluationSource, error) {
		f.sources++
		evidence := &SnapshotEvidence{store: &fixtureStore{files: map[string]Object{"source.go": {Mode: "100644", Data: []byte("original source")}}}, snapshot: request.Snapshot, packet: "synthetic immutable evidence", paths: []string{"source.go"}, coverage: []Coverage{}, lines: map[string]map[int]bool{"source.go": {1: true}}}
		return EvaluationSource{Packet: evidence.Packet(), Bind: func(_ context.Context, job Job) (*JobEvidence, error) { return evidence.Bind(job) }}, nil
	}, Workers: func(_ context.Context, arm EvaluationArm, job Job, bound *JobEvidence) (EvaluationWorkers, error) {
		f.factories++
		f.order = append(f.order, arm)
		check := func(s Session) error {
			f.calls++
			f.inputs = append(f.inputs, Digest(s))
			raw, _ := json.Marshal(s)
			if strings.Contains(string(raw), "SECRET_GROUND_TRUTH_DO_NOT_SEND") {
				t.Fatal("label leaked into input")
			}
			var saved *EvaluationExecution
			for _, test := range f.corpus.Cases {
				if test.Snapshot == job.Snapshot {
					for repeat := 0; repeat < f.plan.Repeats; repeat++ {
						record, err := f.journal.LoadExecution(context.Background(), test.ID, arm, repeat)
						if err != nil {
							t.Fatal(err)
						}
						if record != nil && record.Job.ID == job.ID {
							saved = record
							break
						}
					}
				}
			}
			if saved == nil || len(saved.Attempts) == 0 || saved.Attempts[len(saved.Attempts)-1].State != "inflight" || !saved.Attempts[len(saved.Attempts)-1].Charged {
				t.Fatal("worker started before durable reservation")
			}
			if saved.Attempts[len(saved.Attempts)-1].CapabilityHash != Digest(s) {
				t.Fatal("reservation captured different input")
			}
			if f.fail != nil {
				return f.fail(f.calls, s)
			}
			return nil
		}
		if arm == ArmCodexControl || arm == ArmClaudeControl {
			provider := arm.Schedule()[0]
			return EvaluationWorkers{Controls: map[Provider]AuditedControlReviewer{provider: evaluationControlFunc(func(_ context.Context, s ControlSession) (ControlReport, *SourceAccess, *ProviderEvidence, error) {
				capability, err := ControlCapabilitySession(s)
				if err != nil {
					t.Fatal(err)
				}
				failure := check(capability)
				evidence := actualProviderEvidence(t, capability, false)
				report := fakeControl(s)
				report.Usage = Usage{Known: true, Turns: 1, InputTokens: 2, OutputTokens: 3}
				return report, evaluationTestAccess(capability), evidence, failure
			})}}, nil
		}
		workers := EvaluationWorkers{Reviews: map[Provider]AuditedReviewer{}}
		for _, provider := range job.Config.Providers() {
			workers.Reviews[provider] = auditedReviewFunc(func(_ context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
				failure := check(s)
				report := protocolReport(s)
				report.Usage = Usage{Known: true, Turns: 1, InputTokens: 2, OutputTokens: 3}
				return report, evaluationTestAccess(s), actualProviderEvidence(t, s, false), failure
			})
		}
		return workers, nil
	}}
	t.Cleanup(func() {
		if err := f.journal.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}
func TestEvaluationRunnerSixNaturalArmsAndRestartWithoutReplay(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	out, err := RunEvaluation(context.Background(), f.options)
	if err != nil || out.State != "execution_complete" || len(out.Executions) != 120 || out.ExecutedSessions != 320 || f.calls != 320 || f.factories != 120 || f.sources != 20 {
		t.Fatal("natural execution", out.State, len(out.Executions), out.ExecutedSessions, f.calls, f.factories, f.sources, err)
	}
	for caseIndex := range f.corpus.Cases {
		for i, arm := range f.plan.Order(caseIndex, 0) {
			record := out.Executions[caseIndex*6+i]
			if record.Arm != arm || len(record.Attempts) != arm.Sessions() || record.Sessions() != arm.Sessions() || f.order[caseIndex*6+i] != arm {
				t.Fatal("schedule changed", caseIndex, arm)
			}
		}
	}
	before := f.calls
	if err = f.journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenEvaluationJournal(f.path, alphaEnrollment(), f.corpus, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	f.journal = reopened
	f.options.Journal = reopened
	again, err := RunEvaluation(context.Background(), f.options)
	if err != nil || again.ReusedArms != 120 || again.ExecutedSessions != 0 || f.calls != before || f.factories != 120 || f.sources != 20 {
		t.Fatal("restart replayed retained work", again.State, again.ReusedArms, f.calls, err)
	}
	if Digest(out.Executions) != Digest(again.Executions) {
		t.Fatal("restart changed retained execution")
	}
}
func TestEvaluationRunnerDefersIndependentActionability(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	f.options.Workers = func(ctx context.Context, arm EvaluationArm, job Job, bound *JobEvidence) (EvaluationWorkers, error) {
		return EvaluationWorkers{}, ErrLiveBlocked
	}
	// No automatic labels or scoreable results are created by opening a journal.
	if _, err := f.journal.Assess(context.Background(), f.corpus.Cases[0].ID, ArmAdversarial, 0, nil, time.Now()); err == nil {
		t.Fatal("missing execution became scoreable")
	}
	if _, err := RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatal("denied factory invoked worker")
	}
}

func TestEvaluationRunnerPostRunAssessmentUsesRetainedLedger(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	factory := f.options.Workers
	f.options.Workers = func(ctx context.Context, arm EvaluationArm, job Job, bound *JobEvidence) (EvaluationWorkers, error) {
		if job.Snapshot.PR != 1 {
			return EvaluationWorkers{}, ErrLiveBlocked
		}
		return factory(ctx, arm, job, bound)
	}
	out, err := RunEvaluation(context.Background(), f.options)
	if !errors.Is(err, ErrLiveBlocked) || len(out.Executions) != 6 || f.calls != 16 {
		t.Fatal("bounded synthetic case", len(out.Executions), f.calls, err)
	}
	for _, record := range out.Executions {
		if _, err = f.journal.Assess(context.Background(), record.CaseID, record.Arm, record.Repeat, nil, time.Now().UTC()); err == nil {
			t.Fatal("execution generated its own actionability labels")
		}
		at := time.Now().UTC()
		labels := []FindingLabel{}
		for _, entry := range record.Ledger {
			labels = append(labels, FindingLabel{FindingID: entry.Finding.ID, Assessment: FindingAssessment{Rationale: "synthetic independent source assessment only", Provenance: syntheticLabelProvenance(at), DefectIDs: []string{}}, History: []FindingAssessment{}})
		}
		result, err := f.journal.Assess(context.Background(), record.CaseID, record.Arm, record.Repeat, labels, at)
		if err != nil || result.Sessions != record.Arm.Sessions() || result.ReportHash != record.Hash() || result.Turns != record.Arm.Sessions() || !result.UsageKnown || result.InputTokens != int64(2*record.Arm.Sessions()) {
			t.Fatal("assessment lost actual execution", result, err)
		}
		labels[0].Assessment.Provenance.Evidence[0].Kind = "review"
		if _, err = f.journal.Assess(context.Background(), record.CaseID, record.Arm, record.Repeat, labels, at); err == nil {
			t.Fatal("evaluated review output became label evidence")
		}
	}
}
func TestEvaluationRunnerUnknownEvidenceFencesRestart(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	f.fail = func(n int, s Session) error {
		if n == 1 {
			return errors.Join(PhaseError{State: "provider_unavailable"}, ErrWorkerStopPending)
		}
		return nil
	}
	out, err := RunEvaluation(context.Background(), f.options)
	if !errors.Is(err, ErrEvaluationRecovery) || f.calls != 1 || len(out.Executions) != 0 {
		t.Fatal("unknown stop advanced", f.calls, err)
	}
	record, err := f.journal.LoadExecution(context.Background(), f.corpus.Cases[0].ID, ArmAdversarial, 0)
	if err != nil || record == nil || record.Sessions() != 1 || len(record.Ledger) != 0 || !record.Attempts[0].Unsafe {
		t.Fatal("unknown reservation lost", record, err)
	}
	if _, err = f.journal.Assess(context.Background(), record.CaseID, record.Arm, record.Repeat, nil, time.Now()); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("uncertain work became scoreable", err)
	}
	f.fail = nil
	if _, err = RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrEvaluationRecovery) || f.calls != 1 {
		t.Fatal("unknown attempt replayed", f.calls, err)
	}
}
func TestEvaluationRunnerBeforeWorkRefundRequiresZeroRequests(t *testing.T) {
	t.Run("contradicting-receipt", func(t *testing.T) {
		f := newEvaluationRunnerFixture(t)
		f.fail = func(n int, s Session) error {
			if n == 1 {
				return PhaseError{State: "quota_wait", BeforeWork: true}
			}
			return nil
		}
		out, err := RunEvaluation(context.Background(), f.options)
		if !errors.Is(err, ErrEvaluationFailed) || out.State != "finished_with_failures" || len(out.Executions) != 120 {
			t.Fatal(out.State, len(out.Executions), err)
		}
		first := out.Executions[0]
		if first.Sessions() != 1 || len(first.Ledger) != 0 || first.Attempts[0].Evidence.Access.Requests != 1 {
			t.Fatal("real request was refunded", first.Sessions())
		}
	})
	t.Run("trusted-before-work", func(t *testing.T) {
		f := newEvaluationRunnerFixture(t)
		factory := f.options.Workers
		first := true
		f.options.Workers = func(ctx context.Context, arm EvaluationArm, job Job, bound *JobEvidence) (EvaluationWorkers, error) {
			workers, err := factory(ctx, arm, job, bound)
			if err != nil {
				return workers, err
			}
			if first {
				first = false
				workers.Reviews[Codex] = auditedReviewFunc(func(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error) {
					f.calls++
					return Report{}, nil, nil, PhaseError{State: "quota_wait", BeforeWork: true}
				})
			}
			return workers, nil
		}
		out, err := RunEvaluation(context.Background(), f.options)
		if !errors.Is(err, ErrEvaluationFailed) || len(out.Executions) != 120 || out.Executions[0].Sessions() != 0 || out.Executions[0].Attempts[0].Charged || len(out.Executions[0].Ledger) != 0 {
			t.Fatal("trusted failure charged or partial", len(out.Executions), err)
		}
		if f.calls != 317 {
			t.Fatal("natural budget was padded or retried", f.calls)
		}
		before := f.calls
		again, err := RunEvaluation(context.Background(), f.options)
		if !errors.Is(err, ErrEvaluationFailed) || again.ReusedArms != 120 || f.calls != before {
			t.Fatal("terminal failure replayed", again.ReusedArms, f.calls, err)
		}
	})
}
func TestEvaluationRunnerPersistenceFailureDoesNotReplay(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	_, err := f.journal.store.db.Exec(`CREATE TRIGGER evaluation_fault BEFORE UPDATE ON evaluation_execution WHEN instr(CAST(new.data AS TEXT),'"State":"complete"')>0 BEGIN SELECT RAISE(ABORT,'synthetic evaluation persistence failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunEvaluation(context.Background(), f.options)
	if !errors.Is(err, ErrEvaluationRecovery) || f.calls != 1 {
		t.Fatal("lost stopped result allowed progress", f.calls, err)
	}
	if _, err = f.journal.store.db.Exec(`DROP TRIGGER evaluation_fault`); err != nil {
		t.Fatal(err)
	}
	saved, err := f.journal.LoadExecution(context.Background(), f.corpus.Cases[0].ID, ArmAdversarial, 0)
	if err != nil || saved == nil || saved.Attempts[0].State != "inflight" || saved.Sessions() != 1 {
		t.Fatal("reservation vanished", saved, err)
	}
	if _, err = RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrEvaluationRecovery) || f.calls != 1 {
		t.Fatal("failed commit replayed", f.calls, err)
	}
}

func evaluationPendingRecovery(t *testing.T, returned bool) (*evaluationRunnerFixture, *EvaluationExecution, EvaluationStoppedRecovery) {
	t.Helper()
	f := newEvaluationRunnerFixture(t)
	f.fail = func(int, Session) error {
		return errors.Join(PhaseError{State: "provider_unavailable"}, ErrWorkerStopPending)
	}
	if _, err := RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal(err)
	}
	record, err := f.journal.LoadExecution(context.Background(), f.corpus.Cases[0].ID, ArmAdversarial, 0)
	if err != nil {
		t.Fatal(err)
	}
	attempt := record.Attempts[0]
	binding := authTestBinding()
	binding.SessionHash = attempt.CapabilityHash
	stopped := authStopCheck(func(_ context.Context, got AuthBinding) error {
		if got != binding {
			return ErrAuthRecoveryRequired
		}
		return nil
	})
	store := authTestStore(t, attempt.Provider)
	profile, err := store.LeaseProfile(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := profile.ExportAuth(context.Background(), authExportCheck(func(_ context.Context, got AuthBinding) error {
		if got != binding {
			return ErrAuthTransfer
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := seed.Info()
	seed.Close()
	if returned {
		transfer, err := NewAuthReturn(info, []byte("synthetic-returned-evaluation-auth"))
		if err != nil {
			t.Fatal(err)
		}
		if err = profile.ImportAuthReturn(context.Background(), transfer, stopped); err != nil {
			t.Fatal(err)
		}
	}
	if err = profile.Close(); err != nil {
		t.Fatal(err)
	}
	options := EvaluationStoppedRecovery{Binding: binding, Auth: store, Stopped: stopped, Source: cloneSourceAccess(attempt.Source), Evidence: cloneProviderEvidence(attempt.Evidence), Check: func(context.Context, EvaluationPlan) error { return nil }}
	return f, record, options
}
func TestEvaluationRunnerExplicitStoppedRecoveryRetiresWithoutReplay(t *testing.T) {
	f, record, options := evaluationPendingRecovery(t, true)
	refused := options
	refused.Stopped = authStopCheck(func(context.Context, AuthBinding) error { return ErrWorkerStopPending })
	if err := f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, refused); !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("unknown stop retired", err)
	}
	before, err := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if err != nil || before.Recovery != nil {
		t.Fatal("failed proof created intent", err)
	}
	wrong := options
	wrong.Binding.SessionHash = Digest("another attempt")
	if err = f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, wrong); !errors.Is(err, ErrFenced) {
		t.Fatal("foreign attempt retired", err)
	}
	if err = f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options); err != nil {
		t.Fatal(err)
	}
	retired, err := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if err != nil || retired.State != "provider_unavailable" || retired.Sessions() != 1 || len(retired.Ledger) != 0 || retired.Attempts[0].Unsafe || retired.Recovery == nil || retired.Recovery.VerifiedAt.IsZero() || !retired.Recovery.Previous.Unsafe {
		t.Fatal("retirement changed charge or discarded original evidence", retired, err)
	}
	oldHash := retired.Hash()
	if err = f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options); err != nil {
		t.Fatal("same recovery not idempotent", err)
	}
	again, _ := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if again.Hash() != oldHash {
		t.Fatal("idempotent recovery changed receipt")
	}
	f.options.Workers = func(context.Context, EvaluationArm, Job, *JobEvidence) (EvaluationWorkers, error) {
		return EvaluationWorkers{}, ErrLiveBlocked
	}
	out, err := RunEvaluation(context.Background(), f.options)
	if !errors.Is(err, ErrLiveBlocked) || out.ReusedArms != 1 || f.calls != 1 {
		t.Fatal("retired attempt replayed", out.ReusedArms, f.calls, err)
	}
	result, err := f.journal.Assess(context.Background(), record.CaseID, record.Arm, 0, nil, time.Now())
	if err != nil || result.Sessions != 1 || len(result.FindingIDs) != 0 || result.UsageKnown {
		t.Fatal("failure fabricated partial quality or usage", result, err)
	}
}
func TestEvaluationRunnerRecoveryIntentSurvivesPostAuthWriteFailure(t *testing.T) {
	f, record, options := evaluationPendingRecovery(t, true)
	_, err := f.journal.store.db.Exec(`CREATE TRIGGER recovery_fault BEFORE UPDATE ON evaluation_execution WHEN instr(CAST(new.data AS TEXT),'"Recovery":')>0 AND instr(CAST(new.data AS TEXT),'"VerifiedAt":"0001-01-01T00:00:00Z"')=0 BEGIN SELECT RAISE(ABORT,'synthetic recovery receipt failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	err = f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options)
	if !errors.Is(err, ErrEvaluationRecovery) {
		t.Fatal("lost final receipt ignored", err)
	}
	pending, err := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if err != nil || pending.Recovery == nil || !pending.Recovery.VerifiedAt.IsZero() || !pending.Attempts[0].Unsafe {
		t.Fatal("intent lost original unsafe charge", pending, err)
	}
	if _, err = os.Lstat(filepath.Join(options.Auth.directory, "lease.json")); !os.IsNotExist(err) {
		t.Fatal("fault did not follow completed auth disposal", err)
	}
	if _, err = f.journal.store.db.Exec(`DROP TRIGGER recovery_fault`); err != nil {
		t.Fatal(err)
	}
	if err = f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options); err != nil {
		t.Fatal("durable intent could not finish without a recreated profile", err)
	}
	retired, _ := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if retired.Recovery.VerifiedAt.IsZero() || retired.Sessions() != 1 || f.calls != 1 {
		t.Fatal("retirement replayed or refunded work")
	}
}
func TestEvaluationRunnerRecoveryCannotInventLostAuthReturn(t *testing.T) {
	f, record, options := evaluationPendingRecovery(t, false)
	if err := f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("exported seed substituted for return", err)
	}
	called := false
	err := options.Auth.Refresh(context.Background(), func(current []byte) ([]byte, error) { called = true; return current, nil })
	if !errors.Is(err, ErrAuthRecoveryRequired) || called {
		t.Fatal("lost return lease was discarded", err)
	}
	saved, err := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if err != nil || saved.Recovery != nil || !saved.Attempts[0].Unsafe || saved.Sessions() != 1 {
		t.Fatal("lost return cleared recovery", saved, err)
	}
}

func TestEvaluationRunnerCrashWorkerChild(t *testing.T) {
	path := os.Getenv("SFL_EVALUATION_CRASH_PATH")
	if path == "" {
		return
	}
	corpus := syntheticCorpus()
	for i := range corpus.Cases {
		corpus.Cases[i].Label.Rationale = "SECRET_GROUND_TRUTH_DO_NOT_SEND"
	}
	plan := syntheticPlan(corpus)
	journal, err := OpenEvaluationJournal(path, alphaEnrollment(), corpus, plan)
	if err != nil {
		os.Exit(91)
	}
	opts := EvaluationRunnerOptions{Journal: journal, Check: func(context.Context, EvaluationPlan) error { return nil }, Source: func(_ context.Context, q EvaluationSourceRequest) (EvaluationSource, error) {
		e := &SnapshotEvidence{store: &fixtureStore{files: map[string]Object{"source.go": {Mode: "100644", Data: []byte("original source")}}}, snapshot: q.Snapshot, packet: "synthetic immutable evidence", paths: []string{"source.go"}}
		return EvaluationSource{Packet: e.Packet(), Bind: func(_ context.Context, j Job) (*JobEvidence, error) { return e.Bind(j) }}, nil
	}, Workers: func(context.Context, EvaluationArm, Job, *JobEvidence) (EvaluationWorkers, error) {
		worker := auditedReviewFunc(func(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error) {
			saved, err := journal.LoadExecution(context.Background(), corpus.Cases[0].ID, ArmAdversarial, 0)
			if err != nil || saved == nil || len(saved.Attempts) != 1 || saved.Attempts[0].State != "inflight" || !saved.Attempts[0].Charged {
				os.Exit(92)
			}
			os.Exit(93)
			return Report{}, nil, nil, nil
		})
		return EvaluationWorkers{Reviews: map[Provider]AuditedReviewer{Codex: worker, Claude: worker}}, nil
	}}
	RunEvaluation(context.Background(), opts)
	os.Exit(94)
}
func TestEvaluationRunnerRealProcessCrashCannotReplayReservation(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	child := exec.Command(os.Args[0], "-test.run=^TestEvaluationRunnerCrashWorkerChild$")
	child.Env = []string{"SFL_EVALUATION_CRASH_PATH=" + f.path}
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 93 {
		t.Fatal("child did not cross committed reservation boundary", err)
	}
	record, err := f.journal.LoadExecution(context.Background(), f.corpus.Cases[0].ID, ArmAdversarial, 0)
	if err != nil || record == nil || record.Sessions() != 1 || record.Attempts[0].State != "inflight" {
		t.Fatal("crash lost reservation", record, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err = RunEvaluation(ctx, f.options); !errors.Is(err, ErrEvaluationRecovery) || f.calls != 0 {
		t.Fatal("crash replayed or retained dead flock", f.calls, err)
	}
}
func TestEvaluationRunnerCompletedPhasesSurviveFinalizationFailure(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	_, err := f.journal.store.db.Exec(`CREATE TRIGGER finalization_fault BEFORE UPDATE ON evaluation_execution WHEN json_extract(CAST(new.data AS TEXT),'$.State')='complete' BEGIN SELECT RAISE(ABORT,'synthetic finalization failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrEvaluationRecovery) || f.calls != 4 {
		t.Fatal("fault did not follow four stopped phases", f.calls, err)
	}
	if _, err = f.journal.store.db.Exec(`DROP TRIGGER finalization_fault`); err != nil {
		t.Fatal(err)
	}
	f.options.Workers = func(context.Context, EvaluationArm, Job, *JobEvidence) (EvaluationWorkers, error) {
		return EvaluationWorkers{}, ErrLiveBlocked
	}
	out, err := RunEvaluation(context.Background(), f.options)
	if !errors.Is(err, ErrLiveBlocked) || out.ReusedArms != 1 || len(out.Executions) != 1 || out.Executions[0].State != "complete" || f.calls != 4 {
		t.Fatal("retained full phases replayed or were discarded", out.ReusedArms, out.State, f.calls, err)
	}
}
func TestEvaluationRunnerFrozenPacketAndPlanChangesRefuse(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	source := f.options.Source
	f.options.Source = func(ctx context.Context, q EvaluationSourceRequest) (EvaluationSource, error) {
		s, err := source(ctx, q)
		s.Packet += " "
		return s, err
	}
	if _, err := RunEvaluation(context.Background(), f.options); !errors.Is(err, ErrFenced) || f.calls != 0 || f.factories != 0 {
		t.Fatal("changed packet reached worker factory", err)
	}
	changed := f.corpus
	raw, _ := json.Marshal(changed)
	json.Unmarshal(raw, &changed)
	changed.Cases[0].Label.Rationale = "different independent label"
	plan := syntheticPlan(changed)
	if other, err := OpenEvaluationJournal(f.path, alphaEnrollment(), changed, plan); err == nil {
		other.Close()
		t.Fatal("changed corpus/plan adopted retained journal")
	}
}
func TestEvaluationRunnerExclusiveOwnershipAcrossHandles(t *testing.T) {
	f := newEvaluationRunnerFixture(t)
	other, err := OpenEvaluationJournal(f.path, alphaEnrollment(), f.corpus, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	entered := make(chan struct{})
	released := make(chan struct{})
	done := make(chan error, 1)
	f.options.Workers = func(context.Context, EvaluationArm, Job, *JobEvidence) (EvaluationWorkers, error) {
		call := auditedReviewFunc(func(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
			close(entered)
			<-ctx.Done()
			close(released)
			return Report{}, nil, nil, ctx.Err()
		})
		return EvaluationWorkers{Reviews: map[Provider]AuditedReviewer{Codex: call, Claude: call}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := RunEvaluation(ctx, f.options); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first owner did not reserve")
	}
	if err = f.journal.Close(); !errors.Is(err, ErrFenced) {
		t.Fatal("Close freed active owner", err)
	}
	contender := f.options
	contender.Journal = other
	bounded, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err = RunEvaluation(bounded, contender)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("second handle bypassed flock", err)
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, ErrEvaluationRecovery) {
			t.Fatal("cancelled unknown work lost its fence", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not join cancelled worker")
	}
	select {
	case <-released:
	default:
		t.Fatal("owner returned before worker")
	}
}

func TestEvaluationJournalRejectsCorpusNormalizationCollision(t *testing.T) {
	corpus := syntheticCorpus()
	corpus.Cases[0].ID = "case-\xff"
	corpus.Cases[1].ID = "case-\xfe"
	plan := syntheticPlan(corpus)
	if err := plan.Validate(corpus); err != nil {
		t.Fatal("raw fixture was already rejected", err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "execution.db")
	journal, err := OpenEvaluationJournal(path, alphaEnrollment(), corpus, plan)
	if err == nil {
		journal.Close()
		t.Fatal("two frozen cases collapsed to one serialized case ID")
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("invalid captured corpus opened a journal", err)
	}
}

func TestEvaluationRunnerRecoveryCannotReplaceCapturedReceipt(t *testing.T) {
	f, record, options := evaluationPendingRecovery(t, true)
	changed := cloneProviderEvidence(options.Evidence)
	changed.Access.Failed = true
	if changed.validateStoppedCaptured(record.Attempts[0].Provider, record.Job.Config.Limits, record.Attempts[0].CapabilityHash) != nil {
		t.Fatal("mutation did not retain a valid stopped receipt")
	}
	options.Evidence = changed
	if err := f.journal.RetireStopped(context.Background(), record.CaseID, record.Arm, 0, options); !errors.Is(err, ErrFenced) {
		t.Fatal("recovery replaced a captured receipt", err)
	}
	saved, err := f.journal.LoadExecution(context.Background(), record.CaseID, record.Arm, 0)
	if err != nil || saved.Hash() != record.Hash() {
		t.Fatal("rejected replacement mutated execution", err)
	}
}
