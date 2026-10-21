//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type gitWorkerReady struct {
	Version      int
	Binding      gitMaterializationBinding
	ManifestHash string
}

// Recipient.PID is the supervisor's host PID coordinate, while local.PID is
// a different PID in the worker namespace. They cannot be compared numerically.
// This is used only after original-supervisor authentication and strict packet
// validation. The sender separately binds the full host identity to the
// connection's original SO_PEERPIDFD in owner.grant, before its durable send.
func gitWorkerLocalIdentityMatches(recipient, local GateIdentity) bool {
	return recipient.InvocationID == local.InvocationID && recipient.StartTicks == local.StartTicks && recipient.BootID == local.BootID
}

// These are trusted bootstrap inputs, not a candidate configuration or public
// launcher. The future fixed executable supplies the namespace-local paths.
type gitWorkerInput struct {
	Binding                                   gitMaterializationBinding
	InvocationID, ControlPath, Git, Workspace string
	Endpoint                                  GitReadEndpoint
	Supervisor, Image, Manifest, Export       *os.File
}

// runGitAcquisitionWorker owns all supplied files, including on rejection. It
// authenticates the original supervisor and waits inert for its worker grant.
// This does NOT independently attest cgroups, mounts, runtime or namespaces.
// The unfinished factory must do that before calling owner.grant. Tests run
// small trusted synthetic repos in actual sealed child processes, not qualified
// production acquisition units.
func runGitAcquisitionWorker(ctx context.Context, input gitWorkerInput) error {
	defer closeVMFiles([]*os.File{input.Supervisor, input.Image, input.Manifest, input.Export})
	if ctx == nil || ctx.Err() != nil || !input.Binding.valid() || !gitQuarantineHex(input.InvocationID, 32) ||
		!filepath.IsAbs(input.ControlPath) || filepath.Clean(input.ControlPath) != input.ControlPath || filepath.Base(input.ControlPath) != "worker.sock" || len(input.ControlPath) > 107 ||
		input.Supervisor == nil || input.Image == nil || input.Manifest == nil || input.Export == nil {
		return errGitUnit
	}
	// Reject a copied/mutable worker even if its bytes happen to match. The
	// private sealed manifest is the inherited release input, not a wire claim.
	info, err := input.Manifest.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > gitGrantManifestBytes {
		return errGitUnit
	}
	raw, err := io.ReadAll(io.NewSectionReader(input.Manifest, 0, gitGrantManifestBytes+1))
	manifest, decodeErr := DecodeStrict[gitGrantManifest](raw, gitGrantManifestBytes)
	canonical, marshalErr := json.Marshal(manifest)
	if err != nil || decodeErr != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || !manifest.valid(input.Binding) ||
		verifyGrantedVMAsset(ctx, input.Manifest, VMAssetPin{input.Binding.RuntimeHash, int64(len(raw))}, 1) != nil ||
		verifyGrantedVMAsset(ctx, input.Image, manifest.Worker, 0) != nil || verifyRunningStub(os.Getpid(), manifest.Worker.SHA256) != nil {
		return errGitUnit
	}
	actual, err := os.Stat("/proc/self/exe")
	expected, expectedErr := input.Image.Stat()
	if err != nil || expectedErr != nil || !os.SameFile(actual, expected) {
		return errGitUnit
	}
	input.Image.Close()
	input.Manifest.Close()
	// A duplicate is made nonblocking BEFORE NewFile attaches Go's poller.
	// Cancellation must interrupt a full export pipe, not wait for a reader.
	fd, err := gitControlFileFD(input.Export)
	if err != nil {
		return errGitUnit
	}
	var stat unix.Stat_t
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != unix.O_WRONLY {
		return errGitUnit
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return errGitUnit
	}
	if unix.SetNonblock(duplicate, true) != nil {
		unix.Close(duplicate)
		return errGitUnit
	}
	output := os.NewFile(uintptr(duplicate), "sfl-git-export")
	defer output.Close()
	input.Export.Close()
	unix.CloseOnExec(int(input.Supervisor.Fd()))
	var dialer net.Dialer
	connected, err := dialer.DialContext(ctx, "unixpacket", input.ControlPath)
	if err != nil {
		return errGitUnit
	}
	defer connected.Close()
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		return errGitUnit
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
		output.Close()
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	peer, err := authenticateGitSupervisor(ctx, conn, input.Supervisor)
	if err != nil {
		return fmt.Errorf("worker supervisor authentication: %w", err)
	}
	defer peer.Close()
	input.Supervisor.Close()
	// CLOEXEC alone does not protect a worker's FDs against its own Git child.
	// Drop dumpability before readiness and keep this process alive throughout.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("worker dumpability: %w", err)
	}
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	nnp, nnpErr := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	seccomp, filterErr := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	status, statusErr := os.ReadFile("/proc/self/status")
	if err != nil || nnpErr != nil || filterErr != nil || statusErr != nil || dumpable != 0 || nnp != 1 || seccomp != 2 {
		return fmt.Errorf("worker state: dumpable=%d nnp=%d seccomp=%d: %w", dumpable, nnp, seccomp, errGitUnit)
	}
	for _, key := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		if !strings.Contains(string(status), key+":\t0000000000000000\n") {
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, key+":") {
					return fmt.Errorf("worker capability %s: %w", line, errGitUnit)
				}
			}
			return fmt.Errorf("missing worker capability %s: %w", key, errGitUnit)
		}
	}
	ready, _ := json.Marshal(gitWorkerReady{1, input.Binding, input.Binding.RuntimeHash})
	if err := writeGitControlPacket(ctx, conn, ready, nil); err != nil {
		return fmt.Errorf("worker readiness send: %w", err)
	}
	grant, files, err := readGitUnitGrant(ctx, conn, input.Binding, gitWorkerGrant)
	closeVMFiles(files)
	if err != nil {
		return errGitUnit
	}
	own, err := processGateIdentity(os.Getpid(), input.InvocationID)
	if err != nil || !gitWorkerLocalIdentityMatches(grant.Recipient, own) {
		return errGitUnit
	}
	// A host PID need not be visible in the worker PID namespace. The matching
	// original authenticated supervisor supplied it; host admission owns that
	// numeric identity and all physical boundary checks, never the worker.
	if gitAuditInheritedFDs(2) != nil {
		return errGitUnit
	}
	if err := acquireGitMaterialization(ctx, output, input.Git, input.Workspace, input.Endpoint, input.Binding, DefaultGitAcquisitionBounds()); err != nil {
		return err
	}
	return output.Close()
}
