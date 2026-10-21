//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func gitFactoryWorkRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "sfl-source-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return directory
}

func TestGitSourceFactoryStartupPrecedesCredentialWork(t *testing.T) {
	r, f, runtime := gitFactoryAssetsFixture(t)
	owner, _, _, _, signs := credentialOwnerFixture(t)
	factory, err := newGitSourceFactory(context.Background(), owner, f.owner, r.archive, runtime, r.files[2], r.manifest, testJob().Release, r.binding.ReleaseHash, gitFactoryWorkRoot(t))
	if err != nil {
		t.Fatal("offline startup", err)
	}
	if signs.Load() != 0 || f.manager.starts != 0 {
		t.Fatal("startup dispatched credentials or inference")
	}
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("prepared fixture cleanup")
	}
	f.owner.enter(context.Background())
	before, beforeErr := f.owner.records()
	f.owner.leave()
	if beforeErr != nil {
		t.Fatal(beforeErr)
	}
	beforePaths, pathErr := os.ReadDir(factory.directory)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	for i := 0; i < 130; i++ {
		if evidence, err := factory.Capture(context.Background(), testSnapshot()); err == nil || evidence != nil {
			t.Fatal("unready credentials created source")
		}
	}
	f.owner.enter(context.Background())
	after, afterErr := f.owner.records()
	f.owner.leave()
	afterPaths, pathErr := os.ReadDir(factory.directory)
	if afterErr != nil || pathErr != nil || len(before) != len(after) || len(beforePaths) != len(afterPaths) {
		t.Fatalf("unready captures consumed durable slots/directories: %d/%d -> %d/%d", len(before), len(beforePaths), len(after), len(afterPaths))
	}
	if signs.Load() != 0 || f.manager.starts != 0 {
		t.Fatal("unready capture refreshed credentials or launched a unit")
	}
	if factory.Close(context.Background()) != nil {
		t.Fatal("factory close")
	}
}

