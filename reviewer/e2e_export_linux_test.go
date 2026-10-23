//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// This file exposes read-only test seams to the external aggregate
// qualification in e2e_qualification_linux_test.go. That test lives in package
// reviewer_test so it can import the fake-only native backend producers, which
// themselves import this package. Nothing here starts, stops or mutates work.

// E2ESourceVMRelease derives the version 2 source VM package pins from a
// measured source image and an exact-head host stub. It returns the image's
// recorded source commit so the caller can bind the release revision to it.
func E2ESourceVMRelease(t *testing.T, image, stub string) (GitReviewerSourceVMRelease, string) {
	t.Helper()
	spec, _, _ := gitVMActualImageFixture(t, image)
	raw, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal("source stub", err)
	}
	vm := GitReviewerSourceVMRelease{Stub: VMBootAsset{stub, gitRawHash(raw), int64(len(raw))}, QEMU: spec.QEMU, Kernel: spec.Kernel, Initrd: spec.Initrd, BIOS: spec.BIOS, QEMURuntime: spec.QEMURuntime, NamespaceHelper: spec.NamespaceHelper, BootstrapLibraries: spec.BootstrapLibraries, Worker: spec.Manifest.Worker, Runtime: spec.Manifest.Runtime}
	derived, err := vm.assetSpec()
	if err != nil || Digest(derived.Manifest) != Digest(spec.Manifest) {
		t.Fatal("source VM release does not derive the measured image manifest", err)
	}
	manifest, err := os.ReadFile(filepath.Join(image, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Commit string `json:"source_commit"`
		Dirty  bool   `json:"source_dirty"`
	}
	if json.Unmarshal(manifest, &recorded) != nil || !objectID.MatchString(recorded.Commit) || recorded.Dirty {
		t.Fatal("source image lacks a clean recorded commit")
	}
	return vm, recorded.Commit
}

// E2ESourceRecord is a nonsecret projection of one source VM ownership record.
type E2ESourceRecord struct {
	Attempt, Unit, InvocationID, RequestHash string
	ChannelID                                string
	Generation                               uint64
	Snapshot                                 Snapshot
	StartIntent, StartSettled, Captured      bool
	Revoked, Stopped                         bool
	GrantHash, StopBoot                      string
}

// E2EExportRecord projects one source VM archive record.
type E2EExportRecord struct {
	Attempt, State, RequestHash string
	Snapshot                    Snapshot
}

func (s *GitReviewerService) e2eFactory() *gitVMSourceFactory {
	if s == nil || s.inner == nil {
		return nil
	}
	factory, _ := s.inner.source.(*gitVMSourceFactory)
	return factory
}

// E2ESourceRecords reads the owner journal and archive under their own
// operation tokens. It reports an error once the service has closed them.
func (s *GitReviewerService) E2ESourceRecords(ctx context.Context) ([]E2ESourceRecord, []E2EExportRecord, error) {
	factory := s.e2eFactory()
	if factory == nil {
		return nil, nil, ErrLiveBlocked
	}
	journal, err := factory.owner.journal.read()
	if err != nil {
		return nil, nil, err
	}
	records := []E2ESourceRecord{}
	for _, r := range journal.Records {
		records = append(records, E2ESourceRecord{Attempt: r.Request.Source.Binding.Attempt, Unit: r.Unit, InvocationID: r.Birth.InvocationID, RequestHash: r.RequestHash, ChannelID: r.Request.Read.ChannelID, Generation: r.Request.Read.Generation, Snapshot: r.Request.Source.Binding.Snapshot, StartIntent: r.StartIntent, StartSettled: r.StartSettled, Captured: r.Captured, Revoked: r.Revoked, Stopped: r.Stopped, GrantHash: r.GrantHash, StopBoot: r.StopBoot})
	}
	archived, _, err := factory.archive.records(ctx)
	if err != nil {
		return nil, nil, err
	}
	exports := []E2EExportRecord{}
	for _, r := range archived {
		exports = append(exports, E2EExportRecord{Attempt: r.Binding.Attempt, State: r.State, RequestHash: r.RequestHash, Snapshot: r.Binding.Snapshot})
	}
	return records, exports, nil
}

// E2EProbe is one isolation read against a restored job's retained source.
// Store is the raw materialized object store; Reader is the model-facing
// reader bound to the job's snapshot and allowed paths.
type E2EProbe struct {
	Revision, Path       string
	StoreOK, ReaderOK    bool
	StoreHash, ReaderErr string
}

// E2ERestoreProbe restores a durable job from retained bytes only, performs
// the requested reads and releases it. Restore never fetches upstream.
func (s *GitReviewerService) E2ERestoreProbe(ctx context.Context, job Job, reads [][2]string) ([]E2EProbe, error) {
	factory := s.e2eFactory()
	if factory == nil {
		return nil, ErrLiveBlocked
	}
	bound, err := factory.Restore(ctx, job)
	if err != nil {
		return nil, err
	}
	defer factory.ReleaseJob(context.WithoutCancel(ctx), bound)
	reader, err := bound.Reader(job.Snapshot)
	if err != nil {
		return nil, err
	}
	probes := []E2EProbe{}
	for _, read := range reads {
		p := E2EProbe{Revision: read[0], Path: read[1]}
		if object, err := bound.evidence.store.Read(ctx, read[0], read[1], snapshotObjectBytes); err == nil {
			sum := sha256.Sum256(object.Data)
			p.StoreOK, p.StoreHash = true, hex.EncodeToString(sum[:])
		}
		if _, err := reader.Read(ctx, read[0], read[1]); err == nil {
			p.ReaderOK = true
		} else {
			p.ReaderErr = err.Error()
		}
		probes = append(probes, p)
	}
	return probes, nil
}
