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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The source VM stub is the only process the source unit starts. It holds no
// provider, auth or GitHub credential. It receives one authenticated grant of
// pinned boot inputs and three private channel endpoints, then runs QEMU
// through the pinned namespace helper. It never selects a path or argument.
const gitVMStubProtocol = 1
const gitVMStubPurpose = "source-vm-stub-grant"
const gitVMRequestArgBytes = 16 << 10

// Boot packet roles: QEMU, kernel, initrd, KVM, BIOS, QEMU runtime artifact,
// original namespace helper and the host-prepared read-only runtime root.
const gitVMStubBootFDs = 8

// Channel packet roles: guest control, Git read and export endpoints.
const gitVMStubChannelFDs = 3

type gitVMStubHello struct {
	Version     int
	RequestHash string
	Identity    GateIdentity
}

type gitVMStubGrant struct {
	Version     int
	Purpose     string
	RequestHash string
	Recipient   GateIdentity
}

func (g gitVMStubGrant) valid(hash string) bool {
	return g.Version == gitVMStubProtocol && g.Purpose == gitVMStubPurpose && validAuthHash(hash) && g.RequestHash == hash && g.Recipient.PID > 0 && g.Recipient.StartTicks > 0 && g.Recipient.BootID != "" && gitQuarantineHex(g.Recipient.InvocationID, 32)
}

// The second packet carries only the channel endpoints and binds them to the
// exact first grant on the same authenticated connection.
type gitVMStubChannels struct {
	Version   int
	GrantHash string
}

// gitVMSourceQEMUArgs is the measured source guest launch. KVM only, no NIC,
// host share, monitor or fallback. Three virtio ports carry the private source
// control, Git read and export channels. Descriptor numbers are fixed by role:
// QEMU 3, kernel 4, initrd 5, KVM 6, control 7, read 8, export 9, BIOS 10,
// runtime artifact 11, helper 12, boot diagnostics 13, runtime root 14.
func gitVMSourceQEMUArgs(q gitVMGuestRequest) []string {
	args := []string{"-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off", "-add-fd", "fd=6,set=1,opaque=sfl-kvm", "-accel", "kvm,device=/dev/fdset/1", "-cpu", "host", "-m", "1536M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot", "-L", "/firmware", "-bios", "/proc/self/fd/10", "-kernel", "/proc/self/fd/4", "-initrd", "/proc/self/fd/5", "-append", "console=ttyS0 loglevel=3 initramfs_options=size=1024m,nr_inodes=4096 rdinit=/source-init panic=1 sfl-source=" + Digest(q), "-chardev", "socket,id=sflbootdiag,fd=13", "-serial", "chardev:sflbootdiag", "-device", "virtio-serial-pci,id=sflserial"}
	for i, name := range []string{"source-control", "source-read", "source-export"} {
		id := "sfl" + strconv.Itoa(i)
		args = append(args, "-chardev", fmt.Sprintf("socket,id=%s,fd=%d", id, i+7), "-device", "virtserialport,bus=sflserial.0,chardev="+id+",name=sfl."+name)
	}
	return append(args, "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny")
}

// gitVMLaunchFiles orders granted roles into the fixed QEMU descriptor layout.
func gitVMLaunchFiles(boot, channels []*os.File, serial *os.File) []*os.File {
	return []*os.File{boot[0], boot[1], boot[2], boot[3], channels[0], channels[1], channels[2], boot[4], boot[5], boot[6], serial, boot[7]}
}

func parseGitVMStubRequest(raw, hash string) (gitVMGuestRequest, error) {
	if len(raw) == 0 || len(raw) > gitVMRequestArgBytes || !validAuthHash(hash) {
		return gitVMGuestRequest{}, errGitVMOwner
	}
	q, err := DecodeStrict[gitVMGuestRequest]([]byte(raw), gitVMRequestArgBytes)
	canonical, marshalErr := json.Marshal(q)
	if err != nil || marshalErr != nil || !bytes.Equal(canonical, []byte(raw)) || !q.valid() || Digest(q) != hash {
		return gitVMGuestRequest{}, errGitVMOwner
	}
	return q, nil
}

