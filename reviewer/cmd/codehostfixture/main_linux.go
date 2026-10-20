//go:build linux

// codehostfixture is a fixed offline relay for the pinned Codex code-mode host.
// It is not a provider launcher or an auth/VM qualification boundary.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const hostHash = "8724d73ec190536233ab4628d24a071bfbcf27e5124f08fe35129f26fe48b916"
const streamLimit = 8 << 20
const marker = "sfl-codehost-synthetic-only-1\n"

// These are synthetic values created by codexfixture, never actual credentials.
var forbidden = []string{"sfl-fake-access", "sfl-fake-refresh", "sfl-fake-account", "fixture@example.invalid"}

type streamObservation struct {
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	SecretSeen bool   `json:"synthetic_secret_seen"`
	Capped     bool   `json:"capped"`
}
type observation struct {
	Version                     int               `json:"version"`
	Passed                      bool              `json:"passed"`
	Input                       streamObservation `json:"input"`
	Output                      streamObservation `json:"output"`
	ProbePassed                 bool              `json:"probe_passed"`
	FakeOnly                    bool              `json:"fake_only"`
	RelayFailure                bool              `json:"relay_failure"`
	BwrapSHA256                 string            `json:"bwrap_sha256"`
	ProductionBoundaryQualified bool              `json:"production_boundary_qualified"`
}

// Scan overlapping chunks so a canary split across read boundaries cannot hide.
type canaryWriter struct {
	hash  io.Writer
	tail  []byte
	seen  bool
	count int64
}

func (s *canaryWriter) Write(data []byte) (int, error) {
	joined := append(append([]byte{}, s.tail...), data...)
	for _, token := range forbidden {
		if bytes.Contains(joined, []byte(token)) {
			s.seen = true
		}
	}
	keep := 0
	for _, token := range forbidden {
		keep = max(keep, len(token)-1)
	}
	if len(joined) > keep {
		joined = joined[len(joined)-keep:]
	}
	s.tail = append(s.tail[:0], joined...)
	s.count += int64(len(data))
	return s.hash.Write(data)
}
func copyStream(to io.Writer, from io.Reader) (streamObservation, error) {
	hash := sha256.New()
	scan := &canaryWriter{hash: hash}
	// The extra byte diagnoses overflow but is never forwarded to the peer.
	read := io.LimitReader(from, streamLimit+1)
	buf := make([]byte, 32<<10)
	var err error
	capped := false
	for {
		n, e := read.Read(buf)
		if n > 0 {
			if scan.count+int64(n) > streamLimit {
				capped = true
				err = errors.New("fixture relay byte limit")
				break
			}
			scan.Write(buf[:n])
			if scan.seen {
				err = errors.New("synthetic credential in code-host stream")
				break
			}
			written, writeErr := to.Write(buf[:n])
			if writeErr != nil || written != n {
				err = io.ErrShortWrite
				break
			}
		}
		if e != nil {
			if e != io.EOF {
				err = e
			}
			break
		}
	}
	return streamObservation{Bytes: scan.count, SHA256: hex.EncodeToString(hash.Sum(nil)), SecretSeen: scan.seen, Capped: capped}, err
}
func pinned(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 80<<20))
	return err == nil && n > 0 && hex.EncodeToString(h.Sum(nil)) == hostHash
}

const hello = "SFL_CODEHOST_STDIO_2\n"
const ready = "SFL_CODEHOST_READY_2\n"
const controlHello = "SFL_CODEHOST_CONTROL_2\n"
const bwrapProgram = "/usr/bin/bwrap"

