//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type nativeGuestFile struct {
	Source, Target, SHA256 string
	Bytes                  int64
	Roles                  []string
}
type nativeGuestRelease struct {
	Version             int
	Purpose, SourceHead string
	Files               []nativeGuestFile
	Modules             []nativeGuestModule
}

type nativeGuestModule struct {
	Path, SHA256 string
	Bytes        int64
}

func nativeGuestTarget(path string) bool {
	if !sourcePath(path) {
		return false
	}
	switch path {
	case "/native-init", "/sourceproxy", "/usr/bin/env", "/client/codex", "/client/claude", "/client/codex-code-mode-host", "/codehost":
		return true
	}
	return strings.HasPrefix(path, "/lib/") || strings.HasPrefix(path, "/lib64/") || strings.HasPrefix(path, "/usr/lib/")
}
func (r nativeGuestRelease) valid() bool {
	if r.Version != 1 || r.Purpose != "native-review" || !objectID.MatchString(r.SourceHead) || len(r.Files) == 0 || len(r.Files) > 128 {
		return false
	}
	if len(r.Modules) > 8 {
		return false
	}
	modulePaths := map[string]bool{}
	for _, module := range r.Modules {
		if !sourcePath(module.Path) || !strings.HasPrefix(module.Path, "/runtime/modules/") || !strings.HasSuffix(module.Path, ".ko") || !validAuthHash(module.SHA256) || module.Bytes <= 0 || module.Bytes > 32<<20 || modulePaths[module.Path] {
			return false
		}
		modulePaths[module.Path] = true
	}
	seen := map[string]bool{}
	targets := map[string]map[string]bool{}
	var total int64
	for _, file := range r.Files {
		if !sourcePath(file.Source) || (file.Source != "/native-init" && !strings.HasPrefix(file.Source, "/runtime/")) || !nativeGuestTarget(file.Target) || !validAuthHash(file.SHA256) || file.Bytes <= 0 || file.Bytes > 512<<20 || len(file.Roles) == 0 || len(file.Roles) > 4 {
			return false
		}
		total += file.Bytes
		if total > 768<<20 {
			return false
		}
		for _, role := range file.Roles {
			switch role {
			case "reader", "codex", "claude", "codehost":
			default:
				return false
			}
			key := role + " " + file.Target
			if seen[key] {
				return false
			}
			seen[key] = true
			if targets[role] == nil {
				targets[role] = map[string]bool{}
			}
			targets[role][file.Target] = true
		}
	}
	for _, role := range []string{"reader", "codex", "claude", "codehost"} {
		if !targets[role]["/native-init"] {
			return false
		}
	}
	for _, role := range []string{"codex", "claude"} {
		if !targets[role]["/sourceproxy"] || !targets[role]["/usr/bin/env"] || !targets[role]["/client/"+role] {
			return false
		}
	}
	return targets["codex"]["/client/codex-code-mode-host"] && targets["codehost"]["/codehost"]
}

