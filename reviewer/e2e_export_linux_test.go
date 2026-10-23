//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// This file exposes read-only test seams to the external aggregate
// qualification in e2e_qualification_linux_test.go. That test lives in package
// reviewer_test so it can import the fake-only native backend producers, which
// themselves import this package. Nothing here starts, stops or mutates work.

// E2ESourceVMRelease derives the version 2 source VM package pins from a
// measured source image and an exact-head host stub. It returns the image's
// recorded source commit so the caller can bind the release revision to it.
// The image's guest release must name that commit as its source head, and the
// stub must carry Go VCS stamping for that commit from an unmodified tree.
func E2ESourceVMRelease(t *testing.T, image, stub string) (GitReviewerSourceVMRelease, string) {
	t.Helper()
	spec, _, guest := gitVMActualImageFixture(t, image)
	raw, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal("source stub", err)
	}
	// Parse the hashed bytes, not a second read of the path.
	info, err := buildinfo.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal("source stub build info", err)
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
	commit, err := e2eRecordedCommit(manifest, guest)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2eStubRevision(info, commit); err != nil {
		t.Fatal(err)
	}
	return vm, commit
}

// e2eRecordedCommit returns the source image's clean recorded commit. The
// image's embedded guest release must name the same source head: the derived
// manifest pins the guest assets but not their head, so a guest built from
// another commit could otherwise run under this commit's release revision.
func e2eRecordedCommit(manifest []byte, guest gitVMGuestRelease) (string, error) {
	var recorded struct {
		Commit string `json:"source_commit"`
		Dirty  bool   `json:"source_dirty"`
	}
	if json.Unmarshal(manifest, &recorded) != nil || !objectID.MatchString(recorded.Commit) || recorded.Dirty {
		return "", errors.New("source image lacks a clean recorded commit")
	}
	if guest.SourceHead != recorded.Commit {
		return "", fmt.Errorf("source image guest release head %q differs from its recorded commit %s", guest.SourceHead, recorded.Commit)
	}
	return recorded.Commit, nil
}

const e2eStubPackage = "github.com/hemsoft-dev/set-it-free-loop/reviewer/cmd/gitsourcevmstub"

// e2eTestPackage is the program path Go records for this package's test binary.
const e2eTestPackage = "github.com/hemsoft-dev/set-it-free-loop/reviewer.test"

// E2EBuildStamp is the Go VCS stamp recorded in a qualified binary.
type E2EBuildStamp struct {
	Path, GoVersion, VCS, Revision, Modified string
}

// e2eBuildStamp requires info to describe program path, stamped by Go as a
// build of commit from an unmodified Git tree. A repeated setting is refused.
// Go does not stamp builds in a linked worktree, so such a binary is refused
// rather than trusted by hash alone.
func e2eBuildStamp(label string, info *debug.BuildInfo, path, commit string) (E2EBuildStamp, error) {
	stamp := E2EBuildStamp{Path: info.Path, GoVersion: info.GoVersion}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		if _, duplicate := settings[setting.Key]; duplicate {
			return stamp, errors.New(label + " repeats build setting " + setting.Key)
		}
		settings[setting.Key] = setting.Value
	}
	stamp.VCS, stamp.Revision, stamp.Modified = settings["vcs"], settings["vcs.revision"], settings["vcs.modified"]
	if info.Path != path || stamp.VCS != "git" || stamp.Revision != commit || stamp.Modified != "false" {
		return stamp, fmt.Errorf("%s %s is not stamped as an unmodified build of %s (vcs %q revision %q modified %q)", label, info.Path, commit, stamp.VCS, stamp.Revision, stamp.Modified)
	}
	return stamp, nil
}

// e2eStubRevision requires the source stub's stamp to name commit.
func e2eStubRevision(info *debug.BuildInfo, commit string) error {
	_, err := e2eBuildStamp("source stub", info, e2eStubPackage, commit)
	return err
}