// The stub must run in its own source unit, with no new privileges, before it
// dials the supervisor or opens any granted input.
func gitVMOwnedUnit(q gitVMGuestRequest, invocation string, kernel containmentKernel) error {
	if invocation != os.Getenv("INVOCATION_ID") || !gitQuarantineHex(invocation, 32) {
		return errGitVMOwner
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errGitVMOwner
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "0::") {
		return errGitVMOwner
	}
	if _, err := kernel.Capture(strings.TrimPrefix(lines[0], "0::"), "sfl-alpha-"+q.Source.Binding.Attempt+".service"); err != nil {
		return errGitVMOwner
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1\n") {
		return errGitVMOwner
	}
	return nil
}

// The prepared runtime root is sealed read-only (0555) after verification.
func verifyGitVMRuntimeRoot(file *os.File) error {
	info, err := file.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0555 {
		return errGitVMOwner
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return errGitVMOwner
	}
	return nil
}

// verifyGitVMBootFiles checks every boot role against the request's pinned
// manifest. A grant cannot pair a request with different boot bytes.
func verifyGitVMBootFiles(ctx context.Context, m gitVMManifest, files []*os.File) error {
	if len(files) != gitVMStubBootFDs || !m.valid() {
		return errGitVMOwner
	}
	for _, item := range []struct {
		index, role int
		pin         VMAssetPin
	}{{0, 0, m.QEMU}, {1, 1, m.Kernel}, {2, 2, m.Initrd}, {4, 3, m.BIOS}, {5, 4, m.QEMURuntime}} {
		if verifyGrantedVMAsset(ctx, files[item.index], item.pin, item.role) != nil {
			return errGitVMOwner
		}
	}
	if verifyGrantedKVM(files[3]) != nil || verifyNativeBootstrap(ctx, files[6], m.NamespaceHelper) != nil || verifyNativeBootstrapLibraries(ctx, m.BootstrapLibraries) != nil || verifyGitVMRuntimeRoot(files[7]) != nil {
		return errGitVMOwner
	}
	return nil
}

type gitVMStubAssets struct {
	boot, channels []*os.File
}

func (a *gitVMStubAssets) Close() error {
	if a == nil {
		return nil
	}
	return closeVMFiles(append(append([]*os.File{}, a.boot...), a.channels...))
}

