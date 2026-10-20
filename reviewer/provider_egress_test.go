package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type egressRoundTrip func(*http.Request) (*http.Response, error)

func (f egressRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func egressTestPolicy(p Provider) EgressPolicy {
	s := testSession(p)
	return EgressPolicy{Provider: p, Routes: []EgressRoute{{"provider.example", "POST", "/infer", "inference", ""}, {"auth.provider.example", "POST", "/refresh", "refresh", ""}, {"provider.example", "GET", "/metadata", "metadata", ""}}, Headers: []string{"Authorization", "Content-Type", "Accept", "Accept-Encoding", "User-Agent"}, Limits: DefaultEgressLimits(s)}
}

func egressTestBridge(t *testing.T, p EgressPolicy, handler http.Handler) (*ProviderEgress, *httptest.Server, *atomic.Int32) {
	t.Helper()
	backend := httptest.NewTLSServer(handler)
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	var calls atomic.Int32
	rt := egressRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Scheme != "https" || r.URL.Host != "provider.example" && r.URL.Host != "auth.provider.example" || r.GetBody != nil || !r.Close {
			t.Error("unsafe upstream request")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Host = target.Host
		copy.URL = &u
		return backend.Client().Transport.RoundTrip(copy)
	})
	b, err := NewProviderEgress(context.Background(), testSession(p.Provider), p, rt)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = b.Close(ctx)
	})
	return b, server, &calls
}

func egressRequest(t *testing.T, server *httptest.Server, method, authority, endpoint, body string, change func(*http.Request)) (int, string) {
	t.Helper()
	r, err := http.NewRequest(method, server.URL+endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = authority
	r.Header.Set("Authorization", "Bearer synthetic-access-canary")
	r.Header.Set("Content-Type", "application/json")
	if change != nil {
		change(r)
	}
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func TestProviderEgressBothProvidersCountActualHTTPAndNoSessionInflation(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			p := egressTestPolicy(provider)
			p.Limits.Requests = 3
			p.Limits.InferenceRequests = 2
			p.Limits.RefreshRequests = 1
			p.Limits.MetadataRequests = 0
			var physical atomic.Int32
			b, server, calls := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				physical.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic-access-canary" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Cookie") != "" {
					t.Error("wrong credential/header destination")
				}
				input, _ := io.ReadAll(r.Body)
				if string(input) != "synthetic-source" {
					t.Error("request changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "must-not-forward")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			for _, endpoint := range []string{"/infer", "/refresh", "/infer"} {
				authority := "provider.example"
				if endpoint == "/refresh" {
					authority = "auth.provider.example"
				}
				status, body := egressRequest(t, server, "POST", authority, endpoint, "synthetic-source", nil)
				if status != 200 || body != `{"ok":true}` {
					t.Fatal(status, body)
				}
			}
			a, err := b.Close(context.Background())
			if err != nil || a.Failed || !a.Stopped || a.Requests != 3 || a.Forwarded != 3 || a.Responses != 3 || a.InferenceRequests != 2 || a.RefreshRequests != 1 || a.SessionHash != Digest(testSession(provider)) || a.PolicyHash != Digest(p) || calls.Load() != 3 || physical.Load() != 3 {
				t.Fatalf("wrong private receipt: %+v, %v", a, err)
			}
			again, err := b.Close(context.Background())
			if err != nil || again != a {
				t.Fatal("Close changed terminal receipt")
			}
			if err := a.ValidateCompleted(testSession(provider), p); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(a)
			if strings.Contains(string(raw), "canary") {
				t.Fatal("credential/source appeared in audit")
			}
			status, _ := egressRequest(t, server, "POST", "provider.example", "/infer", "synthetic-source", nil)
			if status != 429 || b.Snapshot() != a || calls.Load() != 3 {
				t.Fatal("stopped bridge changed or forwarded")
			}
		})
	}
}

