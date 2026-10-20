package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
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
func nativeProviderTransport(channel net.Conn) *http.Transport {
	var mu sync.Mutex
	spent := false
	return &http.Transport{Proxy: nil, DisableCompression: true, ForceAttemptHTTP2: false, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 5 * time.Minute, ResponseHeaderTimeout: 5 * time.Minute,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if spent || ctx.Err() != nil || network != "tcp" || address != "sfl-private.invalid:80" {
				return nil, ErrProviderEgressFailed
			}
			spent = true
			return channel, nil
		},
	}
}
