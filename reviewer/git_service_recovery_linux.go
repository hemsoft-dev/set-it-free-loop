//go:build linux

package reviewer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// ProviderRecoveryRequest names one outstanding provider attempt and the
// original native launch that ran it. Only the trusted operator layer builds
// it; no PR, comment, configuration or model output reaches this type.
// RetireUnreturnedAuth consents to retiring a provider's canonical auth when
// its exported seed was never durably returned. Without it, that case refuses.
type ProviderRecoveryRequest struct {
	JobID, AttemptID, LaunchID string
	RetireUnreturnedAuth       bool
}

// ProviderRecoveryResult reports what an accepted recovery did.
type ProviderRecoveryResult struct {
	JobID, AttemptID, LaunchID string
	Provider                   Provider
	Round                      int
	Auth, State                string
}

// ProviderRecoveryCandidate is a nonsecret view of a job that holds a durable
// lease. Launches lists launch directories under the release's provider root
// whose unit journal binds the attempt's session and whose stopped audit
// receipt binds its attempt ID. They are hints only; recovery reverifies
// everything under locks.
type ProviderRecoveryCandidate struct {
	JobID, AttemptID         string
	Provider                 Provider
	Round                    int
	SessionHash              string
	Outstanding              bool
	ProviderEvidenceRecorded bool
	Launches                 []string
}

// recoveryPhase admits operator recovery only on a started service whose
// controller is open with no Run, worker, Close or concurrent operation.
func (s *gitReviewerService) recoveryPhase(ctx context.Context) (context.Context, func(), error) {
	if s == nil {
		return nil, nil, ErrLiveBlocked
	}
	phase, finish, err := s.begin(ctx, 3*time.Minute, false)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	idle := s.startup && !s.ran && !s.closing && !s.closed && s.runDone == nil
	s.mu.Unlock()
	if !idle || s.controller == nil || s.options.NativeReleases == nil {
		finish()
		return nil, nil, ErrLiveBlocked
	}
	c := s.controller
	c.mu.Lock()
	open := !c.closed && c.active == nil && c.owner != nil
	c.mu.Unlock()
	if !open {
		finish()
		return nil, nil, ErrLiveBlocked
	}
	return phase, finish, nil
}

func (s *gitReviewerService) openNativeSupervisor(ctx context.Context, o NativeVMOptions) (*NativeVMSupervisor, error) {
	if s.openNative != nil {
		return s.openNative(ctx, o)
	}
	return OpenNativeVMSupervisor(ctx, o.Root, o.Stub, o.StubHash)
}

// RecoverStoppedProvider is the explicit operator recovery for a provider
// attempt that kept its lease because its worker stopped without a complete
// result, for example after Close during a session. It never runs on a timer,
// from polling or from any GitHub or model input.
//
// It refuses without durable change unless: the service is started but not
// running; the job's lease, outstanding attempt ID and recorded stopped
// provider evidence match; the original launch's unit journal, aborted gate
// and private stopped audit bind that session and evidence; and a fresh
// whole-unit stop proof succeeds. Auth then follows the stopped-worker rules
// (settleStoppedAttemptAuth), runtime residue is removed, and the lease is
// released as auth_failed or provider_unavailable with the session charged.
// A job fenced meanwhile keeps cancelled or stale; State reports the final one.
func (s *gitReviewerService) RecoverStoppedProvider(ctx context.Context, request ProviderRecoveryRequest) (ProviderRecoveryResult, error) {
	phase, finish, err := s.recoveryPhase(ctx)
	if err != nil {
		return ProviderRecoveryResult{}, err
	}
	defer finish()
	launch, decodeErr := hex.DecodeString(request.LaunchID)
	if request.JobID == "" || request.AttemptID == "" || decodeErr != nil || len(launch) != 16 || hex.EncodeToString(launch) != request.LaunchID {
		return ProviderRecoveryResult{}, ErrFenced
	}
	store := s.options.Credentials.Journal.store
	d, err := store.Snapshot(phase)
	if err != nil {
		return ProviderRecoveryResult{}, err
	}
	j := d.Jobs[request.JobID]
	if j == nil || j.LeaseToken == "" || !j.Outstanding || !j.AttemptProviderEvidenceRequired || j.AttemptID != request.AttemptID {
		return ProviderRecoveryResult{}, ErrFenced
	}
	evidence, err := recordedStoppedAttempt(j)
	if err != nil {
		return ProviderRecoveryResult{}, errors.Join(ErrProviderRecovery, err)
	}
	limits, err := protocolLimits(j.Job)
	if err != nil {
		return ProviderRecoveryResult{}, err
	}
	options, ok := s.options.NativeReleases[j.Job.Release][j.AttemptProvider]
	if !ok || options.Auth == nil || options.Auth.provider != j.AttemptProvider {
		return ProviderRecoveryResult{}, ErrProviderRecovery
	}
	supervisor, err := s.openNativeSupervisor(phase, options)
	if err != nil {
		return ProviderRecoveryResult{}, errors.Join(ErrProviderRecovery, err)
	}
	defer supervisor.Close()
	attempt := nativeStoppedAttempt{Provider: j.AttemptProvider, AttemptID: j.AttemptID, SessionHash: j.AttemptInputHash, LaunchID: request.LaunchID, Limits: limits, Evidence: evidence}
	binding, receipt, err := supervisor.stoppedAttemptReceipt(phase, attempt)
	if err != nil {
		return ProviderRecoveryResult{}, err
	}
	auth, err := settleStoppedAttemptAuth(phase, options.Auth, supervisor, binding, receipt, request.RetireUnreturnedAuth)
	if err != nil {
		return ProviderRecoveryResult{}, err
	}
	if err = supervisor.CleanupRuntime(phase, binding); err != nil {
		return ProviderRecoveryResult{}, errors.Join(ErrProviderRecovery, err)
	}
	state := recoveredAttemptState(auth, receipt)
	now := time.Now
	if s.options.Credentials.Journal.now != nil {
		now = s.options.Credentials.Journal.now
	}
	release := ProviderRecoveryReceipt{AttemptID: j.AttemptID, LaunchID: request.LaunchID, Provider: j.AttemptProvider, Round: j.AttemptRound, EvidenceHash: Digest(cloneProviderEvidence(evidence)), Auth: auth, State: state}
	durable, cancel := context.WithTimeout(context.WithoutCancel(phase), 10*time.Second)
	defer cancel()
	if state, err = store.releaseRecoveredProviderAttempt(durable, j.Job.ID, j.LeaseToken, release, now()); err != nil {
		return ProviderRecoveryResult{}, err
	}
	return ProviderRecoveryResult{JobID: j.Job.ID, AttemptID: j.AttemptID, LaunchID: request.LaunchID, Provider: j.AttemptProvider, Round: j.AttemptRound, Auth: auth, State: state}, nil
}

