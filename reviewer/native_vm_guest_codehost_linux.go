//go:build linux

package reviewer

import (
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
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// These are the v2 data/failure-control preludes qualified by codehostfixture.
// This path has a different purpose and actually requires the guest identities;
// the synthetic fixture marker cannot enable it.
const nativeCodeHello = "SFL_CODEHOST_STDIO_2\n"
const nativeCodeReady = "SFL_CODEHOST_READY_2\n"
const nativeCodeControl = "SFL_CODEHOST_CONTROL_2\n"
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

func nativeCodeControlResult(r io.Reader) error {
	var value [1]byte
	n, err := r.Read(value[:])
	// Normal client teardown can kill its relay. An explicit failure must be
	// sent before data EOF; a missing success marker never attests completion.
	if n == 0 && err == io.EOF {
		return nil
	}
	return ErrVMBoot
}

func nativeGuestCodeRelay() int {
	if !nativeGuestUnprivileged(nativeVMCodexUID) {
		return 2
	}
	data, err := net.DialTimeout("unix", "/codehost/stdio.sock", 5*time.Second)
	if err != nil {
		return 2
	}
	defer data.Close()
	control, err := net.DialTimeout("unix", "/codehost/control.sock", 5*time.Second)
	if err != nil {
		return 2
	}
	defer control.Close()
	// The peer is guest PID1 outside this PID namespace, so its PID may be 0.
	// Fixed root-owned socket mounts and UID0 establish this endpoint.
	for _, connection := range []net.Conn{data, control} {
		peer, err := nativeCodePeer(connection.(*net.UnixConn))
		if err != nil || peer.Uid != 0 {
			return 2
		}
		connection.SetDeadline(time.Now().Add(40 * time.Second))
	}
	if _, err = io.WriteString(control, nativeCodeControl); err != nil {
		return 2
	}
	if _, err = io.WriteString(data, nativeCodeHello); err != nil {
		return 2
	}
	prelude := make([]byte, len(nativeCodeReady))
	if _, err = io.ReadFull(data, prelude); err != nil || string(prelude) != nativeCodeReady {
		return 2
	}
	var failure sync.Once
	fail := func() {
		failure.Do(func() { control.SetWriteDeadline(time.Now().Add(time.Second)); control.Write([]byte{1}) })
	}
	input := make(chan nativeCodeStream, 1)
	go func() {
		copied := nativeCodeCopy(data, os.Stdin, nil)
		if copied.err != nil && !errors.Is(copied.err, os.ErrClosed) {
			fail()
		}
		data.(*net.UnixConn).CloseWrite()
		input <- copied
	}()
	out := nativeCodeCopy(os.Stdout, data, nil)
	if out.err != nil {
		fail()
	}
	os.Stdin.Close()
	select {
	case in := <-input:
		if in.err != nil && !errors.Is(in.err, os.ErrClosed) {
			fail()
			return 1
		}
	case <-time.After(time.Second):
		fail()
		return 1
	}
	if out.err != nil {
		return 1
	}
	return 0
}

type nativeGuestCodeBroker struct {
	data, control *net.UnixListener
	root          string
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
	b.control, err = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "control.sock"), Net: "unix"})
	if err != nil {
		b.close()
		return nil, ErrVMBoot
	}
	for _, name := range []string{"stdio.sock", "control.sock"} {
		path := filepath.Join(dir, name)
		if os.Chown(path, 0, nativeVMIPCGID) != nil || os.Chmod(path, 0660) != nil {
			b.close()
			return nil, ErrVMBoot
		}
	}
	return b, nil
}

func (b *nativeGuestCodeBroker) close() {
	if b != nil {
		if b.data != nil {
			b.data.Close()
		}
		if b.control != nil {
			b.control.Close()
		}
	}
}

func (b *nativeGuestCodeBroker) serve(ctx context.Context, s Session, values [][]byte) (result nativeVMCodeHostAccess) {
	result = nativeVMCodeHostAccess{Version: 1, SessionHash: Digest(s), UID: nativeVMCodeHostUID, InputHash: authHash(nil), OutputHash: authHash(nil), Failed: true, Stopped: true}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
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
	result.Stage = "accept-control"
	control, err := b.control.AcceptUnix()
	if err != nil {
		return result
	}
	defer control.Close()
	stopConnections := context.AfterFunc(ctx, func() { data.Close(); control.Close() })
	defer stopConnections()
	result.Stage = "peer"
	a, err := nativeCodePeer(data)
	if err != nil || a.Uid != nativeVMCodexUID || a.Gid != nativeVMIPCGID || a.Pid <= 0 {
		return result
	}
	c, err := nativeCodePeer(control)
	if err != nil || *a != *c {
		return result
	}
	group, err := os.ReadFile(filepath.Join("/proc", strconvNativePID(a.Pid), "cgroup"))
	if err != nil || string(group) != "0::/native-client\n" {
		return result
	}
	result.Stage = "prelude"
	for _, prelude := range []struct {
		connection *net.UnixConn
		value      string
	}{{data, nativeCodeHello}, {control, nativeCodeControl}} {
		prelude.connection.SetDeadline(time.Now().Add(40 * time.Second))
		raw := make([]byte, len(prelude.value))
		if _, err = io.ReadFull(prelude.connection, raw); err != nil || string(raw) != prelude.value {
			return result
		}
	}
	controlDone := make(chan error, 1)
	go func() {
		err := nativeCodeControlResult(control)
		controlDone <- err
		if err != nil {
			cancel()
		}
	}()
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
	result.Stage = "ready"
	if _, err = io.WriteString(data, nativeCodeReady); err != nil {
		return result
	}
	result.Stage = "exchange"
	input, output := make(chan nativeCodeStream, 1), make(chan nativeCodeStream, 1)
	go func() {
		copied := nativeCodeCopy(process.in, data, values)
		process.in.Close()
		input <- copied
		if copied.err != nil {
			cancel()
		}
	}()
	go func() {
		copied := nativeCodeCopy(data, process.out, values)
		output <- copied
		if copied.err != nil {
			cancel()
		}
	}()
	var in, out nativeCodeStream
	select {
	case out = <-output:
	case <-ctx.Done():
		return result
	}
	data.CloseWrite()
	select {
	case in = <-input:
	case <-ctx.Done():
		return result
	}
	result.InputBytes, result.InputHash = in.bytes, in.hash
	result.OutputBytes, result.OutputHash = out.bytes, out.hash
	result.Stage = "wait"
	select {
	case err = <-controlDone:
	case <-ctx.Done():
		return result
	}
	// stderr is directed to /dev/null by the trusted wrapper. No undrained
	// StderrPipe can race Wait or masquerade as a complete code-host exchange.
	select {
	case <-process.done:
		result.Failed = err != nil || process.waitErr != nil || in.err != nil || out.err != nil || ctx.Err() != nil
		if !result.Failed {
			result.Stage = "complete"
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
