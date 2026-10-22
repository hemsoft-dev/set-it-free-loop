//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Explicit model-free production-host qualification only. It needs a measured
// source image and a gitsourcevmstub built from this exact head. It starts a
// real transient unit in the caller's user manager, runs QEMU with KVM and
// serves Git from a local fixture through synthetic installation credentials.
// No real credential, provider, App or network destination participates.
func TestGitVMRunProductionHostActualGit(t *testing.T) {
	image, stub := os.Getenv("SFL_SOURCE_VM_HOST_IMAGE"), os.Getenv("SFL_SOURCE_VM_HOST_STUB")
	if image == "" || stub == "" {
		t.Skip("explicit source image and exact-head stub required")
	}
	fixtureCase := os.Getenv("SFL_SOURCE_VM_HOST_CASE")
	if fixtureCase == "" {
		fixtureCase = "normal"
	}
	if fixtureCase != "normal" && fixtureCase != "upstream-fail" && fixtureCase != "cancel" && fixtureCase != "crash" {
		t.Fatal("unknown host qualification case")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 170*time.Second)
	defer cancel()
	spec, source, release := gitVMActualImageFixture(t, image)
	store, snapshot := gitQuarantineFixture(t)
	source.Binding.Snapshot = snapshot
	stubBytes, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal("stub", err)
	}
	parent, err := os.MkdirTemp("/tmp", "sfl-vmq-")
	if err != nil {
		t.Fatal(err)
	}
	ownerRoot, archiveRoot, runRoot := filepath.Join(parent, "o"), filepath.Join(parent, "a"), filepath.Join(parent, "r")
	for _, directory := range []string{ownerRoot, archiveRoot, runRoot} {
		if os.Mkdir(directory, 0700) != nil {
			t.Fatal("private directories")
		}
	}
	owner, err := openGitVMUnitOwner(ctx, ownerRoot, stub, gitRawHash(stubBytes))
	if err != nil {
		t.Fatal("owner", err)
	}
	var final *gitVMUnitOwner
	defer func() { final = owner }()
	t.Cleanup(func() {
		owner := final
		if owner == nil || owner.closed {
			return
		}
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		// Never leave an unproved unit: recover proves or refuses every record.
		if err := owner.recover(cleanup); err != nil {
			t.Error("source unit recovery", err)
		}
		if err := owner.Close(); err != nil {
			t.Error("owner close", err)
			return
		}
		os.RemoveAll(parent)
	})
	assets, err := openGitVMAssets(ctx, spec)
	if err != nil {
		t.Fatal("assets", err)
	}
	defer assets.Close()
	archive, err := openGitVMExportArchive(ctx, archiveRoot)
	if err != nil {
		t.Fatal("archive", err)
	}
	defer archive.Close(context.Background())
	credentials, _, _, _, _ := credentialOwnerFixture(t)
	if credentials.Refresh(ctx) != nil {
		t.Fatal("synthetic credential pair")
	}
	held, unhold := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(unhold) }) }
	defer releaseHeld()
	installation, calls := gitReadFixture(t, GitHubReader, gitVMUploadPackHandler(t, store, fixtureCase, held, unhold))
	pair := credentials.state.current
	inner := pair.reader.state.inner
	pair.reader.state.inner = installation.state.inner
	defer func() { pair.reader.state.inner = inner }()
	run, err := newGitVMRun(ctx, owner, archive, assets, source, strings.Repeat("c", 32), runRoot)
	if err != nil {
		t.Fatal("run", err)
	}
	q := gitVMGuestRequest{1, gitVMGuestPurpose, source, gitVMReadBinding{1, "source-vm-git-read", source.Binding, strings.Repeat("c", 32), 1}}
	if !release.matches(q) {
		t.Fatal("image release does not match source request")
	}
	acquireCtx := ctx
	if fixtureCase == "cancel" {
		var stop context.CancelFunc
		acquireCtx, stop = context.WithCancel(ctx)
		go func() {
			// Cancel after the guest has started reading, mid-acquisition.
			for calls.Load() == 0 && acquireCtx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			stop()
		}()
	}
	started := time.Now()
	type outcome struct {
		materialized *gitMaterialization
		err          error
	}
	result := make(chan outcome, 1)
	go func() {
		m, err := runGitFactoryAcquisition(acquireCtx, credentials, source.Binding, run)
		result <- outcome{m, err}
	}()
	var recovered *gitVMUnitOwner
	if fixtureCase == "crash" {
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal("guest never reached pack negotiation")
		}
		// The controller loses all process-local ownership while the guest
		// is held mid-read: no stop, no pidfds, journal lock released.
		gitVMOwnerCrash(owner)
		var openErr error
		recovered, openErr = openGitVMUnitOwner(ctx, ownerRoot, stub, gitRawHash(stubBytes))
		if openErr != nil {
			t.Fatal("reopen after crash", openErr)
		}
		owner = recovered
		before, _ := recovered.journal.read()
		if len(before.Records) != 1 || before.Records[0].Stopped || !validAuthHash(before.Records[0].GrantHash) {
			t.Fatal("crash did not leave the granted attempt owned", before)
		}
		live, _ := recovered.manager.Observe(ctx, before.Records[0].Unit)
		if live.InvocationID != before.Records[0].Identity.InvocationID || live.MainPID == 0 {
			t.Fatal("original instance was not live at recovery", live)
		}
		// Recovery has only the durable capture: no retained pidfd or grant.
		if recovered.grantEligible[before.Records[0].Request.Source.Binding.Attempt] {
			t.Fatal("recovery re-armed grant authority")
		}
		if err := recovered.recover(ctx); err != nil {
			t.Fatal("recovery of a live original instance", err)
		}
		releaseHeld()
	}
	done := <-result
	materialized, err := done.materialized, done.err
	elapsed := time.Since(started)
	t.Logf("acquisition result %v, first refused stage %q", err, run.failure())
	if fixtureCase == "crash" {
		if err == nil {
			materialized.Close()
			t.Fatal("crashed controller returned source")
		}
		// Without stop proof the reader generation stayed held. A restarted
		// process re-derives the stop capability from the durable record;
		// model that by pointing the run at the recovered owner, then require
		// the generation to release only through that proved stop.
		credentials.state.mu.Lock()
		var generation *githubReaderGeneration
		for _, g := range credentials.state.acquisitions {
			generation = g
		}
		credentials.state.mu.Unlock()
		if generation == nil {
			t.Fatal("generation released without stop proof")
		}
		run.mu.Lock()
		run.owner = recovered
		run.mu.Unlock()
		if err := generation.stop(ctx); err != nil {
			t.Fatal("generation release after proved stop", err)
		}
	}
	value, readErr := owner.journal.read()
	if readErr != nil || len(value.Records) != 1 {
		t.Fatal("ownership journal", readErr)
	}
	r := value.Records[0]
	if !r.Stopped || !r.Revoked || !r.Captured || r.StopBoot != "" || !validAuthHash(r.GrantHash) {
		t.Fatal("original instance not proved stopped", r)
	}
	observed, observeErr := owner.manager.Observe(ctx, r.Unit)
	if observeErr == nil && observed.InvocationID == r.Identity.InvocationID && observed.MainPID != 0 {
		t.Fatal("original source unit still has a main process")
	}
	if fixtureCase != "normal" {
		if err == nil {
			materialized.Close()
			t.Fatal("failed acquisition returned source", fixtureCase)
		}
		t.Logf("source VM %s refused after %s with original unit stopped; upstream calls %d", fixtureCase, elapsed, calls.Load())
		return
	}
	if err != nil {
		t.Fatal("acquisition", err)
	}
	defer materialized.Close()
	object, err := materialized.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
	want, wantErr := store.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
	if err != nil || wantErr != nil || !bytes.Equal(object.Data, want.Data) {
		t.Fatal("source VM materialization differs", err, wantErr)
	}
	t.Logf("production host source VM acquisition passed in %s through %d upstream calls; unit %s invocation %s stopped", elapsed, calls.Load(), r.Unit, r.Identity.InvocationID)
}

