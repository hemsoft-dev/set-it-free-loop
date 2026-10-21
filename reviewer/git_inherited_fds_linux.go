//go:build linux

package reviewer

import (
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// The fixed entry has already validated every selected raw role. Permit only
// those non-CLOEXEC slots and safe standard streams. Go's private runtime and
// poller descriptors are allowed only with CLOEXEC; never close unknown handles
// while rejecting a malformed launch. Call again with lastRole=2 immediately
// before a child exec, after all private roles have been marked CLOEXEC.
func gitAuditInheritedFDs(lastRole int) error {
	if lastRole != 2 && lastRole != 4 && lastRole != 6 {
		return errGitUnit
	}
	for fd := 0; fd <= 2; fd++ {
		var stat unix.Stat_t
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil || unix.Fstat(fd, &stat) != nil {
			return errGitUnit
		}
		null := stat.Mode&unix.S_IFMT == unix.S_IFCHR && stat.Rdev == unix.Mkdev(1, 3)
		pipe := stat.Mode&unix.S_IFMT == unix.S_IFIFO
		want := unix.O_WRONLY
		if fd == 0 {
			want = unix.O_RDONLY
		}
		if (!null && !pipe) || flags&unix.O_ACCMODE != want && !(null && flags&unix.O_ACCMODE == unix.O_RDWR) {
			return errGitUnit
		}
	}
	directory, err := os.Open("/proc/self/fd")
	if err != nil {
		return errGitUnit
	}
	defer directory.Close()
	names, err := directory.Readdirnames(257)
	if err != nil && err != io.EOF || len(names) > 256 {
		return errGitUnit
	}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil || fd < 0 || strconv.Itoa(fd) != name {
			return errGitUnit
		}
		if fd <= lastRole {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		// An already-closed transient runtime FD cannot survive exec. No
		// other failed query or unknown inheritable descriptor is accepted.
		if err == unix.EBADF {
			continue
		}
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			return errGitUnit
		}
	}
	return nil
}
