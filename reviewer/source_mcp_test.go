package reviewer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type mcpFixture struct {
	client  net.Conn
	scan    *bufio.Scanner
	done    chan error
	cancel  context.CancelFunc
	broker  *sourceBroker
	session Session
}

func newMCPFixture(t *testing.T, p Provider, limits Limits, source SourceTools) *mcpFixture {
	t.Helper()
	s := testSession(p)
	s.Limits = limits
	ctx, cancel := context.WithCancel(context.Background())
	verifier, err := brokerReader(s.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := newSourceBroker(ctx, s, verifier)
	if err != nil {
		t.Fatal(err)
	}
	a := Adapter{Provider: p, AuthDirectory: t.TempDir()}
	inv, err := a.Invocation(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	inv.Source = broker
	if source != nil {
		inv.Source = source
	}
	server, client := net.Pipe()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	f := &mcpFixture{client: client, scan: bufio.NewScanner(client), done: make(chan error, 1), cancel: cancel, broker: broker, session: s}
	f.scan.Buffer(make([]byte, 4096), 16<<20)
	go func() { f.done <- ServeSourceMCP(ctx, inv, server) }()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		select {
		case <-f.done:
		case <-time.After(time.Second):
			t.Error("MCP server did not stop")
		}
	})
	return f
}
func (f *mcpFixture) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := io.WriteString(f.client, raw+"\n"); err != nil {
		t.Fatal(err)
	}
}
func (f *mcpFixture) response(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	if !f.scan.Scan() {
		t.Fatal("missing MCP response", f.scan.Err())
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(f.scan.Bytes(), &r) != nil {
		t.Fatal("invalid response")
	}
	return r
}
func (f *mcpFixture) call(t *testing.T, raw string) map[string]json.RawMessage {
	f.send(t, raw)
	return f.response(t)
}
func (f *mcpFixture) initialize(t *testing.T) {
	t.Helper()
	r := f.call(t, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true},"sampling":{}},"clientInfo":{"name":"fake-cli","version":"1"}}}`)
	var result struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(r["result"], &result) != nil || result.ProtocolVersion != SourceMCPVersion || len(result.Capabilities) != 1 || result.Capabilities["tools"] == nil {
		t.Fatal("unexpected MCP capabilities", string(r["result"]))
	}
	f.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}
func errorCode(t *testing.T, r map[string]json.RawMessage) int {
	t.Helper()
	var e struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(r["error"], &e) != nil {
		t.Fatal("missing RPC error", r)
	}
	return e.Code
}
func TestSourceMCPBothProvidersUseOnlyCapturedReadSearch(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			f := newMCPFixture(t, provider, DefaultLimits(), nil)
			f.initialize(t)
			r := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			var list struct {
				Tools []struct {
					Name        string         `json:"name"`
					InputSchema map[string]any `json:"inputSchema"`
				} `json:"tools"`
			}
			if json.Unmarshal(r["result"], &list) != nil || len(list.Tools) != 2 || list.Tools[0].Name != "read" || list.Tools[1].Name != "search" || list.Tools[0].InputSchema["additionalProperties"] != false {
				t.Fatal("unexpected source tools", string(r["result"]))
			}
			request := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"revision":"` + f.session.Snapshot.Head + `","path":"worker.go"}}}`
			r = f.call(t, request)
			var content struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if json.Unmarshal(r["result"], &content) != nil || content.IsError || len(content.Content) != 1 || content.Content[0].Text != "worker-only evidence" {
				t.Fatal("read did not map to broker", string(r["result"]))
			}
			r = f.call(t, `{"jsonrpc":"2.0","id":"search","method":"tools/call","params":{"name":"search","arguments":{"revision":"`+f.session.Snapshot.Head+`","needle":"worker-only","max_results":1}}}`)
			if json.Unmarshal(r["result"], &content) != nil || content.IsError || !strings.Contains(content.Content[0].Text, "worker.go") {
				t.Fatal("search did not map to broker", string(r["result"]))
			}
			// The broker receipt is authoritative and includes both search scans.
			receipt := f.broker.seal()
			if receipt.ToolCalls != 2 || receipt.ReaderCalls != 3 || len(receipt.Reads) != 3 {
				t.Fatal("actual MCP reads missing", receipt)
			}
		})
	}
}
func TestSourceMCPRequiresInitializationAndRejectsOtherCapabilities(t *testing.T) {
	f := newMCPFixture(t, Codex, DefaultLimits(), nil)
	r := f.call(t, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"read","arguments":{"revision":"x","path":"y"}}}`)
	if errorCode(t, r) != -32002 {
		t.Fatal("tool ran before initialize")
	}
	f.initialize(t)
	for i, method := range []string{"resources/read", "roots/list", "sampling/createMessage", "shell", "tools/execute"} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 20 + i, "method": method, "params": map[string]string{"uri": "file:///host/private"}})
		if errorCode(t, f.call(t, string(raw))) != -32601 {
			t.Fatal("unadvertised capability accepted", method)
		}
	}
	if f.broker.seal().ToolCalls != 0 {
		t.Fatal("unapproved method accessed source")
	}
}
func TestSourceMCPRejectsUnknownArgumentsAndCrossJobAccess(t *testing.T) {
	f := newMCPFixture(t, Claude, DefaultLimits(), nil)
	f.initialize(t)
	r := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"revision":"`+f.session.Snapshot.Head+`","path":"worker.go","job_id":"other"}}}`)
	if errorCode(t, r) != -32602 {
		t.Fatal("job selector accepted")
	}
	r = f.call(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"revision":"`+f.session.Snapshot.Head+`","path":"../host-secret-canary"}}}`)
	var denied struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(r["result"], &denied) != nil || !denied.IsError || denied.Content[0].Text != "Source request denied" || strings.Contains(string(r["result"]), "canary") {
		t.Fatal("denial leaked source error", string(r["result"]))
	}
	r = f.call(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"revision":"`+strings.Repeat("d", 40)+`","path":"worker.go"}}}`)
	if json.Unmarshal(r["result"], &denied) != nil || !denied.IsError {
		t.Fatal("cross-snapshot read accepted")
	}
	receipt := f.broker.seal()
	if receipt.ToolCalls != 2 || len(receipt.Reads) != 0 || len(receipt.Coverage) != 2 {
		t.Fatal("denials not audited", receipt)
	}
}
func TestSourceMCPNotificationsNeverInvokeTools(t *testing.T) {
	f := newMCPFixture(t, Codex, DefaultLimits(), nil)
	f.initialize(t)
	f.send(t, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read","arguments":{"revision":"`+f.session.Snapshot.Head+`","path":"worker.go"}}}`)
	r := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if string(r["id"]) != "1" || f.broker.seal().ToolCalls != 0 {
		t.Fatal("notification invoked source or emitted a response", r)
	}
}
func TestSourceMCPRejectsMalformedDuplicateAndOversizedFrames(t *testing.T) {
	for name, payload := range map[string]string{
		"batch": `[]`, "duplicate": `{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/list"}`,
		"null-id": `{"jsonrpc":"2.0","id":null,"method":"ping"}`, "trailing": `{"jsonrpc":"2.0","id":1,"method":"ping"} {}`,
		"oversized": strings.Repeat("x", (16<<10)+1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPFixture(t, Codex, DefaultLimits(), nil)
			// A large write may be interrupted by fail-closed connection shutdown.
			_, _ = io.WriteString(f.client, payload+"\n")
			err := <-f.done
			f.done <- err
			if !errors.Is(err, ErrSourceMCPProtocol) || f.broker.seal().ToolCalls != 0 {
				t.Fatal("unsafe frame accepted", err)
			}
		})
	}
}
func TestSourceMCPRejectsReusedCanonicalStringIDs(t *testing.T) {
	f := newMCPFixture(t, Codex, DefaultLimits(), nil)
	f.call(t, `{"jsonrpc":"2.0","id":"a","method":"ping"}`)
	f.send(t, `{"jsonrpc":"2.0","id":"\u0061","method":"ping"}`)
	err := <-f.done
	f.done <- err
	if !errors.Is(err, ErrSourceMCPProtocol) {
		t.Fatal("reused request ID accepted", err)
	}
}