func TestGitSourceFactoryRestoresAdmittedPacketWithoutHostGit(t *testing.T) {
	_, binding, store := gitMaterializationFixture(t, "sha1")
	original, err := CaptureSnapshotEvidence(context.Background(), store, binding.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	r, f, runtime := gitFactoryAssetsFixture(t, binding.Snapshot)
	helper, err := duplicateGitControlFile(r.files[2])
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	if f.owner.start(context.Background(), r.binding, "/private/bootstrap.sock") != nil {
		t.Fatal("fake unit start")
	}
	// Explicit fake admission history: the manager/cgroup fixture is synthetic.
	// These canonical receipts exercise recovery correlation, not containment.
	if !f.owner.enter(context.Background()) {
		t.Fatal("unit lock")
	}
	record, err := f.owner.read(r.binding.Attempt)
	if err != nil {
		f.owner.leave()
		t.Fatal(err)
	}
	worker, err := processGateIdentity(os.Getpid(), record.Identity.InvocationID)
	if err != nil {
		f.owner.leave()
		t.Fatal(err)
	}
	record.BootstrapGrantHash = Digest(gitUnitGrantPacket{1, gitBootstrapGrant, r.binding, record.Identity, r.binding.RuntimeHash})
	record.WorkerIdentity = worker
	record.WorkerGrantHash = Digest(gitUnitGrantPacket{1, gitWorkerGrant, r.binding, worker, r.binding.RuntimeHash})
	err = f.owner.write(record)
	f.owner.leave()
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if writeGitMaterialization(context.Background(), &wire, store, r.binding, DefaultGitAcquisitionBounds()) != nil {
		t.Fatal("synthetic Git export")
	}
	m, err := r.archive.stage(context.Background(), r.binding, io.NopCloser(bytes.NewReader(wire.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	packet := m.packet
	m.Close()
	if r.stop(context.Background()) != nil || r.close() != nil || f.owner.Close() != nil {
		t.Fatal("fake unit/old owner shutdown")
	}
	reopened, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal("new startup owner", err)
	}
	// A durable terminal receipt survives collection of the transient unit and
	// its old cgroup. Restart and later Restore must not require them again.
	f.manager.mu.Lock()
	f.manager.observeErr = errors.New("fake NoSuchUnit after collection")
	f.manager.mu.Unlock()
	f.manager.kernel.mu.Lock()
	f.manager.kernel.changed = true
	f.manager.kernel.mu.Unlock()
	owner, _, _, _, signs := credentialOwnerFixture(t)
	factory, err := newGitSourceFactory(context.Background(), owner, reopened, r.archive, runtime, helper, r.manifest, testJob().Release, r.binding.ReleaseHash, gitFactoryWorkRoot(t))
	if err != nil {
		t.Fatal("recovery", err)
	}
	defer factory.Close(context.Background())
	if signs.Load() != 0 {
		t.Fatal("retained source recovery touched credentials")
	}
	if os.RemoveAll(store.GitDir) != nil {
		t.Fatal("remove owned synthetic quarantine")
	}
	durable, _ := openFixtureStore(t)
	request := native(1, controlTime.Add(time.Minute))
	request.Snapshot = r.binding.Snapshot
	mustCapture(t, durable, nil, controlTime.Add(2*time.Minute))
	if durable.RecordRequest(context.Background(), alphaScope(), request, true) != nil {
		t.Fatal("source retention request")
	}
	job, err := durable.Admit(context.Background(), alphaScope(), 1, testJob().Release, packet, controlTime.Add(3*time.Minute))
	if err != nil {
		t.Fatal("source retention admission", err)
	}
	bound, err := factory.Restore(context.Background(), job)
	if err != nil || bound.evidence.Packet() != original.Packet() || !reflect.DeepEqual(bound.evidence.lines, original.lines) || !reflect.DeepEqual(bound.evidence.Coverage(), original.Coverage()) {
		t.Fatal("restored evidence changed", err)
	}
	// The admission path uses the same blob-based anchor validation without
	// fabricating a Job or executing a host Git fallback.
	evidence, err := captureGitMaterializationEvidence(context.Background(), factory.materialized[r.binding.Attempt])
	if err != nil || evidence.Packet() != packet || !reflect.DeepEqual(evidence.lines, original.lines) {
		t.Fatal("capture/restore evidence mismatch", err)
	}
	wrong := job
	wrong.Release = strings.Repeat("e", 40)
	if got, err := factory.Restore(context.Background(), wrong); err == nil || got != nil {
		t.Fatal("other release reused retained source")
	}
	if signs.Load() != 0 {
		t.Fatal("restore dispatched credential work")
	}
	if factory.Close(context.Background()) == nil || factory.closed || factory.unit.closed {
		t.Fatal("active restored evidence lost backing storage")
	}
	if factory.ReleaseJob(context.Background(), bound) != nil || factory.ReleaseJob(context.Background(), bound) == nil {
		t.Fatal("restored evidence was not released exactly once")
	}
	// Expired lease time does not prove a worker stopped. Retention must keep
	// the exact source and its cache while that durable lease remains owned.
	token, err := durable.Claim(context.Background(), job.ID, controlTime.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	later := controlTime.Add(40 * 24 * time.Hour)
	if factory.PruneSource(context.Background(), durable, later) != nil || factory.materialized[r.binding.Attempt] == nil {
		t.Fatal("unknown durable lease lost retained source")
	}
	state, err := durable.Snapshot(context.Background())
	if err != nil || state.Jobs[job.ID] == nil {
		t.Fatal("expired lease was stolen", err)
	}
	bound, err = factory.Restore(context.Background(), job)
	if err != nil {
		t.Fatal("retained lease source could not restore", err)
	}
	// This is an explicitly synthetic pending admission using independently
	// reconstructed source evidence, not a physical Capture qualification.
	factory.pendingCaptures = map[*SnapshotEvidence]string{evidence: r.binding.Attempt}
	if durable.ReleaseStopped(context.Background(), job.ID, token, "cancelled") != nil {
		t.Fatal("known stopped fake lease release")
	}
	if factory.PruneSource(context.Background(), durable, later) != nil {
		t.Fatal("retained consumer pruning")
	}
	state, err = durable.Snapshot(context.Background())
	if err != nil || state.Jobs[job.ID] != nil || len(factory.bindings) != 1 {
		t.Fatal("active source or expired job retention incorrect", err)
	}
	if factory.ReleaseJob(context.Background(), bound) != nil || factory.PruneSource(context.Background(), durable, later) != nil || len(factory.bindings) != 1 {
		t.Fatal("pending admission source retired")
	}
	if factory.ReleaseCapture(context.Background(), evidence) != nil || factory.PruneSource(context.Background(), durable, later) != nil || len(factory.bindings) != 0 || len(factory.materialized) != 0 {
		t.Fatal("released expired source permanently consumed slots")
	}
	if _, err := factory.unit.root.Lstat(gitUnitDirectory(r.binding.Attempt)); !os.IsNotExist(err) {
		t.Fatal("unit receipt not retired", err)
	}
	if _, err := factory.archive.root.Lstat(gitExportDirectory(r.binding.Attempt)); !os.IsNotExist(err) {
		t.Fatal("export not retired", err)
	}
}

func TestGitSourceFactoryValidLongRootAndFailedStartupOwnership(t *testing.T) {
	for _, mode := range []string{"long-root", "failed-recovery"} {
		t.Run(mode, func(t *testing.T) {
			r, f, runtime := gitFactoryAssetsFixture(t)
			helper, err := duplicateGitControlFile(r.files[2])
			if err != nil {
				t.Fatal(err)
			}
			defer helper.Close()
			if r.stop(context.Background()) != nil || r.close() != nil {
				t.Fatal("prepared fixture cleanup")
			}
			root := gitFactoryWorkRoot(t)
			if mode == "long-root" {
				root = filepath.Join(root, strings.Repeat("x", 40-len(root)))
				if len(root) <= 33 || len(filepath.Join(root, "run-"+strings.Repeat("a", 32), "broker", "worker.sock")) > 107 || os.Mkdir(root, 0700) != nil {
					t.Fatal("long valid Unix path fixture")
				}
			} else {
				_, binding, store := gitMaterializationFixture(t, "sha1")
				binding.Attempt = strings.Repeat("d", 32)
				// Write a valid orphan archive through its real framing path.
				// Re-encode the binding in trusted synthetic producer output.
				var frame bytes.Buffer
				if writeGitMaterialization(context.Background(), &frame, store, binding, DefaultGitAcquisitionBounds()) != nil {
					t.Fatal("orphan fixture")
				}
				wire := frame.Bytes()
				m, err := r.archive.stage(context.Background(), binding, io.NopCloser(bytes.NewReader(wire)))
				if err != nil {
					t.Fatal(err)
				}
				m.Close()
			}
			owner, _, _, _, _ := credentialOwnerFixture(t)
			factory, err := newGitSourceFactory(context.Background(), owner, f.owner, r.archive, runtime, helper, r.manifest, testJob().Release, r.binding.ReleaseHash, root)
			if mode == "long-root" {
				if err != nil || factory == nil {
					t.Fatal("valid deployment root refused", err)
				}
			} else {
				if err == nil || factory == nil {
					t.Fatal("failed startup lost transferred ownership handle", err)
				}
			}
			if factory.Close(context.Background()) != nil {
				t.Fatal("transferred startup resources not closed")
			}
		})
	}
}

func TestGitSourceFactoryFailedStartupRetainsUnknownOwnerUntilRecovery(t *testing.T) {
	r, f, runtime := gitFactoryAssetsFixture(t)
	helper, err := duplicateGitControlFile(r.files[2])
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	f.manager.startErr = errors.New("fake unknown accepted start")
	if f.owner.start(context.Background(), r.binding, "/private/bootstrap.sock") == nil {
		t.Fatal("unknown start fixture")
	}
	owner, _, _, _, signs := credentialOwnerFixture(t)
	factory, err := newGitSourceFactory(context.Background(), owner, f.owner, r.archive, runtime, helper, r.manifest, testJob().Release, r.binding.ReleaseHash, gitFactoryWorkRoot(t))
	if err == nil || factory == nil {
		t.Fatal("unknown startup lost owner handle", err)
	}
	if factory.Close(context.Background()) == nil || factory.closed || f.owner.closed {
		t.Fatal("uncertain startup disposed live ownership")
	}
	if evidence, err := factory.Capture(context.Background(), testSnapshot()); err == nil || evidence != nil || signs.Load() != 0 {
		t.Fatal("failed startup admitted new work")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	other, lockErr := newGitUnitOwner(ctx, f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	cancel()
	if lockErr == nil || other != nil {
		t.Fatal("failed startup released cross-process owner lock")
	}
	fd, err := runtime.directoryFD()
	if err != nil {
		t.Fatal("unknown startup lost runtime")
	}
	fd.Close()
	// Explicit synthetic next-boot evidence resolves this fake manager branch.
	// The fixture's separate actual inert child is still reaped by its cleanup.
	f.manager.kernel.mu.Lock()
	f.manager.kernel.boot = "synthetic-next-boot"
	f.manager.kernel.mu.Unlock()
	if factory.Recover(context.Background()) != nil || !factory.ready {
		t.Fatal("explicit known-terminal startup recovery failed")
	}
	if r.stop(context.Background()) != nil || r.close() != nil || factory.Close(context.Background()) != nil {
		t.Fatal("recovered startup cleanup")
	}
}

func TestGitSourceFactoryRetiresKnownStoppedIntakeAfterLeaseAcceptance(t *testing.T) {
	r, f, runtime := gitFactoryAssetsFixture(t)
	owner, _, _, _, _ := credentialOwnerFixture(t)
	factory, err := newGitSourceFactory(context.Background(), owner, f.owner, r.archive, runtime, r.files[2], r.manifest, testJob().Release, r.binding.ReleaseHash, gitFactoryWorkRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake pair qualification")
	}
	g, err := owner.retainReaderGeneration(context.Background(), r.stop)
	if err != nil {
		t.Fatal(err)
	}
	r.retainGeneration(g)
	factory.runs[r.binding.Attempt] = r
	if m, err := r.archive.stage(context.Background(), r.binding, io.NopCloser(strings.NewReader("incomplete"))); err == nil || m != nil {
		t.Fatal("partial frame staged")
	}
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("known physical stop")
	}
	if factory.retireFailedCapture(context.Background(), r) == nil {
		t.Fatal("unaccepted generation allowed intake retirement")
	}
	if g.stop(context.Background()) != nil {
		t.Fatal("accept stopped generation")
	}
	if factory.retireFailedCapture(context.Background(), r) != nil {
		t.Fatal("known stopped failure poisoned intake")
	}
	r.archive.enter(context.Background())
	attempt, err := r.archive.attempt(r.binding.Attempt)
	if err != nil {
		r.archive.leave()
		t.Fatal(err)
	}
	record, err := readGitExportRecord(attempt)
	_, payloadErr := attempt.Lstat("payload.bin")
	attempt.Close()
	r.archive.leave()
	if err != nil || record.State != "discarded" || !os.IsNotExist(payloadErr) {
		t.Fatal("same-process recovery not durable", record, err, payloadErr)
	}
	if factory.Close(context.Background()) != nil {
		t.Fatal("recovered failed capture cleanup")
	}
}