func nativeGuestVerifiedFile(pin nativeGuestFile) error {
	f, err := os.OpenFile(pin.Source, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ErrVMBoot
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != pin.Bytes || info.Mode().Perm()&0022 != 0 {
		return ErrVMBoot
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || st.Nlink != 1 {
		return ErrVMBoot
	}
	hash := sha256.New()
	if n, err := io.Copy(hash, io.LimitReader(f, pin.Bytes+1)); err != nil || n != pin.Bytes || hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return ErrVMBoot
	}
	return nil
}

// nativeGuestMount prepares a fixed bootstrap mount selected by guest PID1.
// The mount operation is passed explicitly so filesystem preconditions can be
// tested without mounting anything in the host test process.
func nativeGuestMount(source, target, kind string, flags uintptr, options string, mount func(string, string, string, uintptr, string) error) error {
	if err := os.Mkdir(target, 0755); err != nil && !os.IsExist(err) {
		return ErrVMBoot
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return ErrVMBoot
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return ErrVMBoot
	}
	return mount(source, target, kind, flags, options)
}

func nativeGuestRootFSBudget(info unix.Statfs_t) bool {
	return info.Type == unix.TMPFS_MAGIC && info.Bsize > 0 && uint64(info.Bsize) <= 1<<30 && (1<<30)%uint64(info.Bsize) == 0 && info.Blocks == (1<<30)/uint64(info.Bsize) && info.Files == 4096
}

func nativeGuestSetup() (nativeGuestRelease, error) {
	var release nativeGuestRelease
	// Refuse host execution before a mount, cgroup, device or account operation.
	if os.Getpid() != 1 || os.Getuid() != 0 {
		return release, ErrLiveBlocked
	}
	nativeGuestBootStage("release")
	raw, err := os.ReadFile("/native-release.json")
	if err != nil || len(raw) > 65536 {
		return release, ErrVMBoot
	}
	if json.Unmarshal(raw, &release) != nil || !release.valid() {
		return release, ErrVMBoot
	}
	canonical, _ := json.Marshal(release)
	if !bytes.Equal(canonical, raw) {
		return release, ErrVMBoot
	}
	nativeGuestBootStage("rootfs")
	var rootfs unix.Statfs_t
	if unix.Statfs("/", &rootfs) != nil || !nativeGuestRootFSBudget(rootfs) {
		return release, ErrVMBoot
	}
	nativeGuestBootStage("proc")
	if nativeGuestMount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "", unix.Mount) != nil {
		return release, ErrVMBoot
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return release, ErrVMBoot
	}
	nonce := ""
	for _, field := range strings.Fields(string(cmdline)) {
		if strings.HasPrefix(field, "sfl-native=") {
			if nonce != "" {
				return release, ErrVMBoot
			}
			nonce = strings.TrimPrefix(field, "sfl-native=")
		}
	}
	if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" {
		return release, ErrVMBoot
	}
	nativeGuestBootStage("mounts")
	for _, mount := range []struct {
		stage, source, target, kind, options string
		flags                                uintptr
	}{{"mount-dev", "devtmpfs", "/dev", "devtmpfs", "mode=0755", unix.MS_NOSUID}, {"mount-sys", "sysfs", "/sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC}, {"mount-run", "tmpfs", "/run", "tmpfs", "mode=0755,size=192m", unix.MS_NOSUID | unix.MS_NODEV}} {
		nativeGuestBootStage(mount.stage)
		if err := nativeGuestMount(mount.source, mount.target, mount.kind, mount.flags, mount.options, unix.Mount); err != nil {
			failure := nativeBootMountError(err)
			if err == unix.ENOENT {
				info, statErr := os.Lstat(mount.target)
				switch {
				case os.IsNotExist(statErr):
					failure += "-absent"
				case statErr != nil:
					failure += "-staterror"
				case !info.IsDir():
					failure += "-notdir"
				default:
					failure += "-present"
				}
			}
			nativeGuestBootStage(mount.stage + "-" + failure)
			return release, ErrVMBoot
		}
	}
	nativeGuestBootStage("cgroup")
	if os.MkdirAll("/sys/fs/cgroup", 0755) != nil || unix.Mount("none", "/sys/fs/cgroup", "cgroup2", 0, "") != nil || os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory +pids +cpu"), 0600) != nil {
		return release, ErrVMBoot
	}
	if os.Mkdir("/run/native", 0700) != nil {
		return release, ErrVMBoot
	}
	nativeGuestBootStage("runtime")
	for _, file := range release.Files {
		if nativeGuestVerifiedFile(file) != nil {
			stage := "runtime-invalid"
			info, statErr := os.Lstat(file.Source)
			switch {
			case os.IsNotExist(statErr):
				stage = "runtime-missing"
			case statErr != nil || !info.Mode().IsRegular():
				stage = "runtime-metadata"
			case info.Size() != file.Bytes:
				stage = "runtime-short"
			}
			var rootfs unix.Statfs_t
			if unix.Statfs("/", &rootfs) == nil && rootfs.Type == unix.TMPFS_MAGIC && rootfs.Bavail == 0 {
				stage = "runtime-full"
			}
			nativeGuestBootStage(stage)
			return release, ErrVMBoot
		}
	}
	nativeGuestBootStage("modules")
	for _, module := range release.Modules {
		pin := nativeGuestFile{Source: module.Path, SHA256: module.SHA256, Bytes: module.Bytes}
		if nativeGuestVerifiedFile(pin) != nil {
			return release, ErrVMBoot
		}
		file, err := os.OpenFile(module.Path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return release, ErrVMBoot
		}
		err = unix.FinitModule(int(file.Fd()), "", 0)
		file.Close()
		if err != nil && err != unix.EEXIST {
			return release, ErrVMBoot
		}
	}
	return release, nil
}

