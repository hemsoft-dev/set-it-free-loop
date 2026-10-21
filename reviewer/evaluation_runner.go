//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

// EvaluationSourceRequest deliberately excludes corpus labels and assessments.
type EvaluationSourceRequest struct {
	CaseID, PacketHash    string
	Snapshot              Snapshot
	SerializedPacketBytes int
}

// EvaluationSource borrows immutable source capabilities. Bind can restore the
// exact frozen packet with RestoreSnapshotEvidence, preserving its omissions.
type EvaluationSource struct {
	Packet string
	Bind   func(context.Context, Job) (*JobEvidence, error)
}

type EvaluationWorkers struct {
	Reviews  map[Provider]AuditedReviewer
	Controls map[Provider]AuditedControlReviewer
}

// Source and worker capabilities remain caller-owned. Factories only construct
// workers; they must never start inference before the runner reserves a phase.
// Check must establish the plan's exact release, eligibility and owner gates,
// including quota/backoff. It is repeated before every possible worker call.
type EvaluationRunnerOptions struct {
	Journal *EvaluationJournal
	Check   func(context.Context, EvaluationPlan) error
	Source  func(context.Context, EvaluationSourceRequest) (EvaluationSource, error)
	Workers func(context.Context, EvaluationArm, Job, *JobEvidence) (EvaluationWorkers, error)
}
type EvaluationRunOutcome struct {
	State                        string
	Executions                   []EvaluationExecution
	ExecutedSessions, ReusedArms int
}