// E2ETestBinaryStamp attests the running test binary: it must be this
// package's test program, stamped as an unmodified build of the release
// commit. The returned stamp is recorded in the evidence either way.
func E2ETestBinaryStamp(commit string) (E2EBuildStamp, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return E2EBuildStamp{}, errors.New("test binary carries no build info")
	}
	return e2eTestBinaryStamp(info, commit)
}

func e2eTestBinaryStamp(info *debug.BuildInfo, commit string) (E2EBuildStamp, error) {
	return e2eBuildStamp("test binary", info, e2eTestPackage, commit)
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

func e2eStamped(path string, settings ...string) *debug.BuildInfo {
	info := &debug.BuildInfo{Path: path}
	for i := 0; i+1 < len(settings); i += 2 {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return info
}

func TestE2EStubRevision(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	stamped := e2eStamped
	if err := e2eStubRevision(stamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"), commit); err != nil {
		t.Fatal("exact clean stamp refused", err)
	}
	for name, info := range map[string]*debug.BuildInfo{
		"unstamped":          stamped(e2eStubPackage),
		"other commit":       stamped(e2eStubPackage, "vcs", "git", "vcs.revision", other, "vcs.modified", "false"),
		"modified":           stamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "true"),
		"modified unknown":   stamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit),
		"other vcs":          stamped(e2eStubPackage, "vcs", "hg", "vcs.revision", commit, "vcs.modified", "false"),
		"duplicate revision": stamped(e2eStubPackage, "vcs", "git", "vcs.revision", other, "vcs.revision", commit, "vcs.modified", "false"),
		"other program":      stamped(e2eStubPackage+"x", "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"),
	} {
		if e2eStubRevision(info, commit) == nil {
			t.Error(name, "stub accepted for the recorded commit")
		}
	}
}

func TestE2ETestBinaryStamp(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	stamp, err := e2eTestBinaryStamp(e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"), commit)
	if err != nil || stamp.Path != e2eTestPackage || stamp.Revision != commit || stamp.Modified != "false" || stamp.VCS != "git" {
		t.Fatal("exact clean test binary stamp refused", stamp, err)
	}
	for name, info := range map[string]*debug.BuildInfo{
		"unstamped":          e2eStamped(e2eTestPackage),
		"other commit":       e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", other, "vcs.modified", "false"),
		"modified":           e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "true"),
		"modified unknown":   e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", commit),
		"other vcs":          e2eStamped(e2eTestPackage, "vcs", "hg", "vcs.revision", commit, "vcs.modified", "false"),
		"duplicate revision": e2eStamped(e2eTestPackage, "vcs", "git", "vcs.revision", other, "vcs.revision", commit, "vcs.modified", "false"),
		"source stub":        e2eStamped(e2eStubPackage, "vcs", "git", "vcs.revision", commit, "vcs.modified", "false"),
	} {
		if _, err := e2eTestBinaryStamp(info, commit); err == nil {
			t.Error(name, "test binary accepted for the release commit")
		}
	}
}

func TestE2ERecordedCommit(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	manifest := func(commit string, dirty bool) []byte {
		raw, _ := json.Marshal(map[string]any{"source_commit": commit, "source_dirty": dirty})
		return raw
	}
	if got, err := e2eRecordedCommit(manifest(commit, false), gitVMGuestRelease{SourceHead: commit}); err != nil || got != commit {
		t.Fatal("matching guest release refused", got, err)
	}
	for name, in := range map[string]struct {
		manifest []byte
		head     string
	}{
		"guest from another commit": {manifest(commit, false), other},
		"guest without a head":      {manifest(commit, false), ""},
		"dirty image":               {manifest(commit, true), commit},
		"no recorded commit":        {manifest("", false), commit},
		"malformed manifest":        {[]byte("{"), commit},
	} {
		if got, err := e2eRecordedCommit(in.manifest, gitVMGuestRelease{SourceHead: in.head}); err == nil {
			t.Error(name, "accepted as release revision", got)
		}
	}
}