func TestProviderEgressRejectsRoutesHeadersAndTunnelsBeforeForward(t *testing.T) {
	cases := []struct {
		name, method, host, path string
		change                   func(*http.Request)
	}{
		{"foreign", "POST", "other.example", "/infer", nil}, {"private", "POST", "127.0.0.1", "/infer", nil}, {"wrong-path", "POST", "provider.example", "/other", nil},
		{"traversal", "POST", "provider.example", "/a/../infer", nil}, {"encoded", "POST", "provider.example", "/%69nfer", nil}, {"query", "POST", "provider.example", "/infer?target=private", nil},
		{"connect", "CONNECT", "provider.example", "/infer", nil}, {"websocket", "POST", "provider.example", "/infer", func(r *http.Request) { r.Header.Set("Upgrade", "websocket"); r.Header.Set("Connection", "upgrade") }},
		{"api-key", "POST", "provider.example", "/infer", func(r *http.Request) { r.Header.Set("X-Api-Key", "synthetic-key-canary") }},
		{"cookie", "POST", "provider.example", "/infer", func(r *http.Request) { r.Header.Set("Cookie", "synthetic-cookie-canary") }},
		{"smuggled-connection", "POST", "provider.example", "/infer", func(r *http.Request) { r.Header.Set("Connection", "Authorization") }},
		{"duplicate-auth", "POST", "provider.example", "/infer", func(r *http.Request) { r.Header.Add("Authorization", "Bearer other") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, server, calls := egressTestBridge(t, egressTestPolicy(Codex), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("denied request reached backend") }))
			status, body := egressRequest(t, server, c.method, c.host, c.path, "synthetic-source", c.change)
			if status != 403 || calls.Load() != 0 || strings.Contains(body, "canary") {
				t.Fatal(status, body, calls.Load())
			}
			a, err := b.Close(context.Background())
			if err != nil || !a.Stopped || !a.Failed || a.Forwarded != 0 {
				t.Fatal(a, err)
			}
		})
	}
}

func TestProviderEgressRequestRetriesCannotExceedSeparateBudgets(t *testing.T) {
	for _, kind := range []string{"inference", "refresh", "total"} {
		t.Run(kind, func(t *testing.T) {
			p := egressTestPolicy(Claude)
			p.Limits.InferenceRequests = 2
			p.Limits.RefreshRequests = 1
			p.Limits.MetadataRequests = 0
			p.Limits.Requests = 3
			if kind == "total" {
				p.Limits.Requests = 1
			}
			b, server, calls := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				_, _ = io.WriteString(w, `{"quota":true}`)
			}))
			max := 2
			host, endpoint := "provider.example", "/infer"
			if kind == "refresh" {
				max = 1
				host = "auth.provider.example"
				endpoint = "/refresh"
			}
			if kind == "total" {
				max = 1
			}
			for i := 0; i <= max; i++ {
				status, _ := egressRequest(t, server, "POST", host, endpoint, `{}`, nil)
				if status != 429 {
					t.Fatal(status)
				}
			}
			a, err := b.Close(context.Background())
			if err != nil || !a.Failed || !a.Exhausted || a.Forwarded != max || int(calls.Load()) != max {
				t.Fatal(a, err)
			}
		})
	}
}

func TestProviderEgressExactByteCapsAndOverrun(t *testing.T) {
	for _, kind := range []string{"exact", "input", "output", "total"} {
		t.Run(kind, func(t *testing.T) {
			p := egressTestPolicy(Codex)
			p.Limits.RequestBytes = 5
			p.Limits.ResponseBytes = 5
			p.Limits.TotalBytes = 10
			if kind == "total" {
				p.Limits.TotalBytes = 9
			}
			b, server, calls := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.(http.Flusher).Flush()
				data := `"bar"`
				if kind == "output" {
					data += "x"
				}
				_, _ = io.WriteString(w, data)
			}))
			input := `"foo"`
			if kind == "input" {
				input += "x"
			}
			status, body := egressRequest(t, server, "POST", "provider.example", "/infer", input, func(r *http.Request) { r.ContentLength = -1 })
			a, err := b.Close(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if kind == "exact" {
				if status != 200 || body != `"bar"` || a.Failed || a.RequestBytes != 5 || a.ResponseBytes != 5 {
					t.Fatal(status, body, a)
				}
			} else {
				if status == 200 || !a.Failed || !a.Exhausted || a.Responses != 0 {
					t.Fatal(status, body, a)
				}
				if kind == "input" && calls.Load() != 0 {
					t.Fatal("oversized input forwarded")
				}
			}
		})
	}
}

