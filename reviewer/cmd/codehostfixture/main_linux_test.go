//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type chunkReader struct{ chunks [][]byte }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	data := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, data), nil
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }
func TestRelayCopiesOpaqueNativeBytesAndRejectsSplitCredentialCanary(t *testing.T) {
	raw := []byte{0, 255, 0, 1, 2, 3, 10}
	var output bytes.Buffer
	got, err := copyStream(&output, bytes.NewReader(raw))
	if err != nil || !bytes.Equal(output.Bytes(), raw) || got.Bytes != int64(len(raw)) || got.SecretSeen || got.Capped {
		t.Fatal(got, err)
	}
	for _, token := range forbidden {
		for split := 1; split < len(token); split++ {
			output.Reset()
			got, err = copyStream(&output, &chunkReader{chunks: [][]byte{[]byte(token[:split]), []byte(token[split:])}})
			if err == nil || !got.SecretSeen || strings.Contains(output.String(), token) {
				t.Fatal("split secret forwarded", token, split, got, err)
			}
		}
	}
}
func TestRelayEnforcesByteLimitAndWriteFailure(t *testing.T) {
	got, err := copyStream(io.Discard, strings.NewReader(strings.Repeat("x", streamLimit)))
	if err != nil || got.Capped || got.Bytes != streamLimit {
		t.Fatal(got, err)
	}
	got, err = copyStream(io.Discard, strings.NewReader(strings.Repeat("x", streamLimit+1)))
	if err == nil || !got.Capped || got.Bytes > streamLimit {
		t.Fatal(got, err)
	}
	got, err = copyStream(brokenWriter{}, strings.NewReader("opaque"))
	if err == nil || got.Capped {
		t.Fatal("write failure hidden or mislabeled", got, err)
	}
}
func TestChildAndRelayRefuseOrdinaryHostExecution(t *testing.T) {
	if os.Getenv("SFL_TEST_CODEHOST_CHILD") == "1" {
		var args []string
		if json.Unmarshal([]byte(os.Getenv("SFL_TEST_CODEHOST_ARGS")), &args) != nil {
			os.Exit(3)
		}
		os.Args = append([]string{os.Args[0]}, args...)
		main()
		return
	}
	for _, args := range [][]string{{}, {"--child", "net:[0]", "pid:[0]"}, {"--unexpected"}} {
		c := exec.Command(os.Args[0], "-test.run=TestChildAndRelayRefuseOrdinaryHostExecution")
		data, _ := json.Marshal(args)
		c.Env = []string{"SFL_TEST_CODEHOST_CHILD=1", "SFL_TEST_CODEHOST_ARGS=" + string(data)}
		if c.Run() == nil {
			t.Fatal("host relay invocation accepted", args)
		}
	}
}

func TestBrokerPreludeAndRealPeerCredentials(t *testing.T) {
	for _, kind := range []string{"valid", "wrong-peer", "command-selector", "short-prelude"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdio.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() {
				conn, err := listener.AcceptUnix()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				uid := uint32(os.Getuid())
				if kind == "wrong-peer" {
					uid++
				}
				done <- acceptBrokerPeer(conn, uid)
			}()
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			prelude := hello
			if kind == "command-selector" {
				prelude = `{"exec":"/bin/sh"}` + "\n"
			}
			if kind == "short-prelude" {
				prelude = hello[:5]
			}
			conn.Write([]byte(prelude))
			conn.CloseWrite()
			select {
			case err := <-done:
				if (err == nil) != (kind == "valid") {
					t.Fatal(kind, err)
				}
			case <-time.After(time.Second):
				t.Fatal("broker prelude did not terminate")
			}
		})
	}
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }
func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(time.Second))
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.AcceptUnix()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	client.SetDeadline(time.Now().Add(3 * time.Second))
	server.SetDeadline(time.Now().Add(3 * time.Second))
	return client, server
}