func RunEvaluation(ctx context.Context, o EvaluationRunnerOptions) (EvaluationRunOutcome, error) {
	out := EvaluationRunOutcome{State: "incomplete", Executions: []EvaluationExecution{}}
	if ctx == nil || o.Journal == nil || o.Check == nil || o.Source == nil || o.Workers == nil {
		return out, ErrLiveBlocked
	}
	j := o.Journal
	release, err := j.acquire(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	if err = o.Check(ctx, j.plan); err != nil || ctx.Err() != nil {
		return out, ErrLiveBlocked
	}
	if !time.Now().After(j.plan.CreatedAt) {
		return out, ErrLiveBlocked
	}
	failures := false
	for caseIndex, test := range j.corpus.Cases {
		var source *EvaluationSource
		for repeat := 0; repeat < j.plan.Repeats; repeat++ {
			for _, arm := range j.plan.Order(caseIndex, repeat) {
				if err = ctx.Err(); err != nil {
					return out, err
				}
				saved, err := j.read(ctx, test.ID, arm, repeat)
				if err != nil {
					return out, err
				}
				if saved != nil {
					if saved.State == "running" {
						for _, a := range saved.Attempts {
							if a.State == "inflight" || a.Unsafe {
								return out, ErrEvaluationRecovery
							}
						}
						// Every persisted phase is stopped. Never silently replay a partial
						// control or pad its budget after a crash between phase boundaries.
						before := cloneEvaluationExecution(*saved)
						saved.State = "incomplete_expired"
						saved.FinishedAt = time.Now().UTC()
						saved.Ledger = nil
						if len(saved.Attempts) == arm.Sessions() {
							saved.Ledger, err = evaluationLedger(*saved)
							if err != nil {
								return out, err
							}
							saved.State = "complete"
						}
						if err = j.write(ctx, *saved, &before); err != nil {
							return out, ErrEvaluationRecovery
						}
					}
					for _, a := range saved.Attempts {
						if a.Unsafe {
							return out, ErrEvaluationRecovery
						}
					}
					out.Executions = append(out.Executions, cloneEvaluationExecution(*saved))
					out.ReusedArms++
					failures = failures || saved.State != "complete"
					continue
				}
				if err = o.Check(ctx, j.plan); err != nil || ctx.Err() != nil {
					return out, ErrLiveBlocked
				}
				if source == nil {
					captured, sourceErr := o.Source(ctx, EvaluationSourceRequest{test.ID, test.PacketHash, test.Snapshot, test.SerializedPacketBytes})
					source = &captured
					err = sourceErr
					if err != nil {
						return out, err
					}
					if source.Bind == nil || Digest(source.Packet) != test.PacketHash {
						return out, ErrFenced
					}
					packet, _ := json.Marshal(source.Packet)
					if len(packet) != test.SerializedPacketBytes {
						return out, ErrFenced
					}
				}
				job := evaluationJob(j.plan, test.ID, test.Snapshot, source.Packet, arm, repeat)
				bound, err := source.Bind(ctx, job)
				if err != nil || bound.validateJob(job) != nil {
					return out, ErrFenced
				}
				workers, err := o.Workers(ctx, arm, job, bound)
				if err != nil {
					return out, err
				}
				if !evaluationWorkersValid(workers, job, arm) {
					return out, ErrLiveBlocked
				}
				record := EvaluationExecution{Version: 1, CaseID: test.ID, Arm: arm, Repeat: repeat, CorpusHash: j.corpusHash, PlanHash: j.planHash, Job: job, StartedAt: time.Now().UTC(), State: "running", Attempts: []EvaluationAttempt{}, Ledger: nil}
				if err = j.write(ctx, record, nil); err != nil {
					return out, ErrEvaluationRecovery
				}
				phase := &evaluationPhase{options: o, record: &record, bound: bound}
				if arm == ArmCodexControl || arm == ArmClaudeControl {
					worker := evaluationControl{phase: phase, inner: workers.Controls[arm.Schedule()[0]]}
					result, runErr := RunSelfCritique(ctx, job, arm.Schedule()[0], worker, bound.Reader)
					err = runErr
					if err == nil {
						record.Ledger = result.Ledger
					}
				} else {
					reviewers := map[Provider]Reviewer{}
					for provider, worker := range workers.Reviews {
						reviewers[provider] = evaluationReview{phase: phase, inner: worker}
					}
					result, runErr := (Engine{Reviewers: reviewers, Reader: bound.Reader}).Run(ctx, job, Checkpoint{})
					err = runErr
					if err == nil {
						record.Ledger = result.Ledger
					}
				}
				out.ExecutedSessions += phase.calls
				if errors.Is(err, ErrEvaluationRecovery) {
					return out, ErrEvaluationRecovery
				}
				for _, a := range record.Attempts {
					if a.State == "inflight" || a.Unsafe {
						return out, ErrEvaluationRecovery
					}
				}
				before := cloneEvaluationExecution(record)
				// The phase writer only persists attempts. Final ledger is accepted once
				// the complete reducer has returned and all attempt evidence is durable.
				before.Ledger = nil
				record.FinishedAt = time.Now().UTC()
				record.State = "complete"
				if err != nil {
					record.State = phaseState(err)
					record.Ledger = nil
					failures = true
				}
				durable, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				saveErr := j.write(durable, record, &before)
				stop()
				if saveErr != nil {
					return out, ErrEvaluationRecovery
				}
				out.Executions = append(out.Executions, cloneEvaluationExecution(record))
				if record.State == "blocked" {
					return out, ErrLiveBlocked
				}
			}
		}
	}
	out.State = "execution_complete"
	if failures {
		out.State = "finished_with_failures"
		return out, ErrEvaluationFailed
	}
	return out, nil
}
func nilEvaluationWorker(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func:
		return value.IsNil()
	}
	return false
}
func evaluationWorkersValid(w EvaluationWorkers, job Job, arm EvaluationArm) bool {
	if arm == ArmCodexControl || arm == ArmClaudeControl {
		return len(w.Reviews) == 0 && len(w.Controls) == 1 && !nilEvaluationWorker(w.Controls[arm.Schedule()[0]])
	}
	if len(w.Controls) != 0 || len(w.Reviews) != len(job.Config.Providers()) {
		return false
	}
	for _, provider := range job.Config.Providers() {
		if nilEvaluationWorker(w.Reviews[provider]) {
			return false
		}
	}
	return true
}

