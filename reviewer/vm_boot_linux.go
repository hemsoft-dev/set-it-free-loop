//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var ErrVMBoot = errors.New("VM boot assets or fixture boundary are unqualified")

type VMBootAsset struct {
	Path, SHA256 string
	Bytes        int64
}
type VMBootSpec struct {
	Version              int
	Binding              AuthBinding
	QEMU, Kernel, Initrd VMBootAsset
}

// VMBootImages holds private immutable boot descriptors, not an executable
// pathname or a worker/auth lease. It does not create a process, unit or guest.
// Pins and paths must come from trusted release configuration, never PR content.
type VMBootImages struct {
	mu     sync.Mutex
	files  [3]*os.File
	spec   VMBootSpec
	closed bool
}

func vmAssetLimit(index int) int64 {
	if index == 2 {
		return 512 << 20
	}
	return 64 << 20
}
func (s VMBootSpec) validate() error {
	if s.Version != 1 || !s.Binding.valid() {
		return ErrVMBoot
	}
	for i, a := range []VMBootAsset{s.QEMU, s.Kernel, s.Initrd} {
		if !sourcePath(a.Path) || !validAuthHash(a.SHA256) || a.Bytes <= 0 || a.Bytes > vmAssetLimit(i) {
			return ErrVMBoot
		}
	}
	return nil
}
func sealVMAsset(ctx context.Context, a VMBootAsset, executable bool) (*os.File, error) {
	input, err := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrVMBoot
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, ErrVMBoot
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || (st.Uid != uint32(os.Geteuid()) && st.Uid != 0) || info.Mode().Perm()&0022 != 0 || info.Size() != a.Bytes {
		return nil, ErrVMBoot
	}
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_NOEXEC_SEAL
	mode := uint32(0400)
	if executable {
		flags = unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_EXEC
		mode = 0500
	}
	fd, err := unix.MemfdCreate("sfl-vm-boot", flags)
	if err != nil {
		return nil, ErrVMBoot
	}
	output := os.NewFile(uintptr(fd), "sfl-vm-boot")
	success := false
	defer func() {
		if !success {
			output.Close()
		}
	}()
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var size int64
	// Stream into the private descriptor rather than allocating an initramfs-sized
	// byte slice. Each read is bounded and cancellation is checked between chunks.
	for {
		if ctx.Err() != nil {
			return nil, errors.Join(ErrVMBoot, ctx.Err())
		}
		n, readErr := input.Read(buffer)
		if n > 0 {
			size += int64(n)
			if size > a.Bytes {
				return nil, ErrVMBoot
			}
			if count, err := output.Write(buffer[:n]); err != nil || count != n {
				return nil, ErrVMBoot
			}
			_, _ = hash.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, ErrVMBoot
		}
	}
	if size != a.Bytes || hex.EncodeToString(hash.Sum(nil)) != a.SHA256 || unix.Fchmod(fd, mode) != nil {
		return nil, ErrVMBoot
	}
	if _, err = unix.FcntlInt(output.Fd(), unix.F_ADD_SEALS, requiredStubSeals); err != nil {
		return nil, ErrVMBoot
	}
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		return nil, ErrVMBoot
	}
	success = true
	return output, nil
}
func OpenVMBootImages(ctx context.Context, spec VMBootSpec) (*VMBootImages, error) {
	if spec.validate() != nil || ctx.Err() != nil {
		return nil, ErrVMBoot
	}
	images := &VMBootImages{spec: spec}
	assets := []VMBootAsset{spec.QEMU, spec.Kernel, spec.Initrd}
	for i, asset := range assets {
		file, err := sealVMAsset(ctx, asset, i == 0)
		if err != nil {
			images.Close()
			return nil, err
		}
		images.files[i] = file
	}
	return images, nil
}
func (i *VMBootImages) Close() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	var err error
	for _, file := range i.files {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}
	return err
}
func closeVMFiles(files []*os.File) error {
	var err error
	for _, file := range files {
		err = errors.Join(err, file.Close())
	}
	return err
}
func (i *VMBootImages) duplicate() ([]*os.File, error) {
	if i == nil {
		return nil, ErrVMBoot
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil, ErrVMBoot
	}
	files := []*os.File{}
	for _, file := range i.files {
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		if err != nil || seals&requiredStubSeals != requiredStubSeals {
			closeVMFiles(files)
			return nil, ErrVMBoot
		}
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			closeVMFiles(files)
			return nil, ErrVMBoot
		}
		files = append(files, os.NewFile(uintptr(fd), "sfl-vm-child"))
	}
	return files, nil
}

