//go:build linux

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

func nativeProviderFixture(t *testing.T, output string, configure ...func(*EgressPolicy)) (*nativeProviderBridge, *http.Client, *atomic.Int32, Session) {
	t.Helper()
	client, server := vmSocketPair(t)
	s := testSession(Codex)
	policy := egressTestPolicy(Codex)
	policy.ResponsePolicy = CodexSourceResponses
	for _, edit := range configure {
		edit(&policy)
	}
	var calls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls.Add(1)
		if q.Header.Get("Authorization") != "Bearer synthetic-native-access" {
			t.Error("credential changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, output)
	}))
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	transport := egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		if q.URL.Scheme != "https" || q.URL.Host != "provider.example" || q.GetBody != nil {
			t.Error("private bridge bypassed HTTPS/pinned authority")
		}
		clone := q.Clone(q.Context())
		u := *q.URL
		u.Host = target.Host
		clone.URL = &u
		return backend.Client().Transport.RoundTrip(clone)
	})
	bridge, err := startNativeProviderBridge(context.Background(), s, policy, transport, server)
	if err != nil {
		t.Fatal(err)
	}
	rt := nativeProviderTransport(client)
	httpClient := &http.Client{Transport: rt, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		rt.CloseIdleConnections()
		client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		bridge.close(ctx)
	})
	return bridge, httpClient, &calls, s
}

func TestNativeVMProviderNegotiationSharesTheMetadataAndInferenceStream(t *testing.T) {
	bridge, client, calls, s := nativeProviderFixture(t, codexToolStream("exec"), func(p *EgressPolicy) {
		p.Routes = append(p.Routes, EgressRoute{Authority: "provider.example", Method: "GET", Path: "/infer", Kind: "local_negotiation"})
	})
	request := func(path string, upgrade bool) int {
		q, _ := http.NewRequest("GET", "http://sfl-private.invalid"+path, nil)
		q.Host = "provider.example"
		q.Header.Set("Authorization", "Bearer synthetic-native-access")
		if upgrade {
			q.Header.Set("Connection", "Upgrade")
			q.Header.Set("Upgrade", "websocket")
			q.Header.Set("Sec-WebSocket-Version", "13")
			q.Header.Set("Sec-WebSocket-Key", "MDEyMzQ1Njc4OWFiY2RlZg==")
		}
		response, err := client.Do(q)
		if err != nil {
			t.Fatal("private stream cannot mix WebSocket negotiation with ordinary HTTP", err)
		}
		if _, err = io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if request("/metadata", false) != 200 {
		t.Fatal("metadata failed")
	}
	if request("/infer", true) != 426 {
		t.Fatal("local-only WebSocket negotiation failed")
	}
	if status, _ := nativeProviderCall(t, client, s.Identity.Model); status != 200 {
		t.Fatal("inference failed after negotiation")
	}
	client.Transport.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := bridge.close(ctx)
	if err != nil || e.Access.ValidateCompleted(s, e.Policy) != nil || calls.Load() != 2 || e.Access.NegotiationRequests != 1 || e.Access.MetadataRequests != 1 || e.Access.InferenceRequests != 1 {
		t.Fatal("mixed protocol receipt differs", e, err, calls.Load())
	}
}

func nativeProviderCall(t *testing.T, client *http.Client, model string) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"model": model, "input": "synthetic immutable evidence"})
	q, err := http.NewRequest("POST", "http://sfl-private.invalid/infer", io.NopCloser(strings.NewReader(string(raw))))
	if err != nil {
		t.Fatal(err)
	}
	q.Host = "provider.example"
	q.Header.Set("Authorization", "Bearer synthetic-native-access")
	q.Header.Set("Content-Type", "application/json")
	r, err := client.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, string(body)
}

func TestNativeVMProviderActualPrivateHTTPAndStoppedReceipt(t *testing.T) {
	output := codexToolStream("exec")
	bridge, client, calls, s := nativeProviderFixture(t, output)
	for range 2 {
		status, body := nativeProviderCall(t, client, s.Identity.Model)
		if status != 200 || body != output {
			t.Fatal("private provider stream changed response", status, body)
		}
	}
	client.Transport.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := bridge.close(ctx)
	if err != nil || e.Access.ValidateCompleted(s, e.Policy) != nil || calls.Load() != 2 || e.Access.InferenceRequests != 2 {
		t.Fatal("private stopped receipt differs", e, err, calls.Load())
	}
}