type evaluationPhase struct {
	options EvaluationRunnerOptions
	record  *EvaluationExecution
	bound   *JobEvidence
	calls   int
}

func (p *evaluationPhase) reserve(ctx context.Context, review *Session, control *ControlSession) (EvaluationAttempt, error) {
	j := p.options.Journal
	if err := p.options.Check(ctx, j.plan); err != nil || ctx.Err() != nil {
		return EvaluationAttempt{}, errors.Join(PhaseError{State: "blocked", BeforeWork: true}, ErrLiveBlocked)
	}
	index := len(p.record.Attempts)
	expectedReview, expectedControl, capability, err := evaluationInput(p.record.Job, p.record.Arm, index, p.record.Attempts)
	if err != nil || Digest(review) != Digest(expectedReview) || Digest(control) != Digest(expectedControl) {
		return EvaluationAttempt{}, PhaseError{State: "invalid_output", BeforeWork: true}
	}
	attempt := EvaluationAttempt{Index: index, Provider: p.record.Arm.Schedule()[index], InputHash: evaluationInputHash(review, control), CapabilityHash: Digest(capability), Review: expectedReview, Control: expectedControl, StartedAt: time.Now().UTC(), State: "inflight", Charged: true, Unsafe: true}
	before := cloneEvaluationExecution(*p.record)
	next := cloneEvaluationExecution(before)
	next.Attempts = append(next.Attempts, attempt)
	if err = j.write(ctx, next, &before); err != nil {
		return EvaluationAttempt{}, ErrEvaluationRecovery
	}
	*p.record = next
	p.calls++
	// Input already copied by evaluationInput and the persisted canonical frame.
	// Give the worker a separate copy so it cannot mutate journal provenance.
	return cloneEvaluationExecution(EvaluationExecution{Attempts: []EvaluationAttempt{attempt}}).Attempts[0], nil
}
func evaluationAudit(ctx context.Context, s Session, access *SourceAccess, evidence *ProviderEvidence, err error) (bool, bool, error) {
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	var failure PhaseError
	before := err != nil && errors.As(err, &failure) && failure.BeforeWork && !evaluationSafetyError(err)
	if before && (evidence == nil || (evidence.Access.Requests == 0 && evidence.validateStoppedCaptured(s.Identity.Provider, s.Limits, Digest(s)) == nil)) {
		return false, false, err
	}
	if err != nil && failure.BeforeWork {
		err = errors.Join(workerLauncherError(err, false), ErrProviderEgressFailed)
	}
	unsafe := evaluationSafetyError(err)
	if evidence == nil || evidence.validateStoppedCaptured(s.Identity.Provider, s.Limits, Digest(s)) != nil {
		unsafe = true
		err = errors.Join(err, ErrProviderAuditPending)
	}
	if access == nil || access.validate(s) != nil {
		unsafe = true
		err = errors.Join(err, ErrSourceAuditPending)
	}
	if err == nil && evidence.validateCompletedCaptured(s.Identity.Provider, s.Limits, Digest(s)) != nil {
		err = adapterValidation(ErrProviderEgressFailed, false)
	}
	return true, unsafe, err
}
func (p *evaluationPhase) finish(ctx context.Context, attempt EvaluationAttempt, report any, access *SourceAccess, evidence *ProviderEvidence, err error) error {
	capability := Session{}
	if attempt.Control != nil {
		capability, _ = ControlCapabilitySession(*attempt.Control)
	} else {
		capability = *attempt.Review
	}
	charged, unsafe, err := evaluationAudit(ctx, capability, access, evidence, err)
	attempt.Charged, attempt.Unsafe = charged, unsafe
	attempt.Source = cloneSourceAccess(access)
	attempt.Evidence = cloneProviderEvidence(evidence)
	attempt.FinishedAt = time.Now().UTC()
	attempt.State = "complete"
	if err != nil {
		attempt.State = phaseState(err)
		attempt.Report = nil
	} else {
		attempt.Report, _ = json.Marshal(report)
	}
	before := cloneEvaluationExecution(*p.record)
	next := cloneEvaluationExecution(before)
	next.Attempts[attempt.Index] = attempt
	durable, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if saveErr := p.options.Journal.write(durable, next, &before); saveErr != nil {
		return ErrEvaluationRecovery
	}
	*p.record = next
	if unsafe {
		return ErrEvaluationRecovery
	}
	return err
}

