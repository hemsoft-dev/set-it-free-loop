//go:build linux

package reviewer

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const gitControlPacketBytes = 8192
const gitControlPacketFDs = 8

func gitControlDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(10 * time.Second)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	return deadline
}

func gitControlFileFD(file *os.File) (int, error) {
	if file == nil {
		return -1, errGitUnit
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return -1, errGitUnit
	}
	fd := -1
	err = raw.Control(func(value uintptr) { fd = int(value) })
	if err != nil || fd < 0 {
		return -1, errGitUnit
	}
	return fd, nil
}
func duplicateGitControlFile(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, errGitUnit
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, errGitUnit
	}
	fd := -1
	var duplicateErr error
	err = raw.Control(func(value uintptr) { fd, duplicateErr = unix.FcntlInt(value, unix.F_DUPFD_CLOEXEC, 0) })
	if err != nil || duplicateErr != nil {
		if fd >= 0 {
			unix.Close(fd)
		}
		return nil, errGitUnit
	}
	return os.NewFile(uintptr(fd), "sfl-git-control-owned"), nil
}

// Atomic bounded descriptor transport only. The typed handshake separately
// authenticates the actual connecting peer, binding, stage, JSON and FD roles.
// A decoded packet never grants authority or authenticates a socketpair peer.
func gitControlPacketSocket(conn *net.UnixConn) error {
	if conn == nil {
		return errGitUnit
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return errGitUnit
	}
	var check error
	err = raw.Control(func(fd uintptr) {
		kind, e := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		peer, peerErr := unix.Getpeername(int(fd))
		if _, ok := peer.(*unix.SockaddrUnix); e != nil || peerErr != nil || !ok || kind != unix.SOCK_SEQPACKET {
			check = errGitUnit
		}
	})
	if err != nil || check != nil {
		return errGitUnit
	}
	return nil
}
func writeGitControlPacket(ctx context.Context, conn *net.UnixConn, data []byte, files []*os.File) error {
	if ctx == nil || ctx.Err() != nil || len(data) == 0 || len(data) > gitControlPacketBytes || len(files) > gitControlPacketFDs || gitControlPacketSocket(conn) != nil {
		return errGitUnit
	}
	fds := make([]int, len(files))
	owned := make([]*os.File, 0, len(files))
	defer func() { closeVMFiles(owned) }()
	for i, file := range files {
		duplicate, err := duplicateGitControlFile(file)
		if err != nil {
			return err
		}
		owned = append(owned, duplicate)
		fds[i], err = gitControlFileFD(duplicate)
		if err != nil {
			return err
		}
	}
	var rights []byte
	if len(fds) > 0 {
		rights = unix.UnixRights(fds...)
	}
	if conn.SetWriteDeadline(gitControlDeadline(ctx)) != nil {
		return errGitUnit
	}
	stop := gitBrokerAfterFunc(ctx, func() { conn.Close() })
	defer stop()
	n, on, err := conn.WriteMsgUnix(data, rights, nil)
	if err != nil || n != len(data) || on != len(rights) || ctx.Err() != nil {
		return errGitUnit
	}
	return nil
}
func readGitControlPacket(ctx context.Context, conn *net.UnixConn, expected int) (data []byte, files []*os.File, result error) {
	if ctx == nil || ctx.Err() != nil || expected < 0 || expected > gitControlPacketFDs || gitControlPacketSocket(conn) != nil {
		return nil, nil, errGitUnit
	}
	if conn.SetReadDeadline(gitControlDeadline(ctx)) != nil {
		return nil, nil, errGitUnit
	}
	stop := gitBrokerAfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer func() {
		if result != nil {
			closeVMFiles(files)
			files = nil
		}
	}()
	data = make([]byte, gitControlPacketBytes)
	// Receive one surplus FD so exact-count rejection exercises ordinary SCM
	// ownership too. CTRUNC rejects still larger ancillary data; Linux closes
	// descriptors that do not fit the receiving control buffer.
	oob := make([]byte, unix.CmsgSpace((gitControlPacketFDs+1)*4))
	n, on, flags, err := recvVMMessage(conn, data, oob)
	if on < 0 || on > len(oob) {
		return nil, files, errGitUnit
	}
	messages, parseErr := unix.ParseSocketControlMessage(oob[:on])
	rightsMessages := 0
	for _, message := range messages {
		if message.Header.Level == unix.SOL_SOCKET && message.Header.Type == unix.SCM_PIDFD {
			// SO_PASSPIDFD may deliver an unexpected kernel-owned descriptor.
			// Own it before rejecting the ancillary type, just like rights.
			for at := 0; at+4 <= len(message.Data); at += 4 {
				files = append(files, os.NewFile(uintptr(binary.NativeEndian.Uint32(message.Data[at:at+4])), "sfl-git-unexpected-pidfd"))
			}
			parseErr = errGitUnit
			continue
		}
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SCM_RIGHTS {
			parseErr = errGitUnit
			continue
		}
		rightsMessages++
		fds, rightsErr := unix.ParseUnixRights(&message)
		if rightsErr != nil {
			parseErr = rightsErr
			continue
		}
		for _, fd := range fds {
			files = append(files, os.NewFile(uintptr(fd), "sfl-git-control"))
		}
	}
	if err != nil || parseErr != nil || n <= 0 || n > len(data) || flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 || len(files) != expected || rightsMessages > 1 || ctx.Err() != nil {
		return nil, files, errGitUnit
	}
	for _, file := range files {
		fd, e := gitControlFileFD(file)
		if e != nil {
			return nil, files, errGitUnit
		}
		flags, e := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if e != nil || flags&unix.FD_CLOEXEC == 0 {
			return nil, files, errGitUnit
		}
	}
	return data[:n], files, nil
}
