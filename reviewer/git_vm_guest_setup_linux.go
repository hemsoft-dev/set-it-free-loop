//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func gitVMGuestSetup() (gitVMGuestRelease, string, error) {
	var r gitVMGuestRelease
	if os.Getpid() != 1 || os.Getuid() != 0 {
		return r, "", ErrLiveBlocked
	}
	raw, err := readGitVMGuestFile("/source-release.json", 65536)
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, &r) != nil || !r.valid() {
		return r, "", ErrVMBoot
	}
	canonical, _ := json.Marshal(r)
	if !bytes.Equal(raw, canonical) {
		return r, "", ErrVMBoot
	}
	var fs unix.Statfs_t
	if unix.Statfs("/", &fs) != nil || !nativeGuestRootFSBudget(fs) {
		return r, "", ErrVMBoot
	}
	if nativeGuestMount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "", unix.Mount) != nil {
		return r, "", ErrVMBoot
	}
	line, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return r, "", ErrVMBoot
	}
	hash := ""
	for _, field := range strings.Fields(string(line)) {
		if strings.HasPrefix(field, "sfl-source=") {
			if hash != "" {
				return r, "", ErrVMBoot
			}
			hash = strings.TrimPrefix(field, "sfl-source=")
		}
	}
	if !validAuthHash(hash) {
		return r, "", ErrVMBoot
	}
	for _, m := range []struct {
		source, target, kind, options string
		flags                         uintptr
	}{
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755", unix.MS_NOSUID},
		{"sysfs", "/sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"tmpfs", "/run", "tmpfs", "size=201326592,nr_inodes=2048,noswap,mode=0755", unix.MS_NOSUID | unix.MS_NODEV},
		{"tmpfs", "/tmp", "tmpfs", "size=268435456,nr_inodes=65536,noswap,mode=0700", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	} {
		if nativeGuestMount(m.source, m.target, m.kind, m.flags, m.options, unix.Mount) != nil {
			return r, "", ErrVMBoot
		}
	}
	if os.MkdirAll("/sys/fs/cgroup", 0755) != nil || unix.Mount("none", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") != nil ||
		os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory +pids +cpu"), 0600) != nil {
		return r, "", ErrVMBoot
	}
	for _, pin := range []nativeGuestFile{
		{Source: gitVMGuestWorkerPath, SHA256: r.Worker.SHA256, Bytes: r.Worker.Bytes},
		{Source: gitVMGuestHelperPath, SHA256: r.NamespaceHelper.SHA256, Bytes: r.NamespaceHelper.Bytes},
		{Source: gitVMGuestRuntimePath, SHA256: r.Runtime.Artifact.SHA256, Bytes: r.Runtime.Artifact.Bytes},
	} {
		if nativeGuestVerifiedFile(pin) != nil {
			return r, "", ErrVMBoot
		}
	}
	for _, pin := range r.BootstrapLibraries {
		if nativeGuestVerifiedFile(nativeGuestFile{Source: "/" + pin.Path, SHA256: pin.SHA256, Bytes: pin.Bytes}) != nil {
			return r, "", ErrVMBoot
		}
	}
	for _, m := range r.Modules {
		if nativeGuestVerifiedFile(nativeGuestFile{Source: m.Path, SHA256: m.SHA256, Bytes: m.Bytes}) != nil {
			return r, "", ErrVMBoot
		}
		file, err := os.OpenFile(m.Path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return r, "", ErrVMBoot
		}
		err = unix.FinitModule(int(file.Fd()), "", 0)
		file.Close()
		if err != nil && err != unix.EEXIST {
			return r, "", ErrVMBoot
		}
	}
	return r, hash, nil
}

func readGitVMGuestFile(path string, limit int64) ([]byte, error) {
	if os.Getpid() != 1 || os.Getuid() != 0 || limit <= 0 {
		return nil, ErrVMBoot
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrVMBoot
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, ErrVMBoot
	}
	var stat unix.Stat_t
	if unix.Fstat(int(f.Fd()), &stat) != nil || stat.Uid != 0 || stat.Nlink != 1 {
		return nil, ErrVMBoot
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) != info.Size() {
		return nil, ErrVMBoot
	}
	return raw, nil
}

func gitVMGuestPort(name string) (*os.File, error) {
	if os.Getpid() != 1 || os.Getuid() != 0 || (name != "source-control" && name != "source-read" && name != "source-export") {
		return nil, ErrVMBoot
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, err := nativeGuestPortOnce(name)
		if err == nil {
			if nativeGuestPortConnected(f, deadline) != nil {
				f.Close()
				return nil, ErrVMBoot
			}
			return f, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, ErrVMBoot
}

// The source guest's root owns creation, assigns its fixed worker owner before
// sealing, and retains the original inode. This is never a host asset opener.
func gitVMGuestSeal(ctx context.Context, raw []byte, executable bool) (*os.File, error) {
	limit := 64 << 20
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_EXEC
	mode := uint32(0500)
	if !executable {
		limit = gitVMRecordBytes
		flags = unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_NOEXEC_SEAL
		mode = 0400
	}
	if os.Getpid() != 1 || os.Getuid() != 0 || ctx == nil || ctx.Err() != nil || len(raw) == 0 || len(raw) > limit {
		return nil, ErrVMBoot
	}
	fd, err := unix.MemfdCreate("sfl-source-guest", flags)
	if err != nil {
		return nil, ErrVMBoot
	}
	f := os.NewFile(uintptr(fd), "sfl-source-guest")
	good := false
	defer func() {
		if !good {
			f.Close()
		}
	}()
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		return nil, ErrVMBoot
	}
	if unix.Fchown(fd, gitVMGuestUID, gitVMGuestGID) != nil || unix.Fchmod(fd, mode) != nil {
		return nil, ErrVMBoot
	}
	if _, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, requiredStubSeals); err != nil {
		return nil, ErrVMBoot
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil || ctx.Err() != nil {
		return nil, ErrVMBoot
	}
	good = true
	return f, nil
}
