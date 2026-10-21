//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// NativeVMManifest is a distinct payload authority from the synthetic fixture.
// Its ten descriptors are QEMU, kernel, initrd, KVM, private control, immutable
// object RPC, provider HTTP, BIOS, runtime artifact and original namespace helper. It contains no executable/path/argv selector.
// The immutable initrd supplies the separately pinned privileged guest runtime.
const nativeVMGrantProtocol = 4
const nativeVMDescriptorCount = 10

type NativeVMManifest struct {
	Version            int
	Purpose            string
	Binding            AuthBinding
	SessionHash        string
	Provider           Provider
	Assets             []VMAssetPin
	BootstrapLibraries []nativeRuntimeFile
}

func (m NativeVMManifest) valid() bool {
	if m.Version != 3 || m.Purpose != "native-review" || !m.Binding.valid() || m.SessionHash != m.Binding.SessionHash || !validAuthHash(m.SessionHash) || !m.Provider.Valid() || len(m.Assets) != 6 {
		return false
	}
	for i, a := range m.Assets {
		if !validAuthHash(a.SHA256) || a.Bytes <= 0 || a.Bytes > vmAssetLimit(i) {
			return false
		}
	}
	raw, _ := json.Marshal(m)
	return nativeBootstrapPinsValid(m.BootstrapLibraries) && len(raw) <= 4096
}

func (i *VMBootImages) NativeManifest(s Session) (NativeVMManifest, error) {
	if i == nil || s.Validate() != nil {
		return NativeVMManifest{}, ErrVMBoot
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.firmware == nil || i.runtimeArtifact == nil || i.namespaceHelper == nil || i.spec.Binding.SessionHash != Digest(s) {
		return NativeVMManifest{}, ErrVMBoot
	}
	m := NativeVMManifest{Version: 3, Purpose: "native-review", Binding: i.spec.Binding, SessionHash: Digest(s), Provider: s.Identity.Provider}
	for _, a := range []VMBootAsset{i.spec.QEMU, i.spec.Kernel, i.spec.Initrd, i.firmwareAsset, i.runtimeAsset, i.namespaceHelperAsset} {
		m.Assets = append(m.Assets, VMAssetPin{a.SHA256, a.Bytes})
	}
	m.BootstrapLibraries = append([]nativeRuntimeFile{}, i.bootstrapLibraries...)
	if !m.valid() {
		return NativeVMManifest{}, ErrVMBoot
	}
	return m, nil
}

// nativeVMChannels owns private socketpairs. The host endpoints support context
// deadlines. Only the opposite endpoints enter QEMU, never a provider process on
// the host. A channel name is fixed by descriptor position, not guest input.
type nativeVMChannels struct {
	mu         sync.Mutex
	host       [3]*net.UnixConn
	guest      [3]*os.File
	closed     bool
	guestSpent bool
}

func openNativeVMChannels() (*nativeVMChannels, error) {
	c := &nativeVMChannels{}
	for index := range c.host {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			c.Close()
			return nil, ErrVMBoot
		}
		parent := os.NewFile(uintptr(pair[0]), "sfl-native-host")
		c.guest[index] = os.NewFile(uintptr(pair[1]), "sfl-native-guest")
		conn, err := net.FileConn(parent)
		parent.Close()
		if err != nil {
			c.Close()
			return nil, ErrVMBoot
		}
		var ok bool
		c.host[index], ok = conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			c.Close()
			return nil, ErrVMBoot
		}
	}
	return c, nil
}

// Interrupt blocking control reads/writes on caller cancellation, even when the
// original phase deadline is still far away. Cleanup then stops the whole unit
// and preserves any uncertain auth export; it does not wait for guest progress.
func (c *nativeVMChannels) interruptControlOnCancel(ctx context.Context) func() bool {
	// Set the phase deadline before registering cancellation so a later write
	// cannot replace an already-fired cancellation with a future deadline.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.host[0].SetDeadline(deadline)
	}
	return context.AfterFunc(ctx, func() { _ = c.host[0].SetDeadline(time.Now()) })
}

