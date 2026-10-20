package reviewer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func negotiationPolicy() EgressPolicy {
	p := egressTestPolicy(Codex)
	p.Routes = append(p.Routes, EgressRoute{"provider.example", "GET", "/infer", "local_negotiation", ""})
	return p
}

func negotiationHeaders(r *http.Request) {
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-Websocket-Key", "AAAAAAAAAAAAAAAAAAAAAA==")
	r.Header.Set("Sec-Websocket-Version", "13")
	r.Header.Set("Sec-Websocket-Extensions", "permessage-deflate; client_max_window_bits")
}

func TestProviderEgressLocalNegotiationAndCapturedMetadataBudget(t *testing.T) {
	p := negotiationPolicy()
	p.Limits.RequestBytes, p.Limits.ResponseBytes = 2, 2
	b, server, calls := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" || r.Header.Get("Sec-Websocket-Key") != "" {
			t.Error("negotiation headers reached backend")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	status, body := egressRequest(t, server, "GET", "provider.example", "/infer", "", negotiationHeaders)
	if status != 426 || body != "" || calls.Load() != 0 {
		t.Fatal("negotiation was not local", status, body, calls.Load())
	}
	for i := 0; i < 5; i++ {
		if status, _ := egressRequest(t, server, "GET", "provider.example", "/metadata", "", nil); status != 200 {
			t.Fatal("captured startup budget rejected", i, status)
		}
	}
	if status, _ := egressRequest(t, server, "POST", "provider.example", "/infer", `{}`, nil); status != 200 {
		t.Fatal("HTTP inference failed after negotiation", status)
	}
	a, err := b.Close(context.Background())
	if err != nil || a.ValidateCompleted(testSession(Codex), p) != nil || a.Requests != 7 || a.Forwarded != 6 || a.Responses != 6 || a.NegotiationRequests != 1 || a.NegotiationResponses != 1 || a.MetadataRequests != 5 || calls.Load() != 6 {
		t.Fatal("wrong private count accounting", a, err)
	}
	for _, mutate := range []func(*ProviderAccess){
		func(a *ProviderAccess) { a.NegotiationResponses = 0 },
		func(a *ProviderAccess) { a.Forwarded++ },
		func(a *ProviderAccess) { a.Requests-- },
		func(a *ProviderAccess) { a.RequestBytes = 13 },
		func(a *ProviderAccess) { a.ResponseBytes = 13 },
		func(a *ProviderAccess) { a.NegotiationRequests = -1 },
		func(a *ProviderAccess) { a.NegotiationRequests++; a.NegotiationResponses++ },
	} {
		changed := a
		mutate(&changed)
		if changed.ValidateCompleted(testSession(Codex), p) == nil {
			t.Fatal("inconsistent negotiation receipt accepted", changed)
		}
	}
}

func TestProviderEgressNegotiationCannotOpenTunnelOrAcceptPayload(t *testing.T) {
	for _, kind := range []string{"method", "path", "query", "host", "body", "chunked", "missing-upgrade", "connection-token", "duplicate-key", "bad-key", "version", "duplicate-extension", "api-key", "cookie"} {
		t.Run(kind, func(t *testing.T) {
			p := negotiationPolicy()
			physical := 0
			b, err := NewProviderEgress(context.Background(), testSession(Codex), p, egressRoundTrip(func(*http.Request) (*http.Response, error) {
				physical++
				return nil, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "http://provider.example/infer", nil)
			r.URL.Scheme, r.URL.Host = "", ""
			negotiationHeaders(r)
			switch kind {
			case "method":
				r.Method = "POST"
			case "path":
				r.URL.Path = "/other"
			case "query":
				r.URL.RawQuery = "target=other"
			case "host":
				r.Host = "other.example"
			case "body":
				r.Body = http.NoBody
				r.ContentLength = 1
			case "chunked":
				r.TransferEncoding = []string{"chunked"}
			case "missing-upgrade":
				r.Header.Del("Upgrade")
			case "connection-token":
				r.Header.Set("Connection", "Upgrade, Authorization")
			case "duplicate-key":
				r.Header.Add("Sec-Websocket-Key", "AAAAAAAAAAAAAAAAAAAAAA==")
			case "bad-key":
				r.Header.Set("Sec-Websocket-Key", "invalid")
			case "version":
				r.Header.Set("Sec-Websocket-Version", "12")
			case "duplicate-extension":
				r.Header.Add("Sec-Websocket-Extensions", "other")
			case "api-key":
				r.Header.Set("X-Api-Key", "synthetic-canary")
			case "cookie":
				r.Header.Set("Cookie", "synthetic-canary")
			}
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)
			a, err := b.Close(context.Background())
			if err != nil || w.Code != 403 || !a.Failed || physical != 0 || strings.Contains(w.Body.String(), "canary") {
				t.Fatal(w.Code, a, err, physical)
			}
		})
	}
}

func TestProviderEgressRepeatedNegotiationAndMetadataExhaustBudget(t *testing.T) {
	for _, kind := range []string{"negotiation", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			p := negotiationPolicy()
			b, server, calls := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			count, endpoint, change := 2, "/infer", negotiationHeaders
			if kind == "metadata" {
				count, endpoint, change = 6, "/metadata", nil
			}
			for i := 0; i < count; i++ {
				status, _ := egressRequest(t, server, "GET", "provider.example", endpoint, "", change)
				if i == count-1 && status != 429 {
					t.Fatal("overrun accepted", status)
				}
			}
			a, err := b.Close(context.Background())
			if err != nil || !a.Failed || !a.Exhausted || a.ValidateCompleted(testSession(Codex), p) == nil {
				t.Fatal(a, err)
			}
			if kind == "negotiation" && (calls.Load() != 0 || a.NegotiationRequests != 1 || a.NegotiationResponses != 1) {
				t.Fatal("negotiation forwarded or retried", a, calls.Load())
			}
		})
	}
}

func TestProviderEgressNegotiationRequiresExplicitCodexPolicy(t *testing.T) {
	for _, kind := range []string{"claude", "zero-budget", "excess-budget", "wrong-method", "unpaired"} {
		p := negotiationPolicy()
		provider := Codex
		switch kind {
		case "claude":
			provider = Claude
			p.Provider = Claude
			p.Limits = DefaultEgressLimits(testSession(Claude))
		case "zero-budget":
			p.Limits.NegotiationRequests = 0
		case "excess-budget":
			p.Limits.NegotiationRequests = 2
		case "wrong-method":
			p.Routes[len(p.Routes)-1].Method = "POST"
		case "unpaired":
			p.Routes[len(p.Routes)-1].Path = "/other"
		}
		b, err := NewProviderEgress(context.Background(), testSession(provider), p, egressRoundTrip(func(*http.Request) (*http.Response, error) {
			t.Fatal("constructor contacted upstream")
			return nil, nil
		}))
		if err == nil || b != nil {
			t.Fatal("invalid policy accepted", kind)
		}
	}
}
