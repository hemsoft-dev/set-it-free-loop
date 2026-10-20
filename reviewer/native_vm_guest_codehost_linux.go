//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Protocol 3 donates anonymous stdio pipes. The trusted broker owns all
// forwarding and completion checks; launcher death cannot erase its evidence.
const nativeCodeHandoff = "SFL_CODEHOST_PIPES_3\n"
const nativeCodeLimit = 8 << 20

type nativeCodeStream struct {
	bytes int64
	hash  string
	err   error
}

// Inspect overlapping chunks before forwarding. Canary copies stay in guest
// PID1; neither the relay nor the actual code host receives them.
func nativeCodeCopy(to io.Writer, from io.Reader, values [][]byte) nativeCodeStream {
	hash := sha256.New()
	result := nativeCodeStream{}
	var tail []byte
	keep := 0
	for _, value := range values {
		keep = max(keep, len(value)-1)
	}
	defer func() { clear(tail) }()
	buffer := make([]byte, 32<<10)
	defer clear(buffer)
	for {
		n, err := from.Read(buffer)
		if n > 0 {
			if result.bytes+int64(n) > nativeCodeLimit {
				result.err = ErrVMBoot
				break
			}
			joined := append(append([]byte{}, tail...), buffer[:n]...)
			secret := nativeCredentialCanarySeen(joined, values)
			clear(tail)
			if len(joined) > keep {
				tail = append([]byte{}, joined[len(joined)-keep:]...)
			} else {
				tail = append([]byte{}, joined...)
			}
			clear(joined)
			if secret {
				result.err = ErrVMBoot
				break
			}
			written, writeErr := to.Write(buffer[:n])
			if written > 0 && written <= n {
				hash.Write(buffer[:written])
				result.bytes += int64(written)
			}
			if writeErr != nil || written != n {
				result.err = ErrVMBoot
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				if errors.Is(err, os.ErrClosed) {
					result.err = os.ErrClosed
				} else {
					result.err = ErrVMBoot
				}
			}
			break
		}
		if n == 0 {
			result.err = ErrVMBoot
			break
		}
	}
	result.hash = hex.EncodeToString(hash.Sum(nil))
	return result
}

// Every endpoint belongs to this trusted task. Cancellation closes its owned
// pipes and both copy goroutines finish before private canaries can be cleared.
func nativeCodeForward(parent context.Context, source io.ReadCloser, sink io.WriteCloser, childInput io.WriteCloser, childOutput io.ReadCloser, values [][]byte) (nativeCodeStream, nativeCodeStream) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		source.Close()
		sink.Close()
		childInput.Close()
		childOutput.Close()
	})
	defer stop()
	input, output := make(chan nativeCodeStream, 1), make(chan nativeCodeStream, 1)
	go func() {
		copied := nativeCodeCopy(childInput, source, values)
		if err := childInput.Close(); err != nil && copied.err == nil {
			copied.err = ErrVMBoot
		}
		source.Close()
		input <- copied
		if copied.err != nil {
			cancel()
		}
	}()
	go func() {
		copied := nativeCodeCopy(sink, childOutput, values)
		// Propagate child EOF even while the client still holds its input open.
		// Otherwise both sides can wait for the other during worker shutdown.
		if err := sink.Close(); err != nil && copied.err == nil {
			copied.err = ErrVMBoot
		}
		childOutput.Close()
		output <- copied
		if copied.err != nil {
			cancel()
		}
	}()
	in, out := <-input, <-output
	if parent.Err() != nil && out.err == nil {
		out.err = ErrVMBoot
	}
	return in, out
}