// ProviderRecoveryCandidates lists every job holding a durable lease. It is
// read-only and runs under the same idle-service precondition as recovery.
func (s *gitReviewerService) ProviderRecoveryCandidates(ctx context.Context) ([]ProviderRecoveryCandidate, error) {
	phase, finish, err := s.recoveryPhase(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	d, err := s.options.Credentials.Journal.store.Snapshot(phase)
	if err != nil {
		return nil, err
	}
	candidates := []ProviderRecoveryCandidate{}
	for _, j := range d.Jobs {
		if j.LeaseToken == "" {
			continue
		}
		_, recordErr := recordedStoppedAttempt(j)
		c := ProviderRecoveryCandidate{JobID: j.Job.ID, AttemptID: j.AttemptID, Provider: j.AttemptProvider, Round: j.AttemptRound, SessionHash: j.AttemptInputHash, Outstanding: j.Outstanding, ProviderEvidenceRecorded: j.AttemptProviderEvidenceRequired && recordErr == nil, Launches: []string{}}
		if options, ok := s.options.NativeReleases[j.Job.Release][j.AttemptProvider]; ok && j.Outstanding && validAuthHash(j.AttemptInputHash) {
			c.Launches = nativeLaunchesForAttempt(options.Root, j.AttemptInputHash, j.AttemptID)
		}
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(a, b int) bool { return candidates[a].JobID < candidates[b].JobID })
	return candidates, nil
}

// nativeLaunchesForAttempt is a bounded, read-only scan for operator hints.
func nativeLaunchesForAttempt(root, sessionHash, attemptID string) []string {
	launches := []string{}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return launches
	}
	directory, err := os.Open(root)
	if err != nil {
		return launches
	}
	names, err := directory.Readdirnames(4096)
	directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return launches
	}
	sort.Strings(names)
	for _, name := range names {
		binding := nativeLaunchBinding(sessionHash, name)
		if !binding.valid() {
			continue
		}
		var unit struct{ Binding AuthBinding }
		var audit struct {
			Binding   AuthBinding
			AttemptID string
		}
		if readLaunchHint(filepath.Join(root, name, "unit.json"), &unit) != nil || unit.Binding != binding {
			continue
		}
		if readLaunchHint(filepath.Join(root, name, "native-audit.json"), &audit) != nil || audit.Binding != binding || audit.AttemptID != attemptID {
			continue
		}
		launches = append(launches, name)
	}
	return launches
}

func readLaunchHint(path string, value any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	file.Close()
	if err != nil {
		return err
	}
	if len(raw) > 65536 {
		return errors.New("launch record too large")
	}
	return json.Unmarshal(raw, value)
}
