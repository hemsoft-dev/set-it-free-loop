//go:build linux

package reviewer

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

type gitPIDFDIdentity struct{ Device, Inode uint64 }

// pidfs inode identity is deliberately limited to the qualified 64-bit native
// ABI. Older anon-inode pidfds and 32-bit inode truncation are not accepted.
// Polling works even when the process is outside the caller's PID namespace;
// no signal syscall, numeric PID lookup or namespace change is needed.
func gitLivePIDFDIdentity(file *os.File) (gitPIDFDIdentity, error) {
	if runtime.GOARCH != "amd64" || file == nil {
		return gitPIDFDIdentity{}, errGitUnit
	}
	fd := int(file.Fd())
	var stat unix.Stat_t
	var filesystem unix.Statfs_t
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	// pidfs uses a pseudo-inode with no S_IFMT bits, not S_IFREG. Validate
	// that actual kernel representation alongside the dedicated filesystem.
	if err != nil || flags&unix.FD_CLOEXEC == 0 || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != 0 || stat.Ino == 0 || unix.Fstatfs(fd, &filesystem) != nil || filesystem.Type != unix.PID_FS_MAGIC {
		return gitPIDFDIdentity{}, errGitUnit
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 0); err != nil || n != 0 || poll[0].Revents != 0 {
		return gitPIDFDIdentity{}, errGitUnit
	}
	return gitPIDFDIdentity{uint64(stat.Dev), stat.Ino}, nil
}

// expected is inherited through the authenticated bootstrap, never supplied by
// a packet's claimed PID. Use only a fresh pathname connection initiated by the
// worker; an inherited socketpair describes its creator, not a later writer.
// This proves the supervisor process, not a unit, runtime or source grant.
func authenticateGitSupervisor(ctx context.Context, conn *net.UnixConn, expected *os.File) (peer *os.File, result error) {
	if ctx == nil || ctx.Err() != nil || expected == nil || gitControlPacketSocket(conn) != nil {
		return nil, errGitUnit
	}
	remote, ok := conn.RemoteAddr().(*net.UnixAddr)
	if !ok || !filepath.IsAbs(remote.Name) || filepath.Clean(remote.Name) != remote.Name {
		return nil, errGitUnit
	}
	fd, err := unix.FcntlInt(expected.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, errGitUnit
	}
	trusted := os.NewFile(uintptr(fd), "sfl-git-trusted-supervisor")
	defer trusted.Close()
	want, err := gitLivePIDFDIdentity(trusted)
	if err != nil {
		return nil, err
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, errGitUnit
	}
	remoteFD := -1
	var credentials *unix.Ucred
	var queryErr error
	err = raw.Control(func(socket uintptr) {
		credentials, queryErr = unix.GetsockoptUcred(int(socket), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if queryErr == nil {
			remoteFD, queryErr = unix.GetsockoptInt(int(socket), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		}
	})
	if remoteFD >= 0 {
		peer = os.NewFile(uintptr(remoteFD), "sfl-git-supervisor-peer")
	}
	defer func() {
		if result != nil && peer != nil {
			peer.Close()
			peer = nil
		}
	}()
	// A zero PID is expected for the outside supervisor in a private PIDns.
	// The trusted pidfs identity, not that numeric field, proves the peer.
	if err != nil || queryErr != nil || credentials == nil || credentials.Pid < 0 || credentials.Uid != uint32(os.Geteuid()) || credentials.Gid != uint32(os.Getegid()) || peer == nil {
		return peer, errGitUnit
	}
	actual, err := gitLivePIDFDIdentity(peer)
	after, afterErr := gitLivePIDFDIdentity(trusted)
	if err != nil || afterErr != nil || actual != want || after != want || ctx.Err() != nil {
		return peer, errGitUnit
	}
	return peer, nil
}