// gitVMUploadPackHandler serves the fixture repository with the actual Git
// producer. The upstream-fail case refuses pack negotiation after the
// advertisement so the guest fails mid-acquisition.
func gitVMUploadPackHandler(t *testing.T, store GitStore, fixtureCase string, held chan<- struct{}, release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		advertisement := q.Method == http.MethodGet
		if !advertisement && fixtureCase == "crash" {
			// Keep the guest provably mid-read until recovery has run.
			held <- struct{}{}
			select {
			case <-release:
			case <-q.Context().Done():
			}
			http.Error(w, "synthetic held upstream", http.StatusBadGateway)
			return
		}
		if !advertisement && fixtureCase == "upstream-fail" {
			http.Error(w, "synthetic upstream failure", http.StatusBadGateway)
			return
		}
		args := []string{"upload-pack", "--stateless-rpc"}
		if advertisement {
			args = append(args, "--advertise-refs")
		}
		git := exec.CommandContext(q.Context(), store.Git, append(args, store.GitDir)...)
		git.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "LC_ALL=C"}
		protocol := q.Header.Get("Git-Protocol")
		if protocol != "" {
			git.Env = append(git.Env, "GIT_PROTOCOL="+protocol)
		}
		if !advertisement {
			git.Stdin = q.Body
		}
		data, err := git.Output()
		if err != nil {
			http.Error(w, "fixture Git failed", http.StatusInternalServerError)
			return
		}
		operation := GitHubGitUploadPack
		if advertisement {
			operation = GitHubGitAdvertisement
			if protocol != "version=2" {
				data = append([]byte("001e# service=git-upload-pack\n0000"), data...)
			}
		}
		w.Header().Set("Content-Type", gitReadMedia(operation))
		io.Copy(w, bytes.NewReader(data))
	})
}

// gitVMOwnerCrash drops process-local ownership as a controller crash would.
// The journal remains; the live unit keeps running until recovery stops it.
func gitVMOwnerCrash(o *gitVMUnitOwner) {
	<-o.operation
	for attempt, fd := range o.rootPIDFDs {
		unix.Close(fd)
		delete(o.rootPIDFDs, attempt)
	}
	o.grantEligible = map[string]bool{}
	o.journal.Close(context.Background())
	o.image.Close()
	o.supervisor.Close()
	o.manager.Close()
	o.root.Close()
	o.closed = true
	o.operation <- struct{}{}
}
