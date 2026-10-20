//go:build linux

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestFakeRunnerCannotAdoptExistingProfileRoot(t *testing.T) {
	if err := run(config{Root: t.TempDir()}); err == nil {
		t.Fatal("existing directory adopted")
	}
}

func TestFakeTransportUsesOnlyFixedLocalTLSPeer(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { calls++; w.WriteHeader(204) }))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	base := server.Client().Transport.(*http.Transport).Clone()
	base.Proxy = nil
	defer base.CloseIdleConnections()
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != target.Host {
			t.Error("unexpected network dial", address)
		}
		return dialer.DialContext(ctx, network, address)
	}
	transport := localOnly{target, base}
	for _, endpoint := range []string{"https://real-provider.example/v1/responses", "http://fixture.provider.example/v1/responses"} {
		q, _ := http.NewRequest("GET", endpoint, nil)
		if _, err := transport.RoundTrip(q); err == nil {
			t.Fatal("outside fake authority accepted")
		}
	}
	q, _ := http.NewRequest("GET", "https://fixture.provider.example/v1/responses", nil)
	response, err := transport.RoundTrip(q)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 || calls != 1 {
		t.Fatal("fixed fake peer not used")
	}
}
