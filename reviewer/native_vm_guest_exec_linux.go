//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const nativeDenyExecFrame = "SFL_DENY_EXEC_1\n"
const nativeDenyExecSocket = "/codehost/deny-exec.sock"

// NativeVMDenyShell never inspects command arguments, stdin or credentials and
// never starts another program. Every path returns shell denial status 126.
func NativeVMDenyShell() int {
	if !nativeGuestUnprivileged(nativeVMCodexUID) {
		return 126
	}
	return nativeDenyExec(nativeDenyExecSocket)
}

func nativeDenyExec(socket string) int {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return 126
	}
	defer conn.Close()
	peer, err := nativeCodePeer(conn.(*net.UnixConn))
	if err != nil || peer.Uid != 0 {
		return 126
	}
	return nativeDenyExecExchange(conn.(*net.UnixConn))
}

func nativeDenyExecExchange(conn *net.UnixConn) int {
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, nativeDenyExecFrame); err != nil {
		return 126
	}
	conn.CloseWrite()
	var ack [1]byte
	_, _ = io.ReadFull(conn, ack[:])
	return 126
}

// Socket credentials alone prove no executable identity. SO_PEERPIDFD binds
// liveness to the original connector while proc supplies its fixed executable,
// helper entry name and client cgroup. The helper waits for our acknowledgment.
func nativeDenyExecPeer(conn *net.UnixConn, helper string) bool {
	return nativeDenyExecPeerMatches(conn, helper, nativeVMCodexUID, nativeVMIPCGID, "0::/native-client\n")
}

func nativeDenyExecPeerMatches(conn *net.UnixConn, helper string, uid, gid uint32, cgroup string) bool {
	peer, err := nativeCodePeer(conn)
	if err != nil || peer.Uid != uid || peer.Gid != gid || peer.Pid <= 0 {
		return false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	pidfd, getErr := -1, error(nil)
	if raw.Control(func(fd uintptr) { pidfd, getErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD) }) != nil || getErr != nil {
		return false
	}
	defer unix.Close(pidfd)
	unix.CloseOnExec(pidfd)
	root := filepath.Join("/proc", strconv.Itoa(int(peer.Pid)))
	exe, err := os.Stat(filepath.Join(root, "exe"))
	pin, pinErr := os.Stat(helper)
	if err != nil || pinErr != nil || !os.SameFile(exe, pin) {
		return false
	}
	group, err := os.ReadFile(filepath.Join(root, "cgroup"))
	if err != nil || string(group) != cgroup {
		return false
	}
	args, err := os.Open(filepath.Join(root, "cmdline"))
	if err != nil {
		return false
	}
	// Read only argv[0]. Never retain candidate command text in an observation.
	entry := make([]byte, len("/bin/sh")+1)
	_, err = io.ReadFull(args, entry)
	args.Close()
	if err != nil || !bytes.Equal(entry, []byte("/bin/sh\x00")) {
		return false
	}
	poll := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	n, err := unix.Poll(poll, 0)
	return err == nil && n == 0
}

type nativeGuestExecObserver struct {
	listener *net.UnixListener
	result   NativeVMExecObservation
	done     chan struct{}
	closing  atomic.Bool
}

func newNativeGuestExecObserver(q nativeVMRequest, release nativeGuestRelease) (*nativeGuestExecObserver, error) {
	if os.Getpid() != 1 || os.Getuid() != 0 || !q.valid() || q.Session.Identity.Provider != Codex || !release.valid() {
		return nil, ErrLiveBlocked
	}
	var hash string
	for _, file := range release.Files {
		if file.Source == "/native-init" && file.Target == "/bin/sh" {
			hash = file.SHA256
		}
	}
	if hash == "" {
		return nil, ErrVMBoot
	}
	path := "/run/native/code-rpc/deny-exec.sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, ErrVMBoot
	}
	if os.Chown(path, 0, nativeVMIPCGID) != nil || os.Chmod(path, 0660) != nil {
		listener.Close()
		return nil, ErrVMBoot
	}
	o := &nativeGuestExecObserver{listener: listener, result: NativeVMExecObservation{Version: 1, Binding: q.Binding, RequestHash: Digest(q), ImageSourceHead: release.SourceHead, HelperSHA256: hash, HelperEntry: "sh-deny-v1", UID: nativeVMCodexUID}, done: make(chan struct{})}
	go func() { defer close(o.done); o.serve() }()
	return o, nil
}

func (o *nativeGuestExecObserver) serve() {
	defer o.listener.Close()
	for {
		conn, err := o.listener.AcceptUnix()
		if err != nil {
			if !o.closing.Load() {
				o.result.Failed = true
			}
			return
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		ok := o.observe(conn, nativeDenyExecPeer(conn, "/native-init"))
		conn.Close()
		if !ok {
			o.result.Failed = true
			return
		}
	}
}

func (o *nativeGuestExecObserver) observe(conn *net.UnixConn, authenticated bool) bool {
	if !authenticated || o.result.Attempts == 64 {
		return false
	}
	frame, err := io.ReadAll(io.LimitReader(conn, int64(len(nativeDenyExecFrame)+1)))
	if err != nil || !bytes.Equal(frame, []byte(nativeDenyExecFrame)) {
		return false
	}
	o.result.Attempts++
	o.result.Denied++ // Pinned helper has no execution path for arguments.
	_, err = conn.Write([]byte{0})
	// A failed acknowledgement cannot erase measured helper entry.
	return err == nil
}

// Finish only after the whole client cgroup stopped. No zero-count assertion
// claims to cover an exec that failed before the helper could connect.
func (o *nativeGuestExecObserver) finish(ctx context.Context, stopped bool) *NativeVMExecObservation {
	if o == nil {
		return nil
	}
	o.closing.Store(true)
	o.listener.Close()
	select {
	case <-o.done:
		o.result.Complete = stopped
		result := o.result
		return &result
	case <-ctx.Done():
		return nil
	}
}