func main() {
	if len(os.Args) == 6 && os.Args[1] == "--broker" {
		runBroker(os.Args[2], os.Args[3], os.Args[4], os.Args[5])
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "--child" {
		child(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) != 1 {
		os.Exit(2)
	}
	raw, err := os.ReadFile("/run/sfl-codehost-fixture")
	if err != nil || string(raw) != marker {
		os.Exit(2)
	}
	// The CLI may kill its relay during normal teardown. Completion evidence
	// belongs to the separately supervised broker, outside the client mount.
	result := relay()
	if !result.Passed {
		os.Exit(1)
	}
}
func relay() observation {
	result := observation{Version: 2, FakeOnly: true}
	conn, err := net.DialTimeout("unix", "/codehost/stdio.sock", 5*time.Second)
	if err != nil {
		return result
	}
	defer conn.Close()
	control, err := net.DialTimeout("unix", "/codehost/control.sock", 5*time.Second)
	if err != nil {
		return result
	}
	defer control.Close()
	control.SetDeadline(time.Now().Add(40 * time.Second))
	if _, err = io.WriteString(control, controlHello); err != nil {
		return result
	}
	conn.SetDeadline(time.Now().Add(40 * time.Second))
	if _, err = io.WriteString(conn, hello); err != nil {
		return result
	}
	response := make([]byte, len(ready))
	if _, err = io.ReadFull(conn, response); err != nil || string(response) != ready {
		return result
	}
	streamed := relayStreams(conn.(*net.UnixConn), control.(*net.UnixConn), os.Stdin, os.Stdout)
	streamed.ProbePassed = true
	return streamed
}

func relayStreams(data, control *net.UnixConn, inputSource io.ReadCloser, outputSink io.WriteCloser) observation {
	result := observation{Version: 2, FakeOnly: true}
	var failOnce sync.Once
	fail := func() {
		failOnce.Do(func() { control.SetWriteDeadline(time.Now().Add(time.Second)); control.Write([]byte{1}) })
	}
	type copied struct {
		value streamObservation
		err   error
	}
	input := make(chan copied, 1)
	go func() {
		v, e := copyStream(data, inputSource)
		if e != nil && !errors.Is(e, os.ErrClosed) {
			fail()
		}
		data.CloseWrite()
		input <- copied{v, e}
	}()
	var err error
	result.Output, err = copyStream(outputSink, data)
	if err != nil {
		fail()
	}
	outputSink.Close()
	inputSource.Close()
	var in copied
	select {
	case in = <-input:
	case <-time.After(time.Second):
		in.err = errors.New("input did not stop")
		fail()
	}
	result.Input = in.value
	result.Passed = err == nil && (in.err == nil || errors.Is(in.err, os.ErrClosed)) && !in.value.SecretSeen && !result.Output.SecretSeen
	return result
}

func runBroker(socket, host, probe, receipt string) {
	result := observation{Version: 2, FakeOnly: true}
	code := 1
	defer func() {
		raw, _ := json.MarshalIndent(result, "", "  ")
		if os.WriteFile(receipt, append(raw, '\n'), 0600) != nil {
			code = 1
		}
		os.Exit(code)
	}()
	if os.Getenv("SFL_FAKE_CODEHOST_BROKER") != "1" || !filepath.IsAbs(socket) || !filepath.IsAbs(host) || !filepath.IsAbs(probe) || !filepath.IsAbs(receipt) || filepath.Dir(host) != filepath.Dir(probe) || filepath.Dir(host) != filepath.Dir(receipt) || !pinned(host) {
		return
	}
	self, err := os.Executable()
	if err != nil || self != probe {
		return
	}
	parent, err := os.Stat(filepath.Dir(socket))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return
	}
	controlListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(filepath.Dir(socket), "control.sock"), Net: "unix"})
	if err != nil {
		return
	}
	defer controlListener.Close()
	if os.Chmod(controlListener.Addr().String(), 0600) != nil {
		return
	}
	controlListener.SetDeadline(time.Now().Add(45 * time.Second))
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return
	}
	defer listener.Close()
	if os.Chmod(socket, 0600) != nil {
		return
	}
	listener.SetDeadline(time.Now().Add(45 * time.Second))
	conn, err := listener.AcceptUnix()
	if err != nil {
		return
	}
	defer conn.Close()
	if acceptBrokerPeer(conn, uint32(os.Getuid())) != nil {
		return
	}
	control, err := controlListener.AcceptUnix()
	if err != nil {
		return
	}
	defer control.Close()
	if acceptBrokerControl(conn, control) != nil {
		return
	}
	result = brokerExchange(conn, control, host, probe, filepath.Join(filepath.Dir(receipt), "codehost-stderr.txt"))
	if result.Passed {
		code = 0
	}
}