func TestNativeVMProviderRejectsWrongModelBeforeUpstream(t *testing.T) {
	bridge, client, calls, _ := nativeProviderFixture(t, codexToolStream("exec"))
	status, body := nativeProviderCall(t, client, "wrong-model")
	if status != 400 || strings.Contains(body, "synthetic-native-access") || calls.Load() != 0 {
		t.Fatal("wrong model reached provider", status, calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := bridge.close(ctx)
	if err != nil || !e.Access.Failed || !e.Access.Stopped || e.Access.Forwarded != 0 {
		t.Fatal("wrong model produced clean receipt", e, err)
	}
}

func TestNativeVMProviderRejectsForbiddenToolsBeforeGuest(t *testing.T) {
	bridge, client, calls, s := nativeProviderFixture(t, codexToolStream("apply_patch"))
	status, body := nativeProviderCall(t, client, s.Identity.Model)
	if status != 400 || strings.Contains(body, "apply_patch") || calls.Load() != 1 {
		t.Fatal("forbidden tool reached guest", status, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := bridge.close(ctx)
	if err != nil || !e.Access.Stopped || !e.Access.Failed || e.Access.Responses != 0 || e.Access.Forwarded != 1 {
		t.Fatal("denied tool receipt lost actual work", e, err)
	}
}

func TestNativeVMProviderNeverDialsOutsideItsSingleChannel(t *testing.T) {
	client, peer := vmSocketPair(t)
	rt := nativeProviderTransport(client)
	defer rt.CloseIdleConnections()
	peer.Close()
	for _, endpoint := range []string{"http://other.example/infer", "https://sfl-private.invalid/infer", "http://sfl-private.invalid/infer"} {
		q, _ := http.NewRequest("GET", endpoint, nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		q = q.WithContext(ctx)
		r, err := rt.RoundTrip(q)
		if r != nil {
			r.Body.Close()
		}
		cancel()
		if err == nil {
			t.Fatal("unqualified network destination connected", endpoint)
		}
	}
	if _, err := startNativeProviderBridge(context.Background(), testSession(Codex), egressTestPolicy(Codex), egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("baseline policy reached upstream")
		return nil, nil
	}), client); err != ErrLiveBlocked {
		t.Fatal("native phase accepted unguarded response policy", err)
	}
}

func TestNativeVMProviderCancelledWaitDoesNotCloseTheActiveResponse(t *testing.T) {
	_, client, calls, s := nativeProviderFixture(t, codexToolStream("exec"))
	makeRequest := func(ctx context.Context) *http.Request {
		raw, _ := json.Marshal(map[string]any{"model": s.Identity.Model, "input": "immutable"})
		q, _ := http.NewRequestWithContext(ctx, "POST", "http://sfl-private.invalid/infer", io.NopCloser(strings.NewReader(string(raw))))
		q.Host = "provider.example"
		q.Header.Set("Authorization", "Bearer synthetic-native-access")
		q.Header.Set("Content-Type", "application/json")
		return q
	}
	first, err := client.Do(makeRequest(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r, err := client.Do(makeRequest(ctx))
		if r != nil {
			r.Body.Close()
		}
		done <- err
	}()
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("queued cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not cancel")
	}
	if calls.Load() != 1 {
		t.Fatal("another request reached the occupied private stream")
	}
	if _, err = io.Copy(io.Discard, first.Body); err != nil {
		t.Fatal("queued cancellation damaged the active response", err)
	}
	first.Body.Close()
	if status, _ := nativeProviderCall(t, client, s.Identity.Model); status != 200 || calls.Load() != 2 {
		t.Fatal("private stream did not survive queued cancellation")
	}
}

func TestNativeVMProviderPartialResponseCloseRetiresTheStream(t *testing.T) {
	_, client, calls, s := nativeProviderFixture(t, codexToolStream("exec"))
	raw, _ := json.Marshal(map[string]any{"model": s.Identity.Model, "input": "immutable"})
	q, _ := http.NewRequest("POST", "http://sfl-private.invalid/infer", io.NopCloser(strings.NewReader(string(raw))))
	q.Host = "provider.example"
	q.Header.Set("Authorization", "Bearer synthetic-native-access")
	q.Header.Set("Content-Type", "application/json")
	response, err := client.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	var first [1]byte
	if _, err = io.ReadFull(response.Body, first[:]); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	q, _ = http.NewRequest("GET", "http://sfl-private.invalid/metadata", nil)
	q.Host = "provider.example"
	if r, err := client.Do(q); err == nil {
		r.Body.Close()
		t.Fatal("partly consumed response allowed another request")
	}
	if calls.Load() != 1 {
		t.Fatal("retired stream replayed or reconnected")
	}
}
