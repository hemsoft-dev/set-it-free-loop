//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gitReviewerStartupFixture(t *testing.T) (GitReviewerServiceOptions, gitUnitFixture, func(context.Context, string, string, string) (*gitUnitOwner, error), *atomic.Int32) {
	t.Helper()
	o, f, _, _, signs := gitServiceFixture(t)
	if o.Unit.Close() != nil || o.Archive.Close(context.Background()) != nil || o.RuntimeRoot.Close() != nil {
		t.Fatal("fixture ownership transfer")
	}
	m, bodies := gitRuntimeFixture(t)
	raw, pins := gitRuntimeFrame(t, m, bodies)
	artifact := filepath.Join(t.TempDir(), "runtime.artifact")
	if os.WriteFile(artifact, raw, 0600) != nil {
		t.Fatal("fixture artifact")
	}
	helper, err := os.ReadFile("/usr/bin/bwrap")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.Stat(f.stub)
	if err != nil {
		t.Fatal(err)
	}
	release := GitReviewerRelease{Version: 1, Revision: o.Release, Worker: VMBootAsset{f.stub, f.stubHash, worker.Size()}, NamespaceHelper: VMBootAsset{"/usr/bin/bwrap", gitRawHash(helper), int64(len(helper))}, GitArtifact: VMBootAsset{artifact, pins.Artifact.SHA256, pins.Artifact.Bytes}, GitRuntime: pins, Providers: map[Provider]GitReviewerProviderRelease{}}
	hosts := map[Provider]GitReviewerProviderHost{}
	for _, p := range []Provider{Codex, Claude} {
		var checks, calls atomic.Int32
		native := gitServiceNativeOptions(p, &checks, &calls)
		identity, _ := PinnedIdentity(p)
		asset := VMBootAsset{"/unopened/qualified-fixture-asset", strings.Repeat("a", 64), 1}
		release.Providers[p] = GitReviewerProviderRelease{Identity: identity, Stub: asset, QEMU: asset, Kernel: asset, Initrd: asset, Firmware: asset, Runtime: NativeVMRuntimeSpec{Artifact: asset, NamespaceHelper: asset, BootstrapLibraries: []VMBootAsset{{"/lib/x86_64-linux-gnu/libc.so.6", strings.Repeat("b", 64), 1}, {"/lib64/ld-linux-x86-64.so.2", strings.Repeat("c", 64), 1}}}, Policy: native.Policy}
		hosts[p] = GitReviewerProviderHost{Root: native.Root, Auth: native.Auth, Transport: native.Transport, Check: native.Check}
	}
	options := GitReviewerServiceOptions{Credentials: o.Credentials, Current: release.Revision, Releases: map[string]GitReviewerRelease{release.Revision: release}, Providers: map[string]map[Provider]GitReviewerProviderHost{release.Revision: hosts}, UnitDirectory: f.root, ExportDirectory: o.Archive.directory, RuntimeDirectory: nativeRuntimePrivateParent(t), WorkDirectory: o.Directory, Triggers: o.Triggers, Check: func(context.Context, Enrollment, string, string) error { return nil }, Interval: o.Interval, MaxBackoff: o.MaxBackoff, TickTimeout: o.TickTimeout}
	opener := func(ctx context.Context, root, stub, hash string) (*gitUnitOwner, error) {
		return newGitUnitOwner(ctx, root, stub, hash, f.manager, f.manager.kernel)
	}
	return options, f, opener, signs
}

func TestGitReviewerStartupComposesPinnedPackageWithoutSigning(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	_, expected, err := EncodeGitReviewerRelease(o.Releases[o.Current])
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	o.Check = func(_ context.Context, e Enrollment, revision, hash string) error {
		if e != o.Credentials.Journal.enrollment || revision != o.Current || hash != expected {
			t.Error("wrong package qualified")
		}
		checked++
		return nil
	}
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if checked != 1 || signs.Load() != 0 || s.inner == nil || s.inner.source == nil || !s.inner.source.ready || s.inner.source.releaseHash != expected {
		t.Fatal("startup skipped recovery, changed package or signed")
	}
	container := filepath.Join(o.RuntimeDirectory, s.container)
	if privateDirectory(container) != nil {
		t.Fatal("runtime container not private")
	}
	if s.Run(context.Background()) == nil || signs.Load() != 0 {
		t.Fatal("Run implicitly qualified credentials")
	}
	if s.Refresh(context.Background()) != nil || signs.Load() != 1 {
		t.Fatal("explicit refresh failed")
	}
	if s.Close(context.Background()) != nil || !s.closed {
		t.Fatal("startup owner could not close")
	}
	if _, err := os.Lstat(container); !os.IsNotExist(err) {
		t.Fatal("owned runtime container retained after successful stop", err)
	}
	if s.Run(context.Background()) == nil || s.Refresh(context.Background()) == nil {
		t.Fatal("closed startup reopened admission")
	}
}

