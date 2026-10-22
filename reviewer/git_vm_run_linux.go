//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// gitVMRun is one source VM acquisition behind the existing private
// gitFactoryAcquisition seam. The generation retained by the credential owner
// holds run.stop as its stop capability, so no reader generation is released
// before the read channel is joined and the original unit is proved stopped.
type gitVMRun struct {
	mu            sync.Mutex
	owner         *gitVMUnitOwner
	archive       *gitVMExportArchive
	assets        *gitVMAssets
	source        gitVMRequest
	channelID     string
	directory     string
	listener      *net.UnixListener
	channels      *nativeVMChannels
	runtime       *nativeRuntimeRoot
	generation    *githubReaderGeneration
	request       gitVMGuestRequest
	spent         bool
	revoked       bool
	stopped       bool
	closed        bool
	closeFailed   bool
	cancel        context.CancelFunc
	acquireDone   chan struct{}
	readDone      chan struct{}
	readAudit     gitVMReadAudit
	stopOperation chan struct{}
	// failed is a fixed, non-secret stage label for the first refusal.
	failed string
}

func (r *gitVMRun) fail(stage string) error {
	r.mu.Lock()
	if r.failed == "" {
		r.failed = stage
	}
	r.mu.Unlock()
	return errGitVMOwner
}
func (r *gitVMRun) failure() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

func gitVMRunPath(directory string) bool {
	return sourcePath(directory) && len(filepath.Join(directory, "stub.sock")) <= 100
}

// directory is an existing empty private attempt directory. Construction
// opens the stub listener only; it prepares no record, unit, runtime or VM.
func newGitVMRun(ctx context.Context, owner *gitVMUnitOwner, archive *gitVMExportArchive, assets *gitVMAssets, source gitVMRequest, channelID, directory string) (_ *gitVMRun, result error) {
	if ctx == nil || ctx.Err() != nil || owner == nil || archive == nil || assets == nil || !source.valid() || !gitQuarantineHex(channelID, 32) || !gitVMRunPath(directory) || privateDirectory(directory) != nil {
		return nil, errGitVMOwner
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errGitVMOwner
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		return nil, errGitVMOwner
	}
	r := &gitVMRun{owner: owner, archive: archive, assets: assets, source: source, channelID: channelID, directory: directory, stopOperation: make(chan struct{}, 1)}
	r.stopOperation <- struct{}{}
	r.listener, err = gitFactoryListen(filepath.Join(directory, "stub.sock"))
	if err != nil {
		return nil, errGitVMOwner
	}
	return r, nil
}

func (r *gitVMRun) retainGeneration(g *githubReaderGeneration) {
	r.mu.Lock()
	r.generation = g
	r.mu.Unlock()
}

// check runs before each upstream Git read and after each body closes. The
// binding, unrevoked run and live original granted instance must all hold.
func (r *gitVMRun) check(ctx context.Context, binding gitVMReadBinding) error {
	r.mu.Lock()
	q, revoked := r.request, r.revoked
	r.mu.Unlock()
	if revoked || !q.valid() || binding != q.Read || r.owner.live(ctx, q.Source.Binding.Attempt) != nil {
		return errGitVMRead
	}
	return nil
}

type gitVMMeasuredExport struct {
	io.ReadCloser
	digest hash.Hash
	bytes  int64
}

func (m *gitVMMeasuredExport) Read(p []byte) (int, error) {
	n, err := m.ReadCloser.Read(p)
	m.bytes += int64(n)
	m.digest.Write(p[:n])
	return n, err
}

