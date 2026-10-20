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
	return t.base.RoundTrip(copy)
}

func fixtureEgress(s r.Session, backend http.Handler) (*r.ProviderEgress, r.EgressPolicy, http.Handler, func(), error) {
	const authority = "fixture.provider.example"
	limits := r.DefaultEgressLimits(s)
	limits.Requests, limits.InferenceRequests, limits.RefreshRequests, limits.MetadataRequests = 3, 3, 0, 0
	policy := r.EgressPolicy{Provider: r.Claude, Limits: limits,
		Routes:  []r.EgressRoute{{Authority: authority, Method: "POST", Path: "/v1/messages", RawQuery: "beta=true", Kind: "inference"}},
		Headers: []string{"Accept", "Accept-Encoding", "Anthropic-Beta", "Anthropic-Dangerous-Direct-Browser-Access", "Anthropic-Version", "Authorization", "Content-Type", "User-Agent", "X-App", "X-Claude-Code-Session-Id", "X-Stainless-Arch", "X-Stainless-Lang", "X-Stainless-Os", "X-Stainless-Package-Version", "X-Stainless-Retry-Count", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Timeout"},
	}
	upstream := httptest.NewTLSServer(backend)
	target, _ := url.Parse(upstream.URL)
	guard, err := r.NewProviderEgress(context.Background(), s, policy, fixtureTransport{target, upstream.Client().Transport})
	if err != nil {
		upstream.Close()
		return nil, policy, nil, nil, err
	}
	intake := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// Fixed fake-only authority mapping. No user-supplied destination or
		// production listener, TLS identity or peer-checking claim is involved.
		copy := q.Clone(q.Context())
		copy.Host = authority
		guard.ServeHTTP(w, copy)
	})
	return guard, policy, intake, upstream.Close, nil
}