func nativeGuestPort(name string) (*os.File, error) {
	if os.Getuid() != 0 || os.Getpid() != 1 || (name != "control" && name != "objects" && name != "provider") {
		return nil, ErrVMBoot
	}
	deadline := time.Now().Add(2 * time.Second)
	for range 100 {
		file, err := nativeGuestPortOnce(name)
		if err == nil {
			if nativeGuestPortConnected(file, deadline) != nil {
				file.Close()
				return nil, ErrVMBoot
			}
			return file, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, ErrVMBoot
}

func nativeGuestPortConnected(file *os.File, deadline time.Time) error {
	// virtio-console may publish a port before the host-open notification.
	// In that window reads report EOF and poll reports HUP. Wait for actual
	// connected writability before any framed protocol intake.
	if file == nil {
		return ErrVMBoot
	}
	poll := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLOUT}}
	for time.Now().Before(deadline) {
		poll[0].Revents = 0
		_, err := unix.Poll(poll, 20)
		if err == unix.EINTR {
			continue
		}
		if err != nil || poll[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return ErrVMBoot
		}
		if poll[0].Revents&unix.POLLOUT != 0 && poll[0].Revents&unix.POLLHUP == 0 {
			return nil
		}
		if poll[0].Revents&unix.POLLHUP != 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return ErrVMBoot
}

func nativeGuestPortOnce(name string) (*os.File, error) {
	entries, err := os.ReadDir("/sys/class/virtio-ports")
	if err != nil {
		return nil, ErrVMBoot
	}
	var device string
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("/sys/class/virtio-ports", entry.Name(), "name"))
		if err == nil && strings.TrimSpace(string(data)) == "sfl."+name {
			if device != "" {
				return nil, ErrVMBoot
			}
			device = filepath.Join("/dev", entry.Name())
		}
	}
	if device == "" {
		return nil, ErrVMBoot
	}
	file, err := os.OpenFile(device, os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrVMBoot
	}
	if info, err := file.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		file.Close()
		return nil, ErrVMBoot
	}
	return file, nil
}

