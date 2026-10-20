//go:build linux

// codehostfixture is a fixed offline relay for the pinned Codex code-mode host.
// It is not a provider launcher or an auth/VM qualification boundary.
package main

import (
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
	"strings"
	"sync"
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
func main() {
	if len(os.Args) == 4 && os.Args[1] == "--child" {
		child(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) != 1 {
		os.Exit(2)
	}
	raw, err := os.ReadFile("/run/sfl-codehost-fixture")
	if err != nil || string(raw) != marker || !pinned("/code-host-pinned") {
		os.Exit(2)
	}
	result := relay()
	raw, _ = json.MarshalIndent(result, "", "  ")
	if os.WriteFile("/artifacts/codehost-relay.json", append(raw, '\n'), 0600) != nil || !result.Passed {
		os.Exit(1)
	}
}
func relay() observation {
	result := observation{Version: 1, FakeOnly: true}
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
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--clearenv", "--cap-drop", "ALL",
		"--ro-bind", "/code-host-pinned", "/code-host", "--ro-bind", "/code-host-relay", "/probe",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/work", "--chdir", "/work", "--remount-ro", "/",
		"--", "/probe", "--child", parentNet, parentPID}
	command := exec.CommandContext(ctx, "/usr/bin/bwrap", args...)
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
	stderr := &boundedBuffer{}
	command.Stderr = stderr
	if command.Start() != nil {
		in.Close()
		out.Close()
		return result
	}
	// Input may remain open until the CLI sees output EOF. Do not wait for its
	// EOF after the child exits; close this relay's stdin to release the copier.
	type copied struct {
		value streamObservation
		err   error
	}
	input, output := make(chan copied, 1), make(chan copied, 1)
	go func() {
		v, e := copyStream(in, os.Stdin)
		in.Close()
		input <- copied{v, e}
		if e != nil && !errors.Is(e, os.ErrClosed) {
			cancel()
		}
	}()
	go func() {
		v, e := copyStream(os.Stdout, out)
		output <- copied{v, e}
		if e != nil {
			cancel()
		}
	}()
	o := <-output
	waitErr := command.Wait()
	os.Stdin.Close()
	var i copied
	select {
	case i = <-input:
	case <-time.After(time.Second):
		i.err = errors.New("input did not stop")
	}
	result.Input, result.Output = i.value, o.value
	if os.WriteFile("/artifacts/codehost-stderr.txt", []byte(stderr.String()), 0600) != nil {
		return result
	}
	result.ProbePassed = strings.HasPrefix(stderr.String(), "SFL_CODEHOST_PROBE_OK\n")
	// Closing relay stdin after a clean host exit is expected, not a peer failure.
	inputOK := i.err == nil || errors.Is(i.err, os.ErrClosed)
	result.Passed = waitErr == nil && o.err == nil && inputOK && result.ProbePassed && !i.value.SecretSeen && !o.value.SecretSeen && ctx.Err() == nil
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
