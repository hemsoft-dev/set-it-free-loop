//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// ErrProviderRecovery refuses an operator recovery of a stopped provider
// attempt because a required stop, identity or evidence proof is missing.
// Nothing durable was changed when it is returned before auth settlement.
var ErrProviderRecovery = errors.New("stopped provider attempt recovery refused")

// Auth dispositions of a recovered native attempt. None of them reuses an
// exported seed whose return is unknown.
const (
	// No auth lease remains for this provider. The attempt either never leased
	// auth or an earlier recovery already settled it.
	ProviderAuthNone = "none"
	// The lease was never exported, so the unchanged seed was committed.
	ProviderAuthUnexported = "unexported"
	// A journal-bound stopped return was finished and committed.
	ProviderAuthReturned = "returned"
	// The exported seed's return was lost; canonical auth was retired.
	ProviderAuthRetired = "retired"
)

// nativeStoppedAttempt is the durable controller identity of one outstanding
// audited native attempt plus the operator-selected original launch.
type nativeStoppedAttempt struct {
	Provider    Provider
	AttemptID   string
	SessionHash string
	LaunchID    string
	Limits      Limits
	Evidence    *ProviderEvidence
}

// nativeLaunchBinding derives the binding NativeVMReviewer used for a launch.
func nativeLaunchBinding(sessionHash, launchID string) AuthBinding {
	return AuthBinding{SessionHash: sessionHash, LaunchID: launchID, ContainmentID: Digest(struct{ Session, Launch string }{sessionHash, launchID})}
}

// stoppedAttemptReceipt reads only durable host records of the original
// launch: its unit journal, aborted gate and private stopped audit receipt.
// The receipt must bind the same attempt ID, session, manifest and recorded
// provider evidence, and must not claim completion. An earlier attempt's
// launch for the same session and evidence therefore refuses. It stops,
// imports and retires nothing. A missing receipt, such as after a host
// process kill before the stopped audit, refuses recovery.
func (v *NativeVMSupervisor) stoppedAttemptReceipt(ctx context.Context, a nativeStoppedAttempt) (AuthBinding, nativeVMAuditReceipt, error) {
	fail := func() (AuthBinding, nativeVMAuditReceipt, error) {
		return AuthBinding{}, nativeVMAuditReceipt{}, ErrProviderRecovery
	}
	binding := nativeLaunchBinding(a.SessionHash, a.LaunchID)
	if ctx == nil || ctx.Err() != nil || v == nil || v.supervisor == nil || v.supervisor.payloadKind != "native-vm" || !a.Provider.Valid() || a.AttemptID == "" || !binding.valid() || a.Evidence.validateStoppedCaptured(a.Provider, a.Limits, a.SessionHash) != nil {
		return fail()
	}
	policy := a.Evidence.Policy.ResponsePolicy
	if (a.Provider == Codex && policy != CodexSourceResponses) || (a.Provider == Claude && policy != ClaudeSourceMessages) {
		return fail()
	}
	s := v.supervisor
	gate := s.gate(binding)
	held, err := gate.lock(ctx)
	if err != nil {
		return fail()
	}
	defer held.close()
	unit, err := s.readUnit(held, binding)
	if err != nil || unit.PayloadKind != "native-vm" || !unit.Captured || !unit.Aborted || !unit.Stopped {
		return fail()
	}
	record, err := gate.read(held)
	if err != nil || record.Protocol != nativeVMGrantProtocol || record.Phase != "aborted" || record.ManifestHash != unit.ManifestHash || record.Identity != unit.Identity {
		return fail()
	}
	raw, err := readNativeEvidenceFile(held.root, "native-audit.json", 65536)
	if err != nil {
		return fail()
	}
	var receipt nativeVMAuditReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		return fail()
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, raw) {
		return fail()
	}
	if receipt.Version != 2 || receipt.AttemptID != a.AttemptID || receipt.Binding != binding || receipt.ManifestHash != unit.ManifestHash || !validAuthHash(receipt.RequestHash) || receipt.Complete || receipt.Failure == "" || (receipt.AuthReturned && !receipt.AuthExported) {
		return fail()
	}
	if receipt.Provider.validateStoppedCaptured(a.Provider, a.Limits, a.SessionHash) != nil || Digest(cloneProviderEvidence(receipt.Provider)) != Digest(cloneProviderEvidence(a.Evidence)) {
		return fail()
	}
	return binding, receipt, nil
}

