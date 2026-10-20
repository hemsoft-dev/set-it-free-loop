//go:build linux

package reviewer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sourceServiceConfig(t *testing.T, provider Provider) SourceReaderConfig {
	t.Helper()
	if os.Getuid() == 0 || os.Getgid() == 0 {
		t.Skip("private reader is non-root")
	}
	root, err := os.MkdirTemp("/tmp", "sflread-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	s := testSession(provider)
	s.Limits.Timeout = 3 * time.Second
	return SourceReaderConfig{LaunchID: strings.Repeat("a", 32), Session: s, Paths: []string{"worker.go", "source.go"}, Root: root, ReaderUID: os.Getuid(), WorkerUID: os.Getuid() + 1, WorkerGID: os.Getgid()}
}

type sourceSocketResult struct {
	access *SourceAccess
	err    error
}

func sourceSocketReady(t *testing.T, ready <-chan SourceEndpoint, done <-chan sourceSocketResult) SourceEndpoint {
	t.Helper()
	select {
	case endpoint := <-ready:
		return endpoint
	case result := <-done:
		t.Fatal("service failed before readiness", result.err)
	case <-time.After(time.Second):
		t.Fatal("service readiness timed out")
	}
	return SourceEndpoint{}
}

func rejectSourcePeers(t *testing.T, socket string) {
	t.Helper()
	for i := 0; i < 8; i++ {
		connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(connection, "private-peer-canary\n")
		data, err := io.ReadAll(connection)
		_ = connection.Close()
		if len(data) != 0 || (err != nil && !strings.Contains(err.Error(), "connection reset")) {
			t.Fatal("unauthorized peer got data or remained open", string(data), err)
		}
	}
}

func TestSourceSocketRejectsActualWrongPeerUIDBeforeSourceAccess(t *testing.T) {
	c := sourceServiceConfig(t, Codex)
	reader, _ := brokerReader(c.Session.Snapshot)
	ready := make(chan SourceEndpoint, 1)
	done := make(chan sourceSocketResult, 1)
	go func() {
		a, err := ServeSourceSocket(context.Background(), c, reader, func(endpoint SourceEndpoint) error { ready <- endpoint; return nil })
		done <- sourceSocketResult{a, err}
	}()
	endpoint := sourceSocketReady(t, ready, done)
	rejectSourcePeers(t, endpoint.Socket)
	r := <-done
	if !errors.Is(r.err, ErrSourceService) || r.access == nil || r.access.ToolCalls != 0 || r.access.ReaderCalls != 0 || len(r.access.Reads) != 0 || len(r.access.Coverage) != 8 {
		t.Fatal("peer denial consumed/disclosed source", r)
	}
	if err := r.access.validate(c.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(endpoint.Socket)); !os.IsNotExist(err) {
		t.Fatal("owned source endpoint not removed", err)
	}
}

func TestSourceSocketProtocolWithSimulatedWorkerIdentity(t *testing.T) {
	// This positive protocol fixture simulates the peer UID. It does not prove
	// two OS identities. The public production entry uses SO_PEERCRED only.
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			c := sourceServiceConfig(t, provider)
			reader, _ := brokerReader(c.Session.Snapshot)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan SourceEndpoint, 1)
			done := make(chan sourceSocketResult, 1)
			go func() {
				a, err := serveSourceSocket(ctx, c, reader, func(endpoint SourceEndpoint) error { ready <- endpoint; return nil }, func(*net.UnixConn) (uint32, error) { return uint32(c.WorkerUID), nil })
				done <- sourceSocketResult{a, err}
			}()
			endpoint := sourceSocketReady(t, ready, done)
			if endpoint.SessionHash != Digest(c.Session) || endpoint.ReaderUID != os.Getuid() {
				t.Fatal("reader endpoint not bound to session")
			}
			for path, mode := range map[string]os.FileMode{c.Root: 0700, filepath.Dir(endpoint.Socket): 0710, endpoint.Socket: 0660} {
				info, err := os.Lstat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatal("unsafe source endpoint mode", path, info, err)
				}
			}
			connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			f := &mcpFixture{client: connection, scan: bufio.NewScanner(connection), session: c.Session}
			f.initialize(t)
			for _, request := range []string{
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"revision":"` + c.Session.Snapshot.Head + `","path":"worker.go"}}}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"revision":"` + c.Session.Snapshot.Head + `","needle":"worker-only","max_results":1}}}`,
			} {
				r := f.call(t, request)
				if r["error"] != nil || !strings.Contains(string(r["result"]), "worker") {
					t.Fatal("reader socket did not reach captured broker", r)
				}
			}
			_ = connection.Close()
			r := <-done
			if r.err != nil || r.access == nil || r.access.ToolCalls != 2 || r.access.ReaderCalls != 3 || len(r.access.Reads) != 3 || r.access.validate(c.Session) != nil {
				t.Fatal("actual service audit missing", r)
			}
			if reads, _ := reader.Evidence(); len(reads) != 0 {
				t.Fatal("service reads polluted citation verifier")
			}
		})
	}
}

func TestSourceSocketRejectsUnsafeRootIdentityAndExistingJobs(t *testing.T) {
	for _, mode := range []string{"same-uid", "wrong-gid", "public-root", "symlink-root", "existing-job", "outside-verifier"} {
		t.Run(mode, func(t *testing.T) {
			c := sourceServiceConfig(t, Codex)
			reader, _ := brokerReader(c.Session.Snapshot)
			var retained string
			switch mode {
			case "same-uid":
				c.WorkerUID = os.Getuid()
			case "wrong-gid":
				c.WorkerGID++
			case "outside-verifier":
				c.Paths = append(c.Paths, "outside.go")
			case "public-root":
				_ = os.Chmod(c.Root, 0755)
			case "symlink-root":
				link := c.Root + "-link"
				if err := os.Symlink(c.Root, link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(link) })
				c.Root = link
			case "existing-job":
				dir := filepath.Join(c.Root, Digest(c.Session))
				_ = os.Mkdir(dir, 0700)
				retained = filepath.Join(dir, "source.sock")
				_ = os.WriteFile(retained, []byte("retained-existing-job"), 0600)
			}
			called := false
			a, err := ServeSourceSocket(context.Background(), c, reader, func(SourceEndpoint) error { called = true; return nil })
			if err == nil || a != nil || called {
				t.Fatal("unsafe service admitted", err)
			}
			if retained != "" {
				data, err := os.ReadFile(retained)
				if err != nil || string(data) != "retained-existing-job" {
					t.Fatal("existing job was removed/replaced", err)
				}
			}
		})
	}
}

func TestSourceSocketCancellationClosesListenerAndIdleClient(t *testing.T) {
	c := sourceServiceConfig(t, Claude)
	reader, _ := brokerReader(c.Session.Snapshot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan SourceEndpoint, 1)
	done := make(chan sourceSocketResult, 1)
	go func() {
		a, err := serveSourceSocket(ctx, c, reader, func(endpoint SourceEndpoint) error { ready <- endpoint; return nil }, func(*net.UnixConn) (uint32, error) { return uint32(c.WorkerUID), nil })
		done <- sourceSocketResult{a, err}
	}()
	endpoint := sourceSocketReady(t, ready, done)
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	cancel()
	select {
	case result := <-done:
		if result.err == nil || result.access == nil || result.access.ToolCalls != 0 {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("reader failed to join cancellation")
	}
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if data, _ := io.ReadAll(connection); len(data) != 0 {
		t.Fatal("cancelled reader returned source")
	}
}

func TestSourceSocketCleanupPreservesReplacementPath(t *testing.T) {
	c := sourceServiceConfig(t, Codex)
	reader, _ := brokerReader(c.Session.Snapshot)
	var retained string
	_, err := ServeSourceSocket(context.Background(), c, reader, func(endpoint SourceEndpoint) error {
		retained = endpoint.Socket
		if err := os.Remove(retained); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(retained, []byte("replacement-canary"), 0600); err != nil {
			t.Fatal(err)
		}
		return errors.New("private-ready-error-canary")
	})
	data, readErr := os.ReadFile(retained)
	if !errors.Is(err, ErrSourceService) || strings.Contains(err.Error(), "canary") || readErr != nil || string(data) != "replacement-canary" {
		t.Fatal("cleanup removed replacement or leaked callback error", err, readErr)
	}
}

func TestSourceReaderMainRejectsAmbientCredentialsAndMalformedInput(t *testing.T) {
	c := sourceServiceConfig(t, Codex)
	raw, _ := json.Marshal(c)
	for _, env := range [][]string{{"GH_TOKEN=private-canary"}, {"CODEX_HOME=/private"}, {"CLAUDE_CONFIG_DIR=/private"}, {"HOME=/private"}, {"LANG=" + strings.Repeat("x", 65)}} {
		input := &countedProxyInput{ReadCloser: io.NopCloser(strings.NewReader(string(raw)))}
		output := &countedProxyOutput{}
		if code := SourceReaderMain(context.Background(), env, input, output); code != 2 || input.closes.Load() != 1 || output.closes.Load() != 1 || output.Len() != 0 {
			t.Fatal("reader accepted/leaked ambient environment", code)
		}
	}
	for _, raw := range []string{"null", "{}", string(raw) + "\n", string(raw[:len(raw)-1]) + `,"worker_uid":42}`} {
		if code := SourceReaderMain(context.Background(), []string{"LANG=C.UTF-8"}, io.NopCloser(strings.NewReader(raw)), &proxyOutput{}); code != 2 {
			t.Fatal("malformed/noncanonical reader manifest accepted", code)
		}
	}
}

func TestSourceReaderAuditPipeStaysSeparateFromWorkerProtocol(t *testing.T) {
	c := sourceServiceConfig(t, Codex)
	raw, _ := json.Marshal(c)
	audit, output := io.Pipe()
	defer audit.Close()
	done := make(chan int, 1)
	go func() {
		done <- SourceReaderMain(context.Background(), []string{"LANG=C.UTF-8"}, io.NopCloser(strings.NewReader(string(raw))), output)
	}()
	decode := json.NewDecoder(audit)
	var ready, stopped SourceReaderEvent
	if decode.Decode(&ready) != nil || ready.Kind != "ready" || ready.Access != nil || ready.SessionHash != Digest(c.Session) || ready.ManifestHash != Digest(c) {
		t.Fatal("invalid reader ready event", ready)
	}
	rejectSourcePeers(t, ready.Socket)
	if decode.Decode(&stopped) != nil || stopped.Kind != "stopped" || stopped.ManifestHash != Digest(c) || !stopped.Failed || stopped.Access == nil || stopped.Access.ToolCalls != 0 || len(stopped.Access.Coverage) != 8 || stopped.Access.validate(c.Session) != nil {
		t.Fatal("missing trusted stopped audit", stopped)
	}
	if code := <-done; code != 1 {
		t.Fatal("unauthorized clients completed reader", code)
	}
	if decode.Decode(new(any)) != io.EOF {
		t.Fatal("extra reader control output")
	}
}

func TestSourceReaderMainCancellationUnblocksOwnedIO(t *testing.T) {
	c := sourceServiceConfig(t, Codex)
	raw, _ := json.Marshal(c)
	for _, mode := range []string{"input", "audit-output"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			var input io.ReadCloser = io.NopCloser(strings.NewReader(string(raw)))
			var output io.WriteCloser = &proxyOutput{}
			if mode == "input" {
				read, write := io.Pipe()
				defer write.Close()
				input = read
			} else {
				read, write := io.Pipe()
				defer read.Close()
				output = write // nobody reads the ready event
			}
			done := make(chan int, 1)
			go func() { done <- SourceReaderMain(ctx, []string{"LANG=C.UTF-8"}, input, output) }()
			select {
			case code := <-done:
				if code == 0 {
					t.Fatal("cancelled source service succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("reader did not close/join blocked owned IO")
			}
		})
	}
}

func TestSourceSocketGracefulSignalStopsIdleIntakeAndPreservesActiveDrain(t *testing.T) {
	for _, mode := range []string{"idle", "active", "active-hard-stop"} {
		t.Run(mode, func(t *testing.T) {
			active := mode != "idle"
			c := sourceServiceConfig(t, Codex)
			reader, _ := brokerReader(c.Session.Snapshot)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			drain, signal := context.WithCancel(context.Background())
			defer signal()
			ready := make(chan SourceEndpoint, 1)
			done := make(chan sourceSocketResult, 1)
			go func() {
				a, err := serveSourceSocketDraining(ctx, drain, c, reader, func(e SourceEndpoint) error { ready <- e; return nil }, func(*net.UnixConn) (uint32, error) { return uint32(c.WorkerUID), nil })
				done <- sourceSocketResult{a, err}
			}()
			endpoint := sourceSocketReady(t, ready, done)
			if !active {
				signal()
				select {
				case r := <-done:
					if r.err == nil {
						t.Fatal("idle cancellation became a clean source audit")
					}
				case <-time.After(time.Second):
					t.Fatal("idle reader did not stop")
				}
				return
			}
			connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			f := &mcpFixture{client: connection, scan: bufio.NewScanner(connection), session: c.Session}
			f.initialize(t)
			signal()
			select {
			case r := <-done:
				t.Fatal("signal cancelled the accepted source connection", r.err)
			case <-time.After(50 * time.Millisecond):
			}
			reply := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"revision":"`+c.Session.Snapshot.Head+`","path":"worker.go"}}}`)
			if reply["error"] != nil || !strings.Contains(string(reply["result"]), "worker") {
				t.Fatal("accepted connection could not finish queued source work", reply)
			}
			if mode == "active-hard-stop" {
				cancel()
				select {
				case r := <-done:
					if r.err == nil {
						t.Fatal("hard cancellation became a clean source audit")
					}
				case <-time.After(time.Second):
					t.Fatal("hard cancellation left draining work running")
				}
				return
			}
			connection.Close()
			select {
			case r := <-done:
				if r.err != nil || r.access == nil || r.access.ToolCalls != 1 {
					t.Fatal("clean EOF lost audit after signal", r.err)
				}
			case <-time.After(time.Second):
				t.Fatal("reader did not finish clean EOF")
			}
		})
	}
}
