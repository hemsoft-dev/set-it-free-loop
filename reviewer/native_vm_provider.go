package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// A private VM stream supplies precisely one HTTP connection. Closing either
// endpoint ends intake; there is no listener address or reconnect fallback.
type nativeProviderListener struct {
	mu         sync.Mutex
	used       bool
	connection *nativeProviderConnection
}
type nativeProviderConnection struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *nativeProviderConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}
func (l *nativeProviderListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.connection, nil
	}
	l.mu.Unlock()
	<-l.connection.closed
	return nil, net.ErrClosed
}
func (l *nativeProviderListener) Close() error   { return l.connection.Close() }
func (l *nativeProviderListener) Addr() net.Addr { return l.connection.LocalAddr() }

type nativeProviderBridge struct {
	guard  *ProviderEgress
	policy EgressPolicy
	server *http.Server
	done   chan struct{}
}

func startNativeProviderBridge(ctx context.Context, s Session, policy EgressPolicy, transport http.RoundTripper, channel net.Conn) (*nativeProviderBridge, error) {
	if channel == nil {
		return nil, ErrLiveBlocked
	}
	if (s.Identity.Provider == Codex && policy.ResponsePolicy != CodexSourceResponses) || (s.Identity.Provider == Claude && policy.ResponsePolicy != ClaudeSourceMessages) {
		return nil, ErrLiveBlocked
	}
	guard, err := NewProviderEgress(ctx, s, policy, transport)
	if err != nil {
		return nil, err
	}
	server := NewProviderEgressServer(guard)
	server.IdleTimeout = s.Limits.Timeout
	server.ErrorLog = log.New(io.Discard, "", 0)
	// The native request must carry the captured model, not an implicit default
	// or fallback. This check happens before any inference body reaches upstream.
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		route, ok := guard.route(q)
		if ok && route.Kind == "inference" {
			raw, err := io.ReadAll(io.LimitReader(q.Body, int64(policy.Limits.RequestBytes)+1))
			q.Body.Close()
			defer clear(raw)
			var body map[string]any
			if err != nil || len(raw) > policy.Limits.RequestBytes || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil || json.Unmarshal(raw, &body) != nil || body["model"] != s.Identity.Model {
				guard.fail(len(raw) > policy.Limits.RequestBytes)
				http.Error(w, "captured model required", http.StatusBadRequest)
				return
			}
			q.Body = io.NopCloser(bytes.NewReader(raw))
		}
		guard.ServeHTTP(w, q)
	})
	listener := &nativeProviderListener{connection: &nativeProviderConnection{Conn: channel, closed: make(chan struct{})}}
	bridge := &nativeProviderBridge{guard, policy, server, make(chan struct{})}
	go func() {
		stop := context.AfterFunc(ctx, func() { server.Close(); listener.Close() })
		defer stop()
		_ = server.Serve(listener)
		server.Close()
		listener.Close()
		close(bridge.done)
	}()
	return bridge, nil
}

// close waits for both HTTP intake and all guarded upstream/body work. This is
// an HTTP receipt, never a substitute for the native unit's whole-VM stop proof.
func (b *nativeProviderBridge) close(ctx context.Context) (*ProviderEvidence, error) {
	b.server.Close()
	select {
	case <-b.done:
	case <-ctx.Done():
		return &ProviderEvidence{b.policy, b.guard.Snapshot()}, ErrProviderEgressPending
	}
	access, err := b.guard.Close(ctx)
	return &ProviderEvidence{b.policy, access}, err
}

