//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type VMAssetPin struct {
	SHA256 string
	Bytes  int64
}

// VMFixtureManifest contains no paths, argv, profile or network selector. Its
// only payload is the fixed credential-free guest role exercise, not inference.
type VMFixtureManifest struct {
	Version int
	Purpose string
	Binding AuthBinding
	Assets  []VMAssetPin
}

func (m VMFixtureManifest) valid() bool {
	if m.Version != 1 || m.Purpose != "synthetic-role-fixture" || !m.Binding.valid() || len(m.Assets) != 3 {
		return false
	}
	for i, a := range m.Assets {
		if !validAuthHash(a.SHA256) || a.Bytes <= 0 || a.Bytes > vmAssetLimit(i) {
			return false
		}
	}
	return true
}

func (i *VMBootImages) FixtureManifest() (VMFixtureManifest, error) {
	if i == nil {
		return VMFixtureManifest{}, ErrVMBoot
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return VMFixtureManifest{}, ErrVMBoot
	}
	m := VMFixtureManifest{Version: 1, Purpose: "synthetic-role-fixture", Binding: i.spec.Binding, Assets: make([]VMAssetPin, 3)}
	for index, a := range []VMBootAsset{i.spec.QEMU, i.spec.Kernel, i.spec.Initrd} {
		m.Assets[index] = VMAssetPin{a.SHA256, a.Bytes}
	}
	if !m.valid() {
		return VMFixtureManifest{}, ErrVMBoot
	}
	return m, nil
}

type vmGateGrant struct {
	Version  int
	Binding  AuthBinding
	Identity GateIdentity
	Manifest VMFixtureManifest
}

type VMGrantExecutionReceipt struct {
	Version                               int
	BindingHash, ManifestHash             string
	VerifiedExecutable, QEMUExited        bool
	FakeOnly, ProductionBoundaryQualified bool
	Error                                 string
}

func prepareVMFixtureGate(root string, binding AuthBinding, m VMFixtureManifest) (*ContainmentGate, error) {
	if !m.valid() || m.Binding != binding {
		return nil, ErrContainmentGate
	}
	return prepareContainmentGateHash(root, binding, Digest(m), 2)
}

// grantVMFixture sends retained boot/KVM descriptors only to the manager-bound
// stub peer. Grant persistence consumes the launch before any SCM_RIGHTS send.
func (g *ContainmentGate) grantVMFixture(ctx context.Context, identity GateIdentity, images *VMBootImages) error {
	m, err := images.FixtureManifest()
	if err != nil || m.Binding != g.binding {
		return ErrVMBoot
	}
	files, err := images.duplicate()
	if err != nil {
		return err
	}
	defer closeVMFiles(files)
	kvm, err := openVMKVM()
	if err != nil {
		return err
	}
	defer kvm.Close()
	files = append(files, kvm)
	raw, err := json.Marshal(vmGateGrant{Version: 2, Binding: g.binding, Identity: identity, Manifest: m})
	if err != nil {
		return ErrVMBoot
	}
	return g.grant(ctx, identity, Digest(m), 2, raw, files)
}

// recvVMMessage sets CLOEXEC atomically at descriptor receipt, before another
// goroutine could exec. The net poller still owns readiness and deadlines.
func recvVMMessage(conn *net.UnixConn, data, oob []byte) (n, on, flags int, result error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, 0, err
	}
	err = raw.Read(func(fd uintptr) bool {
		n, on, flags, _, result = unix.Recvmsg(int(fd), data, oob, unix.MSG_CMSG_CLOEXEC)
		return result != unix.EAGAIN && result != unix.EWOULDBLOCK
	})
	return n, on, flags, errors.Join(err, result)
}

func receiveVMDescriptorFrame(conn *net.UnixConn, expected int) (data []byte, files []*os.File, result error) {
	defer func() {
		if result != nil {
			closeVMFiles(files)
			files = nil
		}
	}()
	data = make([]byte, 4097)
	oob := make([]byte, unix.CmsgSpace((expected+1)*4))
	used := 0
	for {
		n, on, flags, err := recvVMMessage(conn, data[used:], oob)
		// Parse and own every delivered descriptor even on truncation/error, so
		// hostile extra ancillary data cannot leak FDs on a rejected grant.
		messages, parseErr := unix.ParseSocketControlMessage(oob[:on])
		for _, message := range messages {
			if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SCM_RIGHTS {
				parseErr = ErrContainmentGate
				continue
			}
			fds, rightsErr := unix.ParseUnixRights(&message)
			if rightsErr != nil {
				parseErr = rightsErr
				continue
			}
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "sfl-vm-grant"))
			}
		}
		if err != nil || parseErr != nil || flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 || len(files) > expected {
			return nil, files, ErrContainmentGate
		}
		used += n
		if used > 4096 {
			return nil, files, ErrContainmentGate
		}
		if n == 0 {
			break
		}
	}
	if len(files) != expected {
		return nil, files, ErrContainmentGate
	}
	return data[:used], files, nil
}

func receiveVMGrant(conn *net.UnixConn) (grant vmGateGrant, files []*os.File, result error) {
	raw, files, result := receiveVMDescriptorFrame(conn, 4)
	if result != nil {
		return grant, nil, result
	}
	defer func() {
		if result != nil {
			closeVMFiles(files)
			files = nil
		}
	}()
	grant, result = DecodeStrict[vmGateGrant](raw, 4096)
	if result != nil || grant.Version != 2 || !grant.Manifest.valid() || grant.Binding != grant.Manifest.Binding {
		return grant, files, ErrContainmentGate
	}
	return grant, files, nil
}

