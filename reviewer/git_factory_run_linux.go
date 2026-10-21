//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Private fixed composition only. Neither a model nor an exported launcher can
// replace these checks with an Admit callback or supply qualification flags.
type gitFactoryRun struct {
	mu                              sync.Mutex
	unit                            *gitUnitOwner
	archive                         *gitExportArchive
	binding                         gitMaterializationBinding
	manifest                        gitGrantManifest
	directory                       string
	bootstrap, worker               *net.UnixListener
	files                           []*os.File
	exportRead                      *os.File
	boundary                        *gitProcessBoundary
	workerIdentity                  GateIdentity
	generation                      *githubReaderGeneration
	spent, revoked, stopped, closed bool
	cancel                          context.CancelFunc
	acquireDone, brokerDone         chan struct{}
	brokerAudit                     GitReadBrokerAudit
	stopOperation                   chan struct{}
}

func gitFactoryRunPaths(directory string) bool {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || strings.ContainsRune(directory, 0) {
		return false
	}
	for _, name := range []string{"bootstrap.sock", "broker/worker.sock", "broker/git.sock"} {
		if len(filepath.Join(directory, name)) > 107 {
			return false
		}
	}
	return true
}

func (r *gitFactoryRun) retainGeneration(g *githubReaderGeneration) {
	r.mu.Lock()
	r.generation = g
	r.mu.Unlock()
}

func retainGitFactoryManifest(binding gitMaterializationBinding, m gitGrantManifest) (*os.File, error) {
	if !m.valid(binding) {
		return nil, errGitUnit
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > gitGrantManifestBytes {
		return nil, errGitUnit
	}
	fd, err := unix.MemfdCreate("sfl-git-factory-manifest", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_NOEXEC_SEAL)
	if err != nil {
		return nil, errGitUnit
	}
	file := os.NewFile(uintptr(fd), "sfl-git-factory-manifest")
	good := false
	defer func() {
		if !good {
			file.Close()
		}
	}()
	if n, err := file.Write(raw); err != nil || n != len(raw) || file.Chmod(0400) != nil {
		return nil, errGitUnit
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, requiredStubSeals); err != nil {
		return nil, errGitUnit
	}
	if _, err := file.Seek(0, 0); err != nil {
		return nil, errGitUnit
	}
	good = true
	return file, nil
}

// directory is an existing empty private attempt directory owned by the outer
// factory's durable registry. Runtime/helper handles are duplicated; the outer
// factory must retain their protected backing until this run is physically
// stopped. Construction never starts a unit or dispatches GitHub HTTP.
func newGitFactoryRun(ctx context.Context, unit *gitUnitOwner, archive *gitExportArchive, binding gitMaterializationBinding, m gitGrantManifest, runtime, helper *os.File, directory string) (_ *gitFactoryRun, result error) {
	if ctx == nil || ctx.Err() != nil || unit == nil || archive == nil || runtime == nil || helper == nil || !m.valid(binding) || !gitFactoryRunPaths(directory) || privateDirectory(directory) != nil {
		return nil, errGitUnit
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errGitUnit
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		return nil, errGitUnit
	}
	if _, err := verifyGitRuntimeDirectory(ctx, runtime, m.Runtime); err != nil || verifyNativeBootstrap(ctx, helper, m.NamespaceHelper) != nil {
		return nil, errGitUnit
	}
	r := &gitFactoryRun{unit: unit, archive: archive, binding: binding, manifest: m, directory: directory, files: make([]*os.File, 8), stopOperation: make(chan struct{}, 1)}
	r.stopOperation <- struct{}{}
	defer func() {
		if result != nil {
			r.disposeFiles()
		}
	}()
	r.files[0], err = retainGitFactoryManifest(binding, m)
	if err != nil {
		return nil, errGitUnit
	}
	for _, item := range []struct {
		index int
		file  *os.File
	}{{1, runtime}, {2, helper}} {
		r.files[item.index], err = duplicateGitControlFile(item.file)
		if err != nil {
			return nil, errGitUnit
		}
	}
	if !unit.enter(ctx) {
		return nil, errGitUnit
	}
	if !unit.closed && unit.image != nil && unit.image.file != nil && unit.stubHash == m.Worker.SHA256 {
		r.files[3], err = duplicateGitControlFile(unit.image.file)
		if err == nil {
			r.files[7], err = duplicateGitControlFile(unit.supervisor)
		}
	} else {
		err = errGitUnit
	}
	unit.leave()
	if err != nil || verifyGrantedVMAsset(ctx, r.files[3], m.Worker, 0) != nil {
		return nil, errGitUnit
	}
	r.files[4], err = retainGitPayloadSeccomp()
	if err != nil {
		return nil, errGitUnit
	}
	if os.Mkdir(filepath.Join(directory, "broker"), 0700) != nil {
		return nil, errGitUnit
	}
	r.files[5], err = os.Open(filepath.Join(directory, "broker"))
	if err != nil {
		return nil, errGitUnit
	}
	r.exportRead, r.files[6], err = os.Pipe()
	if err != nil {
		return nil, errGitUnit
	}
	r.bootstrap, err = gitFactoryListen(filepath.Join(directory, "bootstrap.sock"))
	if err != nil {
		return nil, errGitUnit
	}
	if unit.prepare(ctx, binding) != nil {
		return nil, errGitUnit
	}
	return r, nil
}

func gitFactoryListen(path string) (*net.UnixListener, error) {
	if len(path) > 107 {
		return nil, errGitUnit
	}
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, errGitUnit
	}
	listener.SetUnlinkOnClose(false)
	if os.Chmod(path, 0600) != nil {
		listener.Close()
		return nil, errGitUnit
	}
	return listener, nil
}

