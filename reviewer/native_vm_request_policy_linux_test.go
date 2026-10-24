//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// widenedCodexBody is a measured first-turn request after injected model
// metadata re-advertised direct tools, as the codex-metadata case recorded.
func widenedCodexBody(t *testing.T, model string) []byte {
	t.Helper()
	request := codexMeasuredRequests(t)[0]
	request["model"] = model
	request["tools"] = []any{map[string]any{"type": "custom", "name": "apply_patch"}, map[string]any{"type": "function", "name": "read_mcp_resource"}, map[string]any{"type": "function", "name": "list_mcp_resources"}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNativeVMProviderRequiresTheCodexRequestPolicy(t *testing.T) {
	client, _ := vmSocketPair(t)
	transport := egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("policy check reached upstream")
		return nil, nil
	})
	responseOnly := egressTestPolicy(Codex)
	responseOnly.ResponsePolicy = CodexSourceResponses
	if _, err := startNativeProviderBridge(context.Background(), testSession(Codex), responseOnly, transport, client); err != ErrLiveBlocked {
		t.Fatal("native Codex launch accepted no request policy", err)
	}
	claude := egressTestPolicy(Claude)
	claude.ResponsePolicy = ClaudeSourceMessages
	setRequestPolicy(&claude, codexRequestPolicy)
	if _, err := startNativeProviderBridge(context.Background(), testSession(Claude), claude, transport, client); err != ErrLiveBlocked {
		t.Fatal("native Claude launch accepted the Codex request policy", err)
	}
	q := nativeRequestFixture(t, Codex)
	setRequestPolicy(&q.Policy, "")
	if q.valid() {
		t.Fatal("private native request accepted no Codex request policy")
	}
	if !nativeLaunchPolicies(Codex, nativeRequestFixture(t, Codex).Policy) || !nativeLaunchPolicies(Claude, nativeRequestFixture(t, Claude).Policy) {
		t.Fatal("captured native policies refused")
	}
}

// The host bridge refuses the widened request before upstream and marks the
// private response fatal, exactly like a refused response.
func TestNativeVMProviderRefusesWidenedCodexRequestBeforeUpstream(t *testing.T) {
	bridge, client, calls, s := nativeProviderFixture(t, codexToolStream("exec"))
	q, _ := http.NewRequest("POST", "http://sfl-private.invalid/infer", strings.NewReader(string(widenedCodexBody(t, s.Identity.Model))))
	q.Host = "provider.example"
	q.Header.Set("Authorization", "Bearer synthetic-native-access")
	q.Header.Set("Content-Type", "application/json")
	r, err := client.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 400 || r.Header.Get(nativeProviderFatalHeader) != "1" || strings.TrimSpace(string(body)) != "provider tool request denied" || calls.Load() != 0 {
		t.Fatal("widened request reached upstream or was not fatal", r.StatusCode, r.Header, string(body), calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := bridge.close(ctx)
	if err != nil || !e.Access.Stopped || !e.Access.Failed || e.Access.InferenceRequests != 1 || e.Access.Forwarded != 0 || e.Access.Responses != 0 || requestDenials(e.Access) != 1 || !nativeProviderRefused(e) {
		t.Fatal("request refusal receipt differs", e, err)
	}
	if e.Access.ValidateCompleted(s, e.Policy) == nil {
		t.Fatal("refused request produced a clean receipt")
	}
}

// The guest proxy stops its owned client on the fatal refusal, which is what
// lets guest PID1 return auth, with the same class as a refused response.
func TestNativeVMGuestProviderRequestDenialStopsClient(t *testing.T) {
	bridge, client, calls, s := nativeProviderFixture(t, codexToolStream("exec"))
	var class atomic.Value
	var stops atomic.Int32
	proxy := httptest.NewServer(nativeGuestProviderHandlerObserved(nativeVMRequest{Policy: bridge.policy}, client.Transport, func(c string) { stops.Add(1); class.Store(c) }))
	defer proxy.Close()
	q, _ := http.NewRequest("POST", proxy.URL+"/infer", strings.NewReader(string(widenedCodexBody(t, s.Identity.Model))))
	q.Header.Set("Authorization", "Bearer synthetic-native-access")
	q.Header.Set("Content-Type", "application/json")
	r, err := proxy.Client().Do(q)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 400 || calls.Load() != 0 || stops.Load() != 1 || class.Load() != "provider-response" {
		t.Fatal("request refusal did not stop the owned client", r.StatusCode, calls.Load(), stops.Load(), class.Load())
	}
}

// refusedRequestPhase drives one widened request from the guest proxy through
// a real host bridge whose upstream fails the test if called, then returns
// the guest's finished record with auth.
func refusedRequestPhase(t *testing.T, drain bool) (*nativeVMHostPhase, *AuthStore, *shutdownDrain) {
	t.Helper()
	p, _ := nativeHostPhaseFixture(t)
	store := authTestStore(t, Codex)
	profile, err := store.LeaseProfile(context.Background(), p.request.Binding)
	if err != nil {
		t.Fatal(err)
	}
	p.profile = profile
	guest, host := vmSocketPair(t)
	p.bridge, err = startNativeProviderBridge(context.Background(), p.request.Session, p.request.Policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("refused request reached upstream")
		return nil, ErrLiveBlocked
	}), host)
	if err != nil {
		t.Fatal(err)
	}
	var d *shutdownDrain
	if drain {
		d = newShutdownDrain()
		if !p.holdShutdownGrace(withShutdownDrain(context.Background(), d)) {
			t.Fatal("hold refused")
		}
	}
	seed, err := profile.ExportAuth(context.Background(), authExportCheck(func(context.Context, AuthBinding) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seed.Close() })
	p.exported = true
	transport := nativeProviderTransport(guest)
	var class atomic.Value
	proxy := httptest.NewServer(nativeGuestProviderHandlerObserved(p.request, transport, func(c string) { class.Store(c) }))
	t.Cleanup(proxy.Close)
	t.Cleanup(func() { transport.CloseIdleConnections(); guest.Close(); host.Close() })
	q, _ := http.NewRequest("POST", proxy.URL+"/infer", strings.NewReader(string(widenedCodexBody(t, p.request.Session.Identity.Model))))
	q.Header.Set("Authorization", "Bearer synthetic-native-access")
	q.Header.Set("Content-Type", "application/json")
	r, err := proxy.Client().Do(q)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 400 || class.Load() != "provider-response" {
		t.Fatal("guest proxy did not stop its client on the request refusal", r.StatusCode, class.Load())
	}
	if d != nil && !d.begin() {
		t.Fatal("held export denied the grace")
	}
	info, err := seed.Info()
	if err != nil {
		t.Fatal(err)
	}
	nativeReturnedFinished(t, p, info, "synthetic-returned-after-request-refusal")
	return p, store, d
}