func (r nativeGuestRelease) jail(role string) (string, error) {
	if os.Getuid() != 0 || os.Getpid() != 1 || !r.valid() {
		return "", ErrVMBoot
	}
	switch role {
	case "reader", "codex", "claude", "codehost":
	default:
		return "", ErrVMBoot
	}
	root := filepath.Join("/run/native", role)
	if os.Mkdir(root, 0755) != nil {
		return "", ErrVMBoot
	}
	for _, dir := range []string{"dev", "run", "profile", "tmp", "work", "proc", "etc"} {
		if os.Mkdir(filepath.Join(root, dir), 0755) != nil {
			return "", ErrVMBoot
		}
	}
	for _, file := range r.Files {
		selected := false
		for _, candidate := range file.Roles {
			selected = selected || candidate == role
		}
		if !selected {
			continue
		}
		target := filepath.Join(root, file.Target)
		if os.MkdirAll(filepath.Dir(target), 0755) != nil {
			return "", ErrVMBoot
		}
		if f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500); err != nil {
			return "", ErrVMBoot
		} else {
			f.Close()
		}
		if unix.Mount(file.Source, target, "", unix.MS_BIND, "") != nil || unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "") != nil {
			return "", ErrVMBoot
		}
	}
	for _, name := range []string{"null", "urandom", "zero"} {
		target := filepath.Join(root, "dev", name)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", ErrVMBoot
		}
		f.Close()
		if unix.Mount("/dev/"+name, target, "", unix.MS_BIND, "") != nil {
			return "", ErrVMBoot
		}
	}
	if os.WriteFile(filepath.Join(root, "etc/hosts"), []byte("127.0.0.1 localhost\n"), 0444) != nil {
		return "", ErrVMBoot
	}
	// The executable sees its own passwd entry without consulting a host NSS
	// service. No interactive account database enters the image.
	passwd := "root:x:0:0:root:/:/nonexistent\nreader:x:1201:1301:reader:/:/nonexistent\ncodex:x:1202:1301:codex:/profile:/nonexistent\ncodehost:x:1203:1301:codehost:/:/nonexistent\nclaude:x:1204:1301:claude:/profile:/nonexistent\n"
	if os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0444) != nil {
		return "", ErrVMBoot
	}
	if os.Chmod(root, 0555) != nil {
		return "", ErrVMBoot
	}
	// A chroot directory is not necessarily a mount point. Its namespace's
	// trusted PID1 must be able to mark / private before mounting its own proc.
	// Recursive self-bind preserves the already read-only runtime file binds.
	if unix.Mount(root, root, "", unix.MS_BIND|unix.MS_REC, "") != nil {
		return "", ErrVMBoot
	}
	return root, nil
}

type nativeGuestProcess struct {
	in       *os.File
	out      *os.File
	command  *exec.Cmd
	group    string
	groupFD  *os.File
	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
	stopErr  error
}

