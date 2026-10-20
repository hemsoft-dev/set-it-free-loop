//go:build linux

package reviewer

import (
	"bytes"
	"context"
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

type proxyOutput struct{ bytes.Buffer }

func (*proxyOutput) Close() error { return nil }

type observedProxyInput struct {
	reader *strings.Reader
	reads  int
	closed bool
}

func (r *observedProxyInput) Read(data []byte) (int, error) { r.reads++; return r.reader.Read(data) }
func (r *observedProxyInput) Close() error                  { r.closed = true; return nil }
func proxySocket(t *testing.T) (string, *net.UnixListener) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("alpha reader must use a non-root UID")
	}
	root, err := os.MkdirTemp("/tmp", "sflsrc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "source.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return socket, listener
}
func TestSourceProxyChecksPeerUIDBeforeReadingAnyClientBytes(t *testing.T) {
	socket, listener := proxySocket(t)
	received := make(chan []byte, 1)
	go func() {
		c, err := listener.AcceptUnix()
		if err != nil {
			received <- nil
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		received <- data
	}()
	input := &observedProxyInput{reader: strings.NewReader("private-source-payload")}
	out := &proxyOutput{}
	err := ProxySourceMCP(context.Background(), socket, os.Getuid()+1, DefaultLimits(), input, out)
	if !errors.Is(err, ErrSourceProxyUnavailable) || input.reads != 0 || !input.closed || out.Len() != 0 {
		t.Fatal("untrusted peer received/read client data", err, input, out.Len())
	}
	select {
	case data := <-received:
		if len(data) != 0 {
			t.Fatal("payload sent before peer check")
		}
	case <-time.After(time.Second):
		t.Fatal("untrusted peer connection not closed")
	}
}
func TestSourceProxyOwnsStreamsAndHonorsControllerCancellation(t *testing.T) {
	socket, listener := proxySocket(t)
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		c, err := listener.AcceptUnix()
		if err == nil {
			accepted <- c
		}
	}()
	input, producer := io.Pipe()
	defer producer.Close()
	limits := DefaultLimits()
	limits.Timeout = 50 * time.Millisecond
	start := time.Now()
	err := ProxySourceMCP(context.Background(), socket, os.Getuid(), limits, input, &proxyOutput{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("proxy failed to stop blocked I/O", err)
	}
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(time.Second):
		t.Fatal("fixture did not connect")
	}
	if _, err = producer.Write([]byte("after-stop")); err == nil {
		t.Fatal("cancelled proxy retained open client stream")
	}
}
func TestSourceProxyEnforcesBothWireBudgets(t *testing.T) {
	for _, direction := range []string{"input", "output"} {
		t.Run(direction, func(t *testing.T) {
			socket, listener := proxySocket(t)
			limits := DefaultLimits()
			limits.InputBytes = 1
			limits.ReaderBytes = 1
			limits.ReaderCalls = 1
			inputCap := limits.InputBytes + limits.ReaderCalls*2048
			outputCap := 12*limits.ReaderBytes + limits.ReaderCalls*8192 + (128 << 10)
			received := make(chan int, 1)
			go func() {
				c, err := listener.AcceptUnix()
				if err != nil {
					received <- 0
					return
				}
				defer c.Close()
				data, _ := io.ReadAll(c)
				received <- len(data)
				if direction == "output" {
					_, _ = io.WriteString(c, strings.Repeat("x", outputCap+100))
				}
			}()
			payload := ""
			if direction == "input" {
				payload = strings.Repeat("x", inputCap+100)
			}
			output := &proxyOutput{}
			err := ProxySourceMCP(context.Background(), socket, os.Getuid(), limits, io.NopCloser(strings.NewReader(payload)), output)
			if !errors.Is(err, ErrSourceProxyBudget) {
				t.Fatal("wire budget not enforced", direction, err)
			}
			n := <-received
			if n > inputCap || output.Len() > outputCap {
				t.Fatal("proxy forwarded bytes beyond cap", n, output.Len())
			}
		})
	}
}
func TestSourceProxyRejectsSymlinkAndUnavailableEndpointsWithoutDisclosure(t *testing.T) {
	socket, _ := proxySocket(t)
	link := socket + "-link"
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, socket + "-missing", "relative", socket + "\nprivate-path-canary"} {
		input := &observedProxyInput{reader: strings.NewReader("data")}
		err := ProxySourceMCP(context.Background(), path, os.Getuid(), DefaultLimits(), input, &proxyOutput{})
		if !errors.Is(err, ErrSourceProxyUnavailable) || input.reads != 0 || strings.Contains(err.Error(), "canary") {
			t.Fatal("endpoint failure leaked or consumed source", err)
		}
	}
}

// The generated helper command executes this fixture, never an official client.
// Its wrapper shell is trusted fixture code; model/source text never runs it.
func TestSourceProxyExecutable(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	for _, key := range []string{"HOME", "GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "SSH_AUTH_SOCK"} {
		if os.Getenv(key) != "" {
			os.Exit(3)
		}
	}
	os.Exit(SourceProxyMain(context.Background(), os.Args[separator+1:], os.Stdin, os.Stdout))
}
func TestBothGeneratedClientCommandsExecuteCredentialFreeSourceProxy(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK", "CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(key, "source-helper-secret-canary")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	helper := filepath.Join(t.TempDir(), "fake-sourceproxy")
	script := "#!/bin/sh\nexec " + quote(binary) + " -test.run=^TestSourceProxyExecutable$ -- \"$@\"\n"
	if err = os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			socket, listener := proxySocket(t)
			_ = listener.Close()
			root := filepath.Dir(socket)
			session := testSession(provider)
			reader, _ := brokerReader(session.Snapshot)
			adapter := Adapter{Provider: provider, AuthDirectory: t.TempDir(), Reader: reader, SourceBridge: &SourceBridge{Program: helper, Directory: root, ReaderUID: os.Getuid()}}
			inv, err := adapter.Invocation(session, nil)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := inv.SourceEndpoint.Socket
			if err = os.Mkdir(filepath.Dir(endpoint), 0700); err != nil {
				t.Fatal(err)
			}
			listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			response := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"captured source"}],"isError":false}}` + "\n"
			received := make(chan []byte, 1)
			go func() {
				c, err := listener.AcceptUnix()
				if err != nil {
					received <- nil
					return
				}
				defer c.Close()
				data, _ := io.ReadAll(c)
				received <- data
				_, _ = io.WriteString(c, response)
			}()
			command, args := sourceProxyCommand(t, inv)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, command, args...)
			request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"revision":"` + session.Snapshot.Head + `","path":"source.go"}}}` + "\n"
			cmd.Stdin = strings.NewReader(request)
			actual, err := cmd.Output()
			if err != nil || string(actual) != response {
				t.Fatal("generated helper failed private source transport", err, string(actual))
			}
			if string(<-received) != request {
				t.Fatal("proxy changed source request bytes")
			}
		})
	}
}
func TestSourceProxyMainRejectsMalformedFlagsAndLimits(t *testing.T) {
	raw, _ := json.Marshal(DefaultLimits())
	for _, args := range [][]string{{"--unknown", "value"}, {"--socket", "/none", "--reader-uid", "bad", "--limits", string(raw)}, {"--limits", `{"timeout_ns":null}`}, {"unexpected-positional"}} {
		if code := SourceProxyMain(context.Background(), args, io.NopCloser(strings.NewReader("")), &proxyOutput{}); code != 2 {
			t.Fatal("malformed helper arguments accepted", args, code)
		}
	}
}