type blockedMCPSource struct{ entered, exited chan struct{} }

func (s *blockedMCPSource) Read(ctx context.Context, revision, path string) (string, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.exited)
	return "", ctx.Err()
}
func (s *blockedMCPSource) Search(context.Context, string, string, int) ([]Anchor, error) {
	return nil, errors.New("private-controller-secret-canary")
}
func TestSourceMCPHandlesClientCancellationWithoutSendingLateResult(t *testing.T) {
	source := &blockedMCPSource{make(chan struct{}), make(chan struct{})}
	f := newMCPFixture(t, Codex, DefaultLimits(), source)
	f.initialize(t)
	f.send(t, `{"jsonrpc":"2.0","id":"read","method":"tools/call","params":{"name":"read","arguments":{"revision":"x","path":"p"}}}`)
	<-source.entered
	f.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"read","reason":"untrusted client reason"}}`)
	select {
	case <-source.exited:
	case <-time.After(time.Second):
		t.Fatal("source cancellation not delivered")
	}
	r := f.call(t, `{"jsonrpc":"2.0","id":99,"method":"ping"}`)
	if string(r["id"]) != "99" {
		t.Fatal("cancelled call emitted late response", r)
	}
}
func TestSourceMCPControllerDeadlineClosesBlockedConnection(t *testing.T) {
	limits := DefaultLimits()
	limits.Timeout = 50 * time.Millisecond
	f := newMCPFixture(t, Claude, limits, nil)
	select {
	case err := <-f.done:
		f.done <- err
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle protocol ignored deadline")
	}
}
func TestSourceMCPBoundsMetadataFlood(t *testing.T) {
	limits := DefaultLimits()
	limits.ReaderCalls = 1
	f := newMCPFixture(t, Codex, limits, nil)
	for i := 0; i < 34; i++ {
		_, err := io.WriteString(f.client, `{"jsonrpc":"2.0","method":"notifications/unused"}`+"\n")
		if err != nil {
			break
		}
	}
	err := <-f.done
	f.done <- err
	if !errors.Is(err, ErrSourceMCPProtocol) {
		t.Fatal("metadata bypassed finite message budget", err)
	}
}

type verboseMCPSource struct{}

func (verboseMCPSource) Read(context.Context, string, string) (string, error) {
	return strings.Repeat("x", 256<<10), nil
}
func (verboseMCPSource) Search(context.Context, string, string, int) ([]Anchor, error) {
	return nil, errors.New("private-controller-secret-canary")
}
func TestSourceMCPRedactsBackendErrorsAndEnforcesOutputCap(t *testing.T) {
	t.Run("redaction", func(t *testing.T) {
		f := newMCPFixture(t, Codex, DefaultLimits(), verboseMCPSource{})
		f.initialize(t)
		r := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"revision":"x","needle":"q","max_results":1}}}`)
		if strings.Contains(string(r["result"]), "canary") || !strings.Contains(string(r["result"]), `"isError":true`) {
			t.Fatal("backend error leaked", r)
		}
	})
	t.Run("output-cap", func(t *testing.T) {
		limits := DefaultLimits()
		limits.ReaderBytes = 1
		limits.ReaderCalls = 1
		f := newMCPFixture(t, Codex, limits, verboseMCPSource{})
		f.initialize(t)
		f.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"revision":"x","path":"p"}}}`)
		// The trusted backend fixture deliberately exceeds its contract. The
		// transport must reject the serialized response rather than pass it on.
		if f.scan.Scan() {
			t.Fatal("oversized source response escaped transport cap")
		}
		err := <-f.done
		f.done <- err
		if !errors.Is(err, ErrSourceMCPProtocol) {
			t.Fatal("output cap not enforced", err)
		}
	})
}

func TestSourceMCPRejectsCaseAliasesBeforeSourceAccess(t *testing.T) {
	f := newMCPFixture(t, Codex, DefaultLimits(), nil)
	f.initialize(t)
	r := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"revision":"`+f.session.Snapshot.Head+`","path":"worker.go","Path":"source.go"}}}`)
	if errorCode(t, r) != -32602 || f.broker.seal().ToolCalls != 0 {
		t.Fatal("case alias bypassed argument schema", r)
	}
}

