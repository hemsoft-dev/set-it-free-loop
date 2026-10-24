//go:build linux

package reviewer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func gitServiceNativeOptions(provider Provider, checks, calls *atomic.Int32) NativeVMOptions {
	policy := egressTestPolicy(provider)
	policy.ResponsePolicy = CodexSourceResponses
	withCodexRequestPolicy(&policy)
	if provider == Claude {
		policy.ResponsePolicy = ClaudeSourceMessages
	}
	return NativeVMOptions{
		Provider: provider, Root: "/unopened/" + string(provider), Stub: "/unopened/native-stub", StubHash: strings.Repeat("a", 64),
		Auth: &AuthStore{provider: provider, directory: "/unopened/auth/" + string(provider)}, Policy: policy,
		Transport: egressRoundTrip(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, ErrLiveBlocked }),
		Check:     func(context.Context, Session) error { checks.Add(1); return ErrLiveBlocked },
	}
}

func gitServiceRuntimeJob(t *testing.T) (Job, *SnapshotEvidence, *JobEvidence) {
	t.Helper()
	git := newSnapshotGitFixture(t)
	evidence, err := CaptureSnapshotEvidence(context.Background(), git.store(), git.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob()
	job.Snapshot, job.Packet = git.snapshot, evidence.Packet()
	bound, err := evidence.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	return job, evidence, bound
}

func TestGitServiceNativeRuntimeUsesAuditedWorkersAndExactJobSource(t *testing.T) {
	job, _, bound := gitServiceRuntimeJob(t)
	var providerChecks, calls atomic.Int32
	gateCalls := 0
	providers := map[Provider]NativeVMOptions{Codex: gitServiceNativeOptions(Codex, &providerChecks, &calls), Claude: gitServiceNativeOptions(Claude, &providerChecks, &calls)}
	build, err := newGitServiceNativeRuntime(map[string]map[Provider]NativeVMOptions{job.Release: providers}, func(_ context.Context, release string) error {
		if release != job.Release {
			t.Error("runtime qualified a different release")
		}
		gateCalls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := build(context.Background(), job, bound)
	if err != nil || len(engine.Reviewers) != 2 {
		t.Fatal(err)
	}
	limits, _ := protocolLimits(job)
	readers := map[*Reader]bool{}
	for _, provider := range job.Config.Providers() {
		worker, ok := engine.Reviewers[provider].(*NativeVMReviewer)
		if !ok {
			t.Fatal("runtime did not compose the concrete native VM reviewer")
		}
		if _, ok := engine.Reviewers[provider].(AuditedReviewer); !ok {
			t.Fatal("native source/provider evidence contract lost")
		}
		if worker.options.Reader.store != bound.evidence.store || worker.options.Reader.snapshot != job.Snapshot || readers[worker.options.Reader] {
			t.Fatal("runtime reused a reader or changed source")
		}
		readers[worker.options.Reader] = true
		session, _ := NewSession(job.ID, job.Snapshot, job.Config.Hash(), provider, 1, job.Packet, nil, nil, limits)
		if _, _, _, err := worker.ReviewAudited(context.Background(), session); !errors.Is(err, ErrLiveBlocked) {
			t.Fatal("closed native gate reached an unopened profile/runtime", err)
		}
		before := providerChecks.Load()
		session.JobID = "another_valid_job"
		if _, _, _, err := worker.ReviewAudited(context.Background(), session); !errors.Is(err, ErrLiveBlocked) || providerChecks.Load() != before {
			t.Fatal("another job reached provider authority", err)
		}
	}
	if calls.Load() != 0 || providerChecks.Load() != 2 || gateCalls != 3 {
		t.Fatal("native runtime skipped gates or dispatched inference", calls.Load(), providerChecks.Load(), gateCalls)
	}
}

func TestGitServiceNativeRuntimeFreezesCatalogAndSelectsOlderRelease(t *testing.T) {
	job, evidence, _ := gitServiceRuntimeJob(t)
	var checks, calls atomic.Int32
	current := gitServiceNativeOptions(Codex, &checks, &calls)
	old := current
	old.Root = "/unopened/older-release"
	oldRelease := strings.Repeat("e", 40)
	catalog := map[string]map[Provider]NativeVMOptions{job.Release: {Codex: current}, oldRelease: {Codex: old}}
	allowed := true
	qualified := ""
	build, err := newGitServiceNativeRuntime(catalog, func(_ context.Context, release string) error {
		qualified = release
		if !allowed {
			return ErrLiveBlocked
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's maps and slice backing must not change the
	// trusted options already captured for durable older jobs.
	catalog[oldRelease][Codex] = NativeVMOptions{}
	current.Policy.Routes[0].Authority = "untrusted-changed-host"
	delete(catalog, job.Release)
	job.Release, job.Config.Mode = oldRelease, CodexOnly
	bound, err := evidence.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := build(context.Background(), job, bound)
	if err != nil || len(engine.Reviewers) != 1 || qualified != oldRelease {
		t.Fatal("older release was replaced or padded", err, qualified)
	}
	worker := engine.Reviewers[Codex].(*NativeVMReviewer)
	if worker.options.Root != old.Root || worker.options.Policy.Routes[0].Authority == "untrusted-changed-host" {
		t.Fatal("catalog mutation reached native options")
	}
	allowed = false
	limits, _ := protocolLimits(job)
	session, _ := NewSession(job.ID, job.Snapshot, job.Config.Hash(), Codex, 1, job.Packet, nil, nil, limits)
	if _, _, _, err := worker.ReviewAudited(context.Background(), session); !errors.Is(err, ErrLiveBlocked) || checks.Load() != 0 || calls.Load() != 0 {
		t.Fatal("revoked older release read auth or reached provider", err)
	}
	job.Release = strings.Repeat("f", 40)
	bound, _ = evidence.Bind(job)
	qualified = ""
	if engine, err := build(context.Background(), job, bound); err == nil || len(engine.Reviewers) != 0 || qualified != "" {
		t.Fatal("unknown release used a fallback runtime")
	}
}

func TestGitServiceNativeRuntimeRefusesPartialModeAndInvalidCatalog(t *testing.T) {
	job, _, bound := gitServiceRuntimeJob(t)
	var checks, calls atomic.Int32
	o := gitServiceNativeOptions(Codex, &checks, &calls)
	build, err := newGitServiceNativeRuntime(map[string]map[Provider]NativeVMOptions{job.Release: {Codex: o}}, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if engine, err := build(context.Background(), job, bound); err == nil || len(engine.Reviewers) != 0 || checks.Load() != 0 || calls.Load() != 0 {
		t.Fatal("dual mode silently became single-provider")
	}
	for _, kind := range []string{"reader", "typed-nil-transport", "wrong-provider", "missing-provider-check"} {
		t.Run(kind, func(t *testing.T) {
			bad := o
			switch kind {
			case "reader":
				bad.Reader = &Reader{}
			case "typed-nil-transport":
				var transport *http.Transport
				bad.Transport = transport
			case "wrong-provider":
				bad.Provider = Claude
			case "missing-provider-check":
				bad.Check = nil
			}
			if f, err := newGitServiceNativeRuntime(map[string]map[Provider]NativeVMOptions{job.Release: {Codex: bad}}, func(context.Context, string) error { return nil }); err == nil || f != nil {
				t.Fatal("invalid native release capability accepted")
			}
		})
	}
}

func TestGitReviewerServiceComposesNativeCatalogWithoutAuthOrInference(t *testing.T) {
	o, _, reader, publisher, signs := gitServiceFixture(t)
	var checks, calls atomic.Int32
	o.Runtime = nil
	o.NativeReleases = map[string]map[Provider]NativeVMOptions{o.Release: {Codex: gitServiceNativeOptions(Codex, &checks, &calls), Claude: gitServiceNativeOptions(Claude, &checks, &calls)}}
	s, err := newGitReviewerService(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	job, _, bound := gitServiceRuntimeJob(t)
	engine, err := s.controller.options.Runtime(context.Background(), job, bound)
	if err != nil || len(engine.Reviewers) != 2 {
		t.Fatal("concrete service did not compose its native catalog", err)
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if signs.Load() != 0 || rCalls != 0 || pCalls != 0 || checks.Load() != 0 || calls.Load() != 0 {
		t.Fatal("native composition read credentials or ran providers")
	}
}
