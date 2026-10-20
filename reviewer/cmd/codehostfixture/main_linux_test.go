//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