func nativeCodePeer(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, ErrVMBoot
	}
	var peer *unix.Ucred
	var lookup error
	if raw.Control(func(fd uintptr) { peer, lookup = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil || lookup != nil || peer == nil {
		return nil, ErrVMBoot
	}
	return peer, nil
}

func nativeCodePipes(files []*os.File) error {
	if len(files) != 2 {
		return ErrVMBoot
	}
	seen := map[[2]uint64]bool{}
	for i, file := range files {
		var st unix.Stat_t
		if file == nil || unix.Fstat(int(file.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFIFO {
			return ErrVMBoot
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		want := unix.O_RDONLY
		if i == 1 {
			want = unix.O_WRONLY
		}
		link, linkErr := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(file.Fd())))
		key := [2]uint64{uint64(st.Dev), st.Ino}
		if err != nil || flags&unix.O_ACCMODE != want || linkErr != nil || link != "pipe:["+strconv.FormatUint(st.Ino, 10)+"]" || seen[key] {
			return ErrVMBoot
		}
		seen[key] = true
	}
	return nil
}

func receiveNativeCodePipes(conn *net.UnixConn) ([]*os.File, error) {
	raw, files, err := receiveVMDescriptorFrame(conn, 2)
	if err != nil || !bytes.Equal(raw, []byte(nativeCodeHandoff)) || nativeCodePipes(files) != nil {
		closeVMFiles(files)
		return nil, ErrVMBoot
	}
	// Rust's donated pipe descriptions may be blocking. Adopt them as Go
	// pollable files so cancellation can interrupt a blocked copy by closing
	// its owned endpoint. Only the launcher and broker share these pipe ends.
	for index, file := range files {
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			closeVMFiles(files)
			return nil, ErrVMBoot
		}
		if unix.SetNonblock(fd, true) != nil {
			unix.Close(fd)
			closeVMFiles(files)
			return nil, ErrVMBoot
		}
		files[index] = os.NewFile(uintptr(fd), "owned-codehost-pipe")
		file.Close()
	}
	return files, nil
}

func nativeGuestCodeRelay() int {
	if !nativeGuestUnprivileged(nativeVMCodexUID) || nativeCodePipes([]*os.File{os.Stdin, os.Stdout}) != nil {
		return 2
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: "/codehost/stdio.sock", Net: "unix"})
	if err != nil {
		return 2
	}
	defer connection.Close()
	peer, err := nativeCodePeer(connection)
	if err != nil || peer.Uid != 0 {
		return 2
	}
	connection.SetDeadline(time.Now().Add(DefaultLimits().Timeout))
	rights := unix.UnixRights(0, 1)
	n, on, err := connection.WriteMsgUnix([]byte(nativeCodeHandoff), rights, nil)
	if err != nil || n != len(nativeCodeHandoff) || on != len(rights) || connection.CloseWrite() != nil {
		return 2
	}
	// This process performs no forwarding. The broker retains the pipes even
	// if the CLI kills its launcher during normal teardown.
	var result [1]byte
	if _, err = io.ReadFull(connection, result[:]); err != nil || result[0] != 0 {
		return 1
	}
	return 0
}

type nativeGuestCodeBroker struct {
	data *net.UnixListener
	root string
}

func newNativeGuestCodeBroker(root string) (*nativeGuestCodeBroker, error) {
	if os.Getpid() != 1 || os.Getuid() != 0 || root != "/run/native/codehost" {
		return nil, ErrLiveBlocked
	}
	dir := "/run/native/code-rpc"
	if os.Mkdir(dir, 0710) != nil || os.Chown(dir, 0, nativeVMIPCGID) != nil {
		return nil, ErrVMBoot
	}
	b := &nativeGuestCodeBroker{root: root}
	var err error
	b.data, err = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "stdio.sock"), Net: "unix"})
	if err != nil {
		return nil, ErrVMBoot
	}
	path := filepath.Join(dir, "stdio.sock")
	if os.Chown(path, 0, nativeVMIPCGID) != nil || os.Chmod(path, 0660) != nil {
		b.close()
		return nil, ErrVMBoot
	}
	return b, nil
}

func (b *nativeGuestCodeBroker) close() {
	if b != nil {
		if b.data != nil {
			b.data.Close()
		}
	}
}

