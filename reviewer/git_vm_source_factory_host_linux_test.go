//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Explicit model-free factory qualification on a real host, using the same
// inputs as TestGitVMRunProductionHostActualGit. It exercises PollSource
// capture, restore, restart recovery, failed-capture discard and pruning
// through real source VM units. No real credential, provider or App is used.
func TestGitVMSourceFactoryProductionHost(t *testing.T) {
	image, stub := os.Getenv("SFL_SOURCE_VM_HOST_IMAGE"), os.Getenv("SFL_SOURCE_VM_HOST_STUB")
	if image == "" || stub == "" {
		t.Skip("explicit source image and exact-head stub required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	spec, source, _ := gitVMActualImageFixture(t, image)
	store, snapshot := gitQuarantineFixture(t)
	stubBytes, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.MkdirTemp("/tmp", "sfl-vmf-")
	if err != nil {
		t.Fatal(err)
	}
	ownerRoot, archiveRoot, runRoot := filepath.Join(parent, "o"), filepath.Join(parent, "a"), filepath.Join(parent, "r")
	for _, directory := range []string{ownerRoot, archiveRoot, runRoot} {
		if os.Mkdir(directory, 0700) != nil {
			t.Fatal("private directories")
		}
	}
	credentials, _, _, _, _ := credentialOwnerFixture(t)
	if credentials.Refresh(ctx) != nil {
		t.Fatal("synthetic credential pair")
	}
	var failUpstream atomic.Bool
	upload := gitVMUploadPackHandler(t, store, "normal", nil, nil)
	installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == http.MethodPost && failUpstream.Load() {
			http.Error(w, "synthetic upstream failure", http.StatusBadGateway)
			return
		}
		upload.ServeHTTP(w, q)
	}))
	pair := credentials.state.current
	inner := pair.reader.state.inner
	pair.reader.state.inner = installation.state.inner
	defer func() { pair.reader.state.inner = inner }()
	releaseHash := source.Binding.ReleaseHash
	open := func() *gitVMSourceFactory {
		t.Helper()
		owner, err := openGitVMUnitOwner(ctx, ownerRoot, stub, gitRawHash(stubBytes))
		if err != nil {
			t.Fatal("owner", err)
		}
		archive, err := openGitVMExportArchive(ctx, archiveRoot)
		if err != nil {
			t.Fatal("archive", err)
		}
		assets, err := openGitVMAssets(ctx, spec)
		if err != nil {
			t.Fatal("assets", err)
		}
		factory, err := newGitVMSourceFactory(ctx, credentials, owner, archive, assets, spec.Manifest, releaseHash, runRoot)
		if err != nil {
			t.Fatal("factory recovery", err)
		}
		return factory
	}
	factory := open()
	t.Cleanup(func() {
		if !factory.closed {
			if err := factory.Close(context.Background()); err != nil {
				t.Error("factory close", err)
				return
			}
		}
		os.RemoveAll(parent)
	})
	evidence, err := factory.Capture(ctx, snapshot)
	if err != nil {
		t.Fatal("capture", err)
	}
	job := testJob()
	job.Snapshot, job.Packet = snapshot, evidence.Packet()
	if factory.ReleaseCapture(ctx, evidence) != nil {
		t.Fatal("release capture")
	}
	readSafe := func(bound *JobEvidence) {
		t.Helper()
		object, err := bound.evidence.store.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
		want, wantErr := store.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
		if err != nil || wantErr != nil || !bytes.Equal(object.Data, want.Data) {
			t.Fatal("restored source differs", err, wantErr)
		}
	}
	bound, err := factory.Restore(ctx, job)
	if err != nil {
		t.Fatal("restore", err)
	}
	readSafe(bound)
	if factory.ReleaseJob(ctx, bound) != nil {
		t.Fatal("release job")
	}
	// A process restart reopens every component; recovery keeps the staged,
	// granted, stopped attempt and restores it from retained bytes alone.
	if factory.Close(ctx) != nil {
		t.Fatal("close before restart")
	}
	before := calls.Load()
	factory = open()
	if len(factory.requests) != 1 || len(factory.materialized) != 0 {
		t.Fatal("restart did not retain exactly the staged attempt", len(factory.requests))
	}
	bound, err = factory.Restore(ctx, job)
	if err != nil {
		t.Fatal("restore after restart", err)
	}
	readSafe(bound)
	if factory.ReleaseJob(ctx, bound) != nil || calls.Load() != before {
		t.Fatal("restart restore refetched source")
	}
	// A failed capture is discarded after its proved stop and frees capacity.
	failUpstream.Store(true)
	if evidence, err := factory.Capture(ctx, snapshot); err == nil || evidence != nil {
		t.Fatal("failed upstream captured source")
	}
	failUpstream.Store(false)
	owned, _ := factory.owner.journal.read()
	exports, _, _ := factory.archive.records(ctx)
	entries, _ := os.ReadDir(runRoot)
	if !factory.ready || len(owned.Records) != 1 || len(exports) != 1 || exports[0].State != "staged" || len(entries) != 0 {
		t.Fatal("failed capture left residue", len(owned.Records), exports, len(entries))
	}
	// Pruning with no referencing job retires the retained attempt.
	durable, err := OpenStore(filepath.Join(parent, "controller.db"), alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal("store", err)
	}
	defer durable.Close()
	if err := factory.PruneSource(ctx, durable, time.Now()); err != nil {
		t.Fatal("prune", err)
	}
	owned, _ = factory.owner.journal.read()
	exports, _, _ = factory.archive.records(ctx)
	if len(owned.Records) != 0 || len(exports) != 0 || len(factory.requests) != 0 {
		t.Fatal("prune left records", len(owned.Records), exports)
	}
	if _, err := factory.Restore(ctx, job); err == nil {
		t.Fatal("pruned attempt restored")
	}
	t.Logf("source VM factory capture, restart restore, failed-capture discard and prune passed; upstream calls %d", calls.Load())
}
