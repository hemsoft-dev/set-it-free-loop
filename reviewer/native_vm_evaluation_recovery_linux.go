//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"syscall"
)

type nativeVMAuthReturnIntent struct {
	Version                                                 int
	Binding                                                 AuthBinding
	RequestHash, ManifestHash, SourceHash, ReturnedAuthHash string
	Provider                                                *ProviderEvidence
}

const nativeVMAuthReturnIntentVersion = 2

type NativeVMStoppedAttemptEvidence struct {
	Binding                      AuthBinding
	Source                       *SourceAccess
	Provider                     *ProviderEvidence
	ReturnedAuthHash, IntentHash string
}

// persistReturnedAuthIntent runs after importing the matching stopped return
// and before disposing its profile. A failed write leaves the returned lease
// recoverable. No auth bytes are persisted in the containment directory.
func (p *nativeVMHostPhase) persistReturnedAuthIntent(ctx context.Context, evidence *ProviderEvidence) error {
	if p.profile == nil || p.finished == nil || !p.request.valid() || p.finished.Binding != p.request.Binding || p.finished.RequestHash != Digest(p.request) {
		return ErrProviderAuditPending
	}
	p.profile.mu.Lock()
	journal := p.profile.journal
	p.profile.mu.Unlock()
	if journal.Binding != p.request.Binding || journal.TransferStage != "returned" || !validAuthHash(journal.TransferHash) {
		return ErrAuthRecoveryRequired
	}
	s := p.supervisor.supervisor
	unit, err := s.snapshotUnit(ctx, p.request.Binding)
	if err != nil || !unit.Captured || !unit.Stopped || !unit.Aborted || unit.PayloadKind != "native-vm" || unit.ManifestHash != Digest(p.manifest) {
		return ErrProviderAuditPending
	}
	gate := s.gate(p.request.Binding)
	held, err := gate.lock(ctx)
	if err != nil {
		return ErrProviderAuditPending
	}
	defer held.close()
	audit, err := DecodeSourceReaderAudit(p.request.readerConfig(), p.finished.SourceAudit)
	if err != nil || audit.Access == nil || audit.Access.validate(p.request.Session) != nil {
		// A null source record cannot later qualify stopped retirement. Keep
		// the matching returned profile instead of disposing its last evidence.
		return ErrSourceAuditPending
	}
	if evidence.validateStoppedCaptured(p.request.Session.Identity.Provider, p.request.Session.Limits, Digest(p.request.Session)) != nil || Digest(evidence.Policy) != Digest(p.request.Policy) {
		return ErrProviderAuditPending
	}
	source := cloneSourceAccess(audit.Access)
	sourceRaw, err := json.Marshal(source)
	if err != nil || len(sourceRaw) > 2<<20 || writeAuth(held.root, "source-access.json", sourceRaw) != nil {
		return ErrSourceAuditPending
	}
	// The native request hash covers paths, policy, hints and control input in
	// addition to the capability. Retain those exact canonical, non-auth bytes
	// so recovery can verify the full request rather than only its hash shape.
	requestRaw, err := json.Marshal(p.request)
	if err != nil || len(requestRaw) > 4<<20 || writeAuth(held.root, "native-request.json", requestRaw) != nil {
		return ErrProviderAuditPending
	}
	intent := nativeVMAuthReturnIntent{nativeVMAuthReturnIntentVersion, p.request.Binding, Digest(p.request), unit.ManifestHash, Digest(source), journal.TransferHash, cloneProviderEvidence(evidence)}
	raw, err := json.Marshal(intent)
	if err != nil || len(raw) > 65536 || writeAuth(held.root, "auth-return-intent.json", raw) != nil {
		return ErrProviderAuditPending
	}
	return nil
}
func readNativeEvidenceFile(root *os.Root, name string, limit int) ([]byte, error) {
	file, err := openRootNoFollow(root, name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrEvaluationRecovery
	}
	defer file.Close()
	if privateAuthFile(file) != nil {
		return nil, ErrEvaluationRecovery
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(raw) == 0 || len(raw) > limit {
		return nil, ErrEvaluationRecovery
	}
	return raw, nil
}

// StoppedAttemptEvidence reopens only the original private unit. It accepts no
// client-authored audit and does not stop a live VM, import auth or start work.
// Receipts from releases predating durable return intents remain unqualified.
func (v *NativeVMSupervisor) StoppedAttemptEvidence(ctx context.Context, binding AuthBinding, capability Session) (NativeVMStoppedAttemptEvidence, error) {
	fail := func() (NativeVMStoppedAttemptEvidence, error) {
		return NativeVMStoppedAttemptEvidence{}, ErrEvaluationRecovery
	}
	if ctx == nil || ctx.Err() != nil || v == nil || v.supervisor == nil || !binding.valid() || capability.Validate() != nil || binding.SessionHash != Digest(capability) {
		return fail()
	}
	s := v.supervisor
	unit, err := s.snapshotUnit(ctx, binding)
	if err != nil || unit.PayloadKind != "native-vm" || !unit.Captured || !unit.Stopped || !unit.Aborted {
		return fail()
	}
	if v.ConfirmAuthStopped(ctx, binding) != nil {
		return fail()
	}
	gate := s.gate(binding)
	held, err := gate.lock(ctx)
	if err != nil {
		return fail()
	}
	defer held.close()
	gateRecord, err := gate.read(held)
	if err != nil || gateRecord.Protocol != nativeVMGrantProtocol || gateRecord.Phase != "aborted" || gateRecord.ManifestHash != unit.ManifestHash || gateRecord.Identity != unit.Identity {
		return fail()
	}
	raw, err := readNativeEvidenceFile(held.root, "auth-return-intent.json", 65536)
	if err != nil {
		return fail()
	}
	var intent nativeVMAuthReturnIntent
	if json.Unmarshal(raw, &intent) != nil {
		return fail()
	}
	canonical, _ := json.Marshal(intent)
	if !bytes.Equal(raw, canonical) || intent.Version != nativeVMAuthReturnIntentVersion || intent.Binding != binding || intent.ManifestHash != unit.ManifestHash || !validAuthHash(intent.RequestHash) || !validAuthHash(intent.ReturnedAuthHash) || intent.Provider.validateStoppedCaptured(capability.Identity.Provider, capability.Limits, Digest(capability)) != nil {
		return fail()
	}
	requestRaw, err := readNativeEvidenceFile(held.root, "native-request.json", 4<<20)
	if err != nil {
		return fail()
	}
	var request nativeVMRequest
	if json.Unmarshal(requestRaw, &request) != nil || !request.valid() || request.Binding != binding || Digest(request.Session) != Digest(capability) || Digest(request) != intent.RequestHash || Digest(request.Policy) != Digest(intent.Provider.Policy) {
		return fail()
	}
	canonical, _ = json.Marshal(request)
	if !bytes.Equal(requestRaw, canonical) {
		return fail()
	}
	policy := intent.Provider.Policy.ResponsePolicy
	if (capability.Identity.Provider == Codex && policy != CodexSourceResponses) || (capability.Identity.Provider == Claude && policy != ClaudeSourceMessages) {
		return fail()
	}
	sourceRaw, err := readNativeEvidenceFile(held.root, "source-access.json", 2<<20)
	if err != nil {
		return fail()
	}
	var source *SourceAccess
	if json.Unmarshal(sourceRaw, &source) != nil || source == nil || source.validate(capability) != nil || Digest(source) != intent.SourceHash {
		return fail()
	}
	canonical, _ = json.Marshal(source)
	if !bytes.Equal(sourceRaw, canonical) || ctx.Err() != nil {
		return fail()
	}
	return NativeVMStoppedAttemptEvidence{binding, cloneSourceAccess(source), cloneProviderEvidence(intent.Provider), intent.ReturnedAuthHash, Digest(intent)}, nil
}
func (v *NativeVMSupervisor) ConfirmReturnedAuth(ctx context.Context, binding AuthBinding, capability Session, source *SourceAccess, provider *ProviderEvidence) (EvaluationAuthReturnProof, error) {
	evidence, err := v.StoppedAttemptEvidence(ctx, binding, capability)
	if err != nil || Digest(evidence.Source) != Digest(source) || Digest(evidence.Provider) != Digest(provider) {
		return EvaluationAuthReturnProof{}, ErrEvaluationRecovery
	}
	return EvaluationAuthReturnProof{evidence.ReturnedAuthHash, evidence.IntentHash}, nil
}

var _ EvaluationReturnedAuthVerifier = (*NativeVMSupervisor)(nil)