// The guest proxy uses one fixed plaintext HTTP connection inside its private
// VM channel. Only the host guard can turn a captured route into public HTTPS.
// A dropped stream cannot trigger another dial, inherited proxy or network IO.
// http.Transport splits Upgrade and ordinary requests into different connection
// pools. This private HTTP/1 stream instead serializes requests through body EOF.
type nativeProviderHTTPTransport struct {
	channel   net.Conn
	reader    *bufio.Reader
	available chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func nativeProviderTransport(channel net.Conn) *nativeProviderHTTPTransport {
	t := &nativeProviderHTTPTransport{channel: channel, available: make(chan struct{}, 1), closed: make(chan struct{})}
	if channel != nil {
		t.reader = bufio.NewReaderSize(channel, 16<<10)
	}
	t.available <- struct{}{}
	return t
}

func (t *nativeProviderHTTPTransport) CloseIdleConnections() {
	t.closeOnce.Do(func() {
		close(t.closed)
		if t.channel != nil {
			t.channel.Close()
		}
	})
}

func (t *nativeProviderHTTPTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	reject := func() (*http.Response, error) {
		if q != nil && q.Body != nil {
			q.Body.Close()
		}
		if q != nil {
			return nil, errors.Join(ErrProviderEgressFailed, q.Context().Err())
		}
		return nil, ErrProviderEgressFailed
	}
	if q == nil || q.URL == nil || t.channel == nil || q.URL.Scheme != "http" || (q.URL.Host != "sfl-private.invalid" && q.URL.Host != "sfl-private.invalid:80") || q.URL.User != nil || q.RequestURI != "" || httptrace.ContextClientTrace(q.Context()) != nil {
		return reject()
	}
	select {
	case <-q.Context().Done():
		return reject()
	case <-t.closed:
		return reject()
	case <-t.available:
	}
	select {
	case <-t.closed:
		t.available <- struct{}{}
		return reject()
	default:
	}
	ctx, cancel := context.WithTimeout(q.Context(), 5*time.Minute)
	stop := context.AfterFunc(ctx, func() {
		t.CloseIdleConnections()
		if q.Body != nil {
			q.Body.Close()
		}
	})
	var once sync.Once
	finish := func(reusable bool) {
		once.Do(func() {
			stop()
			cancel()
			if !reusable {
				t.CloseIdleConnections()
			}
			t.available <- struct{}{}
		})
	}
	deadline, _ := ctx.Deadline()
	// Some guest character devices cannot install a descriptor deadline. The
	// context hook still closes the descriptor and interrupts blocked I/O.
	_ = t.channel.SetDeadline(deadline)
	copy := q.Clone(ctx)
	copy.GetBody = nil
	copy.Close = false
	err := copy.Write(t.channel)
	if copy.Body != nil {
		copy.Body.Close()
	}
	if err != nil {
		finish(false)
		return nil, ErrProviderEgressFailed
	}
	// Bound header parsing independently. Once parsed, the same reader carries
	// the body; the host guard separately enforces the captured response bound.
	limit := &io.LimitedReader{R: t.reader, N: 32 << 10}
	reader := bufio.NewReaderSize(limit, 4096)
	response, err := http.ReadResponse(reader, copy)
	if err != nil || response.StatusCode < 200 {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		finish(false)
		return nil, ErrProviderEgressFailed
	}
	limit.N = 1<<63 - 1
	if response.Body == http.NoBody {
		finish(!response.Close)
		return response, nil
	}
	response.Body = &nativeProviderResponseBody{body: response.Body, finish: func(complete bool) { finish(complete && !response.Close && reader.Buffered() == 0) }}
	return response, nil
}

type nativeProviderResponseBody struct {
	body   io.ReadCloser
	finish func(bool)
	once   sync.Once
}

func (b *nativeProviderResponseBody) done(complete bool) { b.once.Do(func() { b.finish(complete) }) }
func (b *nativeProviderResponseBody) Read(data []byte) (int, error) {
	n, err := b.body.Read(data)
	if err != nil {
		b.done(err == io.EOF)
	}
	return n, err
}
func (b *nativeProviderResponseBody) Close() error {
	// An unread or partially read body cannot be replayed or reused as another
	// response. Close the private stream instead of silently draining/retrying.
	b.done(false)
	return b.body.Close()
}