// acquire stages plain output only. Success is neither publication nor stop
// authority: the composing factory still calls stop and releases the
// generation before any evidence or object store leaves it.
func (r *gitVMRun) acquire(ctx context.Context, source *gitGenerationReader) (_ *gitMaterialization, result error) {
	if ctx == nil || ctx.Err() != nil || source == nil || source.generation == nil {
		return nil, errGitVMOwner
	}
	r.mu.Lock()
	if r.spent || r.revoked || r.closed || r.generation != source.generation {
		r.mu.Unlock()
		return nil, errGitVMOwner
	}
	r.spent = true
	phase, cancel := context.WithTimeout(ctx, 150*time.Second)
	r.cancel = cancel
	r.acquireDone = make(chan struct{})
	done := r.acquireDone
	q := gitVMGuestRequest{1, gitVMGuestPurpose, r.source, gitVMReadBinding{1, "source-vm-git-read", r.source.Binding, r.channelID, source.generation.id}}
	r.request = q
	r.mu.Unlock()
	defer close(done)
	defer cancel()
	if !q.valid() || r.owner.prepare(phase, q) != nil {
		return nil, r.fail("prepare")
	}
	boot, err := r.launchInputs(phase)
	if err != nil {
		return nil, r.fail("launch-inputs")
	}
	defer closeVMFiles(boot)
	channels, err := openNativeVMChannels()
	if err != nil {
		return nil, r.fail("channels")
	}
	r.mu.Lock()
	r.channels = channels
	r.mu.Unlock()
	guest, err := channels.duplicateGuest()
	if err != nil {
		return nil, r.fail("guest-channels")
	}
	defer closeVMFiles(guest)
	attempt := q.Source.Binding.Attempt
	if r.owner.start(phase, attempt, filepath.Join(r.directory, "stub.sock")) != nil {
		return nil, r.fail("unit-start")
	}
	conn, err := gitFactoryAccept(phase, r.listener)
	if err != nil {
		return nil, r.fail("stub-accept")
	}
	grantErr := r.owner.grant(phase, attempt, conn, boot, guest)
	conn.Close()
	if grantErr != nil {
		return nil, r.fail("grant")
	}
	// Only the authenticated stub and its QEMU keep the guest endpoints.
	closeVMFiles(guest)
	guest = nil
	control, read, export := channels.host[0], channels.host[1], channels.host[2]
	stopControl := channels.interruptControlOnCancel(phase)
	defer stopControl()
	readDone := make(chan struct{})
	r.mu.Lock()
	r.readDone = readDone
	r.mu.Unlock()
	go func() {
		audit, _ := serveGitVMRead(phase, read, q.Read, source, r.check)
		r.mu.Lock()
		r.readAudit = audit
		r.mu.Unlock()
		close(readDone)
	}()
	if writeVMRecord(control, q, gitVMRecordBytes) != nil {
		return nil, r.fail("guest-request")
	}
	ready, err := readVMRecord[gitVMGuestPhase](control, gitVMRecordBytes)
	if err != nil || !ready.valid(q, "guest-ready") {
		return nil, r.fail("guest-ready")
	}
	if writeVMRecord(control, gitVMGuestPhase{1, gitVMGuestPurpose, Digest(q), "host-grant"}, gitVMRecordBytes) != nil {
		return nil, r.fail("host-grant")
	}
	measured := &gitVMMeasuredExport{ReadCloser: export, digest: sha256.New()}
	materialized, err := r.archive.stage(phase, q.Source, measured)
	if err != nil {
		return nil, r.fail("export")
	}
	good := false
	defer func() {
		if !good {
			materialized.Close()
		}
	}()
	finished, err := readVMRecord[gitVMGuestFinished](control, gitVMRecordBytes)
	if err != nil || !finished.valid(q) || finished.ExportBytes != measured.bytes || finished.ExportHash != hex.EncodeToString(measured.digest.Sum(nil)) || phase.Err() != nil {
		return nil, r.fail("guest-finished")
	}
	good = true
	return materialized, nil
}