func (c *nativeVMChannels) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		for _, conn := range c.host {
			if conn != nil {
				conn.Close()
			}
		}
		for _, file := range c.guest {
			if file != nil {
				file.Close()
			}
		}
	}
	return nil
}

func (c *nativeVMChannels) duplicateGuest() ([]*os.File, error) {
	if c == nil {
		return nil, ErrVMBoot
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.guestSpent {
		return nil, ErrVMBoot
	}
	var files []*os.File
	for _, file := range c.guest {
		if file == nil {
			closeVMFiles(files)
			return nil, ErrVMBoot
		}
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			closeVMFiles(files)
			return nil, ErrVMBoot
		}
		files = append(files, os.NewFile(uintptr(fd), "sfl-native-channel"))
	}
	c.guestSpent = true
	for _, file := range c.guest {
		file.Close()
	}
	return files, nil
}

type nativeVMGrant struct {
	Version  int
	Binding  AuthBinding
	Identity GateIdentity
	Manifest NativeVMManifest
}

func (g *ContainmentGate) grantNativeVM(ctx context.Context, identity GateIdentity, m NativeVMManifest, images *VMBootImages, channels *nativeVMChannels) error {
	if !m.valid() || m.Binding != g.binding || images == nil {
		return ErrVMBoot
	}
	files, err := images.duplicate()
	if err != nil {
		return err
	}
	defer func() { closeVMFiles(files) }()
	// Verify the retained bytes against the granted manifest before authorizing
	// the stub. A caller cannot pair a native manifest with different boot pins.
	for index, file := range files {
		if verifyGrantedVMAsset(ctx, file, m.Assets[index], index) != nil {
			return ErrVMBoot
		}
	}
	firmware, err := images.duplicateFirmware()
	if err != nil {
		return err
	}
	defer func() {
		if firmware != nil {
			firmware.Close()
		}
	}()
	if verifyGrantedVMAsset(ctx, firmware, m.Assets[3], 3) != nil {
		return ErrVMBoot
	}
	kvm, err := openVMKVM()
	if err != nil {
		return err
	}
	files = append(files, kvm)
	private, err := channels.duplicateGuest()
	if err != nil {
		return err
	}
	files = append(files, private...)
	files = append(files, firmware)
	firmware = nil // The grant's descriptor list now owns this retained BIOS.
	runtimeFiles, err := images.duplicateNativeRuntime(ctx)
	if err != nil {
		return err
	}
	files = append(files, runtimeFiles...)
	if verifyGrantedVMAsset(ctx, files[8], m.Assets[4], 4) != nil || verifyNativeBootstrap(ctx, files[9], m.Assets[5]) != nil {
		return ErrNativeRuntime
	}
	raw, err := json.Marshal(nativeVMGrant{nativeVMGrantProtocol, g.binding, identity, m})
	if err != nil {
		return ErrVMBoot
	}
	return g.grant(ctx, identity, Digest(m), nativeVMGrantProtocol, raw, files)
}

// Native boot is KVM only and has no NIC, host share, interactive monitor,
// arbitrary argument or fallback. Three distinct virtio ports carry private
// supervisor traffic. A separate bounded serial reader emits fixed boot stages.
func nativeVMArgs(nonce string) []string {
	args := []string{"-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off",
		"-add-fd", "fd=6,set=1,opaque=sfl-kvm", "-accel", "kvm,device=/dev/fdset/1", "-cpu", "host",
		"-m", "2048M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot",
		"-L", "/firmware", "-bios", "/proc/self/fd/10", "-kernel", "/proc/self/fd/4", "-initrd", "/proc/self/fd/5",
		"-append", "console=ttyS0 loglevel=3 initramfs_options=size=1024m,nr_inodes=4096 rdinit=/native-init panic=1 sfl-native=" + nonce, "-chardev", "socket,id=sflbootdiag,fd=13", "-serial", "chardev:sflbootdiag", "-device", "virtio-serial-pci,id=sflserial"}
	for index, name := range []string{"control", "objects", "provider"} {
		id := "sfl" + name
		args = append(args, "-chardev", "socket,id="+id+",fd="+strconv.Itoa(index+7), "-device", "virtserialport,bus=sflserial.0,chardev="+id+",name=sfl."+name)
	}
	return append(args, "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny")
}

