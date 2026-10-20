//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
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
// Its seven descriptors are QEMU, kernel, initrd, KVM, private control, immutable
// object RPC and provider HTTP. It contains no executable/path/argv selector.
// The immutable initrd supplies the separately pinned privileged guest runtime.
type NativeVMManifest struct {
	Version     int
	Purpose     string
	Binding     AuthBinding
	SessionHash string
	Provider    Provider
	Assets      []VMAssetPin
}

func (m NativeVMManifest) valid() bool {
	if m.Version != 1 || m.Purpose != "native-review" || !m.Binding.valid() || m.SessionHash != m.Binding.SessionHash || !validAuthHash(m.SessionHash) || !m.Provider.Valid() || len(m.Assets) != 3 {
		return false
	}
	for i, a := range m.Assets {
		if !validAuthHash(a.SHA256) || a.Bytes <= 0 || a.Bytes > vmAssetLimit(i) {
			return false
		}
	}
	return true
}

func (i *VMBootImages) NativeManifest(s Session) (NativeVMManifest, error) {
	if i == nil || s.Validate() != nil {
		return NativeVMManifest{}, ErrVMBoot
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.spec.Binding.SessionHash != Digest(s) {
		return NativeVMManifest{}, ErrVMBoot
	}
	m := NativeVMManifest{Version: 1, Purpose: "native-review", Binding: i.spec.Binding, SessionHash: Digest(s), Provider: s.Identity.Provider}
	for _, a := range []VMBootAsset{i.spec.QEMU, i.spec.Kernel, i.spec.Initrd} {
		m.Assets = append(m.Assets, VMAssetPin{a.SHA256, a.Bytes})
	}
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
	raw, err := json.Marshal(nativeVMGrant{3, g.binding, identity, m})
	if err != nil {
		return ErrVMBoot
	}
	return g.grant(ctx, identity, Digest(m), 3, raw, files)
}

// Native boot is KVM only and has no NIC, host share, interactive monitor,
// arbitrary argument or fallback. Three distinct virtio ports carry private
// supervisor traffic. Serial output is disabled rather than mixed with reports.
func nativeVMArgs(nonce string) []string {
	args := []string{"-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off",
		"-add-fd", "fd=6,set=1,opaque=sfl-kvm", "-accel", "kvm,device=/dev/fdset/1", "-cpu", "host",
		"-m", "2048M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot",
		"-L", "/usr/share/qemu", "-bios", "/usr/share/seabios/bios-256k.bin", "-kernel", "/proc/self/fd/4", "-initrd", "/proc/self/fd/5",
		"-append", "rdinit=/native-init panic=1 sfl-native=" + nonce, "-serial", "none", "-device", "virtio-serial-pci,id=sflserial"}
	for index, name := range []string{"control", "objects", "provider"} {
		id := "sfl" + name
		args = append(args, "-chardev", "socket,id="+id+",fd="+strconv.Itoa(index+7), "-device", "virtserialport,bus=sflserial.0,chardev="+id+",name=sfl."+name)
	}
	return append(args, "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny")
}

func receiveNativeVMGrant(conn *net.UnixConn) (grant nativeVMGrant, files []*os.File, result error) {
	raw, files, result := receiveVMDescriptorFrame(conn, 7)
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
	if result != nil || grant.Version != 3 || !grant.Manifest.valid() || grant.Binding != grant.Manifest.Binding {
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
	raw, _ := json.Marshal(gateHello{3, binding, invocation, hash})
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
	if nativeVMStreams(files[4:]) != nil {
		return grant, files, ErrVMBoot
	}
	return grant, files, nil
}

// NativeVMStub is the new fixed native payload. It remains inert until its
// owned-unit and authenticated version-3 descriptor checks succeed. This is not
// a provider configuration, activation command or an auth stop attestation.
func NativeVMStub(ctx context.Context, endpoint string, binding AuthBinding, invocation, hash string, supervisorPID int) error {
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
	command := exec.CommandContext(ctx, "/proc/self/fd/3", nativeVMArgs(grant.Binding.LaunchID)...)
	command.Args[0] = "sfl-native-qemu"
	command.Env = []string{"LANG=C.UTF-8", "PATH=/usr/bin:/bin"}
	command.Dir = "/"
	command.ExtraFiles = files
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if command.Start() != nil {
		return ErrVMBoot
	}
	closeVMFiles(files) // Only QEMU now owns the child sides of private channels.
	if command.Wait() != nil {
		return ErrVMBoot
	}
	return nil
}
