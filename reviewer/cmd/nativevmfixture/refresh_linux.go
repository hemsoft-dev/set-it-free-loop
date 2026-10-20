//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

type refreshPhase struct {
	InputHash                                     string
	Passed, RotatedAuthReturned, RecoveryRequired bool
	Counts                                        f.Counts
	SourceCalls                                   int
	Evidence                                      *r.ProviderEvidence
}

func runNativeRefresh(c config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	for _, dir := range []string{"auth", "launches"} {
		if os.Mkdir(filepath.Join(c.Root, dir), 0700) != nil {
			return errors.New("fresh refresh directories required")
		}
	}
	authRoot := filepath.Join(c.Root, "auth")
	store, err := r.NewAuthStore(c.Provider, authRoot)
	if err != nil {
		return err
	}
	seed := f.ExpiredSeed(c.Provider)
	err = store.Refresh(ctx, func(old []byte) ([]byte, error) {
		if len(old) != 0 {
			return nil, errors.New("existing synthetic seed denied")
		}
		return seed, nil
	})
	clear(seed)
	if err != nil {
		return err
	}
	limits := r.DefaultLimits()
	limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	hints := r.NativeVMProfileHints{}
	if c.Provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	start := time.Now()
	phases := []refreshPhase{}
	unitsFixture := &nativeProtocolFixture{config: c}
	passed := true
	phaseCount := 2
	if c.UncertainRefresh {
		phaseCount = 1
	}
	for index := range phaseCount {
		// Reopen the protected store and construct a new session, reviewer and VM.
		// The second job receives only the returned file, never another seed.
		store, err = r.NewAuthStore(c.Provider, authRoot)
		if err != nil {
			return err
		}
		job := []string{"native-fake-refresh", "native-fake-refreshed-reuse"}[index]
		s, err := r.NewSession(job, snapshot, r.Digest("synthetic-native-refresh"), c.Provider, 1, "immutable source evidence", nil, nil, limits)
		if err != nil {
			return err
		}
		reader, err := r.NewReader(f.Store{}, snapshot, []string{"source.go"}, limits)
		if err != nil {
			return err
		}
		backend, policy, err := f.NewRefresh(s, index == 1)
		if c.UncertainRefresh {
			backend, policy, err = f.NewUncertainRefresh(s)
		}
		if err != nil {
			return err
		}
		transport, closeBackend := protocolUpstream(backend, c.Provider)
		worker, err := r.NewNativeVMReviewer(r.NativeVMOptions{Provider: c.Provider, Root: filepath.Join(c.Root, "launches"), Stub: c.Stub.Path, StubHash: c.Stub.SHA256, QEMU: c.QEMU, Kernel: c.Kernel, Initrd: c.Initrd, Firmware: c.Firmware, Runtime: c.Runtime, Auth: store, Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
			if r.Digest(got) != r.Digest(s) {
				return errors.New("foreign fake refresh session")
			}
			return nil
		}})
		if err != nil {
			closeBackend()
			return err
		}
		report, access, evidence, reviewErr := worker.ReviewAudited(ctx, s)
		closeBackend()
		phase := refreshPhase{InputHash: r.Digest(s), Counts: backend.Counts(), Evidence: evidence}
		if access != nil {
			phase.SourceCalls = access.ToolCalls
		}
		recovery := store.Refresh(context.WithoutCancel(ctx), func(current []byte) ([]byte, error) {
			phase.RotatedAuthReturned = f.RotatedAuth(c.Provider, current)
			return current, nil
		})
		expectedRefresh := 1 - index
		phase.Passed = reviewErr == nil && recovery == nil && phase.RotatedAuthReturned && len(report.Findings) == 1 && phase.SourceCalls == 2 && phase.Counts.Inference == 3 && phase.Counts.Failures == 0 && phase.Counts.Telemetry == 0 && phase.Counts.Refreshes == expectedRefresh && phase.Counts.RefreshFailures == 0 && evidence != nil && evidence.Access.RefreshRequests == expectedRefresh
		if c.UncertainRefresh {
			phase.RecoveryRequired = errors.Is(recovery, r.ErrAuthRecoveryRequired)
			reopened, openErr := r.NewAuthStore(c.Provider, authRoot)
			if openErr == nil {
				retry := reopened.Refresh(context.WithoutCancel(ctx), func(current []byte) ([]byte, error) {
					return current, errors.New("quarantined credentials reached mutation callback")
				})
				phase.RecoveryRequired = phase.RecoveryRequired && errors.Is(retry, r.ErrAuthRecoveryRequired)
			} else {
				phase.RecoveryRequired = false
			}
			phase.Passed = errors.Is(reviewErr, r.ErrAuthRecoveryRequired) && phase.RecoveryRequired && len(report.Findings) == 0 && phase.Counts.Inference == 0 && phase.Counts.Refreshes == 1 && phase.Counts.RefreshFailures == 0 && evidence != nil && evidence.Access.RefreshRequests == 1 && evidence.Access.Forwarded > evidence.Access.Responses && evidence.Access.Failed && evidence.Access.Stopped
		}
		if c.Provider == r.Codex && !c.UncertainRefresh {
			phase.Passed = phase.Passed && phase.Counts.MetadataCaptured && phase.Counts.Negotiation == 0
		}
		phases = append(phases, phase)
		unitsFixture.phases = append(unitsFixture.phases, protocolPhase{InputHash: phase.InputHash})
		if !phase.Passed {
			passed = false
			break
		}
	}
	units, unitPass := unitsFixture.unitsExpectingCompletion(!c.UncertainRefresh)
	roots, globErr := filepath.Glob(filepath.Join(c.Root, "launches", "*", "qemu-runtime"))
	passed = passed && len(phases) == phaseCount && unitPass && globErr == nil && len(roots) == 0
	result := struct {
		Version                                                                               int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead                          string
		Provider                                                                              r.Provider
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified, RuntimeRootsRemoved bool
		ElapsedMS                                                                             int64
		Phases                                                                                []refreshPhase
		Units                                                                                 []protocolUnit
	}{1, "native-fake-refresh", sourceHead, sourceTree, c.ImageSourceHead, c.Provider, passed, true, false, false, len(roots) == 0, time.Since(start).Milliseconds(), phases, units}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(raw, '\n'), 0600) != nil {
		return errors.New("refresh receipt failed")
	}
	if !passed {
		return errors.New("native refresh qualification incomplete")
	}
	return nil
}