func acceptBrokerPeer(conn *net.UnixConn, uid uint32) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	peerOK := false
	if rawConn.Control(func(fd uintptr) {
		peer, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		peerOK = e == nil && peer.Uid == uid && peer.Pid > 0
	}) != nil || !peerOK {
		return errors.New("fixture broker peer denied")
	}
	conn.SetDeadline(time.Now().Add(40 * time.Second))
	request := make([]byte, len(hello))
	if _, err = io.ReadFull(conn, request); err != nil || string(request) != hello {
		return errors.New("fixture broker prelude denied")
	}
	return nil
}

func socketPeer(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var peer *unix.Ucred
	var lookup error
	err = raw.Control(func(fd uintptr) { peer, lookup = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil {
		return nil, err
	}
	return peer, lookup
}
func acceptBrokerControl(data, control *net.UnixConn) error {
	a, err := socketPeer(data)
	if err != nil {
		return err
	}
	b, err := socketPeer(control)
	if err != nil || a.Pid != b.Pid || a.Uid != b.Uid || a.Gid != b.Gid {
		return errors.New("fixture control peer denied")
	}
	control.SetDeadline(time.Now().Add(40 * time.Second))
	prelude := make([]byte, len(controlHello))
	if _, err = io.ReadFull(control, prelude); err != nil || string(prelude) != controlHello {
		return errors.New("fixture control prelude denied")
	}
	return nil
}

// EOF is neutral because the CLI may kill its relay during normal teardown.
// Any explicit failure or malformed control byte prevents a passing receipt.
func relayControl(control io.Reader) error {
	var event [1]byte
	n, err := control.Read(event[:])
	if n == 0 && err == io.EOF {
		return nil
	}
	return errors.New("relay reported failure or control channel was incomplete")
}
func executableHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (80<<20)+1))
	if err != nil || n == 0 || n > 80<<20 {
		return "", errors.New("invalid executable hash input")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func brokerExchange(conn, control *net.UnixConn, host, probe, stderrPath string) (result observation) {
	result = observation{Version: 2, FakeOnly: true}
	var hashErr error
	result.BwrapSHA256, hashErr = executableHash(bwrapProgram)
	if hashErr != nil {
		return result
	}
	parentNet, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return result
	}
	parentPID, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var controlFailed atomic.Bool
	defer func() { result.RelayFailure = result.RelayFailure || controlFailed.Load() }()
	controlDone := make(chan error, 1)
	go func() {
		err := relayControl(control)
		if err != nil {
			controlFailed.Store(true)
		}
		controlDone <- err
		if err != nil {
			cancel()
		}
	}()
	// Use the installed, policy-authorized Bubblewrap executable. No host policy
	// is modified and no namespace is created by a child inside the client root.
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--clearenv", "--cap-drop", "ALL",
		"--ro-bind", host, "/code-host", "--ro-bind", probe, "/probe",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/work", "--chdir", "/work", "--remount-ro", "/",
		"--", "/probe", "--child", parentNet, parentPID}
	command := exec.CommandContext(ctx, bwrapProgram, args...)
	command.Env = []string{"LANG=C.UTF-8"}
	command.WaitDelay = time.Second
	in, err := command.StdinPipe()
	if err != nil {
		return result
	}
	out, err := command.StdoutPipe()
	if err != nil {
		in.Close()
		return result
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		in.Close()
		out.Close()
		return result
	}
	if command.Start() != nil {
		in.Close()
		out.Close()
		stderr.Close()
		return result
	}
	reaped := false
	defer func() {
		if !reaped {
			cancel()
			command.Wait()
		}
	}()
	diagnostics := &boundedBuffer{}
	probeDone := make(chan bool, 1)
	stderrDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(io.LimitReader(stderr, (64<<10)+1))
		first, e := reader.ReadBytes('\n')
		diagnostics.Write(first)
		probeDone <- e == nil && string(first) == "SFL_CODEHOST_PROBE_OK\n"
		_, e = io.Copy(diagnostics, reader)
		stderrDone <- e
	}()
	select {
	case result.ProbePassed = <-probeDone:
	case <-ctx.Done():
	}
	if !result.ProbePassed {
		cancel()
		command.Wait()
		reaped = true
		select {
		case <-stderrDone:
		case <-time.After(time.Second):
		}
		os.WriteFile(stderrPath, []byte(diagnostics.String()), 0600)
		return result
	}
	if _, err = io.WriteString(conn, ready); err != nil {
		return result
	}
	type copied struct {
		value streamObservation
		err   error
	}
	input, output := make(chan copied, 1), make(chan copied, 1)
	go func() {
		v, e := copyStream(in, conn)
		in.Close()
		input <- copied{v, e}
		if e != nil && !errors.Is(e, os.ErrClosed) {
			cancel()
		}
	}()
	go func() {
		v, e := copyStream(conn, out)
		output <- copied{v, e}
		if e != nil {
			cancel()
		}
	}()
	o := <-output
	conn.CloseWrite()
	waitErr := command.Wait()
	reaped = true
	conn.CloseRead()
	var i copied
	select {
	case i = <-input:
	case <-time.After(time.Second):
		i.err = errors.New("input did not stop")
	}
	var stderrErr error
	select {
	case stderrErr = <-stderrDone:
	case <-time.After(time.Second):
		stderrErr = errors.New("stderr did not stop")
	}
	result.Input, result.Output = i.value, o.value
	var controlErr error
	select {
	case controlErr = <-controlDone:
	case <-time.After(time.Second):
		controlErr = errors.New("relay control did not drain")
	}
	result.RelayFailure = controlErr != nil
	actualHash, hashErr := executableHash(bwrapProgram)
	if hashErr != nil || actualHash != result.BwrapSHA256 {
		return result
	}
	saved := os.WriteFile(stderrPath, []byte(diagnostics.String()), 0600) == nil
	result.Passed = controlErr == nil && waitErr == nil && o.err == nil && (i.err == nil || errors.Is(i.err, os.ErrClosed)) && stderrErr == nil && saved && !i.value.SecretSeen && !o.value.SecretSeen && ctx.Err() == nil
	return result
}

type boundedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data)+len(p) > 64<<10 {
		return 0, errors.New("stderr cap")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *boundedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

func child(parentNet, parentPID string) {
	fail := func() { fmt.Fprintln(os.Stderr, "SFL_CODEHOST_PROBE_FAILED"); os.Exit(2) }
	for _, entry := range os.Environ() {
		if entry != "PWD=/work" {
			fail()
		}
	}
	if !pinned("/code-host") {
		fail()
	}
	for _, ns := range []struct{ name, parent string }{{"net", parentNet}, {"pid", parentPID}} {
		current, err := os.Readlink("/proc/self/ns/" + ns.name)
		if err != nil || current == ns.parent {
			fail()
		}
	}
	for _, path := range []string{"/profile/codex/auth.json", "/proc/1/root/profile/codex/auth.json", "/artifacts", "/run/sfl", "/work/sfl-report.schema.json", "/home/franz", "/var/run/docker.sock", "/tmp/ssh-agent.sock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			fail()
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) == 0 {
		fail()
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			fail()
		}
	}
	var caps [2]unix.CapUserData
	if unix.Capget(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &caps[0]) != nil || caps[0].Effective != 0 || caps[1].Effective != 0 || caps[0].Permitted != 0 || caps[1].Permitted != 0 {
		fail()
	}
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		fail()
	}
	if err := os.WriteFile("/must-not-write", []byte("inert"), 0600); err == nil {
		fail()
	}
	fmt.Fprintln(os.Stderr, "SFL_CODEHOST_PROBE_OK")
	// No audit, directory, namespace or parent descriptor reaches the code host.
	if unix.CloseRange(3, ^uint(0), 0) != nil {
		fail()
	}
	if syscall.Exec("/code-host", []string{"/code-host"}, []string{"LANG=C.UTF-8", "GOMAXPROCS=1"}) != nil {
		fail()
	}
}