// launchInputs returns the eight boot roles: sealed pinned assets, a fresh KVM
// handle and the host-prepared read-only QEMU runtime root.
func (r *gitVMRun) launchInputs(ctx context.Context) (_ []*os.File, result error) {
	assets, err := r.assets.duplicate(ctx, r.source)
	if err != nil {
		return nil, errGitVMOwner
	}
	defer closeVMFiles(assets)
	runtime, err := prepareNativeRuntime(r.directory, io.NewSectionReader(assets[4], 0, r.source.Manifest.QEMURuntime.Bytes), r.source.Manifest.QEMU, io.NewSectionReader(assets[0], 0, r.source.Manifest.QEMU.Bytes+1))
	if err != nil {
		return nil, errGitVMOwner
	}
	r.mu.Lock()
	r.runtime = runtime
	r.mu.Unlock()
	root, err := runtime.directoryFD()
	if err != nil {
		return nil, errGitVMOwner
	}
	kvm, err := openVMKVM()
	if err != nil {
		root.Close()
		return nil, errGitVMOwner
	}
	files := make([]*os.File, 0, gitVMStubBootFDs)
	for _, file := range []*os.File{assets[0], assets[1], assets[2]} {
		duplicate, err := duplicateGitControlFile(file)
		if err != nil {
			closeVMFiles(append(files, kvm, root))
			return nil, errGitVMOwner
		}
		files = append(files, duplicate)
	}
	files = append(files, kvm)
	for _, file := range []*os.File{assets[3], assets[4], assets[5]} {
		duplicate, err := duplicateGitControlFile(file)
		if err != nil {
			closeVMFiles(append(files, root))
			return nil, errGitVMOwner
		}
		files = append(files, duplicate)
	}
	return append(files, root), nil
}

// stop is the immutable stop capability held by the credential generation.
// It revokes future reads, joins the read channel and acquisition, then
// requires the owner's original-instance stop before reporting success.
func (r *gitVMRun) stop(ctx context.Context) error {
	if ctx == nil {
		return errGitVMOwner
	}
	select {
	case <-ctx.Done():
		return errGitVMOwner
	case <-r.stopOperation:
	}
	defer func() { r.stopOperation <- struct{}{} }()
	r.mu.Lock()
	// Revocation under the lock means an acquisition either already exposed
	// acquireDone here or will refuse to start.
	r.revoked = true
	cancel, acquireDone, channels, already := r.cancel, r.acquireDone, r.channels, r.stopped
	r.mu.Unlock()
	if already {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	if channels != nil {
		channels.Close()
	}
	if acquireDone != nil {
		select {
		case <-ctx.Done():
			return errGitVMOwner
		case <-acquireDone:
		}
	}
	// Only after acquisition has joined are its channels, read server and
	// attempt final; an earlier snapshot could miss a just-written record.
	r.mu.Lock()
	channels, readDone, attempt := r.channels, r.readDone, r.request.Source.Binding.Attempt
	r.mu.Unlock()
	if channels != nil {
		channels.Close()
	}
	if readDone != nil {
		select {
		case <-ctx.Done():
			return errGitVMOwner
		case <-readDone:
		}
	}
	r.mu.Lock()
	// A read server that started must have returned its stopped audit.
	readStopped := readDone == nil || r.readAudit.Stopped
	r.mu.Unlock()
	if !readStopped || ctx.Err() != nil {
		return errGitVMOwner
	}
	// The durable journal, not a local flag, decides whether owner stop is
	// required: prepare can persist its record and still report failure.
	if attempt != "" {
		owned, err := r.owner.has(ctx, attempt)
		if err != nil || owned && r.owner.stop(ctx, attempt) != nil || ctx.Err() != nil {
			return errGitVMOwner
		}
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	return nil
}

// close removes the runtime root and listener only after a proved stop. An
// uncertain unit keeps its runtime bytes and channels owned.
func (r *gitVMRun) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	// A failed cleanup stays failed: its residue is not retried away silently.
	if !r.stopped || r.closeFailed {
		return errGitVMOwner
	}
	failed := false
	if r.listener != nil {
		r.listener.Close()
		r.listener = nil
	}
	if r.channels != nil {
		r.channels.Close()
		r.channels = nil
	}
	if r.runtime != nil {
		failed = r.runtime.Close() != nil
		r.runtime = nil
	}
	if err := os.Remove(filepath.Join(r.directory, "stub.sock")); err != nil && !os.IsNotExist(err) {
		failed = true
	}
	if failed {
		r.closeFailed = true
		return errGitVMOwner
	}
	r.closed = true
	return nil
}
