//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"net"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func gitPacketPair(t *testing.T, kind int) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, kind|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	convert := func(fd int) *net.UnixConn {
		file := os.NewFile(uintptr(fd), "sfl-synthetic-control")
		conn, err := net.FileConn(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			t.Fatal("wrong connection")
		}
		t.Cleanup(func() { unixConn.Close() })
		return unixConn
	}
	return convert(fds[0]), convert(fds[1])
}
func gitPacketFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "inert-descriptor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("synthetic, no credential"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}
func gitPacketFDCount(t *testing.T) int {
	t.Helper()
	files, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}
func TestGitControlPacketActualRecordBoundariesAndCLOEXEC(t *testing.T) {
	left, right := gitPacketPair(t, unix.SOCK_SEQPACKET)
	file := gitPacketFile(t)
	first := bytes.Repeat([]byte("x"), gitControlPacketBytes)
	if err := writeGitControlPacket(context.Background(), left, first, []*os.File{file}); err != nil {
		t.Fatal(err)
	}
	if err := writeGitControlPacket(context.Background(), left, []byte("second"), nil); err != nil {
		t.Fatal(err)
	}
	data, files, err := readGitControlPacket(context.Background(), right, 1)
	if err != nil || !bytes.Equal(data, first) || len(files) != 1 {
		closeVMFiles(files)
		t.Fatal("actual packet changed", err)
	}
	defer closeVMFiles(files)
	want, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	got, err := files[0].Stat()
	if err != nil || !os.SameFile(want, got) || files[0].Fd() == file.Fd() {
		t.Fatal("wrong transferred descriptor", err)
	}
	flags, err := unix.FcntlInt(files[0].Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("descriptor not atomically close-on-exec")
	}
	data, none, err := readGitControlPacket(context.Background(), right, 0)
	if err != nil || string(data) != "second" || len(none) != 0 {
		closeVMFiles(none)
		t.Fatal("record boundary lost", err)
	}
}
func TestGitControlPacketPreservesPipeNonblockingMode(t *testing.T) {
	left, right := gitPacketPair(t, unix.SOCK_SEQPACKET)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	getFlags := func(file *os.File) int {
		t.Helper()
		raw, err := file.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var flags int
		var query error
		err = raw.Control(func(fd uintptr) { flags, query = unix.FcntlInt(fd, unix.F_GETFL, 0) })
		if err != nil || query != nil {
			t.Fatal(err, query)
		}
		return flags
	}
	if getFlags(writer)&unix.O_NONBLOCK == 0 {
		t.Fatal("pipe producer is not nonblocking")
	}
	if writeGitControlPacket(context.Background(), left, []byte("pipe"), []*os.File{writer}) != nil {
		t.Fatal("pipe send")
	}
	if getFlags(writer)&unix.O_NONBLOCK == 0 {
		t.Fatal("transport changed caller pipe to blocking mode")
	}
	_, files, err := readGitControlPacket(context.Background(), right, 1)
	defer closeVMFiles(files)
	if err != nil || len(files) != 1 || getFlags(files[0])&unix.O_NONBLOCK == 0 {
		t.Fatal("receiver changed pipe mode", err)
	}
}
func TestGitControlPacketRejectedAncillaryAndTruncationDoNotLeak(t *testing.T) {
	for _, kind := range []string{"missing", "extra", "control-truncated", "data-truncated", "empty", "unexpected-credentials", "unexpected-pidfd"} {
		t.Run(kind, func(t *testing.T) {
			left, right := gitPacketPair(t, unix.SOCK_SEQPACKET)
			file := gitPacketFile(t)
			data := []byte("bounded fake control")
			fds := []int{int(file.Fd())}
			expected := 1
			switch kind {
			case "missing":
				fds = nil
			case "extra":
				fds = append(fds, int(file.Fd()))
			case "control-truncated":
				for range 20 {
					fds = append(fds, int(file.Fd()))
				}
			case "data-truncated":
				data = bytes.Repeat([]byte("z"), gitControlPacketBytes+1)
			case "empty":
				data = nil
			case "unexpected-credentials", "unexpected-pidfd":
				raw, err := right.SyscallConn()
				if err != nil {
					t.Fatal(err)
				}
				var setting error
				err = raw.Control(func(fd uintptr) {
					option := unix.SO_PASSCRED
					if kind == "unexpected-pidfd" {
						option = unix.SO_PASSPIDFD
					}
					setting = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, option, 1)
				})
				if err != nil || setting != nil {
					t.Fatal("kernel ancillary fixture unavailable", err, setting)
				}
			}
			var rights []byte
			if len(fds) > 0 {
				rights = unix.UnixRights(fds...)
			}
			if kind == "empty" {
				// Go's WriteMsgUnix inserts a dummy byte for empty ancillary
				// writes. Use the actual seqpacket syscall to produce an empty
				// record with rights instead of assuming that wrapper's framing.
				raw, err := left.SyscallConn()
				if err != nil {
					t.Fatal(err)
				}
				var sent uintptr
				var sendErr unix.Errno
				message := unix.Msghdr{Control: &rights[0]}
				message.SetControllen(len(rights))
				err = raw.Control(func(fd uintptr) {
					sent, _, sendErr = unix.Syscall(unix.SYS_SENDMSG, fd, uintptr(unsafe.Pointer(&message)), unix.MSG_NOSIGNAL)
				})
				runtime.KeepAlive(rights)
				if err != nil || sendErr != 0 || sent != 0 {
					t.Fatal("empty kernel SCM producer", err, sendErr, sent)
				}
			} else if _, _, err := left.WriteMsgUnix(data, rights, nil); err != nil {
				t.Fatal("actual SCM producer", err)
			}
			before := gitPacketFDCount(t)
			result, received, err := readGitControlPacket(context.Background(), right, expected)
			if err == nil || result != nil || len(received) != 0 {
				closeVMFiles(received)
				t.Fatal("hostile frame accepted")
			}
			if after := gitPacketFDCount(t); after != before {
				t.Fatal("rejected frame leaked descriptors", before, after)
			}
			if _, err := file.Stat(); err != nil {
				t.Fatal("sender descriptor closed", err)
			}
		})
	}
}
func TestGitControlPacketCancellationAndInvalidInputs(t *testing.T) {
	left, right := gitPacketPair(t, unix.SOCK_SEQPACKET)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, files, err := readGitControlPacket(ctx, right, 0); closeVMFiles(files); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled receive accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("receive did not stop")
	}
	for _, n := range []int{-1, gitControlPacketFDs + 1} {
		if _, files, err := readGitControlPacket(context.Background(), left, n); err == nil {
			closeVMFiles(files)
			t.Fatal("invalid descriptor count")
		}
	}
	if _, files, err := readGitControlPacket(nil, left, 0); err == nil {
		closeVMFiles(files)
		t.Fatal("nil context accepted")
	}
	for _, data := range [][]byte{nil, bytes.Repeat([]byte("a"), gitControlPacketBytes+1)} {
		if writeGitControlPacket(context.Background(), left, data, nil) == nil {
			t.Fatal("invalid outgoing record")
		}
	}
	if writeGitControlPacket(context.Background(), left, []byte("x"), []*os.File{nil}) == nil {
		t.Fatal("nil descriptor accepted")
	}
	stream, _ := gitPacketPair(t, unix.SOCK_STREAM)
	if writeGitControlPacket(context.Background(), stream, []byte("x"), nil) == nil {
		t.Fatal("stream channel accepted")
	}
	if _, files, err := readGitControlPacket(context.Background(), stream, 0); err == nil {
		closeVMFiles(files)
		t.Fatal("stream receive accepted")
	}
}
