//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type gitBootstrapHello struct {
	Version      int
	Binding      gitMaterializationBinding
	Identity     GateIdentity
	ManifestHash string
}

func gitBootstrapImageHash(ctx context.Context, image *os.File, size int64) (string, error) {
	if size <= 0 || size > 64<<20 {
		return "", errGitUnit
	}
	reader := io.NewSectionReader(image, 0, size+1)
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var count int64
	for {
		if ctx.Err() != nil {
			return "", errGitUnit
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			count += int64(n)
			hash.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errGitUnit
		}
	}
	if count != size {
		return "", errGitUnit
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type gitBootstrapAssets struct {
	packet   gitUnitGrantPacket
	manifest gitGrantManifest
	files    []*os.File
}

func (a *gitBootstrapAssets) Close() error {
	if a == nil {
		return nil
	}
	err := closeVMFiles(a.files)
	a.files = nil
	return err
}

// The root authenticates using the original supervisor pidfd inherited at
// unit birth, never by reopening argv's diagnostic numeric PID. It owns both
// initial files on all outcomes and returns ownership of the eight grant FDs.
// No Git parser or child starts here. Concrete factory admission still owns
// manager/cgroup/runtime/dependency qualification before either grant stage.
func receiveGitBootstrap(ctx context.Context, endpoint string, binding gitMaterializationBinding, invocation string, diagnosticPID int, image, originalSupervisor *os.File) (_ *gitBootstrapAssets, result error) {
	defer closeVMFiles([]*os.File{image, originalSupervisor})
	if ctx == nil || ctx.Err() != nil || !binding.valid() || !gitQuarantineHex(invocation, 32) || diagnosticPID <= 0 || image == nil || originalSupervisor == nil ||
		!filepath.IsAbs(endpoint) || filepath.Clean(endpoint) != endpoint || filepath.Base(endpoint) != "bootstrap.sock" || len(endpoint) > 107 {
		return nil, errGitUnit
	}
	unix.CloseOnExec(int(image.Fd()))
	unix.CloseOnExec(int(originalSupervisor.Fd()))
	want, err := gitLivePIDFDIdentity(originalSupervisor)
	if err != nil {
		return nil, errGitUnit
	}
	own, err := processGateIdentity(os.Getpid(), invocation)
	if err != nil {
		return nil, errGitUnit
	}
	var dialer net.Dialer
	connected, err := dialer.DialContext(ctx, "unixpacket", endpoint)
	if err != nil {
		return nil, errGitUnit
	}
	defer connected.Close()
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		return nil, errGitUnit
	}
	peer, err := authenticateGitSupervisor(ctx, conn, originalSupervisor)
	if err != nil {
		return nil, errGitUnit
	}
	defer peer.Close()
	// Before bwrap, root and supervisor share the host PID view. The numeric
	// value merely checks the fixed argv; the original pidfd proved the peer.
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, errGitUnit
	}
	var credentials *unix.Ucred
	var queryErr error
	if raw.Control(func(fd uintptr) {
		credentials, queryErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || queryErr != nil || credentials == nil || int(credentials.Pid) != diagnosticPID {
		return nil, errGitUnit
	}
	hello, _ := json.Marshal(gitBootstrapHello{1, binding, own, binding.RuntimeHash})
	if writeGitControlPacket(ctx, conn, hello, nil) != nil {
		return nil, errGitUnit
	}
	packet, files, err := readGitUnitGrant(ctx, conn, binding, gitBootstrapGrant)
	if err != nil {
		return nil, errGitUnit
	}
	defer func() {
		if result != nil {
			closeVMFiles(files)
		}
	}()
	if packet.Recipient != own {
		return nil, errGitUnit
	}
	supervisor, err := processGateIdentity(diagnosticPID, invocation)
	if err != nil {
		return nil, errGitUnit
	}
	// Read the sealed manifest's worker pin only after authenticating its
	// sender. Full canonical/hash/role checks follow in the shared verifier.
	info, err := image.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<20 {
		return nil, errGitUnit
	}
	// The granted worker must be the very same sealed inode used at birth.
	granted, err := files[3].Stat()
	running, runErr := os.Stat("/proc/self/exe")
	if err != nil || runErr != nil || !os.SameFile(info, granted) || !os.SameFile(info, running) {
		return nil, errGitUnit
	}
	// verifyGitBootstrapGrant requires the pinned worker hash as an argument;
	// derive it from the retained, bounded executable, not a claimed packet.
	workerHash, err := gitBootstrapImageHash(ctx, image, info.Size())
	if err != nil {
		return nil, errGitUnit
	}
	manifest, err := verifyGitBootstrapGrant(ctx, binding, files, workerHash, supervisor)
	if err != nil || verifyGrantedVMAsset(ctx, image, manifest.Worker, 0) != nil || verifyRunningStub(os.Getpid(), workerHash) != nil {
		return nil, errGitUnit
	}
	actual, err := gitLivePIDFDIdentity(files[7])
	after, afterErr := gitLivePIDFDIdentity(originalSupervisor)
	if err != nil || afterErr != nil || actual != want || after != want || ctx.Err() != nil {
		return nil, errGitUnit
	}
	return &gitBootstrapAssets{packet, manifest, files}, nil
}
