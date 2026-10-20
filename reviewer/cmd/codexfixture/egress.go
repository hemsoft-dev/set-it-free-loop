//go:build linux

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

type fixtureTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t fixtureTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	copy := q.Clone(q.Context())
	u := *q.URL
	u.Host = t.target.Host
	copy.URL = &u
	// This is an explicit fake-only loopback transport, never the public one.
	// The entire command already runs in a private loopback network namespace.
	return t.base.RoundTrip(copy)
}

func fixtureEgress(s r.Session, backend http.Handler) (*r.ProviderEgress, r.EgressPolicy, http.Handler, func(), error) {
	const authority = "fixture.provider.example"
	policy := r.EgressPolicy{Provider: r.Codex, Limits: r.DefaultEgressLimits(s),
		Routes: []r.EgressRoute{
			{Authority: authority, Method: "POST", Path: "/v1/responses", Kind: "inference"},
			{Authority: authority, Method: "GET", Path: "/v1/responses", Kind: "local_negotiation"},
			{Authority: authority, Method: "GET", Path: "/v1/models", RawQuery: "client_version=0.163.0", Kind: "metadata"},
			{Authority: authority, Method: "GET", Path: "/backend-api/wham/accounts/check", Kind: "metadata"},
			{Authority: authority, Method: "GET", Path: "/backend-api/wham/settings/user", Kind: "metadata"},
		},
		Headers: []string{"Accept", "Accept-Encoding", "Authorization", "Chatgpt-Account-Id", "Content-Type", "Originator", "Openai-Beta", "User-Agent", "Version", "Session-Id", "Thread-Id", "X-Codex-Beta-Features", "X-Codex-Turn-Metadata", "X-Codex-Routing-Hint", "X-Codex-Window-Id", "X-Client-Request-Id", "Cache-Control", "X-Openai-Internal-Codex-Responses-Lite"},
	}
	upstream := httptest.NewTLSServer(backend)
	target, _ := url.Parse(upstream.URL)
	guard, err := r.NewProviderEgress(context.Background(), s, policy, fixtureTransport{target, upstream.Client().Transport})
	if err != nil {
		upstream.Close()
		return nil, policy, nil, nil, err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// The exact client uses a single fixture-local base URL. This fixed
		// authority mapping stands in for the production listener/TLS wiring;
		// it is not a candidate-controlled Host or destination override.
		copy := q.Clone(q.Context())
		copy.Host = authority
		guard.ServeHTTP(w, copy)
	})
	return guard, policy, handler, upstream.Close, nil
}