func receiveNativeVMGrant(conn *net.UnixConn) (grant nativeVMGrant, files []*os.File, result error) {
	raw, files, result := receiveVMDescriptorFrame(conn, nativeVMDescriptorCount)
	if result != nil {
		return grant, nil, result
	}
	defer func() {
		if result != nil {
			closeVMFiles(files)
			files = nil
		}
	}()
	grant, result = DecodeStrict[nativeVMGrant](raw, 4096)
	if result != nil || grant.Version != nativeVMGrantProtocol || !grant.Manifest.valid() || grant.Binding != grant.Manifest.Binding {
		return grant, files, ErrContainmentGate
	}
	return grant, files, nil
}

func nativeVMStream(file *os.File) error {
	if file == nil {
		return ErrVMBoot
	}
	control, err := file.SyscallConn()
	if err != nil {
		return ErrVMBoot
	}
	valid := false
	err = control.Control(func(fd uintptr) {
		typ, e := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		address, peerErr := unix.Getpeername(int(fd))
		_, local := address.(*unix.SockaddrUnix)
		valid = e == nil && peerErr == nil && local && typ == unix.SOCK_STREAM
	})
	if err != nil || !valid {
		return ErrVMBoot
	}
	return nil
}

func nativeVMStreams(files []*os.File) error {
	if len(files) != 3 {
		return ErrVMBoot
	}
	seen := map[[2]uint64]bool{}
	for _, file := range files {
		if nativeVMStream(file) != nil {
			return ErrVMBoot
		}
		var st unix.Stat_t
		if unix.Fstat(int(file.Fd()), &st) != nil {
			return ErrVMBoot
		}
		key := [2]uint64{uint64(st.Dev), st.Ino}
		if seen[key] {
			return ErrVMBoot
		}
		seen[key] = true
	}
	return nil
}

func receiveNativeVM(ctx context.Context, endpoint string, binding AuthBinding, invocation, hash string, supervisorPID int) (grant nativeVMGrant, files []*os.File, result error) {
	if !binding.valid() || !validAuthHash(hash) || supervisorPID <= 0 || !sourcePath(endpoint) || filepath.Base(endpoint) != "gate.sock" || filepath.Base(filepath.Dir(endpoint)) != binding.LaunchID {
		return grant, nil, ErrVMBoot
	}
	own, err := processGateIdentity(os.Getpid(), invocation)
	if err != nil {
		return grant, nil, ErrVMBoot
	}
	connected, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	if err != nil {
		return grant, nil, ErrVMBoot
	}
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		connected.Close()
		return grant, nil, ErrVMBoot
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(gateDeadline(ctx))
	pid, uid, pidfd, err := gatePeer(conn)
	if pidfd >= 0 {
		defer unix.Close(pidfd)
	}
	if err != nil || pid != supervisorPID || uid != uint32(os.Geteuid()) {
		return grant, nil, ErrVMBoot
	}
	raw, _ := json.Marshal(gateHello{nativeVMGrantProtocol, binding, invocation, hash})
	if n, err := conn.Write(raw); err != nil || n != len(raw) {
		return grant, nil, ErrVMBoot
	}
	if conn.CloseWrite() != nil {
		return grant, nil, ErrVMBoot
	}
	grant, files, err = receiveNativeVMGrant(conn)
	if err != nil {
		return grant, nil, err
	}
	defer func() {
		if result != nil {
			closeVMFiles(files)
			files = nil
		}
	}()
	if grant.Binding != binding || grant.Identity != own || Digest(grant.Manifest) != hash || unix.PidfdSendSignal(pidfd, 0, nil, 0) != nil {
		return grant, files, ErrVMBoot
	}
	for index, file := range files[:3] {
		if verifyGrantedVMAsset(ctx, file, grant.Manifest.Assets[index], index) != nil {
			return grant, files, ErrVMBoot
		}
	}
	if verifyGrantedKVM(files[3]) != nil {
		return grant, files, ErrVMBoot
	}
	if nativeVMStreams(files[4:7]) != nil || verifyGrantedVMAsset(ctx, files[7], grant.Manifest.Assets[3], 3) != nil {
		return grant, files, ErrVMBoot
	}
	if verifyGrantedVMAsset(ctx, files[8], grant.Manifest.Assets[4], 4) != nil || verifyNativeBootstrap(ctx, files[9], grant.Manifest.Assets[5]) != nil || verifyNativeBootstrapLibraries(ctx, grant.Manifest.BootstrapLibraries) != nil {
		return grant, files, ErrNativeRuntime
	}
	return grant, files, nil
}