func (b *nativeGuestCodeBroker) serve(ctx context.Context, s Session, values [][]byte) (result nativeVMCodeHostAccess) {
	result = nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(s), UID: nativeVMCodeHostUID, InputHash: authHash(nil), OutputHash: authHash(nil), Failed: true, Stopped: true}
	ctx, cancel := context.WithTimeout(ctx, s.Limits.Timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, b.close)
	defer stop()
	defer b.close()
	result.Stage = "accept-data"
	data, err := b.data.AcceptUnix()
	if err != nil {
		return result
	}
	defer data.Close()
	stopConnection := context.AfterFunc(ctx, func() { data.Close() })
	defer stopConnection()
	data.SetDeadline(time.Now().Add(s.Limits.Timeout))
	result.Stage = "peer"
	a, err := nativeCodePeer(data)
	if err != nil || a.Uid != nativeVMCodexUID || a.Gid != nativeVMIPCGID || a.Pid <= 0 {
		return result
	}
	group, err := os.ReadFile(filepath.Join("/proc", strconvNativePID(a.Pid), "cgroup"))
	if err != nil || string(group) != "0::/native-client\n" {
		return result
	}
	result.Stage = "prelude"
	pipes, err := receiveNativeCodePipes(data)
	if err != nil {
		return result
	}
	defer closeVMFiles(pipes)
	stopPipes := context.AfterFunc(ctx, func() { closeVMFiles(pipes) })
	defer stopPipes()
	result.Stage = "spawn"
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return result
	}
	defer readyRead.Close()
	process, err := startNativeGuestRole(ctx, "codehost", b.root, 0, []*os.File{readyWrite}, "--native-codehost")
	readyWrite.Close()
	if err != nil {
		return result
	}
	result.Stopped = false
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		result.Stopped = process.Stop(cleanup) == nil
		process.out.Close()
		if !result.Stopped {
			result.Failed = true
		}
	}()
	stopReady := context.AfterFunc(ctx, func() { readyRead.Close() })
	defer stopReady()
	var observed [1]byte
	result.Stage = "probe"
	if _, err = io.ReadFull(readyRead, observed[:]); err != nil || observed[0] != 1 {
		return result
	}
	result.Stage = "exchange"
	in, out := nativeCodeForward(ctx, pipes[0], pipes[1], process.in, process.out, values)
	result.InputBytes, result.InputHash = in.bytes, in.hash
	result.OutputBytes, result.OutputHash = out.bytes, out.hash
	result.Stage = "wait"
	// stderr is directed to /dev/null by the trusted wrapper. No undrained
	// StderrPipe can race Wait or masquerade as a complete code-host exchange.
	select {
	case <-process.done:
		result.RelayComplete = process.waitErr == nil && in.err == nil && out.err == nil && ctx.Err() == nil
		result.Failed = !result.RelayComplete
		if !result.Failed {
			result.Stage = "complete"
			// Acknowledgment is advisory; only these owned forwarding tasks and
			// the pinned child exit establish completion in the private audit.
			data.Write([]byte{0})
		}
	case <-ctx.Done():
	}
	return result
}

// Root namespace wrapper mounts only its own proc, then runs a credential-free
// UID1203 child. The child closes its private ready FD before executing the pin.
func nativeGuestCodeHostMain() int {
	if os.Getpid() != 1 || os.Getuid() != 0 || unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "") != nil || unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") != nil || unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return 2
	}
	ready := os.NewFile(3, "private-codehost-ready")
	if ready == nil {
		return 2
	}
	defer ready.Close()
	command := exec.Command("/native-init", "--native-codehost-child")
	command.Dir = "/work"
	command.Env = []string{"LANG=C.UTF-8", "GOMAXPROCS=1"}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, io.Discard
	command.ExtraFiles = []*os.File{ready}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nativeVMCodeHostUID, Gid: nativeVMIPCGID, Groups: []uint32{}}, Pdeathsig: syscall.SIGKILL}
	if command.Run() != nil {
		return 1
	}
	return 0
}

func nativeGuestCodeHostChild() int {
	if !nativeGuestUnprivileged(nativeVMCodeHostUID) {
		return 2
	}
	for _, value := range os.Environ() {
		if value != "LANG=C.UTF-8" && value != "GOMAXPROCS=1" {
			return 2
		}
	}
	for _, path := range []string{"/profile/codex/auth.json", "/profile/claude/.credentials.json", "/proc/1/root/profile/codex/auth.json", "/run/sfl", "/run/codehost/stdio.sock", "/var/run/docker.sock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) && !errors.Is(err, os.ErrPermission) {
			return 2
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) == 0 {
		return 2
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			return 2
		}
	}
	ready := os.NewFile(3, "private-codehost-ready")
	if ready == nil {
		return 2
	}
	if n, err := ready.Write([]byte{1}); err != nil || n != 1 {
		ready.Close()
		return 2
	}
	ready.Close()
	if unix.CloseRange(3, ^uint(0), 0) != nil {
		return 2
	}
	if syscall.Exec("/codehost", []string{"/codehost"}, []string{"LANG=C.UTF-8", "GOMAXPROCS=1"}) != nil {
		return 2
	}
	return 0
}

func strconvNativePID(pid int32) string {
	// Keep proc paths fixed to a positive kernel credential, never model text.
	return strconv.Itoa(int(pid))
}
