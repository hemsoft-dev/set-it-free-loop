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
	"sync/atomic"
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
	if !nativeSourcePolicies(o.Provider, o.Policy) {
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
	// attempt is the durable attempt ID bound into the stopped receipt, or
	// empty outside a durable attempt.
	attempt string
	// drain is the controller invocation's shutdown drain this launch holds
	// from auth export until its stopped receipt; see holdShutdownGrace.
	drain        *shutdownDrain
	releaseGrace func()
	stopGrace    func() bool
	// expired is set once the work deadline has ended client work; see
	// armWorkDeadline. stopWork cancels that timer.
	expired  atomic.Bool
	stopWork func() bool
	// now and after are the work-deadline clock; nil uses the real clock.
	now   func() time.Time
	after func(time.Duration, func()) func() bool
}

var _ attemptAuditedReviewer = (*NativeVMReviewer)(nil)

func (r *NativeVMReviewer) ReviewAudited(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
	return r.reviewAttemptAudited(ctx, s, "")
}
func (r *NativeVMReviewer) reviewAttemptAudited(ctx context.Context, s Session, attempt string) (Report, *SourceAccess, *ProviderEvidence, error) {
	report, access, evidence, err := r.reviewTaskAudited(ctx, s, nil, attempt)
	return report.review, access, evidence, err
}
func (r *NativeVMReviewer) reviewTaskAudited(ctx context.Context, s Session, control *ControlSession, attempt string) (report nativeVMTaskReport, access *SourceAccess, evidence *ProviderEvidence, result error) {
	blocked := func() (nativeVMTaskReport, *SourceAccess, *ProviderEvidence, error) {
		return nativeVMTaskReport{}, nil, nil, errors.Join(PhaseError{State: "blocked", BeforeWork: true}, ErrLiveBlocked)
	}
	if r == nil || ctx == nil || httptrace.ContextClientTrace(ctx) != nil || s.Validate() != nil || !nativeControlMatches(control, s) || s.Identity.Provider != r.options.Provider || r.options.Reader.snapshot != s.Snapshot {
		return blocked()
	}
	if ctx.Err() != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(ctx.Err(), true)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Limits.Timeout)
	defer cancel()
	if r.options.Check(ctx, s) != nil {
		if ctx.Err() != nil {
			return nativeVMTaskReport{}, nil, nil, workerLauncherError(ctx.Err(), true)
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
	if verifyFrozenReports(ctx, s, reader) != nil {
		return nativeVMTaskReport{}, nil, nil, adapterValidation(ErrSourceService, true)
	}
	request := nativeVMRequest{Version: 1, Binding: binding, Session: s, Paths: paths, Policy: r.options.Policy, ProfileHints: r.options.ProfileHints}
	if control != nil {
		request.Version = 2
		request.Task = "evaluation-control-v1"
		request.Control = control
		historyStore := &nativeControlHistoryStore{store: store, objects: map[[2]string]Object{}, maxBytes: len(control.History) * s.Limits.ReaderBytes, maxEntries: len(control.History) * s.Limits.ReaderCalls}
		for i, prior := range control.History {
			input := *control
			input.Step = i + 1
			input.History = control.History[:i]
			// Replaying a frozen phase gets that phase's bounded verifier.
			// Neither other history nor current output spends its allowance.
			historyReader, historyErr := NewReader(historyStore, s.Snapshot, paths, s.Limits)
			if historyErr != nil || verifyControlSource(ctx, input, prior, historyReader) != nil {
				return nativeVMTaskReport{}, nil, nil, adapterValidation(ErrSourceService, true)
			}
		}
	}
	if !request.valid() {
		return blocked()
	}
	images, err := OpenNativeVMBootImages(ctx, VMBootSpec{Version: 1, Binding: binding, QEMU: r.options.QEMU, Kernel: r.options.Kernel, Initrd: r.options.Initrd}, r.options.Firmware, r.options.Runtime)
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(err, true)
	}
	defer images.Close()
	manifest, err := images.NativeManifest(s)
	if err != nil {
		return blocked()
	}
	supervisor, err := OpenNativeVMSupervisor(ctx, r.options.Root, r.options.Stub, r.options.StubHash)
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(err, true)
	}
	defer supervisor.Close()
	channels, err := openNativeVMChannels()
	if err != nil {
		return blocked()
	}
	defer channels.Close()
	stopControl := channels.interruptControlOnCancel(ctx)
	defer stopControl()
	phase := &nativeVMHostPhase{request: request, manifest: manifest, reader: reader, supervisor: supervisor, channels: channels, stage: "provider-bridge", attempt: attempt}
	// Every later return drains the concrete containment and private transports.
	defer func() { report, access, evidence, result = phase.finishTask(ctx, report, result) }()
	phase.bridge, err = startNativeProviderBridge(ctx, s, request.Policy, r.options.Transport, channels.host[2])
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(err, true)
	}
	phase.armWorkDeadline(ctx, s.Limits.Timeout)
	phase.objectsDone = make(chan error, 1)
	go func() { phase.objectsDone <- ServeVMObjects(ctx, channels.host[1], s, paths, store) }()
	phase.stage = "prepare"
	if err = supervisor.Prepare(manifest); err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(err, true)
	}
	phase.started = true // Start may be uncertain even when it returns an error.
	phase.stage = "start"
	if err = supervisor.Start(ctx, binding); err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(err, true)
	}
	phase.stage = "grant"
	if err = supervisor.grant(ctx, manifest, images, channels); err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(err, true)
	}
	phase.stage = "request"
	if writeVMRecord(channels.host[0], request, 4<<20) != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(ErrVMBoot, true)
	}
	phase.stage = "ready"
	ready, err := readNativeVMReady(channels.host[0], request, func(stage string) {
		phase.stage = "guest/" + stage
		if strings.HasPrefix(stage, "diag-") && len(phase.bootDiagnostics) < 8 {
			phase.bootDiagnostics = append(phase.bootDiagnostics, stage)
		}
	})
	if err != nil || !ready.valid(request) {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(ErrVMBoot, true)
	}
	// No auth store is leased or read until the exact private guest is ready.
	phase.stage = "auth-lease"
	phase.profile, err = r.options.Auth.LeaseProfile(ctx, binding)
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, PhaseError{State: "auth_failed", BeforeWork: true}
	}
	if nativeCheckLeasedAuth(ctx, phase.profile, nativeVMExportGate{supervisor, manifest, request, ready}) != nil {
		return nativeVMTaskReport{}, nil, nil, PhaseError{State: "auth_failed", BeforeWork: true}
	}
	// Once service shutdown drains this invocation no auth is exported.
	if !phase.holdShutdownGrace(ctx) {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(context.Canceled, true)
	}
	// Nor is any exported once the work deadline has ended client work.
	if phase.workExpired() {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(context.DeadlineExceeded, true)
	}
	phase.stage = "auth-export"
	seed, err := phase.profile.ExportAuth(ctx, nativeVMExportGate{supervisor, manifest, request, ready})
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, PhaseError{State: "auth_failed", BeforeWork: true}
	}
	phase.exported = true
	defer seed.Close()
	wire, err := nativeVMSeedRecord(seed)
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, PhaseError{State: "auth_failed"}
	}
	defer clear(wire)
	phase.stage = "seed-send"
	if writeVMRecord(channels.host[0], wire, 100<<10) != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(ErrVMBoot, false)
	}
	phase.stage = "result"
	finished, err := readVMRecord[nativeVMFinished](channels.host[0], 4<<20)
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, workerLauncherError(ErrVMBoot, false)
	}
	phase.finished = &finished
	phase.stage = "result-validation"
	if err := finished.failure(request); err != nil {
		return nativeVMTaskReport{}, nil, nil, err
	}
	report, err = decodeNativeVMTaskReport(request, finished.Report)
	if err != nil {
		return nativeVMTaskReport{}, nil, nil, adapterValidation(ErrVMBoot, false)
	}
	return report, nil, nil, nil
}

