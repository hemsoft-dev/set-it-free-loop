//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"time"
)

// NativeVMOptions is trusted release/activation configuration, never a PR
// command. Check must establish the separately evidenced provider, account,
// client, isolation, billing and owner activation gates before any auth read.
// No default gate, credentials, transport, supervisor or guest image is chosen.
type NativeVMOptions struct {
	Provider                       Provider
	Root, Stub, StubHash           string
	QEMU, Kernel, Initrd, Firmware VMBootAsset
	Runtime                        NativeVMRuntimeSpec
	Auth                           *AuthStore
	Reader                         *Reader
	Policy                         EgressPolicy
	Transport                      http.RoundTripper
	ProfileHints                   NativeVMProfileHints
	Check                          func(context.Context, Session) error
}

type NativeVMReviewer struct{ options NativeVMOptions }

func NewNativeVMReviewer(o NativeVMOptions) (*NativeVMReviewer, error) {
	if !o.Provider.Valid() || !sourcePath(o.Root) || !sourcePath(o.Stub) || !validAuthHash(o.StubHash) || o.Auth == nil || o.Auth.provider != o.Provider || o.Reader == nil || o.Transport == nil || o.Check == nil || o.Policy.Provider != o.Provider || !o.ProfileHints.valid(o.Provider) {
		return nil, ErrLiveBlocked
	}
	if (o.Provider == Codex && o.Policy.ResponsePolicy != CodexSourceResponses) || (o.Provider == Claude && o.Policy.ResponsePolicy != ClaudeSourceMessages) {
		return nil, ErrLiveBlocked
	}
	// Filesystem and account state remain unopened at construction.
	o.Policy.Routes = append([]EgressRoute{}, o.Policy.Routes...)
	o.Policy.Headers = append([]string{}, o.Policy.Headers...)
	o.Runtime.BootstrapLibraries = append([]VMBootAsset{}, o.Runtime.BootstrapLibraries...)
	return &NativeVMReviewer{o}, nil
}
func (r *NativeVMReviewer) Review(ctx context.Context, s Session) (Report, error) {
	report, _, _, err := r.ReviewAudited(ctx, s)
	return report, err
}

type nativeVMExportGate struct {
	supervisor *NativeVMSupervisor
	manifest   NativeVMManifest
	request    nativeVMRequest
	ready      nativeVMReady
}

func (g nativeVMExportGate) ConfirmAuthExport(ctx context.Context, binding AuthBinding) error {
	if g.supervisor == nil || g.supervisor.supervisor == nil || binding != g.manifest.Binding || !g.ready.valid(g.request) || g.request.Binding != binding {
		return ErrAuthTransfer
	}
	if !g.manifest.valid() || verifyNativeBootstrapLibraries(ctx, g.manifest.BootstrapLibraries) != nil {
		return ErrAuthTransfer
	}
	s := g.supervisor.supervisor
	r, err := s.snapshotUnit(ctx, binding)
	if err != nil || r.PayloadKind != "native-vm" || r.ManifestHash != Digest(g.manifest) || !r.StartSettled || !r.Captured || r.Aborted || r.Stopped {
		return ErrAuthTransfer
	}
	if _, err = s.captureUnit(ctx, r); err != nil {
		return ErrAuthTransfer
	}
	s.mu.Lock()
	gate := s.gates[binding]
	s.mu.Unlock()
	if gate == nil {
		return ErrAuthTransfer
	}
	held, err := gate.lock(ctx)
	if err != nil {
		return ErrAuthTransfer
	}
	defer held.close()
	record, err := gate.read(held)
	if err != nil || record.Protocol != nativeVMGrantProtocol || record.Phase != "granted" || record.ManifestHash != r.ManifestHash || record.Identity != r.Identity {
		return ErrAuthTransfer
	}
	return nil
}

type nativeVMHostPhase struct {
	request           nativeVMRequest
	manifest          NativeVMManifest
	reader            *Reader
	supervisor        *NativeVMSupervisor
	channels          *nativeVMChannels
	bridge            *nativeProviderBridge
	objectsDone       chan error
	started, exported bool
	profile           *AuthProfile
	finished          *nativeVMFinished
	stage             string
	bootDiagnostics   []string
	authReturned      bool
}