// receiveGitVMStub authenticates the original supervisor through the pidfd
// inherited at unit birth, never the diagnostic numeric PID. It owns both
// inherited files on all outcomes and returns ownership of the granted roles.
func receiveGitVMStub(ctx context.Context, endpoint string, q gitVMGuestRequest, invocation string, diagnosticPID int, image, originalSupervisor *os.File) (_ *gitVMStubAssets, result error) {
	defer closeVMFiles([]*os.File{image, originalSupervisor})
	if ctx == nil || ctx.Err() != nil || !q.valid() || !gitQuarantineHex(invocation, 32) || diagnosticPID <= 0 || image == nil || originalSupervisor == nil ||
		!filepath.IsAbs(endpoint) || filepath.Clean(endpoint) != endpoint || filepath.Base(endpoint) != "stub.sock" || len(endpoint) > 100 {
		return nil, errGitVMOwner
	}
	unix.CloseOnExec(int(image.Fd()))
	unix.CloseOnExec(int(originalSupervisor.Fd()))
	want, err := gitLivePIDFDIdentity(originalSupervisor)
	if err != nil {
		return nil, errGitVMOwner
	}
	own, err := processGateIdentity(os.Getpid(), invocation)
	if err != nil {
		return nil, errGitVMOwner
	}
	var dialer net.Dialer
	connected, err := dialer.DialContext(ctx, "unixpacket", endpoint)
	if err != nil {
		return nil, errGitVMOwner
	}
	defer connected.Close()
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		return nil, errGitVMOwner
	}
	peer, err := authenticateGitSupervisor(ctx, conn, originalSupervisor)
	if err != nil {
		return nil, errGitVMOwner
	}
	defer peer.Close()
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, errGitVMOwner
	}
	var credentials *unix.Ucred
	var queryErr error
	if raw.Control(func(fd uintptr) {
		credentials, queryErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || queryErr != nil || credentials == nil || int(credentials.Pid) != diagnosticPID {
		return nil, errGitVMOwner
	}
	hash := Digest(q)
	hello, _ := json.Marshal(gitVMStubHello{gitVMStubProtocol, hash, own})
	if writeGitControlPacket(ctx, conn, hello, nil) != nil {
		return nil, errGitVMOwner
	}
	assets := &gitVMStubAssets{}
	defer func() {
		if result != nil {
			assets.Close()
		}
	}()
	grantRaw, boot, err := readGitControlPacket(ctx, conn, gitVMStubBootFDs)
	assets.boot = boot
	if err != nil {
		return nil, errGitVMOwner
	}
	grant, err := DecodeStrict[gitVMStubGrant](grantRaw, gitControlPacketBytes)
	canonical, marshalErr := json.Marshal(grant)
	if err != nil || marshalErr != nil || !bytes.Equal(canonical, grantRaw) || !grant.valid(hash) || grant.Recipient != own {
		return nil, errGitVMOwner
	}
	channelRaw, channels, err := readGitControlPacket(ctx, conn, gitVMStubChannelFDs)
	assets.channels = channels
	if err != nil {
		return nil, errGitVMOwner
	}
	binding, err := DecodeStrict[gitVMStubChannels](channelRaw, gitControlPacketBytes)
	canonical, marshalErr = json.Marshal(binding)
	if err != nil || marshalErr != nil || !bytes.Equal(canonical, channelRaw) || binding != (gitVMStubChannels{gitVMStubProtocol, gitRawHash(grantRaw)}) {
		return nil, errGitVMOwner
	}
	info, err := image.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<20 {
		return nil, errGitVMOwner
	}
	stubHash, err := gitBootstrapImageHash(ctx, image, info.Size())
	if err != nil || verifyRunningStub(os.Getpid(), stubHash) != nil {
		return nil, errGitVMOwner
	}
	if verifyGitVMBootFiles(ctx, q.Source.Manifest, assets.boot) != nil || nativeVMStreams(assets.channels) != nil {
		return nil, errGitVMOwner
	}
	after, err := gitLivePIDFDIdentity(originalSupervisor)
	if err != nil || after != want || ctx.Err() != nil {
		return nil, errGitVMOwner
	}
	return assets, nil
}

// GitVMSourceStub is the fixed source unit payload: request JSON, its hash,
// the stub endpoint and the diagnostic supervisor PID are its only inputs.
// It writes nothing to disk and never touches a provider or credential.
func GitVMSourceStub(ctx context.Context, args []string, invocation string) error {
	if ctx == nil || ctx.Err() != nil || len(args) != 4 {
		return errGitVMOwner
	}
	q, err := parseGitVMStubRequest(args[1], args[2])
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(args[3])
	if err != nil || pid <= 0 || strconv.Itoa(pid) != args[3] {
		return errGitVMOwner
	}
	if gitProcessInheritedRoles(false) != nil || gitVMOwnedUnit(q, invocation, gitVMContainmentKernel{}) != nil {
		return errGitVMOwner
	}
	ctx, cancel := context.WithTimeout(ctx, 170*time.Second)
	defer cancel()
	assets, err := receiveGitVMStub(ctx, args[0], q, invocation, pid, os.NewFile(3, "image"), os.NewFile(4, "supervisor"))
	if err != nil {
		return err
	}
	defer assets.Close()
	return runGitVMQEMU(ctx, q, assets)
}

func runGitVMQEMU(ctx context.Context, q gitVMGuestRequest, assets *gitVMStubAssets) error {
	serial, serialChild, err := nativeBootDiagnosticChannel()
	if err != nil {
		return errGitVMOwner
	}
	defer serial.Close()
	defer serialChild.Close()
	// Boot diagnostics are bounded and discarded; the control channel belongs
	// to the guest protocol and the stub never writes to it.
	serialDone := make(chan struct{})
	go func() { defer close(serialDone); _, _ = io.Copy(io.Discard, io.LimitReader(serial, 1<<20)) }()
	defer func() { serial.Close(); <-serialDone }()
	command := exec.CommandContext(ctx, "/proc/self/fd/12", nativeRuntimeNamespaceArgs(gitVMSourceQEMUArgs(q))...)
	command.Args[0] = "sfl-source-vm-bootstrap"
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	command.Dir = "/"
	command.ExtraFiles = gitVMLaunchFiles(assets.boot, assets.channels, serialChild)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if command.Start() != nil {
		return errGitVMOwner
	}
	// Only QEMU keeps the granted roles; the host proves whole-unit stop.
	assets.Close()
	serialChild.Close()
	if command.Wait() != nil {
		return errGitVMOwner
	}
	return nil
}
