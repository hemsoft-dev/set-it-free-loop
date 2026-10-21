//go:build linux

package reviewer

import (
	"context"
	"errors"
	"path/filepath"
	"time"
)

// EvaluationRecoveryRecord is a durable retirement intent followed by a
// stopped/auth-return receipt. It cannot turn an uncertain attempt into a
// successful review or refund its reservation.
type EvaluationAuthReturnProof struct{ ReturnedAuthHash, IntentHash string }
type EvaluationReturnedAuthVerifier interface {
	ConfirmReturnedAuth(context.Context, AuthBinding, Session, *SourceAccess, *ProviderEvidence) (EvaluationAuthReturnProof, error)
}

type EvaluationRecoveryRecord struct {
	Previous                EvaluationAttempt
	NativeIntentHash        string
	Index                   int
	Binding                 AuthBinding
	ReturnedAuthHash        string
	RequestedAt, VerifiedAt time.Time
	Source                  *SourceAccess
	Evidence                *ProviderEvidence
}
type EvaluationStoppedRecovery struct {
	Binding  AuthBinding
	Auth     *AuthStore
	Stopped  AuthStopVerifier
	Source   *SourceAccess
	Evidence *ProviderEvidence
	Check    func(context.Context, EvaluationPlan) error
}

// RetireStopped requires the original supervisor and auth store. The operator
// must first recover/import a matching returned auth bundle through the normal
// stopped supervisor path. This method neither imports a profile nor retires
// lost credentials. Unknown stop/return/source/provider evidence remains fenced.
// Keep the original auth store reserved until this receipt is durable.
func (j *EvaluationJournal) RetireStopped(ctx context.Context, caseID string, arm EvaluationArm, repeat int, o EvaluationStoppedRecovery) error {
	if ctx == nil || o.Auth == nil || nilEvaluationWorker(o.Stopped) || o.Check == nil || !o.Binding.valid() {
		return ErrEvaluationRecovery
	}
	done, err := j.acquire(ctx)
	if err != nil {
		return err
	}
	defer done()
	phase, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if o.Check(phase, j.plan) != nil || phase.Err() != nil {
		return ErrLiveBlocked
	}
	if filepath.Clean(o.Auth.directory) == filepath.Dir(j.store.path) {
		return ErrFenced
	}
	record, err := j.read(phase, caseID, arm, repeat)
	if err != nil || record == nil || len(record.Attempts) == 0 {
		return ErrEvaluationRecovery
	}
	index := len(record.Attempts) - 1
	attempt := record.Attempts[index]
	if o.Binding.SessionHash != attempt.CapabilityHash || o.Auth.provider != attempt.Provider {
		return ErrFenced
	}
	capability := Session{}
	if attempt.Control != nil {
		capability, _ = ControlCapabilitySession(*attempt.Control)
	} else {
		capability = *attempt.Review
	}
	if o.Source == nil || o.Source.validate(capability) != nil || o.Evidence.validateStoppedCaptured(attempt.Provider, capability.Limits, attempt.CapabilityHash) != nil {
		return ErrEvaluationRecovery
	}
	// Recovery can fill missing receipts, but cannot rewrite evidence already
	// captured for this attempt, even when another boundary remained unsafe.
	if attempt.Source != nil && attempt.Source.validate(capability) == nil && Digest(attempt.Source) != Digest(o.Source) {
		return ErrFenced
	}
	if attempt.Evidence.validateStoppedCaptured(attempt.Provider, capability.Limits, attempt.CapabilityHash) == nil && Digest(attempt.Evidence) != Digest(o.Evidence) {
		return ErrFenced
	}
	if record.Recovery != nil {
		receipt := record.Recovery
		if receipt.Index != index || receipt.Binding != o.Binding || Digest(receipt.Source) != Digest(o.Source) || Digest(receipt.Evidence) != Digest(o.Evidence) {
			return ErrFenced
		}
		if !receipt.VerifiedAt.IsZero() {
			return nil
		}
	}
	if record.State != "running" || (attempt.State != "inflight" && !attempt.Unsafe) {
		return ErrFenced
	}
	if o.Stopped.ConfirmAuthStopped(phase, o.Binding) != nil {
		return ErrEvaluationRecovery
	}
	profile, profileErr := o.Auth.RecoverProfile(phase, o.Binding)
	if profileErr == nil {
		defer profile.Close()
		profile.mu.Lock()
		journal := profile.journal
		profile.mu.Unlock()
		if journal.TransferStage != "returned" || !validAuthHash(journal.TransferHash) {
			return ErrAuthRecoveryRequired
		}
		if record.Recovery == nil {
			before := cloneEvaluationExecution(*record)
			record.Recovery = &EvaluationRecoveryRecord{Previous: before.Attempts[index], Index: index, Binding: o.Binding, ReturnedAuthHash: journal.TransferHash, RequestedAt: time.Now().UTC(), Source: cloneSourceAccess(o.Source), Evidence: cloneProviderEvidence(o.Evidence)}
			if err = j.write(phase, *record, &before); err != nil {
				return ErrEvaluationRecovery
			}
		} else if record.Recovery.ReturnedAuthHash != journal.TransferHash {
			return ErrFenced
		}
		if profile.Complete(phase, o.Stopped) != nil {
			return ErrAuthRecoveryRequired
		}
	} else if record.Recovery == nil {
		verifier, ok := o.Stopped.(EvaluationReturnedAuthVerifier)
		if !ok {
			return ErrAuthRecoveryRequired
		}
		proof, proofErr := verifier.ConfirmReturnedAuth(phase, o.Binding, capability, o.Source, o.Evidence)
		if proofErr != nil || !validAuthHash(proof.ReturnedAuthHash) || !validAuthHash(proof.IntentHash) {
			return ErrAuthRecoveryRequired
		}
		before := cloneEvaluationExecution(*record)
		record.Recovery = &EvaluationRecoveryRecord{Previous: before.Attempts[index], NativeIntentHash: proof.IntentHash, Index: index, Binding: o.Binding, ReturnedAuthHash: proof.ReturnedAuthHash, RequestedAt: time.Now().UTC(), Source: cloneSourceAccess(o.Source), Evidence: cloneProviderEvidence(o.Evidence)}
		if err = j.write(phase, *record, &before); err != nil {
			return ErrEvaluationRecovery
		}
	}
	// Complete may have succeeded before a prior journal-write failure/crash.
	// A durable intent plus the exact returned state and no remaining lease
	// proves that boundary without recreating credentials or replaying work.
	held, err := o.Auth.lock(phase)
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	auth, readErr := readAuth(held.root, "state.json", false)
	leaseErr := noAuthLease(held.root)
	matches := readErr == nil && leaseErr == nil && authHash(auth) == record.Recovery.ReturnedAuthHash
	clear(auth)
	closeErr := held.close()
	if !matches || closeErr != nil || o.Stopped.ConfirmAuthStopped(phase, o.Binding) != nil {
		return ErrAuthRecoveryRequired
	}
	before := cloneEvaluationExecution(*record)
	now := time.Now().UTC()
	record.Recovery.VerifiedAt = now
	attempt.Source = cloneSourceAccess(o.Source)
	attempt.Evidence = cloneProviderEvidence(o.Evidence)
	attempt.Unsafe = false
	attempt.Charged = true
	attempt.Report = nil
	if attempt.State == "inflight" || attempt.State == "complete" {
		attempt.State = "incomplete_expired"
	}
	if attempt.FinishedAt.IsZero() {
		attempt.FinishedAt = now
	}
	record.Attempts[index] = attempt
	record.State = attempt.State
	record.FinishedAt = now
	record.Ledger = nil
	durable, stop := context.WithTimeout(context.WithoutCancel(phase), 5*time.Second)
	defer stop()
	if err = j.write(durable, *record, &before); err != nil {
		return errors.Join(ErrEvaluationRecovery, err)
	}
	return nil
}
func (j *EvaluationJournal) validateRecovery(e EvaluationExecution) error {
	r := e.Recovery
	if r == nil {
		return nil
	}
	if len(e.Attempts) == 0 || r.Index != len(e.Attempts)-1 || !r.Binding.valid() || r.Binding.SessionHash != e.Attempts[r.Index].CapabilityHash || !validAuthHash(r.ReturnedAuthHash) || !r.RequestedAt.After(e.StartedAt) {
		return ErrFenced
	}
	a := e.Attempts[r.Index]
	if r.NativeIntentHash != "" && !validAuthHash(r.NativeIntentHash) {
		return ErrFenced
	}
	if r.Previous.Index != a.Index || r.Previous.InputHash != a.InputHash || r.Previous.CapabilityHash != a.CapabilityHash || (!r.Previous.Unsafe && r.Previous.State != "inflight") {
		return ErrFenced
	}
	capability := Session{}
	if a.Control != nil {
		capability, _ = ControlCapabilitySession(*a.Control)
	} else {
		capability = *a.Review
	}
	if r.Source == nil || r.Source.validate(capability) != nil || r.Evidence.validateStoppedCaptured(a.Provider, capability.Limits, Digest(capability)) != nil {
		return ErrFenced
	}
	if r.VerifiedAt.IsZero() {
		if e.State != "running" {
			return ErrFenced
		}
	} else if r.VerifiedAt.Before(r.RequestedAt) || r.VerifiedAt.After(e.FinishedAt) || a.Unsafe || !a.Charged || e.State == "running" || e.State == "complete" || Digest(a.Source) != Digest(r.Source) || Digest(a.Evidence) != Digest(r.Evidence) {
		return ErrFenced
	}
	return nil
}
