package reviewer

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicEgressRejectsPrivateSpecialAndTransitionAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.1.2.3", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "::", "::1", "fd7a:115c:a1e0::1", "fe80::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2001::1", "2001:db8::1", "2002:7f00:1::", "3fff::1", "2001:4860:4860::8888%eth0"} {
		t.Run(raw, func(t *testing.T) {
			if publicEgressIP(netip.MustParseAddr(raw)) {
				t.Fatal("forbidden address accepted")
			}
		})
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		if !publicEgressIP(netip.MustParseAddr(raw)) {
			t.Fatal("public address rejected", raw)
		}
	}
}

func TestPublicEgressPinsOneCheckedDNSResultAndIgnoresProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://private-proxy.invalid:3128")
	p := egressTestPolicy(Codex)
	var lookups, dials int
	transport, err := publicEgressTransport(p, func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		lookups++
		if network != "ip" || host != "provider.example" {
			t.Error(network, host)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if network != "tcp" || address != "8.8.8.8:443" {
			t.Error("unvalidated dial", network, address)
		}
		return nil, errors.New("synthetic no-network dial")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil {
		t.Fatal("ambient proxy enabled")
	}
	_, err = transport.DialContext(context.Background(), "tcp", "provider.example:443")
	if err == nil || lookups != 1 || dials != 1 {
		t.Fatal("DNS/dial count", err, lookups, dials)
	}
	for _, target := range []string{"other.example:443", "provider.example:80", "127.0.0.1:443"} {
		if _, err := transport.DialContext(context.Background(), "tcp", target); err == nil {
			t.Fatal("target accepted", target)
		}
	}
	if lookups != 1 || dials != 1 {
		t.Fatal("denied route reached resolver/dialer")
	}
}

func TestPublicEgressRejectsMixedDNSBeforeAnyDial(t *testing.T) {
	for _, ips := range [][]netip.Addr{nil, {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, {netip.MustParseAddr("100.100.100.100")}, {netip.MustParseAddr("fd7a:115c:a1e0::1")}} {
		transport, err := publicEgressTransport(egressTestPolicy(Codex), func(context.Context, string, string) ([]netip.Addr, error) { return ips, nil }, func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("forbidden DNS reached dialer")
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.DialContext(context.Background(), "tcp", "provider.example:443"); err == nil {
			t.Fatal("mixed/private DNS accepted")
		}
		transport.CloseIdleConnections()
	}
}

func TestPublicEgressHTTPConnectionFaultCannotHideAutomaticRetry(t *testing.T) {
	var requests, connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	var authority string
	for _, name := range cert.DNSNames {
		if egressAuthority(name) {
			authority = name
			break
		}
	}
	if authority == "" {
		t.Fatal("fixture TLS certificate has no DNS authority")
	}
	p := egressTestPolicy(Codex)
	p.Routes = []EgressRoute{{authority, "GET", "/metadata", "metadata", "", false}}
	// Only test dependencies redirect the validated numeric dial to a local
	// TLS server. Production uses the real resolver/dialer and denies loopback.
	transport, err := publicEgressTransport(p, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Error(address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	for i := 0; i < 2; i++ {
		r, _ := http.NewRequest("GET", "https://"+authority+"/metadata", nil)
		response, err := transport.RoundTrip(r)
		if i == 0 {
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		} else if err == nil {
			response.Body.Close()
			t.Fatal("synthetic connection fault hidden")
		}
	}
	if requests.Load() != 2 || connections.Load() != 2 {
		t.Fatalf("unexpected retry/reuse: requests=%d connections=%d", requests.Load(), connections.Load())
	}
}

func TestProviderEgressPolicyRejectsOppositeProviderAndUnsafeRoutes(t *testing.T) {
	mutations := []func(*EgressPolicy){
		func(p *EgressPolicy) { p.Provider = Claude }, func(p *EgressPolicy) { p.Routes[0].Authority = "127.0.0.1" },
		func(p *EgressPolicy) { p.Routes[0].Authority = "provider.example:443" }, func(p *EgressPolicy) { p.Routes[0].Path = "/a/../infer" },
		func(p *EgressPolicy) { p.Routes[0].Kind = "tunnel" }, func(p *EgressPolicy) { p.Headers = append(p.Headers, "Cookie") },
		func(p *EgressPolicy) { p.Limits.Requests = 0 }, func(p *EgressPolicy) { p.Limits.InferenceRequests = 1000 },
	}
	for i, mutate := range mutations {
		p := egressTestPolicy(Codex)
		mutate(&p)
		if _, err := NewProviderEgress(context.Background(), testSession(Codex), p, egressRoundTrip(func(*http.Request) (*http.Response, error) { t.Error("invalid policy executed"); return nil, nil })); !errors.Is(err, ErrLiveBlocked) {
			t.Fatal(i, err)
		}
	}
	if _, err := NewPublicEgressTransport(EgressPolicy{Routes: []EgressRoute{{Authority: strings.Repeat("a", 254)}}}); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("bad transport authority", err)
	}
}