func TestSourceMCPRejectsReusedNumericZeroIDs(t *testing.T) {
	f := newMCPFixture(t, Codex, DefaultLimits(), nil)
	f.call(t, `{"jsonrpc":"2.0","id":0,"method":"ping"}`)
	f.send(t, `{"jsonrpc":"2.0","id":-0,"method":"ping"}`)
	if f.scan.Scan() {
		t.Fatal("numeric zero alias reused a request ID")
	}
	err := <-f.done
	f.done <- err
	if !errors.Is(err, ErrSourceMCPProtocol) {
		t.Fatal("zero alias not rejected", err)
	}
}

func TestSourceMCPLifecycleAcceptsWhitespaceAndStandardMetadata(t *testing.T) {
	f := newMCPFixture(t, Codex, DefaultLimits(), nil)
	r := f.call(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{ }}`)
	if r["error"] != nil {
		t.Fatal("whitespace in empty ping params rejected", r)
	}
	r = f.call(t, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fake-client","version":"1"},"_meta":{"progressToken":"init","example.org/extension":{"instructions":"inert untrusted text"}}}}`)
	if r["error"] != nil {
		t.Fatal("standard initialize metadata rejected", r)
	}
	f.send(t, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\",\"params\":{ \t }}")
	r = f.call(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{ }}`)
	if r["error"] != nil {
		t.Fatal("whitespace in empty tool-list params rejected", r)
	}
	r = f.call(t, `{"jsonrpc":"2.0","id":4,"method":"ping","params":{"_meta":{"progressToken":5}}}`)
	if r["error"] != nil {
		t.Fatal("standard ping metadata rejected", r)
	}
	r = f.call(t, `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{"_meta":{"example.org/extra":true}}}`)
	if r["error"] != nil || f.broker.seal().ToolCalls != 0 {
		t.Fatal("metadata invoked source or prevented discovery", r)
	}
}
func TestSourceMCPMetadataRemainsBoundedAndTyped(t *testing.T) {
	for _, meta := range []string{`null`, `[]`, `"not-an-object"`, `7`} {
		t.Run(meta, func(t *testing.T) {
			f := newMCPFixture(t, Claude, DefaultLimits(), nil)
			request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fake","version":"1"},"_meta":` + meta + `}}`
			if errorCode(t, f.call(t, request)) != -32602 {
				t.Fatal("non-object initialize metadata accepted")
			}
			if errorCode(t, f.call(t, `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"unexpected":true}}`)) != -32602 {
				t.Fatal("operational control params accepted")
			}
		})
	}
}