// NativeVMStub is the new fixed native payload. It remains inert until its
// owned-unit and authenticated version-4 descriptor checks succeed. This is not
// a provider configuration, activation command or an auth stop attestation.
func NativeVMStub(ctx context.Context, endpoint string, binding AuthBinding, invocation, hash string, supervisorPID int) (result error) {
	if vmGrantedUnitWithKernel(binding, invocation, nativeContainmentKernel{}) != nil {
		return ErrVMBoot
	}
	ctx, cancel := context.WithTimeout(ctx, 7*time.Minute)
	defer cancel()
	grant, files, err := receiveNativeVM(ctx, endpoint, binding, invocation, hash, supervisorPID)
	if err != nil {
		return err
	}
	defer closeVMFiles(files)
	serial, serialChild, err := nativeBootDiagnosticChannel()
	if err != nil {
		return err
	}
	defer serial.Close()
	defer serialChild.Close()
	control, err := net.FileConn(files[4])
	if err != nil {
		return ErrVMBoot
	}
	defer control.Close()
	prepared, err := prepareNativeRuntimeOwned(filepath.Dir(endpoint), io.NewSectionReader(files[8], 0, grant.Manifest.Assets[4].Bytes), grant.Manifest.Assets[0], io.NewSectionReader(files[0], 0, grant.Manifest.Assets[0].Bytes+1), func(root *nativeRuntimeRoot) error {
		return root.recordOwnership(binding, hash)
	})
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, prepared.Close()) }()
	runtimeRoot, err := prepared.directoryFD()
	if err != nil {
		return err
	}
	defer runtimeRoot.Close()
	if writeVMRecord(control, nativeVMProgress{1, "guest-stage", "runtime-prepared"}, 4096) != nil {
		return ErrNativeRuntime
	}
	diagnostics := &nativeBootDiagnostics{}
	diagnosticDone := make(chan struct{})
	go func() { readNativeBootDiagnostics(serial, diagnostics); close(diagnosticDone) }()
	defer func() { serial.Close(); <-diagnosticDone }()
	command := exec.CommandContext(ctx, "/proc/self/fd/12", nativeRuntimeNamespaceArgs(nativeVMArgs(grant.Binding.LaunchID))...)
	command.Args[0] = "sfl-native-bootstrap"
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	command.Dir = "/"
	command.ExtraFiles = append(append([]*os.File{}, files...), serialChild, runtimeRoot)
	command.Stdout = io.Discard
	command.Stderr = diagnostics
	command.WaitDelay = 2 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if command.Start() != nil {
		return ErrVMBoot
	}
	closeVMFiles(files) // Only QEMU and the diagnostic control duplicate remain.
	serialChild.Close()
	runtimeRoot.Close()
	waitErr := command.Wait()
	select {
	case <-diagnosticDone:
	case <-time.After(time.Second):
		serial.Close()
		<-diagnosticDone
	}
	control.SetWriteDeadline(time.Now().Add(time.Second))
	for _, stage := range diagnostics.stages(waitErr != nil) {
		if writeVMRecord(control, nativeVMProgress{1, "guest-stage", stage}, 4096) != nil {
			break
		}
	}
	if waitErr != nil {
		return ErrVMBoot
	}
	return nil
}