func (r *NativeVMReviewer) ReviewAudited(ctx context.Context, s Session) (report Report, access *SourceAccess, evidence *ProviderEvidence, result error) {
	blocked := func() (Report, *SourceAccess, *ProviderEvidence, error) {
		return Report{}, nil, nil, errors.Join(PhaseError{State: "blocked", BeforeWork: true}, ErrLiveBlocked)
	}
	if r == nil || ctx == nil || httptrace.ContextClientTrace(ctx) != nil || s.Validate() != nil || s.Identity.Provider != r.options.Provider || r.options.Reader.snapshot != s.Snapshot {
		return blocked()
	}
	if ctx.Err() != nil {
		return Report{}, nil, nil, workerLauncherError(ctx.Err(), true)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Limits.Timeout)
	defer cancel()
	if r.options.Check(ctx, s) != nil {
		if ctx.Err() != nil {
			return Report{}, nil, nil, workerLauncherError(ctx.Err(), true)
		}
		return blocked()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return blocked()
	}
	binding := AuthBinding{SessionHash: Digest(s), LaunchID: hex.EncodeToString(nonce[:]), ContainmentID: Digest(struct{ Session, Launch string }{Digest(s), hex.EncodeToString(nonce[:])})}
	r.options.Reader.mu.Lock()
	paths := make([]string, 0, len(r.options.Reader.paths))
	for path := range r.options.Reader.paths {
		paths = append(paths, path)
	}
	initial := append([]Coverage{}, r.options.Reader.initialCoverage...)
	store := r.options.Reader.store
	r.options.Reader.mu.Unlock()
	sort.Strings(paths)
	reader, err := NewReader(store, s.Snapshot, paths, s.Limits)
	if err != nil {
		return blocked()
	}
	reader.initialCoverage = initial
	reader.coverage = append([]Coverage{}, initial...)
	verifier := Adapter{Reader: reader}
	for _, peer := range s.PeerReports {
		if verifier.verifyReport(ctx, s, peer) != nil {
			return Report{}, nil, nil, adapterValidation(ErrSourceService, true)
		}
	}
	request := nativeVMRequest{Version: 1, Binding: binding, Session: s, Paths: paths, Policy: r.options.Policy, ProfileHints: r.options.ProfileHints}
	if !request.valid() {
		return blocked()
	}
	images, err := OpenNativeVMBootImages(ctx, VMBootSpec{Version: 1, Binding: binding, QEMU: r.options.QEMU, Kernel: r.options.Kernel, Initrd: r.options.Initrd}, r.options.Firmware, r.options.Runtime)
	if err != nil {
		return Report{}, nil, nil, workerLauncherError(err, true)
	}
	defer images.Close()
	manifest, err := images.NativeManifest(s)
	if err != nil {
		return blocked()
	}
	supervisor, err := OpenNativeVMSupervisor(ctx, r.options.Root, r.options.Stub, r.options.StubHash)
	if err != nil {
		return Report{}, nil, nil, workerLauncherError(err, true)
	}
	defer supervisor.Close()
	channels, err := openNativeVMChannels()
	if err != nil {
		return blocked()
	}
	defer channels.Close()
	phase := &nativeVMHostPhase{request: request, manifest: manifest, reader: reader, supervisor: supervisor, channels: channels, stage: "provider-bridge"}
	// Every later return drains the concrete containment and private transports.
	defer func() { report, access, evidence, result = phase.finish(ctx, report, result) }()
	phase.bridge, err = startNativeProviderBridge(ctx, s, request.Policy, r.options.Transport, channels.host[2])
	if err != nil {
		return Report{}, nil, nil, workerLauncherError(err, true)
	}
	phase.objectsDone = make(chan error, 1)
	go func() { phase.objectsDone <- ServeVMObjects(ctx, channels.host[1], s, paths, store) }()
	phase.stage = "prepare"
	if err = supervisor.Prepare(manifest); err != nil {
		return Report{}, nil, nil, workerLauncherError(err, true)
	}
	phase.started = true // Start may be uncertain even when it returns an error.
	phase.stage = "start"
	if err = supervisor.Start(ctx, binding); err != nil {
		return Report{}, nil, nil, workerLauncherError(err, true)
	}
	phase.stage = "grant"
	if err = supervisor.grant(ctx, manifest, images, channels); err != nil {
		return Report{}, nil, nil, workerLauncherError(err, true)
	}
	if deadline, ok := ctx.Deadline(); ok {
		channels.host[0].SetDeadline(deadline)
	}
	phase.stage = "request"
	if writeVMRecord(channels.host[0], request, 4<<20) != nil {
		return Report{}, nil, nil, workerLauncherError(ErrVMBoot, true)
	}
	phase.stage = "ready"
	ready, err := readNativeVMReady(channels.host[0], request, func(stage string) {
		phase.stage = "guest/" + stage
		if strings.HasPrefix(stage, "diag-") && len(phase.bootDiagnostics) < 8 {
			phase.bootDiagnostics = append(phase.bootDiagnostics, stage)
		}
	})
	if err != nil || !ready.valid(request) {
		return Report{}, nil, nil, workerLauncherError(ErrVMBoot, true)
	}
	// No auth store is leased or read until the exact private guest is ready.
	phase.stage = "auth-lease"
	phase.profile, err = r.options.Auth.LeaseProfile(ctx, binding)
	if err != nil {
		return Report{}, nil, nil, PhaseError{State: "auth_failed", BeforeWork: true}
	}
	if nativeCheckLeasedAuth(ctx, phase.profile, nativeVMExportGate{supervisor, manifest, request, ready}) != nil {
		return Report{}, nil, nil, PhaseError{State: "auth_failed", BeforeWork: true}
	}
	phase.stage = "auth-export"
	seed, err := phase.profile.ExportAuth(ctx, nativeVMExportGate{supervisor, manifest, request, ready})
	if err != nil {
		return Report{}, nil, nil, PhaseError{State: "auth_failed", BeforeWork: true}
	}
	phase.exported = true
	defer seed.Close()
	wire, err := nativeVMSeedRecord(seed)
	if err != nil {
		return Report{}, nil, nil, PhaseError{State: "auth_failed"}
	}
	defer clear(wire)
	phase.stage = "seed-send"
	if writeVMRecord(channels.host[0], wire, 100<<10) != nil {
		return Report{}, nil, nil, workerLauncherError(ErrVMBoot, false)
	}
	phase.stage = "result"
	finished, err := readVMRecord[nativeVMFinished](channels.host[0], 4<<20)
	if err != nil {
		return Report{}, nil, nil, workerLauncherError(ErrVMBoot, false)
	}
	phase.finished = &finished
	phase.stage = "result-validation"
	if !finished.valid(request) {
		return Report{}, nil, nil, adapterValidation(ErrVMBoot, false)
	}
	if finished.State != "complete" {
		return Report{}, nil, nil, PhaseError{State: finished.State}
	}
	report, err = DecodeStrict[Report](finished.Report, s.Limits.OutputBytes)
	if err != nil || ValidateReport(report, s) != nil {
		return Report{}, nil, nil, adapterValidation(ErrVMBoot, false)
	}
	return report, nil, nil, nil
}

