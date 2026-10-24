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

// A drained, held launch revokes provider intake while the pinned client
// waits for a withheld inference response. The guest's existing client proxy
// sees its private exchange fail and stops its CLI, which is what lets guest
// PID1 return auth. The host receipt records the aborted request as failed.
func TestNativeVMShutdownDrainRevokesWithheldInferenceAndStopsGuestClient(t *testing.T) {
	p, _ := nativeHostPhaseFixture(t)
	guest, host := vmSocketPair(t)
	reached, released := make(chan struct{}), make(chan error, 1)
	var err error
	p.bridge, err = startNativeProviderBridge(context.Background(), p.request.Session, p.request.Policy, egressRoundTrip(func(q *http.Request) (*http.Response, error) {
		close(reached)
		<-q.Context().Done()
		released <- q.Context().Err()
		return nil, q.Context().Err()
	}), host)
	if err != nil {
		t.Fatal(err)
	}
	drain := newShutdownDrain()
	if !p.holdShutdownGrace(withShutdownDrain(context.Background(), drain)) {
		t.Fatal("an undrained launch was refused its export")
	}
	transport := nativeProviderTransport(guest)
	defer transport.CloseIdleConnections()
	var class atomic.Value
	proxy := httptest.NewServer(nativeGuestProviderHandlerObserved(p.request, transport, func(c string) { class.Store(c) }))
	// Cleanups run last first: a failed run closes the private stream before
	// the proxy waits for its handler.
	t.Cleanup(proxy.Close)
	t.Cleanup(func() { guest.Close(); host.Close() })
	raw, _ := json.Marshal(map[string]any{"model": p.request.Session.Identity.Model})
	q, _ := http.NewRequest("POST", proxy.URL+"/infer", strings.NewReader(string(raw)))
	q.Header.Set("Authorization", "Bearer synthetic-native-access")
	q.Header.Set("Content-Type", "application/json")
	answered := make(chan int, 1)
	go func() {
		r, err := proxy.Client().Do(q)
		if err != nil {
			answered <- 0
			return
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		answered <- r.StatusCode
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("inference never reached the withheld upstream")
	}
	if !drain.begin() {
		t.Fatal("a held export was not granted the grace")
	}
	select {
	case status := <-answered:
		if status != http.StatusBadGateway || class.Load() != "provider-transport" {
			t.Fatal("guest proxy did not stop its client on revoked intake", status, class.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked intake left the guest's request waiting")
	}
	select {
	case err := <-released:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("withheld upstream request survived revocation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := p.bridge.close(ctx)
	if err != nil || !e.Access.Stopped || !e.Access.Failed || e.Access.Forwarded != 1 || e.Access.Responses != 0 || nativeProviderRefused(e) {
		t.Fatal("revoked intake did not leave a stopped failed receipt", e, err)
	}
	if !p.shutdownDrained() {
		t.Fatal("held launch lost its drain")
	}
	p.releaseGrace()
	if drain.held != 0 {
		t.Fatal("hold not released")
	}
	// A launch reaching export after the drain began exports nothing.
	late, _ := nativeHostPhaseFixture(t)
	if late.holdShutdownGrace(withShutdownDrain(context.Background(), drain)) || late.releaseGrace != nil {
		t.Fatal("a launch exported auth after shutdown began")
	}
	// Outside a controller invocation nothing is held or drained.
	free, _ := nativeHostPhaseFixture(t)
	if !free.holdShutdownGrace(context.Background()) || free.shutdownDrained() {
		t.Fatal("an evaluation launch was tied to a controller drain")
	}
}

func nativeDrainedPhase(t *testing.T, drain bool) (*nativeVMHostPhase, *AuthStore, *shutdownDrain, AuthTransferInfo) {
	t.Helper()
	p, _ := nativeHostPhaseFixture(t)
	store := authTestStore(t, Codex)
	profile, err := store.LeaseProfile(context.Background(), p.request.Binding)
	if err != nil {
		t.Fatal(err)
	}
	p.profile = profile
	attachIdleNativeProviderBridge(t, p)
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
	if d != nil && !d.begin() {
		t.Fatal("held export denied the grace")
	}
	info, err := seed.Info()
	if err != nil {
		t.Fatal(err)
	}
	return p, store, d, info
}

func nativeReturnedFinished(t *testing.T, p *nativeVMHostPhase, info AuthTransferInfo, payload string) {
	t.Helper()
	returned, err := NewAuthReturn(info, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "provider_unavailable", AuthReturn: authWire(t, returned), SourceAudit: nativeHostAuditFixture(t, p.request)}
}

type nativeDrainReceipt struct {
	Complete, AuthExported, AuthReturned bool
	Failure                              string
}

func readNativeDrainReceipt(t *testing.T, p *nativeVMHostPhase) nativeDrainReceipt {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID, "native-audit.json"))
	if err != nil {
		t.Fatal("stopped receipt missing", err)
	}
	var r nativeDrainReceipt
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The guest returned its finished record within the grace: the host proves
// stop, commits the stopped auth return, writes the stopped receipt and
// reports the shutdown, which the controller requeues.
func TestNativeVMDrainedSessionCommitsStoppedReturnAsShutdown(t *testing.T) {
	p, store, drain, info := nativeDrainedPhase(t, true)
	nativeReturnedFinished(t, p, info, "synthetic-returned-after-drain")
	_, _, evidence, err := p.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"})
	if !errors.Is(err, context.Canceled) || phaseState(err) != "cancelled" || errors.Is(err, ErrWorkerStopPending) || errors.Is(err, ErrAuthRecoveryRequired) || errors.Is(err, ErrSourceAuditPending) || errors.Is(err, ErrProviderAuditPending) {
		t.Fatal("drained stopped session was not reported as a proved shutdown", err)
	}
	var pe PhaseError
	if !errors.As(err, &pe) || pe.BeforeWork {
		t.Fatal("exported session was refunded as before-work", err)
	}
	if !p.authReturned || evidence == nil || !evidence.Access.Stopped {
		t.Fatal("drained session lost its stopped return or receipt", p.authReturned, evidence)
	}
	state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if err != nil || string(state) != "synthetic-returned-after-drain" {
		t.Fatal("stopped return not committed", err)
	}
	if r := readNativeDrainReceipt(t, p); r.Complete || !r.AuthExported || !r.AuthReturned || r.Failure != "cancelled" {
		t.Fatal("stopped receipt does not record the returned drained session", r)
	}
	if drain.held != 0 {
		t.Fatal("hold outlived the stopped receipt")
	}
	// The same result without a drain keeps its own state.
	q, _, _, qInfo := nativeDrainedPhase(t, false)
	nativeReturnedFinished(t, q, qInfo, "synthetic-returned-without-drain")
	if _, _, _, err = q.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"}); errors.Is(err, context.Canceled) || phaseState(err) != "provider_unavailable" {
		t.Fatal("undrained failure was reclassified", err)
	}
}

// A latched route refusal during the grace stays a refusal, not a shutdown.
func TestNativeVMDrainedRefusalKeepsItsClassification(t *testing.T) {
	p, _, _, info := nativeDrainedPhase(t, true)
	nativeReturnedFinished(t, p, info, "synthetic-returned-after-refusal")
	p.bridge.guard.denyRoute()
	if _, _, _, err := p.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"}); errors.Is(err, context.Canceled) || phaseState(err) != "provider_unavailable" {
		t.Fatal("a provider refusal was hidden as shutdown", err)
	}
}

// The guest never returned within the grace: Close cancelled, the unit was
// still proved stopped, and the exported auth stays fenced with the lease
// retained for source audit, exactly as without a grace.
func TestNativeVMDrainedSessionWithoutReturnStaysFenced(t *testing.T) {
	p, store, drain, _ := nativeDrainedPhase(t, true)
	_, _, _, err := p.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"})
	if !errors.Is(err, ErrAuthRecoveryRequired) || !errors.Is(err, ErrSourceAuditPending) {
		t.Fatal("a missing return after the grace was not kept for recovery", err)
	}
	if r := readNativeDrainReceipt(t, p); r.AuthReturned || !r.AuthExported {
		t.Fatal("receipt claims a return that never happened", r)
	}
	if _, err = store.LeaseProfile(context.Background(), p.request.Binding); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("next phase reused a possibly refreshed seed", err)
	}
	if drain.held != 0 {
		t.Fatal("hold outlived the stopped receipt")
	}
}