func TestRelayFailureReachesBrokerBeforeTruncatedInputEOF(t *testing.T) {
	for _, kind := range []string{"canary", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			data, serverData := unixPair(t)
			control, serverControl := unixPair(t)
			raw := forbidden[0]
			if kind == "overflow" {
				raw = strings.Repeat("x", streamLimit+1)
			}
			copied := make(chan error, 1)
			controlled := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, serverData); copied <- err }()
			go func() { err := relayControl(serverControl); controlled <- err; serverData.CloseWrite() }()
			result := relayStreams(data, control, io.NopCloser(strings.NewReader(raw)), discardCloser{})
			if result.Passed {
				t.Fatal("relay failure passed", result)
			}
			select {
			case err := <-controlled:
				if err == nil {
					t.Fatal("broker saw clean truncated input instead of relay failure")
				}
			case <-time.After(time.Second):
				t.Fatal("failure was not reported")
			}
			select {
			case err := <-copied:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("input EOF did not arrive")
			}
		})
	}
}

func TestControlPreludeBindsTheSameActualUnixPeer(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			data, serverData := unixPair(t)
			control, serverControl := unixPair(t)
			defer data.Close()
			prelude := controlHello
			if !valid {
				prelude = "wrong-control-version\n"
			}
			control.Write([]byte(prelude))
			control.CloseWrite()
			err := acceptBrokerControl(serverData, serverControl)
			if (err == nil) != valid {
				t.Fatal(valid, err)
			}
		})
	}
	if err := relayControl(strings.NewReader("")); err != nil {
		t.Fatal("normal relay teardown was treated as failure", err)
	}
	if err := relayControl(bytes.NewReader([]byte{255})); err == nil {
		t.Fatal("malformed control event passed")
	}
}

func TestControlRejectsAnotherProcessEvenWithTheSameUID(t *testing.T) {
	if path := os.Getenv("SFL_TEST_FOREIGN_CONTROL"); path != "" {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte(controlHello))
		return
	}
	data, serverData := unixPair(t)
	defer data.Close()
	path := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(2 * time.Second))
	command := exec.Command(os.Args[0], "-test.run=^TestControlRejectsAnotherProcessEvenWithTheSameUID$")
	command.Env = []string{"SFL_TEST_FOREIGN_CONTROL=" + path}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	control, err := listener.AcceptUnix()
	if err != nil {
		command.Wait()
		t.Fatal(err)
	}
	defer control.Close()
	if err = acceptBrokerControl(serverData, control); err == nil {
		t.Error("different process supplied relay control")
	}
	if err = command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerExecutableUsesFileIdentityThroughSymlinkedAncestors(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(filepath.Dir(self), alias); err != nil {
		t.Fatal(err)
	}
	if !sameExecutableFile(self, filepath.Join(alias, filepath.Base(self))) {
		t.Fatal("same executable through lexical alias rejected")
	}
	other := filepath.Join(t.TempDir(), "other")
	if err = os.WriteFile(other, []byte("different inode"), 0700); err != nil {
		t.Fatal(err)
	}
	if sameExecutableFile(self, other) || sameExecutableFile(self, other+"-missing") {
		t.Fatal("different/missing executable accepted")
	}
}

func TestStderrDrainCompletesBeforeCommandWait(t *testing.T) {
	if os.Getenv("SFL_TEST_STDERR_DRAIN") == "1" {
		os.Stdout.Close()
		for i := 0; i < 16; i++ {
			os.Stderr.Write(bytes.Repeat([]byte("diagnostics"), 256))
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStderrDrainCompletesBeforeCommandWait$")
	command.Env = []string{"SFL_TEST_STDERR_DRAIN=1"}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	done := make(chan error, 1)
	go func() { _, e := io.Copy(&captured, stderr); done <- e }()
	if _, err = io.Copy(io.Discard, stdout); err != nil {
		t.Fatal(err)
	}
	if err = drainStderr(ctx, stderr, done); err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err != nil {
		t.Fatal(err)
	}
	if captured.Len() != 16*256*len("diagnostics") {
		t.Fatal("buffered stderr lost", captured.Len())
	}
}
