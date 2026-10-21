//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Actual retained files, pipes, sealed manifest and original pidfds; fake
// manager/cgroup. The inert fixture never parses candidate Git or runs a model.
func gitFactoryRunFixture(t *testing.T) (*gitFactoryRun, gitUnitFixture) {
	r, f, _ := gitFactoryAssetsFixture(t)
	return r, f
}

func gitFactoryAssetsFixture(t *testing.T, snapshot ...Snapshot) (*gitFactoryRun, gitUnitFixture, *gitRuntimeRoot) {
	t.Helper()
	f := newGitUnitFixture(t)
	if len(snapshot) > 0 {
		f.binding.Snapshot = snapshot[0]
	}
	manifest, bodies := gitRuntimeFixture(t)
	artifact, pins := gitRuntimeFrame(t, manifest, bodies)
	root, err := prepareGitRuntime(context.Background(), nativeRuntimePrivateParent(t), bytes.NewReader(artifact), pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	runtime, err := root.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	raw, err := os.ReadFile("/usr/bin/bwrap")
	if err != nil {
		t.Fatal(err)
	}
	helperPin := VMAssetPin{gitRawHash(raw), int64(len(raw))}
	helper, err := retainNativeBootstrap(context.Background(), VMBootAsset{Path: "/usr/bin/bwrap", SHA256: helperPin.SHA256, Bytes: helperPin.Bytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { helper.Close() })
	worker, err := f.owner.image.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	filter, err := gitFixedFilterPin()
	if err != nil {
		t.Fatal(err)
	}
	m := gitGrantManifest{2, "isolated-git-acquisition", pins, helperPin, VMAssetPin{f.stubHash, worker.Size()}, filter}
	f.binding.RuntimeHash = Digest(m)
	archive, _ := exportArchiveFixture(t)
	directory, err := os.MkdirTemp("/tmp", "sfl-git-factory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	r, err := newGitFactoryRun(context.Background(), f.owner, archive, f.binding, m, runtime, helper, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if r.stop(ctx) == nil {
			r.close()
		}
	})
	return r, f, root
}

func TestGitFactoryManifestSealedAndPreparationDoesNoWork(t *testing.T) {
	r, f := gitFactoryRunFixture(t)
	if f.manager.starts != 0 || f.manager.stops != 0 {
		t.Fatal("construction dispatched manager work")
	}
	record, err := f.owner.read(f.binding.Attempt)
	if err != nil || record.Started || record.Stopped || record.Binding != f.binding {
		t.Fatal("prepared durable binding", record, err)
	}
	raw, err := io.ReadAll(io.NewSectionReader(r.files[0], 0, gitGrantManifestBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(r.manifest)
	if !bytes.Equal(raw, want) || verifyGrantedVMAsset(context.Background(), r.files[0], VMAssetPin{f.binding.RuntimeHash, int64(len(raw))}, 1) != nil {
		t.Fatal("factory manifest is not the exact sealed non-executable release binding")
	}
	if _, err := r.files[0].WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("sealed manifest mutated")
	}
	if r.close() == nil {
		t.Fatal("unstopped attempt lost assets")
	}
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("unstarted attempt failed terminal cleanup")
	}
	if f.manager.starts != 0 || f.manager.stops != 0 {
		t.Fatal("unstarted stop dispatched a unit")
	}
}

func TestGitFactoryCancellationJoinsBrokerBeforePhysicalUnitStop(t *testing.T) {
	r, f := gitFactoryRunFixture(t)
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake credential qualification")
	}
	g, err := owner.retainReaderGeneration(context.Background(), r.stop)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		m, err := r.acquire(g.ctx, &gitGenerationReader{generation: g})
		if m != nil {
			m.Close()
		}
		result <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if !f.owner.enter(context.Background()) {
			t.Fatal("unit operation")
		}
		record, err := f.owner.read(f.binding.Attempt)
		f.owner.leave()
		if err != nil {
			t.Fatal(err)
		}
		if record.Captured {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake unit never reached inert bootstrap wait")
		}
		time.Sleep(time.Millisecond)
	}
	if err := g.stop(context.Background()); err != nil {
		t.Fatal("coordinated stop", err)
	}
	if err := <-result; err == nil {
		t.Fatal("canceled acquisition published output")
	}
	r.mu.Lock()
	audit, done, stopped := r.brokerAudit, r.brokerDone, r.stopped
	r.mu.Unlock()
	select {
	case <-done:
	default:
		t.Fatal("broker callback still owned after stop")
	}
	if !audit.Stopped || !stopped || audit.Requests != 0 {
		t.Fatal("broker/physical stop proof", audit, stopped)
	}
	f.manager.mu.Lock()
	starts, stops := f.manager.starts, f.manager.stops
	f.manager.mu.Unlock()
	if starts != 1 || stops != 1 {
		t.Fatal("unexpected manager replay", starts, stops)
	}
	if r.close() != nil {
		t.Fatal("stopped runtime descriptors retained")
	}
	owner.state.mu.Lock()
	owned := len(owner.state.acquisitions)
	owner.state.mu.Unlock()
	if owned != 0 {
		t.Fatal("successful stop retained generation")
	}
}