func TestGitReviewerStartupPublicDenialPrecedesResourceOpening(t *testing.T) {
	o, _, _, signs := gitReviewerStartupFixture(t)
	o.Check = func(context.Context, Enrollment, string, string) error { return ErrLiveBlocked }
	// These paths are deliberately unavailable. The public constructor must
	// refuse at the local gate before it can connect to a real manager.
	o.UnitDirectory, o.ExportDirectory = "/unopened/unit", "/unopened/export"
	s, err := OpenGitReviewerService(context.Background(), o)
	if !errors.Is(err, ErrLiveBlocked) || s != nil || signs.Load() != 0 {
		t.Fatal("denied release opened startup", err)
	}
	if _, err := os.Lstat(filepath.Join(o.Credentials.Journal.directory, "reviewer-trigger-policy.json")); !os.IsNotExist(err) {
		t.Fatal("denial persisted a policy")
	}
}

func TestGitReviewerStartupCurrentPackageRequiresBothConfigurableProviders(t *testing.T) {
	for _, missing := range []Provider{Codex, Claude} {
		t.Run(string(missing), func(t *testing.T) {
			o, _, opener, signs := gitReviewerStartupFixture(t)
			release := o.Releases[o.Current]
			delete(release.Providers, missing)
			delete(o.Providers[o.Current], missing)
			o.Releases[o.Current] = release
			checks := 0
			o.Check = func(context.Context, Enrollment, string, string) error { checks++; return nil }
			s, err := openGitReviewerService(context.Background(), o, opener)
			if s != nil {
				if closeErr := s.Close(context.Background()); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if err == nil || checks != 0 || signs.Load() != 0 {
				t.Fatal("current package cannot serve every configurable alpha mode", err, checks)
			}
		})
	}
}

func TestGitReviewerStartupPersistsEpochAcrossRestartAndRefusesChange(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil || s.Close(context.Background()) != nil {
		t.Fatal("first startup", err)
	}
	policy := filepath.Join(o.Credentials.Journal.directory, "reviewer-trigger-policy.json")
	before, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	s, err = openGitReviewerService(context.Background(), o, opener)
	if err != nil || s.Close(context.Background()) != nil || signs.Load() != 0 {
		t.Fatal("restart recreated credentials or lost policy", err)
	}
	after, _ := os.ReadFile(policy)
	if string(before) != string(after) {
		t.Fatal("restart rewrote trigger epoch")
	}
	// The epoch is an instant, independent of the operator's display timezone.
	o.Triggers.EnabledAt = o.Triggers.EnabledAt.In(time.FixedZone("EDT", -4*60*60))
	s, err = openGitReviewerService(context.Background(), o, opener)
	if err != nil || s.Close(context.Background()) != nil {
		t.Fatal("timezone changed the epoch", err)
	}
	o.Triggers.EnabledAt = o.Triggers.EnabledAt.Add(time.Second)
	opened := false
	badOpener := func(context.Context, string, string, string) (*gitUnitOwner, error) {
		opened = true
		return nil, ErrLiveBlocked
	}
	bad, err := openGitReviewerService(context.Background(), o, badOpener)
	if err == nil || opened || signs.Load() != 0 {
		t.Fatal("changed epoch reached source ownership")
	}
	if bad != nil && bad.Close(context.Background()) != nil {
		t.Fatal("policy refusal retained resources")
	}
	after, _ = os.ReadFile(policy)
	if string(before) != string(after) {
		t.Fatal("changed epoch overwrote durable policy")
	}
}

func TestGitReviewerStartupUnknownStopRetainsOuterAndCredentialLocks(t *testing.T) {
	o, f, opener, signs := gitReviewerStartupFixture(t)
	unknown := func(ctx context.Context, root, stub, hash string) (*gitUnitOwner, error) {
		unit, err := opener(ctx, root, stub, hash)
		if err != nil {
			return nil, err
		}
		r, err := unit.read(f.binding.Attempt)
		if err != nil {
			t.Fatal(err)
		}
		r.Started, r.Stopped, r.Revoked = true, false, false
		r.StartJob = "/org/freedesktop/systemd1/job/1"
		if unit.write(r) != nil {
			t.Fatal("synthetic unknown receipt")
		}
		f.manager.observeErr = errors.New("synthetic unavailable old unit")
		return unit, nil
	}
	s, err := openGitReviewerService(context.Background(), o, unknown)
	if err == nil || s == nil || s.closed || s.inner == nil || s.lock == nil || s.unit.closed || signs.Load() != 0 {
		t.Fatal("unknown startup lost owners", err)
	}
	container := filepath.Join(o.RuntimeDirectory, s.container)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	other, err := openGitReviewerService(ctx, o, opener)
	cancel()
	if err == nil || other == nil || !other.closed {
		t.Fatal("retained startup lock was bypassed", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	credentials, err := OpenGitHubCredentialOwner(ctx, o.Credentials)
	cancel()
	if err == nil || credentials != nil {
		t.Fatal("uncertain startup released credential lock")
	}
	if privateDirectory(container) != nil || s.Refresh(context.Background()) == nil {
		t.Fatal("uncertain startup removed runtime or admitted work")
	}
	f.manager.kernel.boot = "synthetic explicit boot transition for startup recovery"
	if s.Recover(context.Background()) != nil || s.Close(context.Background()) != nil || !s.closed || signs.Load() != 0 {
		t.Fatal("explicit stopped recovery could not finish")
	}
	if _, err := os.Lstat(container); !os.IsNotExist(err) {
		t.Fatal("recovered owned runtime not removed")
	}
}

func TestGitReviewerStartupCloseRetainsOuterAssetsUntilCallbackReturns(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	o.Credentials.JWT = func(context.Context) ([]byte, error) {
		signs.Add(1)
		close(entered)
		<-release
		return []byte("synthetic-app-jwt"), nil
	}
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.Refresh(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("signer not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = s.Close(ctx)
	cancel()
	if err == nil || s.lock == nil || s.parent == nil || s.runtime.closed {
		t.Fatal("unreturned signer lost outer ownership")
	}
	unblock()
	if err := <-result; err == nil {
		t.Fatal("cancelled startup refresh succeeded")
	}
	if s.Close(context.Background()) != nil || !s.closed || signs.Load() != 1 {
		t.Fatal("explicit retry did not dispose exact owners")
	}
}

func TestGitReviewerStartupRefusesAliasesAndPreservesUnownedRuntimeEntries(t *testing.T) {
	t.Run("alias", func(t *testing.T) {
		o, _, opener, signs := gitReviewerStartupFixture(t)
		o.RuntimeDirectory = o.UnitDirectory
		if s, err := openGitReviewerService(context.Background(), o, opener); err == nil || s != nil || signs.Load() != 0 {
			t.Fatal("aliased roots accepted")
		}
	})
	t.Run("unowned-entry", func(t *testing.T) {
		o, _, opener, _ := gitReviewerStartupFixture(t)
		name := filepath.Join(o.RuntimeDirectory, "runtime-unknown-retained")
		if os.WriteFile(name, []byte("unowned old startup evidence"), 0600) != nil {
			t.Fatal("fixture")
		}
		s, err := openGitReviewerService(context.Background(), o, opener)
		if err != nil || s.Close(context.Background()) != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(name); err != nil || string(raw) != "unowned old startup evidence" {
			t.Fatal("cleanup removed an unowned entry", err)
		}
	})
	t.Run("bounded-retained-entries", func(t *testing.T) {
		o, _, _, signs := gitReviewerStartupFixture(t)
		for i := 0; i < 128; i++ {
			if os.WriteFile(filepath.Join(o.RuntimeDirectory, "retained-"+strconv.Itoa(i)), []byte("unowned"), 0600) != nil {
				t.Fatal("fixture")
			}
		}
		opened := false
		opener := func(context.Context, string, string, string) (*gitUnitOwner, error) {
			opened = true
			return nil, ErrLiveBlocked
		}
		s, err := openGitReviewerService(context.Background(), o, opener)
		if err == nil || s == nil || !s.closed || opened || signs.Load() != 0 {
			t.Fatal("runtime retention cap bypassed", err)
		}
		entries, err := os.ReadDir(o.RuntimeDirectory)
		if err != nil || len(entries) != 128 {
			t.Fatal("retention refusal removed evidence", err)
		}
	})
}

func TestGitReviewerStartupOlderPackageRemainsBoundAndCanBeRevoked(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	old := o.Releases[o.Current]
	old.Revision = strings.Repeat("e", 40)
	o.Releases[old.Revision], o.Providers[old.Revision] = old, o.Providers[o.Current]
	_, expected, err := EncodeGitReviewerRelease(old)
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	seen := ""
	o.Check = func(_ context.Context, _ Enrollment, revision, hash string) error {
		if revision == old.Revision {
			seen = hash
			if !allowed {
				return ErrLiveBlocked
			}
		}
		return nil
	}
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	job, evidence, _ := gitServiceRuntimeJob(t)
	job.Release = old.Revision
	bound, err := evidence.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's release map cannot replace the frozen package.
	delete(o.Releases, old.Revision)
	if engine, err := s.inner.options.Runtime(context.Background(), job, bound); err != nil || len(engine.Reviewers) != 2 || seen != expected {
		t.Fatal("older package did not retain its exact digest", err)
	}
	allowed = false
	if engine, err := s.inner.options.Runtime(context.Background(), job, bound); err == nil || len(engine.Reviewers) != 0 || signs.Load() != 0 {
		t.Fatal("revoked package admitted work")
	}
}

func TestGitReviewerStartupRetainsOlderSingleProviderJob(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	old := o.Releases[o.Current]
	old.Revision = strings.Repeat("e", 40)
	old.Providers = map[Provider]GitReviewerProviderRelease{Codex: old.Providers[Codex]}
	o.Releases[old.Revision] = old
	o.Providers[old.Revision] = map[Provider]GitReviewerProviderHost{Codex: o.Providers[o.Current][Codex]}
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	job, evidence, _ := gitServiceRuntimeJob(t)
	job.Release, job.Config.Mode = old.Revision, CodexOnly
	bound, err := evidence.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := s.inner.options.Runtime(context.Background(), job, bound)
	if err != nil || len(engine.Reviewers) != 1 || engine.Reviewers[Codex] == nil || signs.Load() != 0 {
		t.Fatal("current dual-provider requirement broke the original older single-provider job", err)
	}
}

func TestGitReviewerStartupCorruptPolicyFailsClosedWithoutRewrite(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil || s.Close(context.Background()) != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.Credentials.Journal.directory, "reviewer-trigger-policy.json")
	if os.WriteFile(path, []byte(`{"Version":1}`), 0600) != nil {
		t.Fatal("fixture")
	}
	opened := false
	opener = func(context.Context, string, string, string) (*gitUnitOwner, error) {
		opened = true
		return nil, ErrLiveBlocked
	}
	s, err = openGitReviewerService(context.Background(), o, opener)
	if err == nil || s == nil || !s.closed || opened || signs.Load() != 0 {
		t.Fatal("corrupt policy was replaced or used")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != `{"Version":1}` {
		t.Fatal("corrupt policy evidence overwritten")
	}
}

func TestGitReviewerStartupOptionsRedactCapabilities(t *testing.T) {
	o, _, _, _ := gitReviewerStartupFixture(t)
	for _, value := range []any{o, o.Providers[o.Current][Codex], &GitReviewerService{}} {
		raw, err := json.Marshal(value)
		if err != nil || !strings.Contains(string(raw), "redacted") || strings.Contains(fmt.Sprintf("%#v", value), "/unopened") {
			t.Fatal("startup capability formatting leaked configuration")
		}
	}
	if (&GitReviewerService{}).Close(context.Background()) == nil {
		t.Fatal("zero service accepted cleanup")
	}
}

func TestGitReviewerStartupRejectsCorruptPackageBytesBeforeUnitOrSign(t *testing.T) {
	o, _, _, signs := gitReviewerStartupFixture(t)
	if os.WriteFile(o.Releases[o.Current].GitArtifact.Path, []byte("changed bytes"), 0600) != nil {
		t.Fatal("fixture")
	}
	opened := false
	opener := func(context.Context, string, string, string) (*gitUnitOwner, error) {
		opened = true
		return nil, ErrLiveBlocked
	}
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err == nil || opened || signs.Load() != 0 || s == nil || !s.closed {
		t.Fatal("corrupt package reached unit or signing", err)
	}
}

func TestGitReviewerStartupActualGitPackageProducer(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	build := actualGitRuntime(t)
	release := o.Releases[o.Current]
	if os.WriteFile(release.GitArtifact.Path, build.Artifact, 0600) != nil {
		t.Fatal("producer artifact")
	}
	release.GitArtifact.SHA256, release.GitArtifact.Bytes, release.GitRuntime = build.Pins.Artifact.SHA256, build.Pins.Artifact.Bytes, build.Pins
	o.Releases[o.Current] = release
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil {
		t.Fatal("actual BuildGitRuntime artifact refused", err)
	}
	if s.inner.source.manifest.Runtime != build.Pins || signs.Load() != 0 || s.Close(context.Background()) != nil {
		t.Fatal("producer pins changed or startup signed")
	}
}

func TestGitReviewerStartupNativeStubPinMeasuredAfterLocalGate(t *testing.T) {
	for _, correct := range []bool{false, true} {
		t.Run(fmt.Sprint(correct), func(t *testing.T) {
			o, _, opener, signs := gitReviewerStartupFixture(t)
			release := o.Releases[o.Current]
			provider := release.Providers[Codex]
			provider.Stub = release.Worker
			if !correct {
				provider.Stub.Bytes++
			}
			release.Providers[Codex] = provider
			o.Releases[o.Current] = release
			host := o.Providers[o.Current][Codex]
			checks := 0
			host.Check = func(context.Context, Session) error { checks++; return nil }
			o.Providers[o.Current][Codex] = host
			s, err := openGitReviewerService(context.Background(), o, opener)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			job, _, bound := gitServiceRuntimeJob(t)
			engine, err := s.inner.options.Runtime(context.Background(), job, bound)
			if err != nil {
				t.Fatal(err)
			}
			limits, _ := protocolLimits(job)
			session, err := NewSession(job.ID, job.Snapshot, job.Config.Hash(), Codex, 1, job.Packet, nil, nil, limits)
			if err != nil {
				t.Fatal(err)
			}
			err = engine.Reviewers[Codex].(*NativeVMReviewer).options.Check(context.Background(), session)
			if (err == nil) != correct || checks != 1 || signs.Load() != 0 {
				t.Fatal("stub pin gate omitted byte count or invoked credentials", err)
			}
		})
	}
}

func TestGitReviewerStartupRetriesSameRuntimeCleanupAfterPathRepair(t *testing.T) {
	o, _, opener, signs := gitReviewerStartupFixture(t)
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	runtime := s.runtime
	if runtime.parent.Rename(gitRuntimeDirectory, "retained-runtime") != nil {
		t.Fatal("fixture rename")
	}
	if s.Close(context.Background()) == nil || s.lock == nil || s.closed || runtime.closed {
		t.Fatal("failed cleanup lost original runtime handles or outer lock")
	}
	if _, err := runtime.root.Stat("."); err != nil {
		t.Fatal("failed cleanup closed the retained directory capability", err)
	}
	if runtime.parent.Rename("retained-runtime", gitRuntimeDirectory) != nil {
		t.Fatal("cannot repair original directory identity")
	}
	if s.Close(context.Background()) != nil || !s.closed || s.runtime != runtime || signs.Load() != 0 {
		t.Fatal("same-owner cleanup retry did not finish without requalification")
	}
}

func TestGitReviewerStartupFailedConstructionRetainsRetryableRuntime(t *testing.T) {
	o, _, _, signs := gitReviewerStartupFixture(t)
	opener := func(context.Context, string, string, string) (*gitUnitOwner, error) {
		entries, err := os.ReadDir(o.RuntimeDirectory)
		if err != nil || len(entries) != 1 {
			t.Fatal("fixture runtime not prepared", err)
		}
		container := filepath.Join(o.RuntimeDirectory, entries[0].Name())
		if os.Rename(filepath.Join(container, gitRuntimeDirectory), filepath.Join(container, "retained-runtime")) != nil {
			t.Fatal("fixture rename")
		}
		return nil, ErrLiveBlocked
	}
	s, err := openGitReviewerService(context.Background(), o, opener)
	if err == nil || s == nil || s.closed || s.lock == nil || s.runtime == nil || s.runtime.closed || s.inner != nil || signs.Load() != 0 {
		t.Fatal("failed construction lost its cleanup owner", err)
	}
	defer s.Close(context.Background())
	runtime := s.runtime
	if runtime.parent.Rename("retained-runtime", gitRuntimeDirectory) != nil {
		t.Fatal("original path repair failed")
	}
	if s.Close(context.Background()) != nil || !s.closed || s.runtime != runtime {
		t.Fatal("constructor cleanup retry could not finish the same runtime")
	}
}
