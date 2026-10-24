//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

// gitFDEntry is one open descriptor number bound to the kernel object it
// names. Pipe ends share an inode, so the access mode separates them too.
type gitFDEntry struct {
	FD     int
	Dev    uint64
	Ino    uint64
	Mode   uint32
	Access int
	Target string
	PID    int
}

// gitFDMatch selects descriptors that the code under test could have received
// or created. Anything else in the process is unrelated to the assertion.
type gitFDMatch func(gitFDEntry) bool

// gitFDWatch compares descriptor sets, never the process-wide count: an
// unrelated descriptor opened in the window is outside every match, and an
// unrelated close cannot offset a retained descriptor.
type gitFDWatch struct {
	before map[gitFDEntry]bool
	scope  []gitFDMatch
}

func gitWatchFDs(t *testing.T, scope ...gitFDMatch) gitFDWatch {
	t.Helper()
	if len(scope) == 0 {
		t.Fatal("descriptor leak check has no scope")
	}
	return gitFDWatch{before: gitFDSnapshot(t), scope: scope}
}

// leaked reports in-scope descriptors open now that were not open with the
// same number and identity before the window. A number reused for a retained
// in-scope object after an unrelated close is still new.
func (w gitFDWatch) leaked(t *testing.T) []gitFDEntry {
	t.Helper()
	var leaked []gitFDEntry
	for entry := range gitFDSnapshot(t) {
		if w.before[entry] {
			continue
		}
		for _, match := range w.scope {
			if match(entry) {
				leaked = append(leaked, entry)
				break
			}
		}
	}
	slices.SortFunc(leaked, func(a, b gitFDEntry) int { return a.FD - b.FD })
	return leaked
}

func gitFDSnapshot(t *testing.T) map[gitFDEntry]bool {
	t.Helper()
	directory, err := os.Open("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	listing := int(directory.Fd())
	names, err := directory.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[gitFDEntry]bool{}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			t.Fatal("descriptor table name", name)
		}
		if fd == listing {
			continue
		}
		if entry, ok := gitFDDescribe(fd); ok {
			snapshot[entry] = true
		}
	}
	return snapshot
}

// A number closed or replaced while it is described is not a retained
// descriptor; describe only a number whose link is stable across the reads.
func gitFDDescribe(fd int) (gitFDEntry, bool) {
	link := "/proc/self/fd/" + strconv.Itoa(fd)
	for range 3 {
		target, err := os.Readlink(link)
		if err != nil {
			return gitFDEntry{}, false
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			return gitFDEntry{}, false
		}
		info, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
		if err != nil {
			return gitFDEntry{}, false
		}
		if again, err := os.Readlink(link); err != nil {
			return gitFDEntry{}, false
		} else if again != target {
			continue
		}
		entry := gitFDEntry{FD: fd, Dev: st.Dev, Ino: st.Ino, Mode: st.Mode, Target: target}
		for _, line := range strings.Split(string(info), "\n") {
			key, value, _ := strings.Cut(line, ":")
			value = strings.TrimSpace(value)
			switch {
			case key == "flags":
				flags, err := strconv.ParseInt(value, 8, 64)
				if err != nil {
					return gitFDEntry{}, false
				}
				entry.Access = int(flags) & unix.O_ACCMODE
			case key == "Pid" && target == "anon_inode:[pidfd]":
				if entry.PID, err = strconv.Atoi(value); err != nil {
					return gitFDEntry{}, false
				}
			}
		}
		return entry, true
	}
	return gitFDEntry{}, false
}

type gitFDObject struct{ dev, ino uint64 }

func gitFDObjects(objects map[gitFDObject]bool) gitFDMatch {
	return func(entry gitFDEntry) bool { return objects[gitFDObject{entry.Dev, entry.Ino}] }
}

// Same kernel object as any given file, including duplicates and SCM copies.
func gitFDFiles(t *testing.T, files ...*os.File) gitFDMatch {
	t.Helper()
	objects := map[gitFDObject]bool{}
	for _, file := range files {
		if file == nil {
			continue
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		objects[gitFDObject{st.Dev, st.Ino}] = true
	}
	return gitFDObjects(objects)
}

// Same socket as any given connection.
func gitFDConns(t *testing.T, conns ...*net.UnixConn) gitFDMatch {
	t.Helper()
	objects := map[gitFDObject]bool{}
	for _, conn := range conns {
		raw, err := conn.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		var statErr error
		if err := raw.Control(func(fd uintptr) { statErr = unix.Fstat(int(fd), &st) }); err != nil || statErr != nil {
			t.Fatal(err, statErr)
		}
		objects[gitFDObject{st.Dev, st.Ino}] = true
	}
	return gitFDObjects(objects)
}

// Same object as any given path after following links.
func gitFDPaths(t *testing.T, paths ...string) gitFDMatch {
	t.Helper()
	objects := map[gitFDObject]bool{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		objects[gitFDObject{st.Dev, st.Ino}] = true
	}
	return gitFDObjects(objects)
}

// A pidfd for any given process.
func gitFDPIDFDs(pids ...int) gitFDMatch {
	return func(entry gitFDEntry) bool { return entry.PID > 0 && slices.Contains(pids, entry.PID) }
}

// A procfs view of any given process: its directory, a file below it, or one
// of its namespaces.
func gitFDProcfs(t *testing.T, pids ...int) gitFDMatch {
	t.Helper()
	objects := map[gitFDObject]bool{}
	var prefixes []string
	for _, pid := range pids {
		directory := "/proc/" + strconv.Itoa(pid)
		prefixes = append(prefixes, directory)
		// A nondumpable process's ns directory is not listable, but each
		// namespace stays reachable to the observer that may open it.
		for _, name := range []string{"cgroup", "ipc", "mnt", "net", "pid", "pid_for_children", "time", "time_for_children", "user", "uts"} {
			info, err := os.Stat(directory + "/ns/" + name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			st := info.Sys().(*syscall.Stat_t)
			objects[gitFDObject{st.Dev, st.Ino}] = true
		}
	}
	namespaces := gitFDObjects(objects)
	return func(entry gitFDEntry) bool {
		for _, prefix := range prefixes {
			if entry.Target == prefix || strings.HasPrefix(entry.Target, prefix+"/") {
				return true
			}
		}
		return namespaces(entry)
	}
}

// A descriptor whose link names a given prefix, such as a memfd name.
func gitFDTargets(prefixes ...string) gitFDMatch {
	return func(entry gitFDEntry) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry.Target, prefix) {
				return true
			}
		}
		return false
	}
}

