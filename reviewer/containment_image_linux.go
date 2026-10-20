//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const requiredStubSeals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL | unix.F_SEAL_EXEC
const retainedStubExec = "/proc/self/fd/3"
const stubBootstrapCode = "import os,sys; os.execve(3, ['sfl-unitstub', *sys.argv[1:]], {'INVOCATION_ID': os.environ['INVOCATION_ID']})"

var errStubImagePending = errors.New("pinned image has not replaced trusted bootstrap")

// systemd resolves ExecStart before packing ExtraFileDescriptors to fd 3.
// The fixed root-owned OS interpreter runs only this isolated execve bootstrap
// after that packing. No shell, user site, inherited Python config or dynamic
// program text participates, and all four arguments remain argv data.
func trustedStubBootstrap() (string, error) {
	binary, err := filepath.EvalSymlinks("/usr/bin/python3")
	if err != nil {
		return "", ErrAuthRecoveryRequired
	}
	for current := binary; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return "", ErrAuthRecoveryRequired
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrAuthRecoveryRequired
		}
		if current == binary && (!info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0) {
			return "", ErrAuthRecoveryRequired
		}
		if current == "/" {
			break
		}
	}
	return binary, nil
}

type namedUnitFD struct {
	FD   dbus.UnixFD
	Name string
}

type retainedStubImage struct {
	file *os.File
	once sync.Once
	err  error
}

func (i *retainedStubImage) Close() error {
	if i == nil {
		return nil
	}
	i.once.Do(func() { i.err = i.file.Close() })
	return i.err
}
func (i *retainedStubImage) verify() error {
	if i == nil || i.file == nil {
		return ErrAuthRecoveryRequired
	}
	seals, err := unix.FcntlInt(i.file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredStubSeals != requiredStubSeals {
		return ErrAuthRecoveryRequired
	}
	return nil
}
func readPinnedStub(path, hash string, cache bool) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 || info.Size() > 64<<20 || cache && info.Mode().Perm()&0077 != 0 {
		return nil, ErrAuthRecoveryRequired
	}
	raw, err := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	sum := sha256.Sum256(raw)
	if err != nil || int64(len(raw)) != info.Size() || hex.EncodeToString(sum[:]) != hash {
		return nil, ErrAuthRecoveryRequired
	}
	return raw, nil
}

// retainStubImage verifies the bytes that will execute, not a mutable pathname.
// The private content-addressed copy permits recovery after the original path
// is replaced. Only a sealed memfd is passed to systemd, never that copy's path.
func retainStubImage(root, original, hash string) (*retainedStubImage, error) {
	if privateDirectory(root) != nil || !filepath.IsAbs(original) || filepath.Clean(original) != original || !validAuthHash(hash) {
		return nil, ErrAuthRecoveryRequired
	}
	directory := filepath.Join(root, "executables")
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return nil, ErrAuthRecoveryRequired
	}
	if privateDirectory(directory) != nil {
		return nil, ErrAuthRecoveryRequired
	}
	cache := filepath.Join(directory, hash)
	raw, err := readPinnedStub(cache, hash, true)
	if os.IsNotExist(err) {
		raw, err = readPinnedStub(original, hash, false)
		if err != nil {
			return nil, ErrAuthRecoveryRequired
		}
		anchored, err := os.OpenRoot(directory)
		if err != nil {
			return nil, ErrAuthRecoveryRequired
		}
		err = writeAuth(anchored, hash, raw)
		if err == nil {
			err = anchored.Chmod(hash, 0500)
		}
		if err == nil {
			err = syncAuthDirectory(anchored)
		}
		anchored.Close()
		if err != nil {
			return nil, ErrAuthRecoveryRequired
		}
	} else if err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	fd, err := unix.MemfdCreate("sfl-verified-unitstub", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	file := os.NewFile(uintptr(fd), "sfl-verified-unitstub")
	success := false
	defer func() {
		if !success {
			file.Close()
		}
	}()
	if n, err := file.Write(raw); err != nil || n != len(raw) {
		return nil, ErrAuthRecoveryRequired
	}
	if unix.Fchmod(fd, 0500) != nil {
		return nil, ErrAuthRecoveryRequired
	}
	if _, err = unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, requiredStubSeals); err != nil {
		return nil, ErrAuthRecoveryRequired
	}
	image := &retainedStubImage{file: file}
	if image.verify() != nil {
		return nil, ErrAuthRecoveryRequired
	}
	success = true
	return image, nil
}
func verifyRunningStub(pid int, hash string) error {
	if pid <= 0 || !validAuthHash(hash) {
		return ErrAuthRecoveryRequired
	}
	file, err := os.Open("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	defer file.Close()
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredStubSeals != requiredStubSeals {
		return errors.Join(ErrAuthRecoveryRequired, errStubImagePending)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(file, (64<<20)+1))
	if err != nil || n > 64<<20 || hex.EncodeToString(h.Sum(nil)) != hash {
		return ErrAuthRecoveryRequired
	}
	return nil
}

func (s *SystemdSupervisor) captureStartedUnit(ctx context.Context, r unitJournal) (unitJournal, error) {
	for {
		captured, err := s.captureUnit(ctx, r)
		if !errors.Is(err, errStubImagePending) {
			return captured, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r, ErrAuthRecoveryRequired
		case <-timer.C:
		}
	}
}