type evaluationReview struct {
	phase *evaluationPhase
	inner AuditedReviewer
}

func (w evaluationReview) Review(ctx context.Context, s Session) (Report, error) {
	r, _, _, err := w.ReviewAudited(ctx, s)
	return r, err
}
func (w evaluationReview) ReviewAudited(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
	attempt, err := w.phase.reserve(ctx, &s, nil)
	if err != nil {
		return Report{}, nil, nil, err
	}
	wire, _ := json.Marshal(attempt.Review)
	var captured Session
	json.Unmarshal(wire, &captured)
	report, source, evidence, err := w.inner.ReviewAudited(ctx, captured)
	source = cloneSourceAccess(source)
	evidence = cloneProviderEvidence(evidence)
	_, _, err = evaluationAudit(ctx, s, source, evidence, err)
	if err == nil {
		if ValidateReport(report, s) != nil {
			err = adapterValidation(ErrFenced, false)
		} else {
			reader, readErr := w.phase.bound.Reader(s.Snapshot)
			if readErr != nil || (Adapter{Reader: reader}).verifyReport(ctx, s, report) != nil {
				err = adapterValidation(ErrSourceService, false)
			} else {
				report, err = mergeReaderCoverage(report, reader)
				if err == nil {
					report, err = mergeCoverageRecords(report, source.Coverage)
				}
				raw, _ := json.Marshal(report)
				if err != nil || len(raw) > s.Limits.OutputBytes || ValidateReport(report, s) != nil {
					err = adapterValidation(ErrFenced, false)
				}
			}
		}
	}
	err = w.phase.finish(ctx, attempt, report, source, evidence, err)
	if err != nil {
		return Report{}, source, evidence, err
	}
	return report, source, evidence, nil
}

type evaluationControl struct {
	phase *evaluationPhase
	inner AuditedControlReviewer
}

func (w evaluationControl) Critique(ctx context.Context, s ControlSession) (ControlReport, error) {
	r, _, _, err := w.CritiqueAudited(ctx, s)
	return r, err
}
func (w evaluationControl) CritiqueAudited(ctx context.Context, s ControlSession) (ControlReport, *SourceAccess, *ProviderEvidence, error) {
	attempt, err := w.phase.reserve(ctx, nil, &s)
	if err != nil {
		return ControlReport{}, nil, nil, err
	}
	wire, _ := json.Marshal(attempt.Control)
	var captured ControlSession
	json.Unmarshal(wire, &captured)
	report, source, evidence, err := w.inner.CritiqueAudited(ctx, captured)
	source = cloneSourceAccess(source)
	evidence = cloneProviderEvidence(evidence)
	capability, _ := ControlCapabilitySession(s)
	_, _, err = evaluationAudit(ctx, capability, source, evidence, err)
	if err == nil {
		if ValidateControlReport(report, s) != nil {
			err = adapterValidation(ErrFenced, false)
		} else {
			reader, readErr := w.phase.bound.Reader(s.Job.Snapshot)
			if readErr != nil || verifyControlSource(ctx, s, report, reader) != nil {
				err = adapterValidation(ErrSourceService, false)
			} else {
				report, err = mergeControlCoverage(s, report, source.Coverage, reader)
				if err != nil {
					err = adapterValidation(ErrFenced, false)
				}
			}
		}
	}
	err = w.phase.finish(ctx, attempt, report, source, evidence, err)
	if err != nil {
		return ControlReport{}, source, evidence, err
	}
	return report, source, evidence, nil
}