func TestProviderEgressRejectsRedirectCompressedAndPartialResponses(t *testing.T) {
	for _, kind := range []string{"redirect", "compressed", "trailer", "incomplete", "error-canary"} {
		t.Run(kind, func(t *testing.T) {
			p := egressTestPolicy(Codex)
			rt := egressRoundTrip(func(*http.Request) (*http.Response, error) {
				if kind == "error-canary" {
					return nil, errors.New("private-provider-error-canary")
				}
				r := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}
				switch kind {
				case "redirect":
					r.StatusCode = 302
					r.Header.Set("Location", "http://127.0.0.1/private-canary")
				case "compressed":
					r.Header.Set("Content-Encoding", "gzip")
				case "trailer":
					r.Trailer = http.Header{"X-Canary": nil}
				case "incomplete":
					r.Body = io.NopCloser(egressFailingReader{})
				}
				return r, nil
			})
			b, err := NewProviderEgress(context.Background(), testSession(Codex), p, rt)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
			r.Host = "provider.example"
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)
			if w.Code != 502 || strings.Contains(w.Body.String(), "canary") || w.Header().Get("Location") != "" {
				t.Fatal(w.Code, w.Body.String())
			}
			a, err := b.Close(context.Background())
			if err != nil || !a.Failed || a.Responses != 0 {
				t.Fatal(a, err)
			}
		})
	}
}

type egressFailingReader struct{}

func (egressFailingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestProviderEgressUnknownTransportStopRetainsIncompleteAudit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	rt := egressRoundTrip(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return nil, errors.New("synthetic loss")
	})
	b, err := NewProviderEgress(context.Background(), testSession(Codex), egressTestPolicy(Codex), rt)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
		r.Host = "provider.example"
		b.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	a, err := b.Close(ctx)
	if !errors.Is(err, ErrProviderEgressPending) || a.Stopped || !a.Failed || a.Forwarded != 1 {
		t.Fatal(a, err)
	}
	close(release)
	<-done
	a, err = b.Close(context.Background())
	if err != nil || !a.Stopped || !a.Failed {
		t.Fatal(a, err)
	}
}

func TestProviderEgressCapturesPolicyAndFailsWithoutExplicitTransport(t *testing.T) {
	p := egressTestPolicy(Codex)
	s := testSession(Codex)
	if _, err := NewProviderEgress(context.Background(), s, p, nil); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("default live transport selected")
	}
	_, server, _ := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	p.Routes[0].Authority = "other.example"
	p.Headers[0] = "Cookie"
	status, _ := egressRequest(t, server, "POST", "provider.example", "/infer", `{}`, nil)
	if status != 200 {
		t.Fatal("caller mutated captured policy", status)
	}
}

func TestProviderEgressConcurrentRequestCancelsActiveWork(t *testing.T) {
	entered := make(chan struct{})
	var forwarded atomic.Int32
	rt := egressRoundTrip(func(r *http.Request) (*http.Response, error) {
		forwarded.Add(1)
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	p := egressTestPolicy(Codex)
	s := testSession(Codex)
	b, err := NewProviderEgress(context.Background(), s, p, rt)
	if err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		r := httptest.NewRequest("POST", "/infer", strings.NewReader(`{}`))
		r.Host = "provider.example"
		return r
	}
	done := make(chan struct{})
	go func() { defer close(done); b.ServeHTTP(httptest.NewRecorder(), request()) }()
	<-entered
	w := httptest.NewRecorder()
	b.ServeHTTP(w, request())
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
	<-done
	a, err := b.Close(context.Background())
	if err != nil || forwarded.Load() != 1 || !a.Failed || !a.Stopped {
		t.Fatal(a, err)
	}
	if err := a.ValidateCompleted(s, p); !errors.Is(err, ErrProviderEgressFailed) {
		t.Fatal("failed phase became clean", err)
	}
	var missing *ProviderAccess
	if err := missing.ValidateCompleted(s, p); !errors.Is(err, ErrProviderEgressPending) {
		t.Fatal("missing receipt accepted", err)
	}
}

func TestProviderEgressMetadataAllowsOnlyCapturedLiteralQuery(t *testing.T) {
	p := egressTestPolicy(Codex)
	p.Routes[2].RawQuery = "client_version=fake-v1"
	b, server, calls := egressTestBridge(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "client_version=fake-v1" {
			t.Error("query changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	status, _ := egressRequest(t, server, "GET", "provider.example", "/metadata?client_version=fake-v1", "", nil)
	if status != 200 {
		t.Fatal(status)
	}
	status, _ = egressRequest(t, server, "GET", "provider.example", "/metadata?client_version=other", "", nil)
	if status != 403 || calls.Load() != 1 {
		t.Fatal("uncaptured query reached backend")
	}
	a, _ := b.Close(context.Background())
	if !a.Failed || a.MetadataRequests != 1 {
		t.Fatal(a)
	}
}