// settleStoppedAttemptAuth applies the documented stopped-worker auth rules
// for one recovered launch. The caller has already validated its receipt.
// The decision is made from the held journal before any mutation, and every
// Complete/Retire repeats the whole-unit stop proof. An exported seed with no
// durable return is retired only with explicit operator consent; it is never
// committed or reused. A journal for another launch refuses recovery.
func settleStoppedAttemptAuth(ctx context.Context, auth *AuthStore, stopped AuthStopVerifier, binding AuthBinding, receipt nativeVMAuditReceipt, retire bool) (string, error) {
	if auth == nil || stopped == nil {
		return "", ErrProviderRecovery
	}
	profile, err := auth.RecoverProfile(ctx, binding)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		held, lockErr := auth.lock(ctx)
		if lockErr != nil {
			return "", ErrAuthRecoveryRequired
		}
		leaseErr := noAuthLease(held.root)
		held.close()
		if leaseErr != nil {
			return "", ErrAuthRecoveryRequired
		}
		if stopped.ConfirmAuthStopped(ctx, binding) != nil {
			return "", ErrProviderRecovery
		}
		return ProviderAuthNone, nil
	}
	defer profile.Close()
	profile.mu.Lock()
	j := profile.journal
	profile.mu.Unlock()
	switch {
	case j.Stage == "active" && j.TransferStage == "":
		if receipt.AuthExported {
			return "", ErrAuthRecoveryRequired
		}
		if profile.Complete(ctx, stopped) != nil {
			return "", ErrAuthRecoveryRequired
		}
		return ProviderAuthUnexported, nil
	case j.Stage == "active" && j.TransferStage == "exported":
		if !receipt.AuthExported || receipt.AuthReturned || !retire {
			return "", ErrAuthRecoveryRequired
		}
		if profile.Retire(ctx, stopped) != nil {
			return "", ErrAuthRecoveryRequired
		}
		return ProviderAuthRetired, nil
	case j.Stage == "active" && (j.TransferStage == "receiving" || j.TransferStage == "returned"):
		if !receipt.AuthExported {
			return "", ErrAuthRecoveryRequired
		}
		if profile.ResumeAuthReturn(ctx, stopped) != nil || profile.Complete(ctx, stopped) != nil {
			return "", ErrAuthRecoveryRequired
		}
		return ProviderAuthReturned, nil
	case (j.Stage == "committing" || j.Stage == "committed") && j.TransferStage == "":
		// An interrupted Complete of a never-exported lease.
		if receipt.AuthExported {
			return "", ErrAuthRecoveryRequired
		}
		if profile.Complete(ctx, stopped) != nil {
			return "", ErrAuthRecoveryRequired
		}
		return ProviderAuthUnexported, nil
	case (j.Stage == "committing" || j.Stage == "committed") && j.TransferStage == "returned":
		if !receipt.AuthExported {
			return "", ErrAuthRecoveryRequired
		}
		if profile.Complete(ctx, stopped) != nil {
			return "", ErrAuthRecoveryRequired
		}
		return ProviderAuthReturned, nil
	case j.Stage == "retiring":
		// Only an explicit earlier Retire reaches this stage; finish it.
		if profile.Retire(ctx, stopped) != nil {
			return "", ErrAuthRecoveryRequired
		}
		return ProviderAuthRetired, nil
	}
	return "", ErrAuthRecoveryRequired
}

// recoveredAttemptState is the job disposition for a recovered attempt. Lost
// or retired auth is an authentication failure; any other stopped attempt
// without a complete result is an unavailable provider. Both remain charged
// and can return to the queue only through ResumeUnavailable.
func recoveredAttemptState(auth string, receipt nativeVMAuditReceipt) string {
	if auth == ProviderAuthRetired || (auth == ProviderAuthNone && receipt.AuthExported && !receipt.AuthReturned) {
		return "auth_failed"
	}
	return "provider_unavailable"
}
