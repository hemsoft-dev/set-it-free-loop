//go:build linux

package reviewer

import (
	"bufio"
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
	"sync/atomic"
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

func TestSourceProxyAcceptsExactByteCapsWithoutForwardingOverflow(t *testing.T) {
	for _, direction := range []string{"input", "output"} {
		t.Run(direction, func(t *testing.T) {
			socket, listener := proxySocket(t)
			limits := DefaultLimits()
			limits.InputBytes, limits.ReaderCalls, limits.ReaderBytes = 1, 1, 1
			input, response := "request", "response"
			if direction == "input" {
				input = strings.Repeat("i", limits.InputBytes+limits.ReaderCalls*2048)
			} else {
				response = strings.Repeat("o", 12*limits.ReaderBytes+limits.ReaderCalls*8192+(128<<10))
			}
			received := make(chan []byte, 1)
			go func() {
				connection, err := listener.AcceptUnix()
				if err != nil {
					received <- nil
					return
				}
				defer connection.Close()
				data, _ := io.ReadAll(connection)
				received <- data
				_, _ = io.WriteString(connection, response)
			}()
			out := &proxyOutput{}
			err := ProxySourceMCP(context.Background(), socket, os.Getuid(), limits, io.NopCloser(strings.NewReader(input)), out)
			if err != nil || out.String() != response || string(<-received) != input {
				t.Fatal("exact bound rejected/truncated", err, out.Len())
			}
		})
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

			sessionContext, cancelSession := context.WithCancel(context.Background())
			defer cancelSession()
			broker, err := newSourceBroker(sessionContext, session, reader)
			if err != nil {
				t.Fatal(err)
			}
			inv.Source = broker
			serverDone := make(chan error, 1)
			go func() {
				connection, err := listener.AcceptUnix()
				if err != nil {
					serverDone <- err
					return
				}
				serverDone <- ServeSourceMCP(sessionContext, inv, connection)
			}()
			command, args := sourceProxyCommand(t, inv)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, command, args...)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			scan := bufio.NewScanner(output)
			scan.Buffer(make([]byte, 4096), 1<<20)
			call := func(request string) map[string]json.RawMessage {
				t.Helper()
				if _, err := io.WriteString(input, request+"\n"); err != nil {
					t.Fatal(err)
				}
				if !scan.Scan() {
					t.Fatal("missing generated-client MCP response", scan.Err())
				}
				var response map[string]json.RawMessage
				if json.Unmarshal(scan.Bytes(), &response) != nil || response["error"] != nil {
					t.Fatal("source proxy protocol failed", string(scan.Bytes()))
				}
				return response
			}
			call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fake-client","version":"1"},"_meta":{"progressToken":"source-init"}}}`)
			if _, err = io.WriteString(input, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{ }}`+"\n"); err != nil {
				t.Fatal(err)
			}
			listed := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{ }}`)
			var tools struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if json.Unmarshal(listed["result"], &tools) != nil || len(tools.Tools) != 2 {
				t.Fatal("wrong source capabilities", string(listed["result"]))
			}
			response := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"revision":"` + session.Snapshot.Head + `","path":"worker.go"}}}`)
			var content struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if json.Unmarshal(response["result"], &content) != nil || content.IsError || len(content.Content) != 1 || content.Content[0].Text != "worker-only evidence" {
				t.Fatal("immutable read failed", string(response["result"]))
			}
			response = call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search","arguments":{"revision":"` + session.Snapshot.Head + `","needle":"worker-only","max_results":1}}}`)
			if json.Unmarshal(response["result"], &content) != nil || content.IsError || !strings.Contains(content.Content[0].Text, "worker.go") {
				t.Fatal("immutable search failed", string(response["result"]))
			}
			_ = input.Close()
			if scan.Scan() {
				t.Fatal("helper emitted extra protocol bytes", string(scan.Bytes()))
			}
			err = cmd.Wait()
			waited = true
			if err != nil {
				t.Fatal("generated proxy failed", err)
			}
			if err = <-serverDone; err != nil {
				t.Fatal("session MCP server failed", err)
			}
			access := broker.seal()
			if access.ToolCalls != 2 || access.ReaderCalls != 3 || len(access.Reads) != 3 || access.SessionHash != Digest(session) {
				t.Fatal("actual process source receipts lost", access)
			}
			reads, _ := reader.Evidence()
			if len(reads) != 0 {
				t.Fatal("citation reads masqueraded as worker reads")
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

type countedProxyInput struct {
	io.ReadCloser
	closes atomic.Int32
}

func (s *countedProxyInput) Close() error { s.closes.Add(1); return s.ReadCloser.Close() }

type countedProxyOutput struct {
	proxyOutput
	closes atomic.Int32
}

func (s *countedProxyOutput) Close() error { s.closes.Add(1); return nil }
func TestSourceProxyClosesEachOwnedStreamOnce(t *testing.T) {
	socket, listener := proxySocket(t)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.ReadAll(connection)
		_, _ = io.WriteString(connection, "reply")
	}()
	input := &countedProxyInput{ReadCloser: io.NopCloser(strings.NewReader("request"))}
	output := &countedProxyOutput{}
	err := ProxySourceMCP(context.Background(), socket, os.Getuid(), DefaultLimits(), input, output)
	if err != nil || input.closes.Load() != 1 || output.closes.Load() != 1 || output.String() != "reply" {
		t.Fatal("owned streams closed repeatedly or transfer lost", err, input.closes.Load(), output.closes.Load())
	}
}