func TestGitFactoryStopRetainsUnreturnedBrokerAndPreventsReentry(t *testing.T) {
	r, f := gitFactoryRunFixture(t)
	blocked := make(chan struct{})
	acquired := make(chan struct{})
	close(acquired)
	r.mu.Lock()
	r.spent = true
	r.acquireDone = acquired
	r.brokerDone = blocked
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if r.stop(ctx) == nil || r.close() == nil {
		t.Fatal("unreturned broker discarded ownership")
	}
	f.owner.enter(context.Background())
	record, err := f.owner.read(f.binding.Attempt)
	f.owner.leave()
	if err != nil || record.Stopped || f.manager.stops != 0 {
		t.Fatal("physical stop preceded broker drain", record, err)
	}
	if m, err := r.acquire(context.Background(), &gitGenerationReader{}); err == nil || m != nil {
		t.Fatal("revoked acquisition reentered")
	}
	r.mu.Lock()
	r.brokerAudit = GitReadBrokerAudit{Version: 1, Stopped: true}
	r.mu.Unlock()
	close(blocked)
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("returned broker could not finish stop")
	}
}

func TestGitFactoryFailedStartKeepsGenerationUntilKnownStop(t *testing.T) {
	r, f := gitFactoryRunFixture(t)
	f.manager.startErr = errors.New("synthetic missing accepted-start reply")
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake credential qualification")
	}
	g, err := owner.retainReaderGeneration(context.Background(), r.stop)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := r.acquire(g.ctx, &gitGenerationReader{generation: g}); err == nil || m != nil {
		t.Fatal("unknown start returned source")
	}
	if g.stop(context.Background()) == nil {
		t.Fatal("unknown unit birth released generation")
	}
	owner.state.mu.Lock()
	owned := len(owner.state.acquisitions)
	owner.state.mu.Unlock()
	if owned != 1 || r.close() == nil {
		t.Fatal("unknown start lost retained assets")
	}
	// This test's manager reports an error before any accepted start reply.
	// The fake kernel's next-boot observation supplies an independent terminal
	// branch for old work; it does not kill this fixture's actual inert child.
	f.manager.kernel.mu.Lock()
	f.manager.kernel.boot = "synthetic-next-boot"
	f.manager.kernel.mu.Unlock()
	if g.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("independently terminal old boot did not recover")
	}
	if _, err := os.Stat(filepath.Join(r.directory, "bootstrap.sock")); err != nil {
		t.Fatal("retained recovery directory unexpectedly removed", err)
	}
}

func TestGitFactoryBrokerSetupFailureRetiresOnlyKnownUnstartedLease(t *testing.T) {
	r, f := gitFactoryRunFixture(t)
	if os.WriteFile(filepath.Join(r.directory, "broker", "unexpected"), []byte("trusted setup fault"), 0600) != nil {
		t.Fatal("setup fixture")
	}
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake credential qualification")
	}
	g, err := owner.retainReaderGeneration(context.Background(), r.stop)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := r.acquire(g.ctx, &gitGenerationReader{generation: g}); err == nil || m != nil {
		t.Fatal("invalid broker setup returned source")
	}
	if g.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("joined setup refusal retained unstarted lease")
	}
	if f.manager.starts != 0 || f.manager.stops != 0 {
		t.Fatal("setup refusal dispatched a unit")
	}
}

func TestGitFactorySocketPathBounds(t *testing.T) {
	longest := "/" + strings.Repeat("x", 84)
	if len(filepath.Join(longest, "bootstrap.sock")) != 100 || !gitFactoryRunPaths(longest) {
		t.Fatal("valid maximum socket path refused")
	}
	for _, path := range []string{longest + "x", "relative", "/tmp/../other", "/tmp/invalid\x00"} {
		if gitFactoryRunPaths(path) {
			t.Fatal("invalid socket path accepted")
		}
	}
}
