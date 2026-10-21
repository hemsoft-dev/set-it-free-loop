//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
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
	if evidence, err := factory.Capture(context.Background(), testSnapshot()); err == nil || evidence != nil {
		t.Fatal("unready credentials created source")
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
	job := testJob()
	job.Snapshot = r.binding.Snapshot
	job.Packet = packet
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
}