func (p *nativeVMHostPhase) finish(ctx context.Context, report Report, primary error) (Report, *SourceAccess, *ProviderEvidence, error) {
	if p.profile != nil {
		defer p.profile.Close()
	}
	if p.finished != nil {
		defer clear(p.finished.AuthReturn)
	}
	if ctx.Err() != nil {
		primary = errors.Join(primary, ctx.Err())
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var stopErr error
	if p.started {
		stopErr = p.supervisor.AbortAndWait(cleanup, p.request.Binding)
	}
	var evidence *ProviderEvidence
	if p.bridge != nil {
		var err error
		evidence, err = p.bridge.close(cleanup)
		primary = errors.Join(primary, err)
	}
	if err := p.drainObjects(cleanup, primary == nil && stopErr == nil); err != nil {
		if errors.Is(err, ErrSourceAuditPending) {
			primary = errors.Join(primary, err)
		} else if primary == nil {
			primary = err
		}
	}
	if stopErr != nil {
		return Report{}, nil, evidence, errors.Join(primary, ErrWorkerStopPending)
	}
	// Valid auth returns can be persisted even when model output is invalid.
	// They remain private and must match the original binding/seed journal.
	if p.profile != nil {
		if p.exported {
			if p.finished == nil || p.finished.Binding != p.request.Binding || p.finished.RequestHash != Digest(p.request) {
				primary = errors.Join(primary, ErrAuthRecoveryRequired)
			} else {
				returned, err := ReadAuthTransfer(bytes.NewReader(p.finished.AuthReturn))
				if err == nil {
					err = p.profile.ImportAuthReturn(cleanup, returned, p.supervisor)
				}
				if err == nil {
					err = p.profile.Complete(cleanup, p.supervisor)
				}
				if err != nil {
					primary = errors.Join(primary, ErrAuthRecoveryRequired)
				} else {
					p.authReturned = true
				}
			}
		} else if p.profile.Complete(cleanup, p.supervisor) != nil {
			primary = errors.Join(primary, ErrAuthRecoveryRequired)
		}
	}
	var access *SourceAccess
	if p.finished != nil && p.finished.Binding == p.request.Binding && p.finished.RequestHash == Digest(p.request) {
		audit, err := DecodeSourceReaderAudit(p.request.readerConfig(), p.finished.SourceAudit)
		if err == nil {
			access = cloneSourceAccess(audit.Access)
			if audit.Failed {
				primary = errors.Join(primary, ErrSourceService)
			}
		} else {
			primary = errors.Join(primary, ErrSourceAuditPending)
		}
	} else if p.exported {
		primary = errors.Join(primary, ErrSourceAuditPending)
	}
	if primary == nil {
		switch {
		case p.finished == nil || !p.finished.valid(p.request):
			primary = adapterValidation(ErrVMBoot, false)
		case access == nil || access.validate(p.request.Session) != nil:
			primary = ErrSourceAuditPending
		case evidence == nil || evidence.validateStoppedCaptured(p.request.Session.Identity.Provider, p.request.Session.Limits, Digest(p.request.Session)) != nil:
			primary = ErrProviderAuditPending
		case evidence.Access.ValidateCompleted(p.request.Session, evidence.Policy) != nil:
			primary = ErrProviderEgressFailed
		}
	}
	if primary == nil {
		if (Adapter{Reader: p.reader}).verifyReport(cleanup, p.request.Session, report) != nil {
			primary = adapterValidation(ErrSourceService, false)
		} else {
			var err error
			report, err = mergeCoverageRecords(report, access.Coverage)
			if err == nil {
				report, err = mergeReaderCoverage(report, p.reader)
			}
			encoded, encodeErr := json.Marshal(report)
			if err != nil || encodeErr != nil || len(encoded) > p.request.Session.Limits.OutputBytes || ValidateReport(report, p.request.Session) != nil {
				primary = adapterValidation(ErrSourceService, false)
			}
		}
	}
	if p.started && p.persistAudit(cleanup, report, access, evidence, primary) != nil {
		primary = errors.Join(primary, ErrProviderAuditPending)
	}
	if primary != nil {
		before := !p.exported && (evidence == nil || evidence.Access.Requests == 0)
		var classified PhaseError
		if errors.As(primary, &classified) {
			classified.BeforeWork = before
			return Report{}, access, evidence, errors.Join(classified, nativeVMCauses(primary))
		}
		return Report{}, access, evidence, errors.Join(PhaseError{State: phaseState(primary), BeforeWork: before}, nativeVMCauses(primary))
	}
	return report, access, evidence, nil
}

func (p *nativeVMHostPhase) drainObjects(ctx context.Context, graceful bool) error {
	// Once the whole VM is stopped, peer EOF is already inevitable. Let the
	// server consume it before local close can replace it with net.ErrClosed.
	// Failed or uncertain shutdown revokes intake immediately instead.
	if !graceful {
		p.channels.Close()
	}
	defer p.channels.Close()
	if p.objectsDone == nil {
		return nil
	}
	select {
	case err := <-p.objectsDone:
		if err != nil {
			return ErrSourceService
		}
		return nil
	case <-ctx.Done():
		return ErrSourceAuditPending
	}
}

func nativeVMCauses(err error) error {
	var retained []error
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrLiveBlocked, ErrWorkerStopPending, ErrProviderAuditPending, ErrProviderEgressPending, ErrSourceAuditPending, ErrAuthRecoveryRequired} {
		if errors.Is(err, cause) {
			retained = append(retained, cause)
		}
	}
	return errors.Join(retained...)
}