func gitFactoryAccept(ctx context.Context, listener *net.UnixListener) (*net.UnixConn, error) {
	if ctx.Err() != nil || listener == nil {
		return nil, errGitUnit
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); listener.Close() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	conn, err := listener.AcceptUnix()
	if err != nil {
		return nil, errGitUnit
	}
	if ctx.Err() != nil {
		conn.Close()
		return nil, errGitUnit
	}
	return conn, nil
}

func (r *gitFactoryRun) admit(ctx context.Context, peer GitReadPeer) error {
	r.mu.Lock()
	boundary, worker, revoked := r.boundary, r.workerIdentity, r.revoked
	r.mu.Unlock()
	if revoked || boundary == nil || boundary.checkHelper(ctx, peer) != nil || r.unit.checkFactoryReadPeer(ctx, r.binding, worker, peer) != nil {
		return ErrGitReadBroker
	}
	return nil
}

// acquire stages plain output only. Returning successfully is not publication
// or stop authority. The composing owner must invoke stop and release its
// generation lease before any evidence or ObjectStore leaves the factory.
func (r *gitFactoryRun) acquire(ctx context.Context, source *gitGenerationReader) (*gitMaterialization, error) {
	if ctx == nil || ctx.Err() != nil || source == nil || source.generation == nil {
		return nil, errGitUnit
	}
	r.mu.Lock()
	if r.spent || r.revoked || r.closed {
		r.mu.Unlock()
		return nil, errGitUnit
	}
	r.spent = true
	phase, cancel := context.WithTimeout(ctx, 2*time.Minute)
	r.cancel = cancel
	r.acquireDone = make(chan struct{})
	r.brokerDone = make(chan struct{})
	done, brokerDone := r.acquireDone, r.brokerDone
	r.mu.Unlock()
	defer close(done)
	defer cancel()
	ready := make(chan error, 1)
	go func() {
		audit, _ := ServeGitReadSocket(phase, GitReadBrokerOptions{Directory: filepath.Join(r.directory, "broker"), Source: source, Admit: r.admit}, func(GitReadEndpoint) error {
			listener, err := gitFactoryListen(filepath.Join(r.directory, "broker", "worker.sock"))
			r.mu.Lock()
			r.worker = listener
			r.mu.Unlock()
			ready <- err
			return err
		})
		r.mu.Lock()
		r.brokerAudit = audit
		r.mu.Unlock()
		close(brokerDone)
	}()
	select {
	case err := <-ready:
		if err != nil {
			return nil, errGitUnit
		}
	case <-brokerDone:
		return nil, errGitUnit
	case <-phase.Done():
		return nil, errGitUnit
	}
	if r.unit.start(phase, r.binding, filepath.Join(r.directory, "bootstrap.sock")) != nil {
		return nil, errGitUnit
	}
	conn, err := gitFactoryAccept(phase, r.bootstrap)
	if err != nil {
		return nil, errGitUnit
	}
	if err = r.bootstrapGrant(phase, conn); err != nil {
		conn.Close()
		return nil, errGitUnit
	}
	conn.Close()
	// The authenticated root owns its received duplicate. Keeping our local
	// writer would prevent exact export EOF after the root exits.
	if r.files[6].Close() != nil {
		return nil, errGitUnit
	}
	r.files[6] = nil
	r.mu.Lock()
	listener := r.worker
	r.mu.Unlock()
	worker, err := gitFactoryAccept(phase, listener)
	if err != nil {
		return nil, errGitUnit
	}
	defer worker.Close()
	if r.workerGrant(phase, worker) != nil {
		return nil, errGitUnit
	}
	return r.archive.stage(phase, r.binding, r.exportRead)
}