func verifyGrantedVMAsset(ctx context.Context, file *os.File, pin VMAssetPin, index int) error {
	if file == nil {
		return ErrVMBoot
	}
	info, err := file.Stat()
	if err != nil {
		return ErrVMBoot
	}
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	mode := os.FileMode(0400)
	if index == 0 {
		mode = 0500
	}
	if err != nil || seals&requiredStubSeals != requiredStubSeals || !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Size() != pin.Bytes {
		return ErrVMBoot
	}
	hash := sha256.New()
	reader := io.NewSectionReader(file, 0, pin.Bytes)
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			hash.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrVMBoot
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return ErrVMBoot
	}
	return nil
}

func verifyGrantedKVM(file *os.File) error {
	if file == nil {
		return ErrVMBoot
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return ErrVMBoot
	}
	version, err := unix.IoctlRetInt(int(file.Fd()), 0xAE00)
	if err != nil || version != 12 {
		return ErrVMBoot
	}
	return nil
}

func receiveVMFixture(ctx context.Context, endpoint string, binding AuthBinding, invocation, hash string, supervisorPID int) (*VMBootImages, *os.File, error) {
	if !binding.valid() || !validAuthHash(hash) || supervisorPID <= 0 || !filepath.IsAbs(endpoint) || filepath.Clean(endpoint) != endpoint || filepath.Base(endpoint) != "gate.sock" || filepath.Base(filepath.Dir(endpoint)) != binding.LaunchID {
		return nil, nil, ErrVMBoot
	}
	own, err := processGateIdentity(os.Getpid(), invocation)
	if err != nil {
		return nil, nil, err
	}
	var dialer net.Dialer
	connected, err := dialer.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	conn, ok := connected.(*net.UnixConn)
	if !ok {
		connected.Close()
		return nil, nil, ErrVMBoot
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
		return nil, nil, ErrVMBoot
	}
	raw, _ := json.Marshal(gateHello{2, binding, invocation, hash})
	if n, err := conn.Write(raw); err != nil || n != len(raw) {
		return nil, nil, ErrVMBoot
	}
	if conn.CloseWrite() != nil {
		return nil, nil, ErrVMBoot
	}
	grant, files, err := receiveVMGrant(conn)
	if err != nil {
		return nil, nil, err
	}
	success := false
	defer func() {
		if !success {
			closeVMFiles(files)
		}
	}()
	if grant.Binding != binding || grant.Identity != own || Digest(grant.Manifest) != hash || unix.PidfdSendSignal(pidfd, 0, nil, 0) != nil {
		return nil, nil, ErrVMBoot
	}
	for index, file := range files[:3] {
		if err := verifyGrantedVMAsset(ctx, file, grant.Manifest.Assets[index], index); err != nil {
			return nil, nil, err
		}
	}
	if err := verifyGrantedKVM(files[3]); err != nil {
		return nil, nil, err
	}
	images := &VMBootImages{spec: VMBootSpec{Version: 1, Binding: binding}}
	for index, file := range files[:3] {
		images.files[index] = file
	}
	images.spec.QEMU = VMBootAsset{SHA256: grant.Manifest.Assets[0].SHA256, Bytes: grant.Manifest.Assets[0].Bytes}
	images.spec.Kernel = VMBootAsset{SHA256: grant.Manifest.Assets[1].SHA256, Bytes: grant.Manifest.Assets[1].Bytes}
	images.spec.Initrd = VMBootAsset{SHA256: grant.Manifest.Assets[2].SHA256, Bytes: grant.Manifest.Assets[2].Bytes}
	success = true
	return images, files[3], nil
}

func vmGrantedUnit(binding AuthBinding, invocation string) error {
	return vmGrantedUnitWithKernel(binding, invocation, linuxContainmentKernel{})
}

func vmGrantedUnitWithKernel(binding AuthBinding, invocation string, kernel containmentKernel) error {
	if invocation != os.Getenv("INVOCATION_ID") {
		return ErrVMBoot
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ErrVMBoot
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	unit := "sfl-alpha-" + binding.LaunchID + ".service"
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "0::") {
		return ErrVMBoot
	}
	if _, err := kernel.Capture(strings.TrimPrefix(lines[0], "0::"), unit); err != nil {
		return ErrVMBoot
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1\n") {
		return ErrVMBoot
	}
	return nil
}

// VMFixtureStub starts no QEMU, opens no boot path/KVM device and creates no
// output before its owned-unit check and authenticated one-use FD grant. The
// caller must still prove physical whole-unit stop, not just this function exit.
func VMFixtureStub(ctx context.Context, endpoint string, binding AuthBinding, invocation, hash string, supervisorPID int) (result error) {
	if vmGrantedUnit(binding, invocation) != nil {
		return ErrVMBoot
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	images, kvm, err := receiveVMFixture(ctx, endpoint, binding, invocation, hash, supervisorPID)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(endpoint))
	if err != nil {
		return errors.Join(ErrVMBoot, images.Close(), kvm.Close())
	}
	defer root.Close()
	serial, err := root.OpenFile("vm-serial.log", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.Join(ErrVMBoot, images.Close(), kvm.Close())
	}
	verified, runErr := images.runVMFixtureWithKVM(ctx, kvm, serial)
	result = errors.Join(runErr, serial.Sync(), serial.Close(), images.Close(), kvm.Close())
	receipt := VMGrantExecutionReceipt{Version: 1, BindingHash: Digest(binding), ManifestHash: hash, VerifiedExecutable: verified, QEMUExited: runErr == nil, FakeOnly: true}
	if result != nil {
		receipt.Error = ErrVMBoot.Error()
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return errors.Join(result, ErrVMBoot)
	}
	return errors.Join(result, writeAuth(root, "vm-execution.json", raw))
}
