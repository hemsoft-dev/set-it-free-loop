//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Fixed guest role only. Host invocation and a non-PID1 namespace refuse
// before touching inherited FD numbers that could belong to Go's runtime.
func gitVMGuestWorkerMain(hash string) int {
	if os.Getpid() != 1 || os.Getuid() != gitVMGuestUID || os.Geteuid() != gitVMGuestUID ||
		os.Getgid() != gitVMGuestGID || os.Getegid() != gitVMGuestGID || !validAuthHash(hash) {
		return 2
	}
	if gitProcessInheritedRolesLimit(true, gitVMRecordBytes) != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	input := gitWorkerInput{Supervisor: os.NewFile(3, "root-supervisor"), Image: os.NewFile(4, "worker-image"), Manifest: os.NewFile(5, "source-request"), Export: os.NewFile(6, "source-export")}
	defer closeVMFiles([]*os.File{input.Supervisor, input.Image, input.Manifest, input.Export})
	info, err := input.Manifest.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > gitVMRecordBytes {
		return 2
	}
	raw, err := io.ReadAll(io.NewSectionReader(input.Manifest, 0, gitVMRecordBytes+1))
	q, decodeErr := DecodeStrict[gitVMGuestRequest](raw, gitVMRecordBytes)
	canonical, marshalErr := json.Marshal(q)
	if err != nil || decodeErr != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || !q.valid() || Digest(q) != hash ||
		verifyGrantedVMAsset(ctx, input.Manifest, VMAssetPin{gitRawHash(raw), int64(len(raw))}, 1) != nil ||
		verifyGrantedVMAsset(ctx, input.Image, q.Source.Manifest.Worker, 0) != nil || verifyRunningStub(os.Getpid(), q.Source.Manifest.Worker.SHA256) != nil {
		return 2
	}
	actual, err := os.Stat("/proc/self/exe")
	expected, expectedErr := input.Image.Stat()
	if err != nil || expectedErr != nil || !os.SameFile(actual, expected) {
		return 2
	}
	input.Image.Close()
	input.Manifest.Close()
	fd, err := gitControlFileFD(input.Export)
	if err != nil {
		return 2
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return 2
	}
	if unix.SetNonblock(duplicate, true) != nil {
		unix.Close(duplicate)
		return 2
	}
	output := os.NewFile(uintptr(duplicate), "source-export")
	defer output.Close()
	input.Export.Close()
	var dialer net.Dialer
	connected, err := dialer.DialContext(ctx, "unixpacket", "/broker/worker.sock")
	if err != nil {
		return 2
	}
	defer connected.Close()
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		return 2
	}
	join := gitBrokerAfterFunc(ctx, func() { conn.Close(); output.Close() })
	defer join()
	peer, err := authenticateGitSupervisorIDs(ctx, conn, input.Supervisor, 0, 0)
	if err != nil {
		return 2
	}
	defer peer.Close()
	input.Supervisor.Close()
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil {
		return 2
	}
	dumpable, e1 := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	nnp, e2 := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	seccomp, e3 := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	status, e4 := os.ReadFile("/proc/self/status")
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || dumpable != 0 || nnp != 1 || seccomp != 2 {
		return 2
	}
	for _, key := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		if !strings.Contains(string(status), key+":\t0000000000000000\n") {
			return 2
		}
	}
	ready, _ := json.Marshal(gitVMGuestPhase{1, gitVMGuestPurpose, hash, "worker-ready"})
	if writeGitControlPacket(ctx, conn, ready, nil) != nil {
		return 2
	}
	grant, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	p, decodeErr := DecodeStrict[gitVMGuestPhase](grant, gitVMRecordBytes)
	canonical, marshalErr = json.Marshal(p)
	if err != nil || decodeErr != nil || marshalErr != nil || !bytes.Equal(grant, canonical) || !p.valid(q, "worker-grant") || gitAuditInheritedFDs(2) != nil {
		return 2
	}
	store, err := acquireGitQuarantine(ctx, "/usr/bin/git", "/work", GitReadEndpoint{"/broker/git.sock", gitReadLocalURL}, q.Source.Binding, DefaultGitAcquisitionBounds())
	if err != nil || writeGitVMExport(ctx, output, store, q.Source) != nil || output.Close() != nil {
		return 2
	}
	return 0
}
