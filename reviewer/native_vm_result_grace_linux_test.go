//go:build linux

package reviewer

import (
	"context"
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

var workClockEpoch = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// The phase deadline is split, not extended: the guest's own deadline and the
// host's work deadline both end the grace before the session deadline, which
// stays the ceiling every other bound was sized for.
func TestNativeVMDeadlineSplitReservesTheResultGraceInsideThePhase(t *testing.T) {
	for _, tc := range []struct{ timeout, grace time.Duration }{
		{75 * time.Second, 15 * time.Second}, {5 * time.Minute, 15 * time.Second},
		{40 * time.Second, 10 * time.Second}, {2 * time.Second, 500 * time.Millisecond},
	} {
		grace := NativeVMResultGrace(tc.timeout)
		if grace != tc.grace || nativeVMGuestTimeout(tc.timeout)+grace != tc.timeout || nativeVMGuestTimeout(tc.timeout) <= 0 {
			t.Fatal("phase deadline split differs", tc.timeout, grace, nativeVMGuestTimeout(tc.timeout))
		}
	}
	// The guest counts its deadline from request intake, after the host's
	// preparation and boot. For any boot shorter than the grace it ends its
	// client after the host's work deadline and before the session deadline,
	// leaving the rest of the grace for its cleanup and finished record.
	timeout := 75 * time.Second
	work, session := workClockEpoch.Add(timeout-NativeVMResultGrace(timeout)), workClockEpoch.Add(timeout)
	for _, boot := range []time.Duration{0, 3 * time.Second, 14 * time.Second} {
		guest := workClockEpoch.Add(boot + nativeVMGuestTimeout(timeout))
		if guest.Before(work) || !guest.Before(session) {
			t.Fatal("guest deadline outside the result grace", boot)
		}
	}
	q := nativeRequestFixture(t, Codex)
	q.Session.Limits.Timeout = timeout
	before := time.Now()
	ctx, cancel := nativeGuestPhaseContext(context.Background(), q)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(60*time.Second)) || deadline.After(time.Now().Add(60*time.Second)) {
		t.Fatal("guest phase does not end the grace before the captured deadline", deadline.Sub(before))
	}
	// Every ceiling sized for the phase deadline is unchanged. The longest
	// allowed phase plus the host's fifteen-second stop path fits the native
	// unit's seven-minute runtime cap and the durable lease's extra minute; the
	// guest's phase, worker stop and finish cleanup fit its seven-minute bound.
	longest := DefaultLimits().Timeout
	if longest+15*time.Second >= 7*time.Minute || 15*time.Second >= time.Minute || nativeVMGuestTimeout(longest)+5*time.Second+12*time.Second >= 7*time.Minute {
		t.Fatal("phase deadline no longer fits its ceilings")
	}
}

// The host's work deadline is the grace before the session context's actual
// deadline, so a caller deadline earlier than the phase deadline bounds it.
func TestNativeVMWorkDeadlineIsBoundedByTheCallerDeadline(t *testing.T) {
	timeout := 75 * time.Second
	for name, tc := range map[string]struct {
		caller time.Duration
		want   time.Duration
	}{
		"phase-deadline":  {0, 60 * time.Second},
		"caller-deadline": {30 * time.Second, 15 * time.Second},
		"caller-expired":  {10 * time.Second, -5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := nativeHostPhaseFixture(t)
			clock := &fakeWorkClock{now: workClockEpoch}
			clock.install(p)
			parent := context.Background()
			if tc.caller != 0 {
				var cancel context.CancelFunc
				parent, cancel = context.WithDeadline(parent, workClockEpoch.Add(tc.caller))
				defer cancel()
			}
			ctx, cancel := context.WithDeadline(parent, workClockEpoch.Add(timeout))
			defer cancel()
			p.armWorkDeadline(ctx, timeout)
			if clock.fire == nil || clock.delay != tc.want {
				t.Fatal("work deadline differs", clock.delay, tc.want)
			}
		})
	}
}

