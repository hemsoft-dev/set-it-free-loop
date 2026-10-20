package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrProviderEgressPending = errors.New("provider egress shutdown requires recovery")
var ErrProviderEgressFailed = errors.New("provider egress receipt cannot establish successful work")

// EgressRoute is an exact endpoint supplied by trusted, pinned-client policy.
// It is never derived from a model prompt, HTTP Host override or redirect.
type EgressRoute struct {
	Authority string `json:"authority"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Kind      string `json:"kind"` // inference, refresh or metadata
	RawQuery  string `json:"raw_query,omitempty"`
}

type EgressLimits struct {
	Requests          int `json:"requests"`
	InferenceRequests int `json:"inference_requests"`
	RefreshRequests   int `json:"refresh_requests"`
	MetadataRequests  int `json:"metadata_requests"`
	RequestBytes      int `json:"request_bytes"`
	ResponseBytes     int `json:"response_bytes"`
	TotalBytes        int `json:"total_bytes"`
}

func DefaultEgressLimits(s Session) EgressLimits {
	return EgressLimits{s.Limits.MaxTurns + 6, s.Limits.MaxTurns, 2, 4, 8 << 20, 16 << 20, 32 << 20}
}

type EgressPolicy struct {
	Provider Provider      `json:"provider"`
	Routes   []EgressRoute `json:"routes"`
	Headers  []string      `json:"headers"`
	Limits   EgressLimits  `json:"limits"`
}

// ProviderAccess is a private supervisor receipt, never model-authored usage.
// Requests counts reservations, including rejected bodies. Forwarded counts
// RoundTrip calls, and Responses counts complete accepted upstream responses.
// Bytes count observed payload, including at most one overrun byte on failure.
// This is HTTP request evidence, not a measure of billed tokens or sessions.
type ProviderAccess struct {
	Version           int    `json:"version"`
	SessionHash       string `json:"session_hash"`
	PolicyHash        string `json:"policy_hash"`
	Requests          int    `json:"requests"`
	InferenceRequests int    `json:"inference_requests"`
	RefreshRequests   int    `json:"refresh_requests"`
	MetadataRequests  int    `json:"metadata_requests"`
	Forwarded         int    `json:"forwarded"`
	Responses         int    `json:"responses"`
	RequestBytes      int    `json:"request_bytes"`
	ResponseBytes     int    `json:"response_bytes"`
	Failed            bool   `json:"failed"`
	Exhausted         bool   `json:"exhausted"`
	Stopped           bool   `json:"stopped"`
}

// ValidateCompleted is required before accepting a model report. A successful
// local stop alone cannot turn denied, incomplete or absent provider work into
// clean success. The supervisor must persist this private receipt on failures.
func (a *ProviderAccess) ValidateCompleted(s Session, p EgressPolicy) error {
	if a == nil || !a.Stopped {
		return ErrProviderEgressPending
	}
	l := p.Limits
	if s.Validate() != nil || p.Provider != s.Identity.Provider || a.Version != 1 || a.SessionHash != Digest(s) || a.PolicyHash != Digest(p) || a.Failed || a.Exhausted || a.InferenceRequests < 1 || a.InferenceRequests > l.InferenceRequests || a.RefreshRequests < 0 || a.RefreshRequests > l.RefreshRequests || a.MetadataRequests < 0 || a.MetadataRequests > l.MetadataRequests || a.Requests != a.InferenceRequests+a.RefreshRequests+a.MetadataRequests || a.Requests > l.Requests || a.Requests != a.Forwarded || a.Forwarded != a.Responses || a.RequestBytes < 0 || a.ResponseBytes < 0 || a.RequestBytes > l.TotalBytes || a.ResponseBytes > l.TotalBytes-a.RequestBytes || a.RequestBytes > a.Requests*l.RequestBytes || a.ResponseBytes > a.Requests*l.ResponseBytes {
		return ErrProviderEgressFailed
	}
	return nil
}

// ProviderEgress is a session-bound HTTP guard, not an activated network route.
// The transport is trusted code: it must honor cancellation, make at most one
// physical request per RoundTrip and never redirect or route outside request.URL.
// There is deliberately no default transport, listener, auth or live policy.
// A production launcher must authenticate the private peer, enforce exclusive
// egress through this guard and require its stopped, nonfailed private receipt.
type ProviderEgress struct {
	mu        sync.Mutex
	policy    EgressPolicy
	headers   map[string]bool
	transport http.RoundTripper
	ctx       context.Context
	cancel    context.CancelFunc
	closing   bool
	active    bool
	done      chan struct{}
	audit     ProviderAccess
}

func egressAuthority(authority string) bool {
	if len(authority) == 0 || len(authority) > 253 || authority != strings.ToLower(authority) || strings.ContainsAny(authority, ":/@?#\\%") || net.ParseIP(authority) != nil {
		return false
	}
	labels := strings.Split(authority, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func egressPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && path.Clean(p) == p && !strings.ContainsAny(p, "%\\?#\x00\r\n")
}

func egressQuery(query string) bool {
	if len(query) > 1024 {
		return false
	}
	v, err := url.ParseQuery(query)
	return err == nil && v.Encode() == query
}

func egressHeaderAllowed(name string) bool {
	switch strings.ToLower(name) {
	case "host", "connection", "keep-alive", "proxy-connection", "proxy-authorization", "proxy-authenticate", "upgrade", "te", "trailer", "transfer-encoding", "content-length", "cookie", "set-cookie", "x-api-key", "api-key":
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return name != "" && http.CanonicalHeaderKey(name) == name
}

func NewProviderEgress(ctx context.Context, s Session, p EgressPolicy, transport http.RoundTripper) (*ProviderEgress, error) {
	if s.Validate() != nil || ctx == nil || ctx.Err() != nil || transport == nil || p.Provider != s.Identity.Provider || len(p.Routes) == 0 || len(p.Routes) > 16 || len(p.Headers) > 24 {
		return nil, ErrLiveBlocked
	}
	m := DefaultEgressLimits(s)
	l := p.Limits
	if l.Requests <= 0 || l.Requests > m.Requests || l.InferenceRequests < 0 || l.InferenceRequests > m.InferenceRequests || l.RefreshRequests < 0 || l.RefreshRequests > m.RefreshRequests || l.MetadataRequests < 0 || l.MetadataRequests > m.MetadataRequests || l.Requests > l.InferenceRequests+l.RefreshRequests+l.MetadataRequests || l.RequestBytes <= 0 || l.RequestBytes > m.RequestBytes || l.ResponseBytes <= 0 || l.ResponseBytes > m.ResponseBytes || l.TotalBytes <= 0 || l.TotalBytes > m.TotalBytes {
		return nil, ErrLiveBlocked
	}
	seen := map[string]bool{}
	for _, route := range p.Routes {
		key := route.Authority + " " + route.Method + " " + route.Path + "?" + route.RawQuery
		if !egressAuthority(route.Authority) || !egressPath(route.Path) || !egressQuery(route.RawQuery) || (route.Method != "POST" && route.Method != "GET") || seen[key] || (route.Kind != "inference" && route.Kind != "refresh" && route.Kind != "metadata") || (route.Kind != "metadata" && route.Method != "POST") {
			return nil, ErrLiveBlocked
		}
		seen[key] = true
	}
	headers := map[string]bool{}
	for _, h := range p.Headers {
		if !egressHeaderAllowed(h) || headers[h] {
			return nil, ErrLiveBlocked
		}
		headers[h] = true
	}
	p.Routes = append([]EgressRoute(nil), p.Routes...)
	p.Headers = append([]string(nil), p.Headers...)
	ctx, cancel := context.WithTimeout(ctx, s.Limits.Timeout)
	done := make(chan struct{})
	close(done)
	return &ProviderEgress{policy: p, headers: headers, transport: transport, ctx: ctx, cancel: cancel, done: done, audit: ProviderAccess{Version: 1, SessionHash: Digest(s), PolicyHash: Digest(p)}}, nil
}

func (b *ProviderEgress) Snapshot() ProviderAccess { b.mu.Lock(); defer b.mu.Unlock(); return b.audit }

func (b *ProviderEgress) fail(exhausted bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.audit.Stopped {
		return
	}
	b.audit.Failed = true
	b.audit.Exhausted = b.audit.Exhausted || exhausted
	b.cancel()
}

func (b *ProviderEgress) route(r *http.Request) (EgressRoute, bool) {
	if r.URL == nil || r.URL.IsAbs() || r.URL.Host != "" || r.URL.User != nil || r.URL.Fragment != "" || !egressQuery(r.URL.RawQuery) || r.URL.ForceQuery || r.URL.RawPath != "" || !egressPath(r.URL.Path) || len(r.Trailer) != 0 {
		return EgressRoute{}, false
	}
	for name, values := range r.Header {
		if name == "Content-Length" {
			if len(values) != 1 {
				return EgressRoute{}, false
			}
			n, err := strconv.ParseInt(values[0], 10, 64)
			if err != nil || n < 0 || n != r.ContentLength {
				return EgressRoute{}, false
			}
			continue // Rebuild framing from the captured body, never forward it.
		}
		if name == "Connection" {
			for _, v := range values {
				for _, token := range strings.Split(v, ",") {
					token = strings.TrimSpace(strings.ToLower(token))
					if token != "close" && token != "keep-alive" {
						return EgressRoute{}, false
					}
				}
			}
			continue
		}
		if !b.headers[name] || len(values) != 1 || len(values[0]) > 8192 {
			return EgressRoute{}, false
		}
		if name == "Authorization" && (!strings.HasPrefix(values[0], "Bearer ") || len(values[0]) <= len("Bearer ") || strings.ContainsAny(values[0], "\r\n\x00")) {
			return EgressRoute{}, false
		}
	}
	for _, route := range b.policy.Routes {
		if r.Host == route.Authority && r.Method == route.Method && r.URL.Path == route.Path && r.URL.RawQuery == route.RawQuery {
			return route, true
		}
	}
	return EgressRoute{}, false
}

func (b *ProviderEgress) reserve(route EgressRoute) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.audit.Stopped {
		return false
	}
	if b.closing || b.audit.Failed || b.ctx.Err() != nil || b.active {
		b.audit.Failed = true
		b.cancel()
		return false
	}
	l := b.policy.Limits
	var n *int
	var max int
	switch route.Kind {
	case "inference":
		n, max = &b.audit.InferenceRequests, l.InferenceRequests
	case "refresh":
		n, max = &b.audit.RefreshRequests, l.RefreshRequests
	case "metadata":
		n, max = &b.audit.MetadataRequests, l.MetadataRequests
	}
	if b.audit.Requests >= l.Requests || *n >= max {
		b.audit.Failed = true
		b.audit.Exhausted = true
		b.cancel()
		return false
	}
	b.audit.Requests++
	*n++
	b.active = true
	b.done = make(chan struct{})
	return true
}

func (b *ProviderEgress) release() { b.mu.Lock(); defer b.mu.Unlock(); b.active = false; close(b.done) }

func (b *ProviderEgress) remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.policy.Limits.TotalBytes - b.audit.RequestBytes - b.audit.ResponseBytes
}

func (b *ProviderEgress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := b.route(r)
	if !ok {
		b.fail(false)
		http.Error(w, "provider route denied", http.StatusForbidden)
		return
	}
	if !b.reserve(route) {
		http.Error(w, "provider bridge stopped or exhausted", http.StatusTooManyRequests)
		return
	}
	defer b.release()
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	stopClient := context.AfterFunc(r.Context(), cancel)
	defer stopClient()
	if r.Body == nil {
		r.Body = http.NoBody
	}
	stopInput := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopInput()
	requestCap := min(b.policy.Limits.RequestBytes, b.remaining())
	if requestCap < 0 || r.ContentLength > int64(requestCap) {
		b.fail(true)
		http.Error(w, "provider input limit", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(requestCap)+1))
	_ = r.Body.Close()
	b.mu.Lock()
	b.audit.RequestBytes += len(body)
	b.mu.Unlock()
	if err != nil || len(body) > requestCap || len(r.Trailer) != 0 || ctx.Err() != nil {
		b.fail(len(body) > requestCap)
		http.Error(w, "provider input incomplete or oversized", http.StatusRequestEntityTooLarge)
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, route.Method, (&url.URL{Scheme: "https", Host: route.Authority, Path: route.Path, RawQuery: route.RawQuery}).String(), bytes.NewReader(body))
	if err != nil {
		b.fail(false)
		http.Error(w, "provider request unavailable", http.StatusBadGateway)
		return
	}
	upstream.GetBody = nil
	upstream.Close = true
	// An absent key lets net/http add its own User-Agent. A nil value
	// explicitly suppresses that default when the client supplied none.
	upstream.Header["User-Agent"] = nil
	for name, values := range r.Header {
		if b.headers[name] && name != "Accept-Encoding" {
			upstream.Header[name] = append([]string(nil), values...)
		}
	}
	upstream.Header.Set("Accept-Encoding", "identity")
	b.mu.Lock()
	b.audit.Forwarded++
	b.mu.Unlock()
	response, err := b.transport.RoundTrip(upstream)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.Body == nil {
		b.fail(false)
		http.Error(w, "provider response unavailable", http.StatusBadGateway)
		return
	}
	stopOutput := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
	defer stopOutput()
	encoding := response.Header.Values("Content-Encoding")
	if response.StatusCode < 200 || response.StatusCode >= 300 && response.StatusCode < 400 || response.StatusCode > 599 || len(response.Trailer) != 0 || response.Header.Get("Upgrade") != "" || len(encoding) != 0 && (len(encoding) != 1 || encoding[0] != "identity") {
		b.fail(false)
		http.Error(w, "provider response denied", http.StatusBadGateway)
		return
	}
	responseCap := min(b.policy.Limits.ResponseBytes, b.remaining())
	if responseCap < 0 || response.ContentLength > int64(responseCap) {
		b.fail(true)
		http.Error(w, "provider output limit", http.StatusBadGateway)
		return
	}
	output, err := io.ReadAll(io.LimitReader(response.Body, int64(responseCap)+1))
	b.mu.Lock()
	b.audit.ResponseBytes += len(output)
	b.mu.Unlock()
	if err != nil || len(output) > responseCap || len(response.Trailer) != 0 || ctx.Err() != nil {
		b.fail(len(output) > responseCap)
		http.Error(w, "provider response incomplete or oversized", http.StatusBadGateway)
		return
	}
	typeHeader := response.Header.Get("Content-Type")
	if len(output) > 0 {
		kind, _, err := mime.ParseMediaType(typeHeader)
		if err != nil || kind != "application/json" && kind != "text/event-stream" {
			b.fail(false)
			http.Error(w, "provider response format denied", http.StatusBadGateway)
			return
		}
	}
	b.mu.Lock()
	b.audit.Responses++
	b.mu.Unlock()
	if typeHeader != "" {
		w.Header().Set("Content-Type", typeHeader)
	}
	w.WriteHeader(response.StatusCode)
	if n, err := w.Write(output); err != nil || n != len(output) {
		b.fail(false)
	}
}

// Close cancels in-flight IO and waits for its completion before attesting stop.
// A timeout returns the incomplete receipt and cannot release a worker lease.
func (b *ProviderEgress) Close(ctx context.Context) (ProviderAccess, error) {
	b.mu.Lock()
	if b.audit.Stopped {
		a := b.audit
		b.mu.Unlock()
		return a, nil
	}
	b.closing = true
	if b.active || b.ctx.Err() != nil {
		b.audit.Failed = true
	}
	b.cancel()
	done := b.done
	b.mu.Unlock()
	select {
	case <-done:
		b.mu.Lock()
		b.audit.Stopped = true
		a := b.audit
		b.mu.Unlock()
		return a, nil
	case <-ctx.Done():
		return b.Snapshot(), ErrProviderEgressPending
	}
}

// NewProviderEgressServer supplies HTTP intake bounds; it neither listens nor
// authenticates peers. The isolated supervisor owns a private, peer-checked
// listener with bounded connections and must never expose this on a host LAN.
func NewProviderEgressServer(b *ProviderEgress) *http.Server {
	return &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
}