func (p *nativeVMHostPhase) persistAudit(ctx context.Context, report Report, access *SourceAccess, evidence *ProviderEvidence, failure error) error {
	s := p.supervisor.supervisor
	r, err := s.snapshotUnit(ctx, p.request.Binding)
	if err != nil || !r.Stopped || !r.Aborted {
		return ErrProviderAuditPending
	}
	s.mu.Lock()
	gate := s.gates[p.request.Binding]
	s.mu.Unlock()
	if gate == nil {
		return ErrProviderAuditPending
	}
	held, err := gate.lock(ctx)
	if err != nil {
		return ErrProviderAuditPending
	}
	defer held.close()
	var host *nativeVMCodeHostAccess
	if p.finished != nil {
		host = p.finished.CodeHost
	}
	receipt := struct {
		Version                                           int
		Binding                                           AuthBinding
		RequestHash, ManifestHash, ReportHash, SourceHash string
		Provider                                          *ProviderEvidence
		CodeHost                                          *nativeVMCodeHostAccess
		Complete                                          bool
		Stage                                             string
		AuthExported, AuthReturned                        bool
		GuestStage                                        string
		Native                                            *nativeResultObservation
		Failure                                           string
		BootDiagnostics                                   []string
	}{1, p.request.Binding, Digest(p.request), Digest(p.manifest), Digest(report), Digest(access), evidence, host, failure == nil, p.stage, p.exported, p.authReturned, "", nil, nativeVMFailureKind(failure), p.bootDiagnostics}
	if p.finished != nil && nativeVMGuestStage(p.finished.GuestStage) {
		receipt.GuestStage = p.finished.GuestStage
	}
	if p.finished != nil && p.finished.Native.valid(p.request.Session) {
		receipt.Native = p.finished.Native
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > 65536 {
		return ErrProviderAuditPending
	}
	return writeAuth(held.root, "native-audit.json", raw)
}

func nativeVMFailureKind(err error) string {
	if err == nil {
		return ""
	}
	for _, cause := range []struct {
		err  error
		kind string
	}{
		{ErrWorkerStopPending, "stop"}, {ErrAuthRecoveryRequired, "auth-return"},
		{ErrSourceAuditPending, "source-audit"}, {ErrSourceService, "source-service"},
		{ErrProviderAuditPending, "provider-audit"}, {ErrProviderEgressPending, "provider-stop"},
		{ErrProviderEgressFailed, "provider-egress"}, {context.DeadlineExceeded, "deadline"},
		{context.Canceled, "cancelled"},
	} {
		if errors.Is(err, cause.err) {
			return cause.kind
		}
	}
	return "other"
}

var _ AuditedReviewer = (*NativeVMReviewer)(nil)

func nativeCheckLeasedAuth(ctx context.Context, p *AuthProfile, gate AuthExportVerifier) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil || gate == nil || gate.ConfirmAuthExport(ctx, p.journal.Binding) != nil {
		return ErrAuthTransfer
	}
	j, err := readAuthJournal(p.held.root)
	if err != nil || j != p.journal || j.Stage != "active" || j.TransferStage != "" {
		return ErrAuthRecoveryRequired
	}
	raw, err := readAuth(p.held.root, p.store.authRelative(), false)
	defer clear(raw)
	if err != nil || authHash(raw) != j.SeedHash {
		return ErrAuthRecoveryRequired
	}
	values, err := nativeAuthValues(j.Provider, raw)
	for _, value := range values {
		clear(value)
	}
	return err
}
