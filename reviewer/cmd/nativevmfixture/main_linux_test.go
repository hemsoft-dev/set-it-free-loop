//go:build linux

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func TestNativeProtocolCaseCannotMixSingleProviderOrDenialModes(t *testing.T) {
	for _, c := range []config{{Provider: r.Codex}, {Provider: r.Claude, Negative: true}, {ProtocolRounds: 1}, {ProtocolRounds: 2}} {
		if !c.validCase() {
			t.Fatal("supported fake case rejected", c)
		}
	}
	for _, c := range []config{{}, {Provider: "both"}, {Provider: r.Codex, ProtocolRounds: 1}, {Negative: true, ProtocolRounds: 1}, {ProtocolRounds: 3}, {ProtocolRounds: -1}} {
		if c.validCase() {
			t.Fatal("hybrid or unbounded fake case accepted", c)
		}
	}
}

func TestNativeProtocolRejectsWrongPhaseBeforeOpeningWorker(t *testing.T) {
	configuration := r.DefaultConfig()
	snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	job := r.Job{ID: "native-protocol-test", Snapshot: snap, Config: configuration, Packet: "immutable source evidence"}
	session, err := r.NewSession(job.ID, snap, configuration.Hash(), r.Codex, 1, job.Packet, nil, nil, configuration.Limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"provider", "order", "snapshot", "configuration", "packet", "job", "concurrent"} {
		t.Run(kind, func(t *testing.T) {
			p := &nativeProtocolFixture{config: config{Root: "/must-remain-unopened"}, job: job}
			s := session
			provider := r.Codex
			switch kind {
			case "provider":
				provider = r.Claude
			case "order":
				p.phases = []protocolPhase{{}}
			case "snapshot":
				s.Snapshot.PR++
				s.SnapshotHash = r.Digest(s.Snapshot)
			case "configuration":
				s.ConfigHash = r.Digest("another configuration")
			case "packet":
				s.Packet = "another packet"
				s.PacketHash = r.Digest(s.Packet)
			case "job":
				s.JobID = "another-job"
			case "concurrent":
				p.mu.Lock()
				defer p.mu.Unlock()
			}
			before := len(p.phases)
			_, access, evidence, err := (protocolReviewer{p, provider}).ReviewAudited(context.Background(), s)
			if err == nil || access != nil || evidence != nil || len(p.phases) != before {
				t.Fatal("wrong phase entered native execution", kind, err)
			}
		})
	}
}

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