// A refused request ends the session as a typed client failure that is never
// clean. Auth is returned and committed through the stopped unit, and the
// stopped receipt records the refusal, as for a refused response.
func TestNativeVMRequestRefusalIsANeverCleanFailureWithReturnedAuth(t *testing.T) {
	for _, guestState := range []string{"invalid_output", "complete-claim"} {
		t.Run(guestState, func(t *testing.T) {
			p, store, _ := refusedRequestPhase(t, false)
			var primary error = PhaseError{State: guestState}
			claimed := Report{}
			if guestState == "complete-claim" {
				// Even a guest that claims a complete, valid report cannot
				// turn the refusal into a clean session.
				primary, claimed = nil, protocolReport(p.request.Session)
				raw, _ := json.Marshal(claimed)
				p.finished.State, p.finished.Report = "complete", raw
				p.finished.CodeHost = &nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(p.request.Session), UID: nativeVMCodeHostUID, InputHash: Digest("input"), OutputHash: Digest("output"), Stopped: true, RelayComplete: true}
				if !p.finished.valid(p.request) {
					t.Fatal("complete claim fixture invalid")
				}
			}
			report, _, evidence, err := p.finish(context.Background(), claimed, primary)
			if err == nil || Digest(report) != Digest(Report{}) {
				t.Fatal("refused request produced a clean session", err)
			}
			if guestState == "invalid_output" && phaseState(err) != "invalid_output" {
				t.Fatal("refusal lost its client failure class", err)
			}
			// With a valid complete record and report, only the provider
			// receipt can refuse the claim: it is stopped and recoverable but
			// never completed, and the host reports a provider failure.
			if primary == nil && (phaseState(err) != "provider_unavailable" || evidence.validateStoppedCaptured(Codex, p.request.Session.Limits, Digest(p.request.Session)) != nil || evidence.Access.ValidateCompleted(p.request.Session, evidence.Policy) == nil) {
				t.Fatal("clean guest claim was not refused by the provider receipt", err)
			}
			var pe PhaseError
			if errors.As(err, &pe) && pe.BeforeWork {
				t.Fatal("exported refused session was refunded as before-work", err)
			}
			if errors.Is(err, ErrAuthRecoveryRequired) || errors.Is(err, ErrWorkerStopPending) || errors.Is(err, ErrProviderAuditPending) || errors.Is(err, context.Canceled) {
				t.Fatal("refusal left auth, stop or receipt pending", err)
			}
			if !p.authReturned || evidence == nil || !evidence.Access.Stopped || requestDenials(evidence.Access) != 1 || evidence.Access.Forwarded != 0 {
				t.Fatal("refused session lost its stopped return or receipt", p.authReturned, evidence)
			}
			state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if err != nil || string(state) != "synthetic-returned-after-request-refusal" {
				t.Fatal("stopped auth return not committed", err)
			}
			if r := readNativeDrainReceipt(t, p); r.Complete || !r.AuthExported || !r.AuthReturned {
				t.Fatal("stopped receipt does not record the returned refused session", r)
			}
		})
	}
}

// A request refusal during a shutdown grace keeps its own classification.
func TestNativeVMDrainedRequestRefusalKeepsItsClassification(t *testing.T) {
	p, _, drain := refusedRequestPhase(t, true)
	_, _, evidence, err := p.finish(context.Background(), Report{}, PhaseError{State: "invalid_output"})
	if errors.Is(err, context.Canceled) || phaseState(err) != "invalid_output" || requestDenials(evidence.Access) != 1 {
		t.Fatal("a request refusal was hidden as shutdown", err)
	}
	if drain.held != 0 {
		t.Fatal("hold outlived the stopped receipt")
	}
}
