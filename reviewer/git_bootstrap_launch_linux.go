//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// No writes occur before the actual requested filesystem is observed. Unit
// property readback and cgroup swap denial are not substitutes for noswap.
func openGitTmpfsWorkspace(binding gitMaterializationBinding) (*os.File, error) {
	if !binding.valid() {
		return nil, errGitUnit
	}
	root, err := os.OpenRoot("/tmp")
	if err != nil {
		return nil, errGitUnit
	}
	defer root.Close()
	directory, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errGitUnit
	}
	defer directory.Close()
	info, err := directory.Stat()
	stat, ok := infoSysGitDirectory(info)
	var fs unix.Statfs_t
	const size = uint64(268435456)
	flags := int64(unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC)
	if err != nil || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0700 || unix.Fstatfs(int(directory.Fd()), &fs) != nil || fs.Type != unix.TMPFS_MAGIC || fs.Bsize <= 0 || size%uint64(fs.Bsize) != 0 || fs.Blocks != size/uint64(fs.Bsize) || fs.Files != 65536 || fs.Flags&flags != flags || fs.Flags&unix.ST_RDONLY != 0 {
		return nil, errGitUnit
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, errGitUnit
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > 1<<20 {
		return nil, errGitUnit
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		before, after, found := strings.Cut(line, " - ")
		a, b := strings.Fields(before), strings.Fields(after)
		if !found || len(a) < 6 || len(b) < 3 || a[4] != "/tmp" {
			continue
		}
		count++
		if b[0] != "tmpfs" || !gitMountOption(b[2], "noswap") || !gitMountOption(a[5], "nosuid") || !gitMountOption(a[5], "nodev") || !gitMountOption(a[5], "noexec") {
			return nil, errGitUnit
		}
	}
	if count != 1 {
		return nil, errGitUnit
	}
	entries, err := directory.ReadDir(1)
	if err != io.EOF || len(entries) != 0 {
		return nil, errGitUnit
	}
	name := "git-work-" + binding.Attempt
	if root.Mkdir(name, 0700) != nil {
		return nil, errGitUnit
	}
	work, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errGitUnit
	}
	return work, nil
}

func gitMountOption(list, option string) bool {
	for _, part := range strings.Split(list, ",") {
		if part == option {
			return true
		}
	}
	return false
}

func gitWorkerNamespaceArgs(binding gitMaterializationBinding, invocation string) ([]string, error) {
	if !binding.valid() || !gitQuarantineHex(invocation, 32) {
		return nil, errGitUnit
	}
	raw, err := jsonMarshalGitBinding(binding)
	if err != nil {
		return nil, errGitUnit
	}
	return []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--setenv", "INVOCATION_ID", invocation,
		"--ro-bind-fd", "8", "/", "--ro-bind-fd", "9", "/broker", "--bind-fd", "10", "/work", "--bind-fd", "11", "/tmp",
		"--proc", "/proc", "--remount-ro", "/proc", "--dev-bind", "/dev/null", "/dev/null", "--dev-bind", "/dev/urandom", "/dev/urandom", "--remount-ro", "/dev/urandom",
		"--chdir", "/work", "--seccomp", "7", "--argv0", "sfl-git-worker", "--", "/proc/self/fd/4", "--worker", raw, Digest(binding)}, nil
}

// Start only the retained original root-owned namespace helper. Unconsumed
// payload descriptors are exactly supervisor3/image4/manifest5/export6;
// bwrap consumes filter7/runtime8/broker9/work10/tmp-alias11 before exec.
// A failed helper is not whole-unit stop or source publication authority.
func launchGitWorker(ctx context.Context, a *gitBootstrapAssets) error {
	if ctx == nil || ctx.Err() != nil || a == nil || len(a.files) != 8 {
		return errGitUnit
	}
	if _, err := verifyGitRuntimeDirectory(ctx, a.files[1], a.manifest.Runtime); err != nil {
		return errGitUnit
	}
	work, err := openGitTmpfsWorkspace(a.packet.Binding)
	if err != nil {
		return errGitUnit
	}
	defer work.Close()
	alias, err := duplicateGitControlFile(work)
	if err != nil {
		return errGitUnit
	}
	defer alias.Close()
	for path, want := range map[string]uint64{"/dev/null": unix.Mkdev(1, 3), "/dev/urandom": unix.Mkdev(1, 9)} {
		var stat unix.Stat_t
		if unix.Lstat(path, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR || stat.Uid != 0 || stat.Rdev != want {
			return errGitUnit
		}
	}
	args, err := gitWorkerNamespaceArgs(a.packet.Binding, a.packet.Recipient.InvocationID)
	if err != nil {
		return errGitUnit
	}
	helper := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), a.files[2].Fd())
	command := exec.CommandContext(ctx, helper, args...)
	command.Env = []string{}
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	command.ExtraFiles = []*os.File{a.files[7], a.files[3], a.files[0], a.files[6], a.files[4], a.files[1], a.files[5], work, alias}
	if command.Run() != nil {
		return errGitUnit
	}
	return nil
}
