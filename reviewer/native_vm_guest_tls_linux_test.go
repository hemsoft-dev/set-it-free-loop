//go:build linux

package reviewer

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeTLSFixture(t *testing.T, handler http.Handler, policies ...EgressPolicy) (*nativeGuestTLSProxy, *http.Client, string, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	stops := &atomic.Int32{}
	policy := EgressPolicy{Routes: []EgressRoute{{Authority: "refresh.example", Method: "POST", Path: "/oauth/token", Kind: "refresh"}}}
	if len(policies) == 1 {
		policy = policies[0]
	}
	p, certificate, err := newNativeGuestTLSProxy(ctx, policy, handler, func() { stops.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	proxyURL, _ := url.Parse(server.URL)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("public session certificate")
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true, ForceAttemptHTTP2: false}
	t.Cleanup(transport.CloseIdleConnections)
	return p, &http.Client{Transport: transport, Timeout: 3 * time.Second}, proxyURL.Host, stops
}

func TestNativeGuestTLSUsesSeparateNonCALeafCertificate(t *testing.T) {
	p, _, _, _ := nativeTLSFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if len(p.certificate.Certificate) != 2 {
		t.Fatal("root and end-entity chain missing")
	}
	leaf, err := x509.ParseCertificate(p.certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(p.certificate.Certificate[1])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || !issuer.IsCA || leaf.CheckSignatureFrom(issuer) != nil || leaf.VerifyHostname("refresh.example") != nil {
		t.Fatal("end-entity certificate violates client PKI requirements")
	}
}

func TestNativeGuestTLSMetadataCannotRedirectAnotherAuthority(t *testing.T) {
	policy := EgressPolicy{Routes: []EgressRoute{{Authority: "refresh.example", Method: "POST", Path: "/oauth/token", Kind: "refresh"}, {Authority: "profile.example", Method: "GET", Path: "/profile", Kind: "metadata"}}, Limits: EgressLimits{ResponseBytes: 128}}
	var calls atomic.Int32
	transport := egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		calls.Add(1)
		if q.Host != "profile.example" {
			t.Error("metadata authority changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, client, _, stops := nativeTLSFixture(t, nativeGuestProviderHandler(nativeVMRequest{Policy: policy}, transport, func() {}), policy)
	response, err := client.Get("https://profile.example/profile")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal("declared metadata failed")
	}
	response, err = client.Get("https://refresh.example/profile")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || calls.Load() != 1 || stops.Load() != 0 {
		t.Fatal("foreign authority selected metadata route")
	}
}

func TestNativeGuestTLSRefreshUsesVerifiedTLSAndOnlyPrivateHandler(t *testing.T) {
	var calls atomic.Int32
	_, client, _, stops := nativeTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(q.Body)
		if q.TLS == nil || q.TLS.ServerName != "refresh.example" || q.Host != "refresh.example" || q.Method != "POST" || q.URL.Path != "/oauth/token" || string(body) != "synthetic refresh token" {
			t.Error("TLS/request provenance lost")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"synthetic rotated token"}`)
	}))
	response, err := client.Post("https://refresh.example/oauth/token", "application/json", strings.NewReader("synthetic refresh token"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(raw), "rotated token") || calls.Load() != 1 || stops.Load() != 0 {
		t.Fatal("private refresh failed", err, response.StatusCode)
	}
}

func TestNativeGuestTLSRejectsOtherAuthoritiesPortsAndHostOverride(t *testing.T) {
	var calls atomic.Int32
	_, client, _, stops := nativeTLSFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	for _, target := range []string{"https://outside.example/oauth/token", "https://127.0.0.1/oauth/token", "https://refresh.example:8443/oauth/token"} {
		response, err := client.Get(target)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("foreign CONNECT accepted", target)
		}
	}
	q, _ := http.NewRequest("POST", "https://refresh.example/oauth/token", nil)
	q.Host = "outside.example"
	response, err := client.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || calls.Load() != 0 || stops.Load() != 4 {
		t.Fatal("foreign inner Host reached handler", response.StatusCode, calls.Load(), stops.Load())
	}
}

func TestNativeGuestTLSCertificateCannotBeReusedAcrossSessions(t *testing.T) {
	_, first, _, _ := nativeTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	_, second, _, _ := nativeTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	first.Transport.(*http.Transport).TLSClientConfig.RootCAs = second.Transport.(*http.Transport).TLSClientConfig.RootCAs
	response, err := first.Get("https://refresh.example/oauth/token")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("different session CA accepted")
	}
}

func TestNativeGuestTLSCloseCancelsAndJoinsAnActiveRequest(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	p, client, _, _ := nativeTLSFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, q *http.Request) { close(started); <-q.Context().Done(); close(finished) }))
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, _ := client.Get("https://refresh.example/oauth/token")
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	if p.Close() != nil {
		t.Fatal("private TLS did not stop")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before handler stopped")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestNativeGuestTLSBoundsIncompleteHandshakes(t *testing.T) {
	p, _, address, stops := nativeTLSFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handshake bypass") }))
	connections := []net.Conn{}
	defer func() {
		for _, conn := range connections {
			conn.Close()
		}
	}()
	for range 4 {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(conn, "CONNECT refresh.example:443 HTTP/1.1\r\nHost: refresh.example:443\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil || response.StatusCode != 200 {
			t.Fatal("bounded handshake intake", err)
		}
	}
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	io.WriteString(conn, "CONNECT refresh.example:443 HTTP/1.1\r\nHost: refresh.example:443\r\n\r\n")
	var one [1]byte
	if _, err := conn.Read(one[:]); err == nil {
		t.Fatal("fifth handshake admitted")
	}
	if p.Close() != nil || stops.Load() == 0 {
		t.Fatal("incomplete handshake cleanup failed")
	}
}

func TestNativeGuestPlaintextRefreshCannotBypassTLSAuthority(t *testing.T) {
	policy := EgressPolicy{Routes: []EgressRoute{{Authority: "refresh.example", Method: "POST", Path: "/oauth/token", Kind: "refresh"}}, Limits: EgressLimits{ResponseBytes: 128}}
	var calls, stopped atomic.Int32
	handler := nativeGuestProviderHandler(nativeVMRequest{Policy: policy}, egressRoundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}), func() { stopped.Add(1) })
	_, _, address, _ := nativeTLSFixture(t, handler, policy)
	for _, host := range []string{"refresh.example", "outside.example"} {
		q, _ := http.NewRequest("POST", "http://"+address+"/oauth/token", strings.NewReader(`{"refresh_token":"synthetic"}`))
		q.Host = host
		response, err := http.DefaultClient.Do(q)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 403 || calls.Load() != 0 {
			t.Fatal("plaintext refresh bypassed TLS authority", response.StatusCode, calls.Load())
		}
	}
	if stopped.Load() != 2 {
		t.Fatal("plaintext denial did not stop the owned client")
	}
}

func TestNativeGuestTLSOnlyMetadataRejectsPlaintextButAllowsTLS(t *testing.T) {
	policy := EgressPolicy{Routes: []EgressRoute{{Authority: "profile.example", Method: "GET", Path: "/profile", Kind: "metadata", TLSOnly: true}}, Limits: EgressLimits{ResponseBytes: 128}}
	var calls, stopped atomic.Int32
	handler := nativeGuestProviderHandler(nativeVMRequest{Policy: policy}, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		calls.Add(1)
		if q.Host != "profile.example" {
			t.Error("metadata authority changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}), func() { stopped.Add(1) })
	_, client, address, _ := nativeTLSFixture(t, handler, policy)
	for _, host := range []string{"profile.example", "outside.example"} {
		q, _ := http.NewRequest("GET", "http://"+address+"/profile", nil)
		q.Host = host
		response, err := http.DefaultClient.Do(q)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 403 || calls.Load() != 0 {
			t.Fatal("TLS-only metadata bypassed authority", response.StatusCode, calls.Load())
		}
	}
	response, err := client.Get("https://profile.example/profile")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 || stopped.Load() != 2 {
		t.Fatal("TLS metadata failed or denial lost", response.StatusCode, calls.Load(), stopped.Load())
	}
}

func TestNativeGuestTLSOnlyMetadataEnablesProxyExplicitly(t *testing.T) {
	policy := EgressPolicy{Routes: []EgressRoute{{Authority: "profile.example", Method: "GET", Path: "/profile", Kind: "metadata", TLSOnly: true}}}
	if !nativeGuestTLSEnabled(policy) {
		t.Fatal("explicit TLS-only metadata has no TLS path")
	}
	policy.Routes[0].TLSOnly = false
	if nativeGuestTLSEnabled(policy) {
		t.Fatal("ordinary metadata unexpectedly enabled TLS")
	}
}

func TestNativeGuestOrdinaryMetadataRemainsAvailableWithoutTLS(t *testing.T) {
	policy := EgressPolicy{Routes: []EgressRoute{{Authority: "metadata.example", Method: "GET", Path: "/v1/models", Kind: "metadata"}}, Limits: EgressLimits{ResponseBytes: 128}}
	var calls atomic.Int32
	server := httptest.NewServer(nativeGuestProviderHandler(nativeVMRequest{Policy: policy}, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		calls.Add(1)
		if q.Host != "metadata.example" {
			t.Error("metadata authority changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}), func() { t.Error("intended local metadata stopped client") }))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 || nativeGuestTLSEnabled(policy) {
		t.Fatal("ordinary metadata route lost its local transport", response.StatusCode, calls.Load())
	}
}