// The leak check sees exactly what a decoder retained: a deliberately kept
// right is reported, while unrelated descriptors opened, closed or renumbered
// in the same window neither fail the check nor offset the retained right.
func TestGitFDWatchReportsExactlyRetainedRights(t *testing.T) {
	// frame queues one hostile two-right record and watches its objects.
	frame := func(t *testing.T) (*net.UnixConn, gitFDWatch) {
		t.Helper()
		left, right := gitPacketPair(t, unix.SOCK_SEQPACKET)
		file := gitPacketFile(t)
		if _, _, err := left.WriteMsgUnix([]byte("hostile"), unix.UnixRights(int(file.Fd()), int(file.Fd())), nil); err != nil {
			t.Fatal("actual SCM producer", err)
		}
		return right, gitWatchFDs(t, gitFDFiles(t, file), gitFDConns(t, left, right))
	}
	// leakyDecode receives like the decoder but keeps the first right open.
	leakyDecode := func(t *testing.T, conn *net.UnixConn) int {
		t.Helper()
		oob := make([]byte, unix.CmsgSpace(2*4))
		_, on, _, _, err := conn.ReadMsgUnix(make([]byte, 64), oob)
		messages, parseErr := unix.ParseSocketControlMessage(oob[:on])
		if err != nil || parseErr != nil || len(messages) != 1 {
			t.Fatal("leaky receive", err, parseErr)
		}
		fds, err := unix.ParseUnixRights(&messages[0])
		if err != nil || len(fds) != 2 {
			t.Fatal("leaky rights", err, fds)
		}
		unix.Close(fds[1])
		return fds[0]
	}
	exact := func(t *testing.T, watch gitFDWatch, want ...int) {
		t.Helper()
		leaked := watch.leaked(t)
		got := []int{}
		for _, entry := range leaked {
			got = append(got, entry.FD)
		}
		if !slices.Equal(got, want) {
			t.Fatal("leak check reported", leaked, "want descriptors", want)
		}
	}
	t.Run("retained-right", func(t *testing.T) {
		conn, watch := frame(t)
		fd := leakyDecode(t, conn)
		defer unix.Close(fd)
		exact(t, watch, fd)
	})
	t.Run("decoder", func(t *testing.T) {
		conn, watch := frame(t)
		if _, files, err := readGitControlPacket(context.Background(), conn, 1); err == nil {
			closeVMFiles(files)
			t.Fatal("hostile frame accepted")
		}
		exact(t, watch)
	})
	t.Run("unrelated-open", func(t *testing.T) {
		conn, watch := frame(t)
		unrelated := gitPacketFile(t)
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		pidfd := gitTestPIDFD(t, os.Getpid())
		if _, files, err := readGitControlPacket(context.Background(), conn, 1); err == nil {
			closeVMFiles(files)
			t.Fatal("hostile frame accepted")
		}
		exact(t, watch)
		for _, file := range []*os.File{unrelated, reader, writer, pidfd} {
			if _, err := file.Stat(); err != nil {
				t.Fatal("unrelated descriptor closed before the check", err)
			}
		}
	})
	t.Run("unrelated-close", func(t *testing.T) {
		unrelated, err := unix.Open(gitPacketFile(t).Name(), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(unrelated)
		conn, watch := frame(t)
		// Replacing the unrelated descriptor with the retained right closes it
		// and reuses its number: the process-wide count is unchanged.
		fd := leakyDecode(t, conn)
		err = unix.Dup3(fd, unrelated, unix.O_CLOEXEC)
		unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		exact(t, watch, unrelated)
	})
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
			watch := gitWatchFDs(t, gitFDFiles(t, file), gitFDConns(t, left, right), gitFDPIDFDs(os.Getpid()))
			result, received, err := readGitControlPacket(context.Background(), right, expected)
			if err == nil || result != nil || len(received) != 0 {
				closeVMFiles(received)
				t.Fatal("hostile frame accepted")
			}
			if leaked := watch.leaked(t); len(leaked) != 0 {
				t.Fatal("rejected frame leaked descriptors", leaked)
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
