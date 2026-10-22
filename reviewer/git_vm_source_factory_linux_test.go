//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type gitVMFactoryRecoveryFixture struct {
	owner       gitVMOwnerFixture
	archive     *gitVMExportArchive
	assets      *gitVMAssets
	manifest    gitVMManifest
	credentials *GitHubCredentialOwner
	directory   string
	store       GitStore
	binding     gitMaterializationBinding
}

func newGitVMFactoryRecoveryFixture(t *testing.T) gitVMFactoryRecoveryFixture {
	t.Helper()
	f := gitVMFactoryRecoveryFixture{owner: newGitVMOwnerFixture(t)}
	_, f.binding, f.store = gitMaterializationFixture(t, "sha1")
	spec, source := gitVMAssetsFixture(t)
	var err error
	f.assets, err = openGitVMAssets(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	f.manifest = source.Manifest
	f.archive, err = openGitVMExportArchive(context.Background(), gitVMArchiveTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f.credentials, _, _, _, _ = credentialOwnerFixture(t)
	f.directory, err = os.MkdirTemp("/tmp", "sfl-vmr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(f.directory) })
	return f
}

// attempt returns a stopped owner record and its source request for one
// synthetic attempt; exports are produced by the actual Git producer.
func (f gitVMFactoryRecoveryFixture) attempt(t *testing.T, index int, granted bool) (gitVMUnitRecord, gitVMRequest) {
	t.Helper()
	binding := f.binding
	binding.Attempt = fmt.Sprintf("%032x", index)
	source := gitVMRequestFixture(t, binding)
	r := gitVMStoppedRecordFixture(t)
	r.Request.Source = source
	r.Request.Read.Source = source.Binding
	r.RequestHash, r.Unit = Digest(r.Request), "sfl-alpha-"+binding.Attempt+".service"
	r.Birth.Cgroup.Path = "/user.slice/user-1000.slice/user@1000.service/" + r.Unit
	r.Cgroup = r.Birth.Cgroup
	r.StubHash = f.owner.stubHash
	if !granted {
		r.GrantHash = ""
	}
	if !r.valid() {
		t.Fatal("synthetic stopped record invalid")
	}
	return r, source
}

func (f gitVMFactoryRecoveryFixture) stage(t *testing.T, source gitVMRequest, truncate bool) {
	t.Helper()
	var wire bytes.Buffer
	if err := writeGitVMExport(context.Background(), &wire, f.store, source); err != nil {
		t.Fatal(err)
	}
	data := wire.Bytes()
	if truncate {
		data = data[:len(data)/2]
	}
	m, err := f.archive.stage(context.Background(), source, io.NopCloser(bytes.NewReader(data)))
	if truncate != (err != nil) {
		t.Fatal("synthetic staging outcome", err)
	}
	if m != nil {
		m.Close()
	}
}

func (f gitVMFactoryRecoveryFixture) open(t *testing.T) (*gitVMSourceFactory, error) {
	t.Helper()
	factory, err := newGitVMSourceFactory(context.Background(), f.credentials, f.owner.owner, f.archive, f.assets, f.manifest, Digest("source release"), f.directory)
	if factory != nil {
		t.Cleanup(func() { factory.Close(context.Background()) })
	}
	return factory, err
}

func TestGitVMSourceFactoryRecoveryRetainsOnlyGrantedStagedSource(t *testing.T) {
	f := newGitVMFactoryRecoveryFixture(t)
	keep, keepSource := f.attempt(t, 1, true)
	ungranted, ungrantedSource := f.attempt(t, 2, false)
	partial, partialSource := f.attempt(t, 3, true)
	failed, _ := f.attempt(t, 4, true)
	if f.owner.owner.journal.write(gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{keep, ungranted, partial, failed}}) != nil {
		t.Fatal("synthetic owner journal")
	}
	f.stage(t, keepSource, false)
	f.stage(t, ungrantedSource, false)
	f.stage(t, partialSource, true)
	// A crashed run left a read-only runtime root behind.
	residue := filepath.Join(f.directory, "run-"+partial.Request.Source.Binding.Attempt, "qemu-runtime", "lib")
	if os.MkdirAll(residue, 0700) != nil || os.Chmod(residue, 0555) != nil || os.Chmod(filepath.Dir(residue), 0555) != nil {
		t.Fatal("run residue")
	}
	factory, err := f.open(t)
	if err != nil {
		t.Fatal("recovery", err)
	}
	if len(factory.requests) != 1 || Digest(factory.requests[keep.Request.Source.Binding.Attempt]) != Digest(keepSource) || !validAuthHash(factory.packets[keep.Request.Source.Binding.Attempt]) {
		t.Fatal("recovery retained the wrong source", factory.requests)
	}
	owned, _ := factory.owner.journal.read()
	exports, _, _ := factory.archive.records(context.Background())
	if len(owned.Records) != 1 || len(exports) != 1 || exports[0].Binding != keepSource.Binding {
		t.Fatal("recovery left unretained records", len(owned.Records), exports)
	}
	if entries, _ := os.ReadDir(f.directory); len(entries) != 0 {
		t.Fatal("run residue survived recovery")
	}
}

func TestGitVMSourceFactoryRecoveryRefusesUnownedExport(t *testing.T) {
	f := newGitVMFactoryRecoveryFixture(t)
	_, source := f.attempt(t, 1, true)
	f.stage(t, source, false)
	factory, err := f.open(t)
	if err == nil || factory == nil || factory.ready {
		t.Fatal("export without an owner stop receipt recovered")
	}
	if exports, _, _ := f.archive.records(context.Background()); len(exports) != 1 {
		t.Fatal("refused recovery mutated the archive")
	}
	if _, err := factory.Capture(context.Background(), testSnapshot()); err == nil {
		t.Fatal("unrecovered factory captured")
	}
}

func TestGitVMSourceFactoryRefusesActiveOwnerAndForeignManifest(t *testing.T) {
	f := newGitVMFactoryRecoveryFixture(t)
	other := f.manifest
	other.Kernel.SHA256 = Digest("other kernel")
	if factory, err := newGitVMSourceFactory(context.Background(), f.credentials, f.owner.owner, f.archive, f.assets, other, Digest("source release"), f.directory); err == nil || factory != nil {
		t.Fatal("factory accepted a manifest its assets do not pin")
	}
	f.owner.owner.grantEligible["synthetic"] = true
	if factory, err := f.open(t); err == nil || factory != nil {
		t.Fatal("factory adopted an owner with live process-local authority")
	}
	delete(f.owner.owner.grantEligible, "synthetic")
}

// Recovery validates every export's owner before retiring any export.
func TestGitVMSourceFactoryRecoveryValidatesBeforeRetiring(t *testing.T) {
	f := newGitVMFactoryRecoveryFixture(t)
	ungranted, ungrantedSource := f.attempt(t, 1, false)
	_, unowned := f.attempt(t, 2, true)
	if f.owner.owner.journal.write(gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{ungranted}}) != nil {
		t.Fatal("owner journal")
	}
	f.stage(t, ungrantedSource, false)
	f.stage(t, unowned, false)
	if factory, err := f.open(t); err == nil || factory == nil || factory.ready {
		t.Fatal("unowned export recovered")
	}
	if exports, _, _ := f.archive.records(context.Background()); len(exports) != 2 {
		t.Fatal("refused recovery retired an export", exports)
	}
}

func gitVMFactoryRetainedFixture(t *testing.T) (gitVMFactoryRecoveryFixture, *gitVMSourceFactory, gitVMRequest) {
	t.Helper()
	f := newGitVMFactoryRecoveryFixture(t)
	keep, source := f.attempt(t, 1, true)
	if f.owner.owner.journal.write(gitVMUnitJournal{1, gitVMOwnerPurpose, []gitVMUnitRecord{keep}}) != nil {
		t.Fatal("owner journal")
	}
	f.stage(t, source, false)
	factory, err := f.open(t)
	if err != nil || len(factory.requests) != 1 {
		t.Fatal("retained fixture", err)
	}
	// Synthetic records were never captured on this boot; the boot change
	// is their stop proof for confirmStopped.
	f.owner.kernel.boot = "synthetic-later-boot"
	return f, factory, source
}

// A prune interrupted between archive and owner retirement finishes on retry.
func TestGitVMSourceFactoryPruneFinishesPartialRetirement(t *testing.T) {
	f, factory, source := gitVMFactoryRetainedFixture(t)
	if f.archive.retire(context.Background(), source.Binding) != nil {
		t.Fatal("simulated completed archive step")
	}
	storeDirectory := filepath.Join(t.TempDir(), "store")
	if os.Mkdir(storeDirectory, 0700) != nil {
		t.Fatal("store directory")
	}
	durable, err := OpenStore(filepath.Join(storeDirectory, "controller.db"), alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	if err := factory.PruneSource(context.Background(), durable, time.Now()); err != nil {
		t.Fatal("prune after partial retirement", err)
	}
	owned, _ := factory.owner.journal.read()
	if len(owned.Records) != 0 || len(factory.requests) != 0 || !factory.ready {
		t.Fatal("partial retirement not finished", len(owned.Records))
	}
}

// Evidence failure after a completed acquisition retires its records.
func TestGitVMSourceFactoryAbandonedCaptureLeavesNoRecords(t *testing.T) {
	_, factory, source := gitVMFactoryRetainedFixture(t)
	factory.requests, factory.packets = map[string]gitVMRequest{}, map[string]string{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	<-factory.operation
	factory.abandonCaptured(ctx, source.Binding)
	factory.leave()
	owned, _ := factory.owner.journal.read()
	exports, _, _ := factory.archive.records(context.Background())
	if len(owned.Records) != 0 || len(exports) != 0 || !factory.ready {
		t.Fatal("abandoned capture left records", len(owned.Records), exports)
	}
}

// An interrupted retirement is validated against the owner journal before it
// is resumed; an unowned retiring export refuses with its bytes intact.
func TestGitVMSourceFactoryRecoveryValidatesRetiringExports(t *testing.T) {
	f := newGitVMFactoryRecoveryFixture(t)
	_, unowned := f.attempt(t, 1, true)
	f.stage(t, unowned, false)
	j, err := f.archive.journal()
	if err != nil {
		t.Fatal(err)
	}
	j.Records[0].State = "retiring"
	if f.archive.writeJournal(j) != nil {
		t.Fatal("mark retiring")
	}
	if factory, err := f.open(t); err == nil || factory == nil || factory.ready {
		t.Fatal("unowned retiring export recovered")
	}
	if _, err := os.Lstat(filepath.Join(f.archive.directory, "materialization", gitExportDirectory(unowned.Binding.Attempt))); err != nil {
		t.Fatal("refused recovery deleted unowned source", err)
	}
	if exports, _, _ := f.archive.records(context.Background()); len(exports) != 1 {
		t.Fatal("refused recovery changed the archive journal", exports)
	}
}

// The run directory may not contain, or sit inside, the owner or archive root.
// Here the live owner root is named like run residue inside the work
// directory; recovery must refuse rather than delete the live journal.
func TestGitVMSourceFactoryRejectsNestedRoots(t *testing.T) {
	f := newGitVMFactoryRecoveryFixture(t)
	work, err := os.MkdirTemp("/tmp", "sfl-vmn-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	nested := filepath.Join(work, "run-"+strings.Repeat("b", 32))
	if os.Mkdir(nested, 0700) != nil || f.owner.owner.Close() != nil {
		t.Fatal("nested owner setup")
	}
	owner, err := newGitVMUnitOwner(context.Background(), nested, f.owner.stub, f.owner.stubHash, f.owner.manager, f.owner.kernel)
	if err != nil {
		t.Fatal("nested owner", err)
	}
	defer owner.Close()
	factory, err := newGitVMSourceFactory(context.Background(), f.credentials, owner, f.archive, f.assets, f.manifest, Digest("source release"), work)
	if err == nil || factory != nil {
		if factory != nil {
			factory.Close(context.Background())
		}
		t.Fatal("work directory containing the owner root accepted")
	}
	if _, err := os.Lstat(filepath.Join(nested, "journal", "source-vm-units.json")); err != nil {
		t.Fatal("live owner journal removed", err)
	}
	inner := filepath.Join(nested, "journal")
	if factory, err := newGitVMSourceFactory(context.Background(), f.credentials, owner, f.archive, f.assets, f.manifest, Digest("source release"), inner); err == nil || factory != nil {
		t.Fatal("work directory inside the owner root accepted")
	}
	if !gitVMDisjointRoots("/a/b", "/a/bc") || gitVMDisjointRoots("/a/b", "/a/b/c") || gitVMDisjointRoots("/a/b/c", "/a/b") || gitVMDisjointRoots("/a", "/a") {
		t.Fatal("disjoint root comparison")
	}
}