func vmFixtureArgs(nonce string) []string {
	return []string{"-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off",
		"-add-fd", "fd=6,set=1,opaque=sfl-kvm", "-accel", "kvm,device=/dev/fdset/1", "-cpu", "host",
		"-m", "512M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot",
		"-L", "/usr/share/qemu", "-bios", "/usr/share/seabios/bios-256k.bin", "-kernel", "/proc/self/fd/4", "-initrd", "/proc/self/fd/5",
		"-append", "console=ttyS0 rdinit=/init panic=1 sfl-fixture=" + nonce + " -- --guest-init",
		"-serial", "stdio", "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny"}
}
func openVMKVM() (*os.File, error) {
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrVMBoot
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		file.Close()
		return nil, ErrVMBoot
	}
	version, err := unix.IoctlRetInt(int(file.Fd()), 0xAE00)
	if err != nil || version != 12 {
		file.Close()
		return nil, ErrVMBoot
	}
	return file, nil
}

// command is private: only the explicit credential-free fixture can use it.
// The production supervisor still needs its separate pinned VM payload adapter,
// authenticated grant, guest roles, auth transfer and durable stop recovery.
func (i *VMBootImages) command(ctx context.Context, kvm *os.File) (*exec.Cmd, []*os.File, error) {
	files, err := i.duplicate()
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.FcntlInt(kvm.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		closeVMFiles(files)
		return nil, nil, ErrVMBoot
	}
	files = append(files, os.NewFile(uintptr(fd), "sfl-vm-kvm"))
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", vmFixtureArgs(i.spec.Binding.LaunchID)...)
	cmd.Args[0] = "sfl-qemu-fixture"
	cmd.Env = []string{"LANG=C.UTF-8", "PATH=/usr/bin:/bin"}
	cmd.Dir = "/"
	cmd.ExtraFiles = files
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, files, nil
}
func vmFixtureUnit(nonce string) (string, error) {
	invocation := os.Getenv("INVOCATION_ID")
	if len(invocation) != 32 || strings.Trim(invocation, "0123456789abcdef") != "" {
		return "", ErrVMBoot
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", ErrVMBoot
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "0::") {
		return "", ErrVMBoot
	}
	group := strings.TrimPrefix(lines[0], "0::")
	if filepath.Clean(group) != group || !strings.HasPrefix(group, "/user.slice/") || filepath.Base(group) != "sfl-vm-fixture-"+nonce+".service" {
		return "", ErrVMBoot
	}
	root, _, err := openContainmentCgroupAt("/sys/fs/cgroup", group, cgroupHierarchy{})
	if err != nil {
		return "", ErrVMBoot
	}
	defer root.Close()
	for key, want := range map[string]string{"memory.max": "1073741824", "memory.swap.max": "0", "pids.max": "64"} {
		if value, err := cgroupFile(root, key); err != nil || value != want {
			return "", ErrVMBoot
		}
	}
	cpu, err := cgroupFile(root, "cpu.max")
	if err != nil || cpu != "100000 100000" {
		return "", ErrVMBoot
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1\n") {
		return "", ErrVMBoot
	}
	return invocation, nil
}

// RunVMBootFixture is opt-in fake-only qualification, never a WorkerLauncher or
// AuthStopVerifier. Its caller must finish whole-unit cleanup even after error.
// Serial output is a bounded synthetic artifact, not a model report or proof of
// native worker confinement. It never supplies any profile or provider route.
func (i *VMBootImages) RunVMBootFixture(ctx context.Context, serial io.Writer) (verified bool, result error) {
	if i == nil {
		return false, ErrVMBoot
	}
	if _, err := vmFixtureUnit(i.spec.Binding.LaunchID); err != nil {
		return false, err
	}
	kvm, err := openVMKVM()
	if err != nil {
		return false, err
	}
	defer func() { result = errors.Join(result, kvm.Close()) }()
	cmd, files, err := i.command(ctx, kvm)
	if err != nil {
		return false, err
	}
	defer func() { result = errors.Join(result, closeVMFiles(files)) }()
	if serial == nil {
		return false, ErrVMBoot
	}
	sink := &vmSerialWriter{target: serial, remaining: 4 << 20}
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		return false, ErrVMBoot
	}
	// QEMU's running image is the sealed descriptor that was pinned, not a path
	// re-opened after verification. A boot may not qualify without this observation.
	imageErr := verifyRunningStub(cmd.Process.Pid, i.spec.QEMU.SHA256)
	if imageErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if imageErr != nil || waitErr != nil || sink.failed || ctx.Err() != nil {
		return false, errors.Join(ErrVMBoot, imageErr, waitErr, ctx.Err())
	}
	return true, nil
}

type vmSerialWriter struct {
	mu        sync.Mutex
	target    io.Writer
	remaining int
	failed    bool
}

func (w *vmSerialWriter) Write(raw []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(raw) > w.remaining {
		w.failed = true
		return 0, ErrVMBoot
	}
	w.remaining -= len(raw)
	n, err := w.target.Write(raw)
	if err != nil || n != len(raw) {
		w.failed = true
		return n, errors.Join(ErrVMBoot, err)
	}
	return n, nil
}