// At the work deadline the host revokes provider intake while the pinned
// client waits for a withheld inference response, as a shutdown drain does.
// The guest's client proxy sees its exchange fail and stops its CLI, so guest
// PID1 can return auth and the source audit within the grace.
func TestNativeVMWorkDeadlineRevokesWithheldInference(t *testing.T) {
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
	clock := &fakeWorkClock{now: workClockEpoch}
	clock.install(p)
	ctx, cancel := context.WithDeadline(context.Background(), workClockEpoch.Add(75*time.Second))
	defer cancel()
	p.armWorkDeadline(ctx, 75*time.Second)
	if clock.fire == nil {
		t.Fatal("no work deadline armed")
	}
	transport := nativeProviderTransport(guest)
	defer transport.CloseIdleConnections()
	var class atomic.Value
	proxy := httptest.NewServer(nativeGuestProviderHandlerObserved(p.request, transport, func(c string) { class.Store(c) }))
	t.Cleanup(proxy.Close)
	t.Cleanup(func() { guest.Close(); host.Close() })
	q, _ := http.NewRequest("POST", proxy.URL+"/infer", strings.NewReader(string(codexMeasuredBody(t, p.request.Session.Identity.Model))))
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
	if p.workExpired() {
		t.Fatal("expired before the work deadline")
	}
	clock.fire()
	select {
	case status := <-answered:
		if status != http.StatusBadGateway || class.Load() != "provider-transport" {
			t.Fatal("guest proxy did not stop its client at the work deadline", status, class.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("work deadline left the guest's request waiting")
	}
	select {
	case err := <-released:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("withheld upstream request survived the work deadline")
	}
	if !p.workExpired() {
		t.Fatal("work deadline not recorded")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	e, err := p.bridge.close(closeCtx)
	if err != nil || !e.Access.Stopped || !e.Access.Failed || e.Access.Forwarded != 1 || e.Access.Responses != 0 || nativeProviderRefused(e) {
		t.Fatal("work deadline did not leave a stopped failed receipt", e, err)
	}
}

// expiredExportedPhase is an exported launch whose work deadline has fired.
func expiredExportedPhase(t *testing.T) (*nativeVMHostPhase, *AuthStore, *fakeWorkClock, AuthTransferInfo) {
	t.Helper()
	p, store, _, info := nativeDrainedPhase(t, false)
	clock := &fakeWorkClock{now: workClockEpoch}
	clock.install(p)
	ctx, cancel := context.WithDeadline(context.Background(), workClockEpoch.Add(75*time.Second))
	t.Cleanup(cancel)
	p.armWorkDeadline(ctx, 75*time.Second)
	if clock.fire == nil {
		t.Fatal("no work deadline armed")
	}
	clock.fire()
	return p, store, clock, info
}

// The guest returned its finished record within the grace: the host proves
// stop, commits the stopped auth return, keeps the source audit and reports a
// typed expired phase, never a clean one and never a fenced one.
func TestNativeVMExpiredSessionCommitsAuthReturnedWithinTheGrace(t *testing.T) {
	p, store, clock, info := expiredExportedPhase(t)
	nativeReturnedFinished(t, p, info, "synthetic-returned-within-grace")
	report, access, evidence, err := p.finish(context.Background(), Report{}, PhaseError{State: "invalid_output"})
	var pe PhaseError
	if !errors.As(err, &pe) || pe.State != "incomplete_expired" || pe.BeforeWork || phaseState(err) != "incomplete_expired" {
		t.Fatal("expired session not typed as an expired phase", err)
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrAuthRecoveryRequired, ErrSourceAuditPending, ErrWorkerStopPending, ErrProviderAuditPending} {
		if errors.Is(err, cause) {
			t.Fatal("expired session that returned within the grace carries", cause, err)
		}
	}
	if len(report.Findings) != 0 || access == nil || access.validate(p.request.Session) != nil || evidence == nil || !evidence.Access.Stopped {
		t.Fatal("expired session lost its audit or receipt", report, access, evidence)
	}
	if !p.authReturned {
		t.Fatal("stopped return not recorded")
	}
	state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if err != nil || string(state) != "synthetic-returned-within-grace" {
		t.Fatal("stopped return not committed", err)
	}
	if _, err = store.LeaseProfile(context.Background(), AuthBinding{SessionHash: Digest("next"), LaunchID: strings.Repeat("b", 32), ContainmentID: Digest("next-containment")}); err != nil {
		t.Fatal("returned auth not usable by the next launch", err)
	}
	if r := readNativeDrainReceipt(t, p); r.Complete || !r.AuthExported || !r.AuthReturned || r.Failure != "deadline" {
		t.Fatal("stopped receipt does not record the expired returned session", r)
	}
	if clock.stopped == 0 {
		t.Fatal("finish left the work-deadline timer armed")
	}
	// The same record without an expiry keeps the guest's own state.
	q, _, _, qInfo := nativeDrainedPhase(t, false)
	nativeReturnedFinished(t, q, qInfo, "synthetic-returned-unexpired")
	if _, _, _, err = q.finish(context.Background(), Report{}, PhaseError{State: "invalid_output"}); phaseState(err) != "invalid_output" {
		t.Fatal("unexpired failure was reclassified", err)
	}
}

// A latched provider refusal during the grace stays a refusal.
func TestNativeVMExpiredRefusalKeepsItsClassification(t *testing.T) {
	p, _, _, info := expiredExportedPhase(t)
	nativeReturnedFinished(t, p, info, "synthetic-returned-after-refusal")
	p.bridge.guard.denyRoute()
	if _, _, _, err := p.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"}); phaseState(err) != "provider_unavailable" {
		t.Fatal("a provider refusal was hidden as an expiry", err)
	}
}

// The guest hung through the grace: the session deadline interrupted the
// result read, the unit was still proved stopped before any auth step, and the
// exported auth stays fenced with the audit pending, exactly as without the
// work deadline.
func TestNativeVMExpiredSessionWithoutRecordIsFencedAsBefore(t *testing.T) {
	past, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	type outcome struct {
		state, kind, class string
		recovery, audit    bool
	}
	run := func(p *nativeVMHostPhase, store *AuthStore) outcome {
		_, access, _, err := p.finish(past, Report{}, workerLauncherError(ErrVMBoot, false))
		var pe PhaseError
		errors.As(err, &pe)
		r := readNativeDrainReceipt(t, p)
		if access != nil || r.AuthReturned || !r.AuthExported || p.authReturned {
			t.Fatal("a missing return claimed an audit or a return", access, r)
		}
		if _, err := store.LeaseProfile(context.Background(), p.request.Binding); !errors.Is(err, ErrAuthRecoveryRequired) {
			t.Fatal("next phase reused a possibly refreshed seed", err)
		}
		return outcome{pe.State, r.Failure, phaseState(err), errors.Is(err, ErrAuthRecoveryRequired), errors.Is(err, ErrSourceAuditPending)}
	}
	expired, expiredStore, _, _ := expiredExportedPhase(t)
	got := run(expired, expiredStore)
	plain, plainStore, _, _ := nativeDrainedPhase(t, false)
	want := run(plain, plainStore)
	if got != want || !got.recovery || !got.audit || got.class != "incomplete_expired" {
		t.Fatal("hung guest after the grace not fenced exactly as before", got, want)
	}
}
