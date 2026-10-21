//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

type controlReviewer struct{ fixture *nativeProtocolFixture }

func (w controlReviewer) Critique(ctx context.Context, input r.ControlSession) (r.ControlReport, error) {
	report, _, _, err := w.CritiqueAudited(ctx, input)
	return report, err
}
func (w controlReviewer) CritiqueAudited(ctx context.Context, input r.ControlSession) (r.ControlReport, *r.SourceAccess, *r.ProviderEvidence, error) {
	p := w.fixture
	blocked := r.PhaseError{State: "blocked", BeforeWork: true}
	if p == nil || !p.mu.TryLock() {
		return r.ControlReport{}, nil, nil, blocked
	}
	defer p.mu.Unlock()
	capability, err := r.ControlCapabilitySession(input)
	if err != nil || r.Digest(input.Job) != r.Digest(p.job) || input.Provider != p.config.Provider || input.Step != len(p.phases)+1 || len(p.phases) >= 4 {
		return r.ControlReport{}, nil, nil, blocked
	}
	for i, prior := range input.History {
		if r.Digest(prior) != p.phases[i].ReportHash {
			return r.ControlReport{}, nil, nil, blocked
		}
	}
	phase := protocolPhase{Provider: input.Provider, Round: input.Step, InputHash: r.Digest(capability), PacketHash: capability.PacketHash, PeerReportHashes: []string{}}
	for _, prior := range input.History {
		phase.PeerReportHashes = append(phase.PeerReportHashes, r.Digest(prior))
	}
	defer func() { p.phases = append(p.phases, phase) }()
	backend, policy, err := f.NewControl(input)
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	transport, closeUpstream := protocolUpstream(backend)
	defer closeUpstream()
	reader, err := r.NewReader(f.Store{}, capability.Snapshot, []string{"source.go"}, capability.Limits)
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	hints := r.NativeVMProfileHints{}
	if input.Provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	worker, err := r.NewNativeVMControlReviewer(r.NativeVMOptions{Provider: input.Provider, Root: filepath.Join(p.config.Root, "launches"), Stub: p.config.Stub.Path, StubHash: p.config.Stub.SHA256, QEMU: p.config.QEMU, Kernel: p.config.Kernel, Initrd: p.config.Initrd, Firmware: p.config.Firmware, Runtime: p.config.Runtime, Auth: p.auth[input.Provider], Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
		if r.Digest(got) != r.Digest(capability) {
			return errors.New("foreign fake control capability")
		}
		return nil
	}})
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	report, access, evidence, runErr := worker.CritiqueAudited(ctx, input)
	phase.Counts = backend.Counts()
	phase.ReportHash = r.Digest(report)
	if access != nil {
		phase.ToolCalls, phase.ReaderCalls = access.ToolCalls, access.ReaderCalls
	}
	phase.AuthRecoveryClear = p.auth[input.Provider].Refresh(context.WithoutCancel(ctx), func(current []byte) ([]byte, error) {
		if len(current) == 0 {
			return nil, errors.New("missing returned fake auth")
		}
		return current, nil
	}) == nil
	phase.Passed = runErr == nil && phase.AuthRecoveryClear && evidence != nil && evidence.Access.Stopped && phase.ToolCalls == 2 && phase.ReaderCalls == 2 && phase.Counts.Inference == 3 && phase.Counts.Failures == 0 && phase.Counts.Telemetry == 0 && report.Usage.Known
	if input.Provider == r.Codex && (!phase.Counts.MetadataCaptured || phase.Counts.Negotiation != 0) {
		phase.Passed = false
	}
	if runErr == nil && !phase.Passed {
		runErr = errors.New("native control phase acceptance failed")
	}
	return report, access, evidence, runErr
}
func runNativeControl(c config) error {
	configuration := r.DefaultConfig()
	configuration.Mode = r.CodexOnly
	if c.Provider == r.Claude {
		configuration.Mode = r.ClaudeOnly
	}
	configuration.Limits.OutputBytes = r.EvaluationOutputBytes
	configuration.Limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	job := r.Job{ID: "native-vm-fake-control", RequesterID: r.OwnerID, Generation: 1, Snapshot: snapshot, Config: configuration, ConfigVersion: 1, Release: sourceHead, Packet: "immutable source evidence"}
	if job.Validate() != nil {
		return errors.New("invalid fake control job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	defer cancel()
	p := &nativeProtocolFixture{config: c, job: job, auth: map[r.Provider]*r.AuthStore{}, phases: []protocolPhase{}}
	for _, name := range []string{"auth", "launches"} {
		if os.Mkdir(filepath.Join(c.Root, name), 0700) != nil {
			return errors.New("fresh control directory failed")
		}
	}
	store, err := r.NewAuthStore(c.Provider, filepath.Join(c.Root, "auth"))
	if err != nil {
		return err
	}
	seed := f.Seed(c.Provider)
	err = store.Refresh(ctx, func(old []byte) ([]byte, error) {
		if len(old) != 0 {
			return nil, errors.New("existing control seed denied")
		}
		return seed, nil
	})
	clear(seed)
	if err != nil {
		return err
	}
	p.auth[c.Provider] = store
	start := time.Now()
	outcome, runErr := r.RunSelfCritique(ctx, job, c.Provider, controlReviewer{p}, func(snap r.Snapshot) (*r.Reader, error) {
		return r.NewReader(f.Store{}, snap, []string{"source.go"}, configuration.Limits)
	})
	passed := runErr == nil && outcome.State == "complete" && outcome.ConsumedSessions == 4 && len(outcome.Reports) == 4 && len(outcome.Ledger) == 1 && outcome.Ledger[0].Status == "retracted" && len(p.phases) == 4
	for _, phase := range p.phases {
		passed = passed && phase.Passed
	}
	units, stopped := p.units()
	passed = passed && stopped
	receipt := struct {
		Version                                                          int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead     string
		Provider                                                         r.Provider
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified bool
		PhaseAttempts, ConsumedSessions                                  int
		State, LedgerHash                                                string
		ElapsedMS                                                        int64
		Phases                                                           []protocolPhase
		Units                                                            []protocolUnit
	}{1, "native-control-fake-only", sourceHead, sourceTree, c.ImageSourceHead, c.Provider, passed, true, false, false, len(p.phases), outcome.ConsumedSessions, outcome.State, r.Digest(outcome.Ledger), time.Since(start).Milliseconds(), p.phases, units}
	data, _ := json.MarshalIndent(receipt, "", "  ")
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(data, '\n'), 0600) != nil {
		return errors.New("fake control receipt failed")
	}
	fmt.Println(string(data))
	if !passed {
		return errors.New("native control acceptance incomplete")
	}
	return nil
}