func startNativeGuestRole(ctx context.Context, role, root string, uid int, files []*os.File, mode string) (*nativeGuestProcess, error) {
	if os.Getpid() != 1 || os.Getuid() != 0 || !sourcePath(root) {
		return nil, ErrLiveBlocked
	}
	var memory int64
	var tasks int
	switch role {
	case "reader":
		memory, tasks = 128<<20, 16
	case "client":
		memory, tasks = 1280<<20, 96
	case "codehost":
		memory, tasks = 256<<20, 32
	default:
		return nil, ErrVMBoot
	}
	group := filepath.Join("/sys/fs/cgroup", "native-"+role)
	if os.Mkdir(group, 0700) != nil {
		return nil, ErrVMBoot
	}
	for name, value := range map[string]string{"memory.max": strconv.FormatInt(memory, 10), "memory.swap.max": "0", "pids.max": strconv.Itoa(tasks), "cpu.max": "100000 100000"} {
		if os.WriteFile(filepath.Join(group, name), []byte(value), 0600) != nil {
			return nil, ErrVMBoot
		}
	}
	fd, err := os.Open(group)
	if err != nil {
		return nil, ErrVMBoot
	}
	input, write, err := os.Pipe()
	if err != nil {
		fd.Close()
		return nil, ErrVMBoot
	}
	read, output, err := os.Pipe()
	if err != nil {
		fd.Close()
		input.Close()
		write.Close()
		return nil, ErrVMBoot
	}
	command := exec.Command("/native-init", mode)
	command.Dir = "/"
	command.Env = []string{"LANG=C.UTF-8"}
	command.Stdin = input
	command.Stdout = output
	command.Stderr = io.Discard
	command.ExtraFiles = files
	command.SysProcAttr = &syscall.SysProcAttr{Chroot: root, Credential: &syscall.Credential{Uid: uint32(uid), Gid: nativeVMIPCGID, Groups: []uint32{}}, Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWIPC, UseCgroupFD: true, CgroupFD: int(fd.Fd()), Pdeathsig: syscall.SIGKILL}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		err = command.Start()
	}
	input.Close()
	output.Close()
	if err != nil {
		write.Close()
		read.Close()
		fd.Close()
		return nil, ErrVMBoot
	}
	p := &nativeGuestProcess{in: write, out: read, command: command, group: group, groupFD: fd, done: make(chan struct{})}
	go func() { p.waitErr = command.Wait(); close(p.done) }()
	return p, nil
}
func (p *nativeGuestProcess) Input() io.WriteCloser { return p.in }
func (p *nativeGuestProcess) Output() io.ReadCloser { return p.out }
func (p *nativeGuestProcess) Audit() io.ReadCloser  { return p.out }
func (p *nativeGuestProcess) Wait() error           { <-p.done; return p.waitErr }
func (p *nativeGuestProcess) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.in.Close()
		if strings.HasSuffix(p.group, "reader") {
			// The finite manifest already consumed stdin. SIGTERM closes reader
			// intake only; accepted source work drains under its phase deadline.
			if p.command == nil || p.command.Process == nil || p.command.Process.Signal(syscall.SIGTERM) == nil {
				select {
				case <-p.done:
				case <-ctx.Done():
				}
			}
		}
		if os.WriteFile(filepath.Join(p.group, "cgroup.kill"), []byte("1"), 0600) != nil {
			p.stopErr = ErrWorkerStopPending
			return
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			p.stopErr = ErrWorkerStopPending
			return
		}
		for {
			events, err := os.ReadFile(filepath.Join(p.group, "cgroup.events"))
			if err == nil && strings.Contains(string(events), "populated 0") {
				break
			}
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				p.stopErr = ErrWorkerStopPending
				return
			}
		}
		p.stopErr = p.groupFD.Close()
	})
	if p.stopErr != nil {
		return ErrWorkerStopPending
	}
	return nil
}

func nativeGuestUnprivileged(uid int) bool {
	groups, err := os.Getgroups()
	if err != nil || len(groups) != 0 || os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != nativeVMIPCGID || os.Getegid() != nativeVMIPCGID {
		return false
	}
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return false
	}
	var data [2]unix.CapUserData
	return unix.Capget(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &data[0]) == nil && data[0].Effective == 0 && data[1].Effective == 0 && data[0].Permitted == 0 && data[1].Permitted == 0 && data[0].Inheritable == 0 && data[1].Inheritable == 0
}

func nativeGuestReaderMain() int {
	if !nativeGuestUnprivileged(nativeVMReaderUID) {
		return 2
	}
	ctx, stopReader := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stopReader()
	objects := os.NewFile(3, "private-vm-objects")
	if objects == nil {
		return 2
	}
	defer objects.Close()
	// Read only the bounded manifest, then reconstruct its finite input for the
	// shared service. No host path/Git executable is present in this reader jail.
	intake, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(intake, func() { os.Stdin.Close() })
	defer stop()
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 || intake.Err() != nil {
		return 2
	}
	var config SourceReaderConfig
	if json.Unmarshal(raw, &config) != nil || config.validate() != nil {
		return 2
	}
	store, err := NewVMObjectStream(objects, config.Session)
	if err != nil {
		return 2
	}
	defer store.Close()
	return sourceReaderMainWithStore(context.Background(), []string{"LANG=C.UTF-8"}, io.NopCloser(bytes.NewReader(raw)), os.Stdout, store,
		func(phase context.Context, config SourceReaderConfig, reader *Reader, ready func(SourceEndpoint) error) (*SourceAccess, error) {
			return serveSourceSocketDraining(phase, ctx, config, reader, ready, sourcePeerUID)
		})
}

var _ Worker = (*nativeGuestProcess)(nil)
var _ SourceReaderProcess = (*nativeGuestProcess)(nil)
