//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func evaluationJob(plan EvaluationPlan, caseID string, snapshot Snapshot, packet string, arm EvaluationArm, repeat int) Job {
	config := plan.Config
	switch arm {
	case ArmIndependent:
		config.Mode = IndependentPair
	case ArmCodex, ArmCodexControl:
		config.Mode = CodexOnly
	case ArmClaude, ArmClaudeControl:
		config.Mode = ClaudeOnly
	}
	id := "evaluation_" + Digest(struct {
		Plan, Case string
		Arm        EvaluationArm
		Repeat     int
	}{plan.Hash(), caseID, arm, repeat})
	return Job{ID: id, RequesterID: OwnerID, Generation: 1, Snapshot: snapshot, Config: config, ConfigVersion: 1, Release: plan.TrustedRelease, Packet: packet}
}
func evaluationInput(job Job, arm EvaluationArm, index int, prior []EvaluationAttempt) (*Session, *ControlSession, Session, error) {
	schedule := arm.Schedule()
	if index < 0 || index >= len(schedule) || len(prior) != index {
		return nil, nil, Session{}, ErrFenced
	}
	provider := schedule[index]
	if arm == ArmCodexControl || arm == ArmClaudeControl {
		history := []ControlReport{}
		for _, a := range prior {
			report, err := DecodeStrict[ControlReport](a.Report, job.Config.Limits.OutputBytes)
			if err != nil {
				return nil, nil, Session{}, err
			}
			history = append(history, report)
		}
		input := ControlSession{Job: job, Provider: provider, Step: index + 1, Limits: job.Config.Limits, History: history}
		capability, err := ControlCapabilitySession(input)
		return nil, &input, capability, err
	}
	round := 1
	var peers []Report
	if arm == ArmAdversarial && index >= 2 {
		round = 2
		for _, a := range prior[:2] {
			report, err := DecodeStrict[Report](a.Report, job.Config.Limits.OutputBytes)
			if err != nil {
				return nil, nil, Session{}, err
			}
			peers = append(peers, report)
		}
	}
	limits, err := protocolLimits(job)
	if err != nil {
		return nil, nil, Session{}, err
	}
	session, err := NewSession(job.ID, job.Snapshot, job.Config.Hash(), provider, round, job.Packet, peers, nil, limits)
	return &session, nil, session, err
}
func evaluationInputHash(review *Session, control *ControlSession) string {
	if control != nil {
		return Digest(*control)
	}
	if review != nil {
		return Digest(*review)
	}
	return ""
}
func evaluationTerminal(state string) bool {
	switch state {
	case "complete", "blocked", "quota_wait", "auth_failed", "provider_unavailable", "invalid_output", "cancelled", "incomplete_expired":
		return true
	}
	return false
}
func (j *EvaluationJournal) validateExecution(e EvaluationExecution) error {
	test, err := j.testCase(e.CaseID)
	if err != nil {
		return err
	}
	if e.Version != 1 || e.Repeat < 0 || e.Repeat >= j.plan.Repeats || e.Arm.Sessions() == 0 || e.PlanHash != j.planHash || e.CorpusHash != j.corpusHash || !e.StartedAt.After(j.plan.CreatedAt) || e.Job.Validate() != nil || e.Job.Snapshot != test.Snapshot || Digest(e.Job.Packet) != test.PacketHash {
		return ErrFenced
	}
	packet, _ := json.Marshal(e.Job.Packet)
	if len(packet) != test.SerializedPacketBytes || Digest(e.Job) != Digest(evaluationJob(j.plan, e.CaseID, test.Snapshot, e.Job.Packet, e.Arm, e.Repeat)) || len(e.Attempts) > e.Arm.Sessions() {
		return ErrFenced
	}
	if e.State != "running" && !evaluationTerminal(e.State) {
		return ErrFenced
	}
	if e.State == "running" {
		if !e.FinishedAt.IsZero() || len(e.Ledger) != 0 {
			return ErrFenced
		}
	} else if !e.FinishedAt.After(e.StartedAt) {
		return ErrFenced
	}
	for index, a := range e.Attempts {
		review, control, capability, err := evaluationInput(e.Job, e.Arm, index, e.Attempts[:index])
		if err != nil {
			return err
		}
		if a.Index != index || a.Provider != e.Arm.Schedule()[index] || a.InputHash != evaluationInputHash(review, control) || a.CapabilityHash != Digest(capability) || Digest(a.Review) != Digest(review) || Digest(a.Control) != Digest(control) || a.StartedAt.Before(e.StartedAt) {
			return ErrFenced
		}
		if index > 0 && !a.StartedAt.After(e.Attempts[index-1].FinishedAt) {
			return ErrFenced
		}
		if a.State == "inflight" {
			if index != len(e.Attempts)-1 || e.State != "running" || !a.FinishedAt.IsZero() || !a.Charged || !a.Unsafe || len(a.Report) != 0 {
				return ErrFenced
			}
			continue
		}
		if !evaluationTerminal(a.State) || !a.FinishedAt.After(a.StartedAt) || (!e.FinishedAt.IsZero() && a.FinishedAt.After(e.FinishedAt)) {
			return ErrFenced
		}
		if a.State != "complete" && index != len(e.Attempts)-1 {
			return ErrFenced
		}
		if a.State == "complete" {
			if !a.Charged || a.Unsafe || a.Source == nil || a.Source.validate(capability) != nil || a.Evidence.validateCompletedCaptured(a.Provider, capability.Limits, Digest(capability)) != nil {
				return ErrFenced
			}
			if control != nil {
				report, err := DecodeStrict[ControlReport](a.Report, capability.Limits.OutputBytes)
				if err != nil || ValidateControlReport(report, *control) != nil {
					return ErrFenced
				}
			} else {
				report, err := DecodeStrict[Report](a.Report, capability.Limits.OutputBytes)
				if err != nil || ValidateReport(report, capability) != nil {
					return ErrFenced
				}
			}
		} else {
			if len(a.Report) != 0 {
				return ErrFenced
			}
			if !a.Charged && (a.Unsafe || (a.Evidence != nil && a.Evidence.Access.Requests != 0)) {
				return ErrFenced
			}
			if a.Charged && !a.Unsafe && (a.Source == nil || a.Source.validate(capability) != nil || a.Evidence.validateStoppedCaptured(a.Provider, capability.Limits, Digest(capability)) != nil) {
				return ErrFenced
			}
		}
	}
	if e.State == "complete" {
		if len(e.Attempts) != e.Arm.Sessions() {
			return ErrFenced
		}
		for _, a := range e.Attempts {
			if a.State != "complete" {
				return ErrFenced
			}
		}
		ledger, err := evaluationLedger(e)
		if err != nil || Digest(ledger) != Digest(e.Ledger) {
			return ErrFenced
		}
	} else if len(e.Ledger) != 0 {
		return ErrFenced
	}
	return j.validateRecovery(e)
}
func evaluationLedger(e EvaluationExecution) ([]Entry, error) {
	if e.Arm == ArmCodexControl || e.Arm == ArmClaudeControl {
		state := newControlLedger()
		for _, a := range e.Attempts {
			if a.Control == nil {
				return nil, ErrFenced
			}
			r, err := DecodeStrict[ControlReport](a.Report, e.Job.Config.Limits.OutputBytes)
			if err != nil {
				return nil, err
			}
			if err = state.check(*a.Control, r); err != nil {
				return nil, err
			}
			state.apply(*a.Control, r)
		}
		ledger := []Entry{}
		for _, id := range state.order {
			ledger = append(ledger, *state.entries[id])
		}
		return ledger, nil
	}
	reports := []Report{}
	for _, a := range e.Attempts {
		report, err := DecodeStrict[Report](a.Report, e.Job.Config.Limits.OutputBytes)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return buildLedger(reports, e.Job.Config.Mode), nil
}

// Assess uses retained execution, then the existing scorer's independent label
// rules. It cannot derive actionability from model output or invent labels.
func (j *EvaluationJournal) Assess(ctx context.Context, caseID string, arm EvaluationArm, repeat int, labels []FindingLabel, at time.Time) (EvaluationResult, error) {
	record, err := j.LoadExecution(ctx, caseID, arm, repeat)
	if err != nil || record == nil {
		return EvaluationResult{}, ErrFenced
	}
	e := *record
	if !evaluationTerminal(e.State) || e.State == "blocked" {
		return EvaluationResult{}, ErrEvaluationRecovery
	}
	for _, a := range e.Attempts {
		if a.Unsafe {
			return EvaluationResult{}, ErrEvaluationRecovery
		}
	}
	result := EvaluationResult{CaseID: caseID, Arm: arm, Repeat: repeat, CorpusHash: e.CorpusHash, PlanHash: e.PlanHash, ReportHash: e.Hash(), PacketHash: Digest(e.Job.Packet), OutputBytesLimit: e.Job.Config.Limits.OutputBytes, Snapshot: e.Job.Snapshot, StartedAt: e.StartedAt, State: e.State, Sessions: e.Sessions(), Providers: []Provider{}, FindingIDs: []string{}, FindingStatuses: map[string]string{}, Labels: labels, LabelledAt: at, Latency: e.FinishedAt.Sub(e.StartedAt), UsageKnown: true, TurnsKnown: true}
	if len(e.Attempts) > 0 {
		result.Queue = e.Attempts[0].StartedAt.Sub(e.StartedAt)
	}
	for _, a := range e.Attempts {
		if !a.Charged {
			continue
		}
		result.Providers = append(result.Providers, a.Provider)
		usage := Usage{}
		if a.State == "complete" {
			if a.Control != nil {
				r, _ := DecodeStrict[ControlReport](a.Report, e.Job.Config.Limits.OutputBytes)
				usage = r.Usage
			} else {
				r, _ := DecodeStrict[Report](a.Report, e.Job.Config.Limits.OutputBytes)
				usage = r.Usage
			}
		}
		result.UsageKnown = result.UsageKnown && usage.Known
		result.TurnsKnown = result.TurnsKnown && usage.Turns > 0
		result.Turns += usage.Turns
		result.InputTokens += usage.InputTokens
		result.OutputTokens += usage.OutputTokens
	}
	for _, entry := range e.Ledger {
		result.FindingIDs = append(result.FindingIDs, entry.Finding.ID)
		result.FindingStatuses[entry.Finding.ID] = entry.Status
		switch entry.Status {
		case "corroborated":
			result.Corroborated++
		case "disputed":
			result.Disputed++
		case "retracted":
			result.Concessions++
		}
	}
	if _, err = ScoreEvaluation(j.corpus, j.plan, []EvaluationResult{result}); err != nil {
		return EvaluationResult{}, err
	}
	return result, nil
}
func evaluationSafetyError(err error) bool {
	for _, cause := range []error{ErrWorkerStopPending, ErrAuthRecoveryRequired, ErrSourceAuditPending, ErrProviderAuditPending, ErrProviderEgressPending} {
		if errors.Is(err, cause) {
			return true
		}
	}
	return false
}
