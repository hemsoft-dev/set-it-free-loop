package reviewer

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// NewPublicEgressTransport supplies the concrete HTTPS-only transport for a
// separately approved policy. Construction opens no network connection. It
// ignores ambient proxies, pins each connection to a checked DNS result and
// disables HTTP2, connection reuse, redirects and replay bodies. One RoundTrip
// cannot hide automatic retries on a reused connection. No credentials or
// subscription endpoint policy are supplied here.
func NewPublicEgressTransport(p EgressPolicy) (*http.Transport, error) {
	return publicEgressTransport(p, net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: 10 * time.Second}).DialContext)
}

type egressLookup func(context.Context, string, string) ([]netip.Addr, error)
type egressDial func(context.Context, string, string) (net.Conn, error)

func publicEgressTransport(p EgressPolicy, lookup egressLookup, dial egressDial) (*http.Transport, error) {
	allowed := map[string]bool{}
	if len(p.Routes) == 0 || len(p.Routes) > 16 || lookup == nil || dial == nil {
		return nil, ErrLiveBlocked
	}
	for _, route := range p.Routes {
		if !egressAuthority(route.Authority) {
			return nil, ErrLiveBlocked
		}
		allowed[route.Authority] = true
	}
	return &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || network != "tcp" || port != "443" || !allowed[host] {
				return nil, errors.New("provider destination denied")
			}
			ips, err := lookup(ctx, "ip", host)
			if err != nil || len(ips) == 0 || len(ips) > 16 {
				return nil, errors.New("provider DNS unavailable")
			}
			for _, ip := range ips {
				if !publicEgressIP(ip) {
					return nil, errors.New("provider DNS contains forbidden address")
				}
			}
			// Resolve once, then dial that numeric address. TLS verifies the
			// original hostname. No second DNS lookup or private-IP fallback.
			return dial(ctx, "tcp", net.JoinHostPort(ips[0].Unmap().String(), "443"))
		},
	}, nil
}

var deniedEgressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicEgressIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Accept native global IPv6 only, excluding NAT64 and transition ranges
	// whose embedded IPv4 address could reach an otherwise forbidden service.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range deniedEgressPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