func (p *nativeVMHostPhase) finish(ctx context.Context, report Report, primary error) (Report, *SourceAccess, *ProviderEvidence, error) {
	result, access, evidence, err := p.finishTask(ctx, nativeVMTaskReport{review: report}, primary)
	return result.review, access, evidence, err
}
func (p *nativeVMHostPhase) finishTask(ctx context.Context, report nativeVMTaskReport, primary error) (nativeVMTaskReport, *SourceAccess, *ProviderEvidence, error) {
	// The grace hold lasts until the stopped receipt below is written.
	if p.releaseGrace != nil {
		defer p.releaseGrace()
	}
	drained := p.shutdownDrained()
	if p.stopWork != nil {
		p.stopWork()
	}
	expired := p.workExpired()
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
		return nativeVMTaskReport{}, nil, evidence, errors.Join(primary, ErrWorkerStopPending)
	}
	// Valid auth returns can be persisted even when model output is invalid.
	// They remain private and must match the original binding/seed journal.
	if p.profile != nil {
		if p.exported {
			if p.finished == nil || p.finished.Binding != p.request.Binding || p.finished.RequestHash != Digest(p.request) {
				primary = errors.Join(primary, ErrAuthRecoveryRequired)
			} else {
				returned, err := ReadAuthTransfer(bytes.NewReader(p.finished.AuthReturn))
				if err == nil && p.bridge != nil {
					info, infoErr := returned.Info()
					if infoErr != nil || evidence == nil || nativeRefreshReturnUncertain(evidence, info) {
						returned.Close()
						err = ErrAuthRecoveryRequired
					}
				}
				if err == nil {
					err = p.profile.ImportAuthReturn(cleanup, returned, p.supervisor)
				}
				if err == nil {
					// Keep a durable stopped receipt before Complete disposes the
					// returned profile. A later execution-journal failure cannot
					// reconstruct this evidence or safely replay the native phase.
					err = p.persistReturnedAuthIntent(cleanup, evidence)
					if err != nil {
						primary = errors.Join(primary, err)
					}
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
	if p.started {
		primary = errors.Join(primary, p.supervisor.CleanupRuntime(cleanup, p.request.Binding))
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
		case access.requestFlooded():
			// A source request flood is never a clean outcome, even with a
			// complete report. Its audit and auth return are kept.
			primary = adapterValidation(ErrSourceRequestFlood, false)
		case evidence == nil || evidence.validateStoppedCaptured(p.request.Session.Identity.Provider, p.request.Session.Limits, Digest(p.request.Session)) != nil:
			primary = ErrProviderAuditPending
		case evidence.Access.ValidateCompleted(p.request.Session, evidence.Policy) != nil:
			primary = ErrProviderEgressFailed
		}
	}
	if primary == nil {
		validated, validationErr := validateNativeVMTaskSource(cleanup, p.request, report, access, p.reader)
		primary = validationErr
		if validationErr == nil {
			report = validated
		}
	}

	// A failure after shutdown revoked provider intake reports the shutdown,
	// so the controller may requeue the stopped job under its consumed
	// budget. A latched route, request or response refusal, or exhaustion, keeps its
	// own classification.
	if primary != nil && drained && !nativeProviderRefused(evidence) {
		primary = errors.Join(primary, context.Canceled)
	} else if primary != nil && expired && p.finished != nil && !nativeProviderRefused(evidence) {
		// The work deadline ended this session and the guest still returned
		// its finished record within the grace. The phase expired; its stop
		// proof, auth return and audit were handled as for any typed failure.
		// Without that record the fenced path above is unchanged.
		primary = errors.Join(PhaseError{State: "incomplete_expired"}, primary, errNativeWorkExpired)
	}
	if p.started && p.persistAudit(cleanup, report.value(), access, evidence, primary) != nil {
		primary = errors.Join(primary, ErrProviderAuditPending)
	}
	if primary != nil {
		before := !p.exported && (evidence == nil || evidence.Access.Requests == 0)
		var classified PhaseError
		if errors.As(primary, &classified) {
			classified.BeforeWork = before
			return nativeVMTaskReport{}, access, evidence, errors.Join(classified, nativeVMCauses(primary))
		}
		return nativeVMTaskReport{}, access, evidence, errors.Join(PhaseError{State: phaseState(primary), BeforeWork: before}, nativeVMCauses(primary))
	}
	return report, access, evidence, nil
}

// holdShutdownGrace registers this launch with its controller invocation's
// shutdown drain before auth export, and reports false once that drain has
// begun. While the hold lasts, a drain revokes provider intake: the private
// provider stream and any in-flight request close, the guest's client proxy
// stops its CLI on the failed exchange, and guest PID1 returns auth in its
// ordinary finished record. Nothing else changes. The host still reads that
// record only under the session deadline and Close's later cancellation,
// proves whole-unit stop first, and commits auth only through its stopped
// return. Launches outside a controller invocation hold nothing.
func (p *nativeVMHostPhase) holdShutdownGrace(ctx context.Context) bool {
	drain := shutdownDrainOf(ctx)
	if drain == nil {
		return true
	}
	release, ok := drain.hold()
	if !ok {
		return false
	}
	p.drain, p.releaseGrace = drain, release
	bridge := p.bridge
	p.stopGrace = context.AfterFunc(drain.signal, func() {
		if bridge != nil {
			bridge.revoke()
		}
	})
	return !drain.draining()
}

// armWorkDeadline ends client work NativeVMResultGrace before the session
// deadline, or before an earlier caller deadline. Like a shutdown drain it
// revokes provider intake: the private provider stream and any in-flight
// request close, the guest's client proxy stops its CLI on the failed
// exchange, and guest PID1 returns auth and the source audit in its ordinary
// finished record. The guest's own phase deadline ends a client that is not
// using the provider. The result read keeps the session deadline, so the
// guest has the grace to return that record; after it the fenced path runs.
func (p *nativeVMHostPhase) armWorkDeadline(ctx context.Context, timeout time.Duration) {
	work, ok := nativeVMWorkDeadline(ctx, timeout)
	if !ok {
		return
	}
	now, after := p.now, p.after
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	p.stopWork = after(work.Sub(now()), p.expire)
}

// errNativeWorkExpired marks, for the stopped receipt only, a session that the
// work deadline ended and whose guest still returned its finished record. It
// is not retained in the returned error.
var errNativeWorkExpired = errors.New("native work deadline ended the session")

// workExpired reports whether the work deadline has ended client work.
func (p *nativeVMHostPhase) workExpired() bool { return p.expired.Load() }

// expire marks the work deadline and revokes provider intake. It attests
// nothing: the stop proof and the stopped receipt are unchanged.
func (p *nativeVMHostPhase) expire() {
	p.expired.Store(true)
	if p.bridge != nil {
		p.bridge.revoke()
	}
}

// shutdownDrained ends the revocation hook and reports whether shutdown had
// begun draining this held launch.
func (p *nativeVMHostPhase) shutdownDrained() bool {
	if p.stopGrace != nil {
		p.stopGrace()
	}
	return p.releaseGrace != nil && p.drain.draining()
}

func nativeProviderRefused(evidence *ProviderEvidence) bool {
	return evidence != nil && (evidence.Access.RouteDenials > 0 || evidence.Access.ResponseDenials > 0 || evidence.Access.RequestDenials > 0 || evidence.Access.Exhausted)
}

// A stopped CLI can return its old file after the provider already rotated it.
// Keep the exported lease pending if any refresh lacks a complete, nonfailed
// provider receipt, or if a refresh did not produce changed local auth. A valid
// changed return after fully captured HTTP may still commit when model output
// is invalid. This is deliberately conservative until explicit recovery.
func nativeRefreshReturnUncertain(evidence *ProviderEvidence, returned AuthTransferInfo) bool {
	if evidence == nil {
		return true
	}
	a := evidence.Access
	if a.RefreshRequests == 0 {
		return false
	}
	return !a.Stopped || a.Failed || a.Exhausted || a.Requests-a.NegotiationRequests != a.Forwarded || a.Forwarded != a.Responses || returned.PayloadHash == returned.SeedHash
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
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrLiveBlocked, ErrWorkerStopPending, ErrProviderAuditPending, ErrProviderEgressPending, ErrSourceAuditPending, ErrAuthRecoveryRequired, ErrSourceRequestFlood} {
		if errors.Is(err, cause) {
			retained = append(retained, cause)
		}
	}
	return errors.Join(retained...)
}

func (p *nativeVMHostPhase) persistAudit(ctx context.Context, report any, access *SourceAccess, evidence *ProviderEvidence, failure error) error {
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
	receipt := nativeVMAuditReceipt{2, p.request.Binding, p.attempt, Digest(p.request), Digest(p.manifest), Digest(report), Digest(access), evidence, host, failure == nil, p.stage, p.exported, p.authReturned, "", nil, nativeVMFailureKind(failure), p.bootDiagnostics}
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

// nativeVMAuditReceipt is the host's private stopped receipt in the launch
// directory, written only after the whole unit is aborted and stopped.
// Version 2 adds AttemptID, the durable attempt that ran the launch.
type nativeVMAuditReceipt struct {
	Version                                           int
	Binding                                           AuthBinding
	AttemptID                                         string
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
		{ErrSourceAuditPending, "source-audit"}, {ErrSourceService, "source-service"}, {ErrSourceRequestFlood, "source-flood"},
		{ErrProviderAuditPending, "provider-audit"}, {ErrProviderEgressPending, "provider-stop"},
		{ErrProviderEgressFailed, "provider-egress"}, {context.DeadlineExceeded, "deadline"}, {errNativeWorkExpired, "deadline"},
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