func (r *gitFactoryRun) bootstrapGrant(ctx context.Context, conn *net.UnixConn) error {
	raw, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil {
		return errGitUnit
	}
	hello, err := DecodeStrict[gitBootstrapHello](raw, gitControlPacketBytes)
	canonical, marshalErr := json.Marshal(hello)
	if err != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || hello.Version != 1 || hello.Binding != r.binding || hello.ManifestHash != r.binding.RuntimeHash {
		return errGitUnit
	}
	peer, err := gitBrokerPeer(conn)
	if err != nil {
		return errGitUnit
	}
	defer peer.kernel.Close()
	identity, fd, err := retainGitBrokerPeer(peer, hello.Identity.InvocationID)
	if err != nil {
		return errGitUnit
	}
	defer unix.Close(fd)
	if identity != hello.Identity {
		return errGitUnit
	}
	return r.unit.grant(ctx, r.binding, gitBootstrapGrant, conn, r.files)
}

func (r *gitFactoryRun) workerGrant(ctx context.Context, conn *net.UnixConn) error {
	raw, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil {
		return errGitUnit
	}
	ready, err := DecodeStrict[gitWorkerReady](raw, gitControlPacketBytes)
	canonical, marshalErr := json.Marshal(ready)
	if err != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || ready != (gitWorkerReady{1, r.binding, r.binding.RuntimeHash}) {
		return errGitUnit
	}
	if !r.unit.enter(ctx) {
		return errGitUnit
	}
	record, err := r.unit.factoryUnit(ctx, r.binding)
	r.unit.leave()
	if err != nil {
		return errGitUnit
	}
	peer, err := gitBrokerPeer(conn)
	if err != nil {
		return errGitUnit
	}
	defer peer.kernel.Close()
	identity, fd, err := retainGitBrokerPeer(peer, record.Identity.InvocationID)
	if err != nil {
		return errGitUnit
	}
	defer unix.Close(fd)
	if r.unit.checkFactoryLaunch(ctx, r.binding, identity, fd, r.files[2], r.manifest.NamespaceHelper) != nil {
		return errGitUnit
	}
	boundary, err := retainGitProcessBoundary(ctx, identity, fd, r.files[1], r.files[3], r.manifest.Worker, r.manifest.Runtime)
	if err != nil {
		return errGitUnit
	}
	r.mu.Lock()
	r.boundary = boundary
	r.workerIdentity = identity
	r.mu.Unlock()
	if checkGitMountBoundary(ctx, boundary, r.files[5], r.binding) != nil || checkGitDescriptorBoundary(ctx, boundary, r.exportRead, r.files[7], conn) != nil {
		return errGitUnit
	}
	return r.unit.grant(ctx, r.binding, gitWorkerGrant, conn, nil)
}

// This is the immutable stop capability held by the credential generation.
// Cancellation first revokes future work, then joins broker callbacks and the
// acquisition method. Only then can physical whole-unit stop release the lease.
func (r *gitFactoryRun) stop(ctx context.Context) error {
	if ctx == nil {
		return errGitUnit
	}
	select {
	case <-ctx.Done():
		return errGitUnit
	case <-r.stopOperation:
	}
	defer func() { r.stopOperation <- struct{}{} }()
	r.mu.Lock()
	r.revoked = true
	cancel, acquireDone, brokerDone := r.cancel, r.acquireDone, r.brokerDone
	already := r.stopped
	r.mu.Unlock()
	if already {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	for _, done := range []chan struct{}{brokerDone, acquireDone} {
		if done == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return errGitUnit
		case <-done:
		}
	}
	r.mu.Lock()
	// ServeGitReadSocket can refuse setup before its accept loop exists. Its
	// returned zero-dispatch audit plus the joined sole serve goroutine proves
	// there was no broker work to retain; a missing return never does.
	brokerStopped := brokerDone == nil || r.brokerAudit.Version == 1 && (r.brokerAudit.Stopped || r.brokerAudit.Connections == 0 && r.brokerAudit.Requests == 0)
	r.mu.Unlock()
	if !brokerStopped || r.unit.confirmFactoryStop(ctx, r.binding) != nil || ctx.Err() != nil {
		return errGitUnit
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	return nil
}

// Caller must hold lifecycle ownership. An uncertain unit or unreturned broker
// never loses its runtime/control descriptors just because cleanup was asked.
func (r *gitFactoryRun) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if !r.stopped {
		return errGitUnit
	}
	r.closed = true
	return r.disposeFiles()
}
func (r *gitFactoryRun) disposeFiles() error {
	failed := false
	if r.bootstrap != nil {
		r.bootstrap.Close()
		r.bootstrap = nil
	}
	if r.worker != nil {
		r.worker.Close()
		r.worker = nil
	}
	if r.boundary != nil {
		if r.boundary.Close() != nil {
			failed = true
		}
		r.boundary = nil
	}
	if closeVMFiles(r.files) != nil {
		failed = true
	}
	r.files = nil
	if r.exportRead != nil {
		r.exportRead.Close()
		r.exportRead = nil
	}
	if failed {
		return errGitUnit
	}
	return nil
}
