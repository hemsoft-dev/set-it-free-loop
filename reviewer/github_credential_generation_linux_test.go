//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A real inert process/pidfd tests stop retention independently of the HTTP
// fixture. This is not a production cgroup/broker stop qualification.
func credentialAcquisitionFixture(t *testing.T, owner *GitHubCredentialOwner) (*githubReaderGeneration, func(), *atomic.Int32) {
	t.Helper()
	child := exec.Command("/usr/bin/sleep", "10")
	child.Env = []string{}
	if child.Start() != nil {
		t.Fatal("inert acquisition process")
	}
	var once sync.Once
	stopChild := func() { once.Do(func() { child.Process.Kill(); child.Wait() }) }
	fd, err := unix.PidfdOpen(child.Process.Pid, 0)
	if err != nil {
		stopChild()
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	stopped := func(ctx context.Context) error {
		calls.Add(1)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, 0)
		if err != nil || n != 1 || poll[0].Revents&unix.POLLIN == 0 || poll[0].Revents & ^int16(unix.POLLIN|unix.POLLHUP) != 0 {
			return errors.New("inert acquisition still live")
		}
		return nil
	}
	g, err := owner.retainReaderGeneration(context.Background(), stopped)
	if err != nil {
		stopChild()
		unix.Close(fd)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopChild()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := g.stop(ctx); err != nil {
			t.Error("acquisition cleanup", err)
		}
		unix.Close(fd)
	})
	return g, stopChild, calls
}

func credentialBorrowRequest(t *testing.T, g *githubReaderGeneration) {
	t.Helper()
	reader, ctx, finish, err := g.borrow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	q := installationQuery(t, "GET", githubRepoPath+"/pulls/183").WithContext(ctx)
	r, err := reader.RoundTrip(q)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(r.Body)
	closeErr := r.Body.Close()
	if readErr != nil || closeErr != nil || r.StatusCode != http.StatusOK {
		t.Fatal("pinned reader actual TLS", readErr, closeErr, r.StatusCode)
	}
	finish() // Exactly-once retirement, independent of the acquisition lease.
}

func TestGitHubCredentialGenerationOutlivesHTTPAndFailedStop(t *testing.T) {
	owner, options, reader, publisher, signs := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	g, stopChild, stops := credentialAcquisitionFixture(t, owner)
	credentialBorrowRequest(t, g)
	owner.state.mu.Lock()
	active, owned := len(owner.state.active), len(owner.state.acquisitions)
	owner.state.mu.Unlock()
	if active != 0 || owned != 1 || credentialPairCleared(pair) {
		t.Fatal("HTTP completion released whole generation", active, owned)
	}
	if other, err := owner.retainReaderGeneration(context.Background(), func(context.Context) error { return nil }); err == nil || other != nil {
		t.Fatal("concurrent acquisition admitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := owner.Refresh(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || g.ctx.Err() == nil || credentialPairCleared(pair) || owner.Ready(context.Background()) == nil {
		t.Fatal("drain timeout lost or reused generation", err)
	}
	_, rMints := credentialCounts(reader)
	_, pMints := credentialCounts(publisher)
	if signs.Load() != 1 || rMints != 1 || pMints != 1 {
		t.Fatal("lease drain signed or minted before physical stop")
	}
	if tr, _, finish, err := g.borrow(context.Background()); err == nil || tr != nil || finish != nil {
		t.Fatal("canceled acquisition borrowed credential")
	}
	if g.stop(context.Background()) == nil || stops.Load() != 1 || credentialPairCleared(pair) {
		t.Fatal("live process stop discarded generation")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
	err = owner.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || credentialPairCleared(pair) {
		t.Fatal("close discarded still-owned generation", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
	other, err := OpenGitHubCredentialOwner(ctx, options)
	cancel()
	if err == nil || other != nil {
		t.Fatal("failed stop released service lock")
	}
	stopChild()
	if g.stop(context.Background()) != nil || owner.Close(context.Background()) != nil || !credentialPairCleared(pair) {
		t.Fatal("confirmed stopped acquisition could not release ownership")
	}
	other, err = OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if other.Ready(context.Background()) == nil || signs.Load() != 1 {
		t.Fatal("reopen inherited lease/readiness or renewed automatically")
	}
	for _, value := range []any{g, *g} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(raw)} {
			if !strings.Contains(text, "redacted") || strings.Contains(text, "synthetic-") {
				t.Fatal("generation leaked private state")
			}
		}
	}
}

func TestGitHubCredentialGenerationExpiryClockAndEligibilityRevokeWork(t *testing.T) {
	for _, kind := range []string{"backward-clock", "expiry", "expiry-after-check", "eligibility"} {
		t.Run(kind, func(t *testing.T) {
			owner, options, reader, publisher, signs := credentialOwnerFixture(t)
			var now atomic.Int64
			now.Store(controlTime.UnixNano())
			options.Journal.now = func() time.Time { return time.Unix(0, now.Load()) }
			var allowed atomic.Bool
			allowed.Store(true)
			owner.state.options.Check = func(context.Context, Enrollment) error {
				if !allowed.Load() {
					return errors.New("private eligibility withdrawn")
				}
				return nil
			}
			if owner.Refresh(context.Background()) != nil {
				t.Fatal("qualify pair")
			}
			pair := owner.state.current
			g, stopChild, _ := credentialAcquisitionFixture(t, owner)
			credentialBorrowRequest(t, g)
			before, _ := credentialCounts(reader)
			switch kind {
			case "backward-clock":
				now.Store(controlTime.Add(-time.Second).UnixNano())
			case "expiry":
				now.Store(controlTime.Add(59 * time.Minute).UnixNano())
			case "expiry-after-check":
				owner.state.options.Check = func(context.Context, Enrollment) error {
					now.Store(controlTime.Add(59 * time.Minute).UnixNano())
					return nil
				}
			case "eligibility":
				allowed.Store(false)
			}
			if transport, _, _, err := g.borrow(context.Background()); err == nil || transport != nil || g.ctx.Err() == nil {
				t.Fatal("invalid qualification retained active work")
			}
			after, rMints := credentialCounts(reader)
			_, pMints := credentialCounts(publisher)
			if after != before || signs.Load() != 1 || rMints != 1 || pMints != 1 || credentialPairCleared(pair) {
				t.Fatal("refusal renewed or disposed a still-owned generation")
			}
			stopChild()
			if g.stop(context.Background()) != nil {
				t.Fatal("revoked generation did not accept actual stopped proof")
			}
		})
	}
}

func TestGitHubCredentialGenerationInvalidAndUnqualifiedAdmissionHasNoWork(t *testing.T) {
	owner, _, reader, publisher, signs := credentialOwnerFixture(t)
	var stops atomic.Int32
	stop := func(context.Context) error { stops.Add(1); return nil }
	if g, err := owner.retainReaderGeneration(context.Background(), stop); err == nil || g != nil {
		t.Fatal("unqualified generation admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trace := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { t.Fatal("trace invoked") }})
	for _, ctx := range []context.Context{nil, ctx, trace} {
		if g, err := owner.retainReaderGeneration(ctx, stop); err == nil || g != nil {
			t.Fatal("invalid acquisition context admitted")
		}
	}
	if g, err := owner.retainReaderGeneration(context.Background(), nil); err == nil || g != nil {
		t.Fatal("missing physical-stop capability admitted")
	}
	for _, o := range []*GitHubCredentialOwner{nil, {}} {
		if g, err := o.retainReaderGeneration(context.Background(), stop); err == nil || g != nil {
			t.Fatal("empty owner admitted")
		}
	}
	for _, g := range []*githubReaderGeneration{nil, {}} {
		if _, _, _, err := g.borrow(context.Background()); err == nil {
			t.Fatal("empty generation borrowed")
		}
		if g.stop(context.Background()) == nil {
			t.Fatal("empty generation stopped")
		}
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if rCalls != 0 || pCalls != 0 || signs.Load() != 0 || stops.Load() != 0 {
		t.Fatal("failed admission dispatched work or a stop")
	}
}

func TestGitHubCredentialGenerationRefreshWaitsAndPinsReader(t *testing.T) {
	owner, _, reader, publisher, signs := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	g, stopChild, _ := credentialAcquisitionFixture(t, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- owner.Refresh(ctx) }()
	for {
		owner.state.mu.Lock()
		refreshing, changed := owner.state.refreshing, owner.state.changed
		owner.state.mu.Unlock()
		if refreshing {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("refresh did not enter drain")
		case <-changed:
		}
	}
	credentialBorrowRequest(t, g)
	_, rMints := credentialCounts(reader)
	_, pMints := credentialCounts(publisher)
	if signs.Load() != 1 || rMints != 1 || pMints != 1 || owner.state.current != pair {
		t.Fatal("active generation changed during drain")
	}
	if g.stop(context.Background()) == nil {
		t.Fatal("still-live process released drain")
	}
	select {
	case <-result:
		t.Fatal("refresh advanced before stopped receipt")
	default:
	}
	stopChild()
	if g.stop(context.Background()) != nil {
		t.Fatal("stop receipt failed")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("stopped generation did not release refresh")
	}
	if signs.Load() != 2 || owner.state.current == pair || !credentialPairCleared(pair) || owner.Ready(context.Background()) != nil {
		t.Fatal("handover lost atomic pair qualification")
	}
	if transport, _, _, err := g.borrow(context.Background()); err == nil || transport != nil {
		t.Fatal("released lease followed a new generation")
	}
}

func TestGitHubCredentialGenerationStopWaitsForBorrowedHTTP(t *testing.T) {
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	g, stopChild, stops := credentialAcquisitionFixture(t, owner)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHTTP := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHTTP()
	inner := pair.reader.state.inner
	pair.reader.state.inner = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		close(entered)
		<-q.Context().Done()
		close(canceled)
		<-release
		return nil, errors.New("synthetic private in-flight failure")
	})
	defer func() { pair.reader.state.inner = inner }()
	reader, ctx, finish, err := g.borrow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	result := make(chan error, 1)
	go func() {
		_, err := reader.RoundTrip(installationQuery(t, "GET", githubRepoPath).WithContext(ctx))
		finish()
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("borrowed request not entered")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err = g.stop(stopCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || stops.Load() != 0 || credentialPairCleared(pair) {
		t.Fatal("stop crossed an unreturned request", err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("stop did not cancel borrowed HTTP")
	}
	releaseHTTP()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled request completed")
		}
	case <-time.After(time.Second):
		t.Fatal("borrowed request failed to return")
	}
	stopChild()
	if g.stop(context.Background()) != nil || stops.Load() != 1 {
		t.Fatal("returned request did not permit stop")
	}
}

func TestGitHubCredentialGenerationConcurrentStopRetainsUnreturnedProof(t *testing.T) {
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	g, stopChild, _ := credentialAcquisitionFixture(t, owner)
	original := g.stopper
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	var enteredOnce sync.Once
	g.stopper = func(ctx context.Context) error {
		calls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		return original(ctx)
	}
	result := make(chan error, 1)
	go func() { result <- g.stop(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stop proof not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := g.stop(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("concurrent stop replayed proof", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
	err = owner.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || credentialPairCleared(owner.state.current) {
		t.Fatal("unreturned stop proof lost ownership", err)
	}
	stopChild()
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop proof did not return")
	}
	if g.stop(context.Background()) != nil || calls.Load() != 1 || owner.Close(context.Background()) != nil {
		t.Fatal("completed stop was replayed or could not close")
	}
}

func TestGitHubCredentialGenerationStopDeadlineRetainsSingleFlight(t *testing.T) {
	owner, options, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	g, stopChild, _ := credentialAcquisitionFixture(t, owner)
	original := g.stopper
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	g.stopper = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release  // Deliberately ignore cancellation to exercise retained ownership.
			return nil // A late success must not release the generation.
		}
		return original(ctx)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- g.stop(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stop capability not entered")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("stop did not return caller deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unreturned stop capability exceeded caller deadline")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := g.stop(ctx2)
	cancel2()
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("timed-out stop proof replayed while still running", err)
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err = owner.Close(ctx3)
	cancel3()
	if !errors.Is(err, context.DeadlineExceeded) || credentialPairCleared(pair) {
		t.Fatal("unreturned stop released tokens", err)
	}
	if replacement, err := OpenGitHubCredentialOwner(context.Background(), options); err == nil {
		replacement.Close(context.Background())
		t.Fatal("unreturned stop released service lock")
	}
	stopChild()
	unblock()
	// The first callback has now returned, but its deadline expired before proof.
	// Only the explicit retry may obtain a fresh, valid stopped result.
	if err := g.stop(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatal("late proof released ownership or prevented explicit retry", err, calls.Load())
	}
	if owner.Close(context.Background()) != nil || !credentialPairCleared(pair) {
		t.Fatal("confirmed stopped retry failed to dispose owner")
	}
}

func TestGitHubCredentialGenerationDeadlineCannotRejectReleasedProof(t *testing.T) {
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	// Race a real caller cancellation against each completed synthetic stop.
	// A successful caller may release; a failed caller must still own its lease.
	// The physical-stop capability itself is deliberately a bounded fake here.
	for iteration := 0; iteration < 3000; iteration++ {
		proof, canceled := make(chan struct{}), make(chan struct{})
		var once sync.Once
		g, err := owner.retainReaderGeneration(context.Background(), func(context.Context) error {
			once.Do(func() { close(proof) })
			return nil
		})
		if err != nil {
			t.Fatal("retain generation", iteration, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-proof; cancel(); close(canceled) }()
		err = g.stop(ctx)
		<-canceled
		owner.state.mu.Lock()
		released, held := g.released, owner.state.acquisitions[g.id] == g
		owner.state.mu.Unlock()
		if err != nil && (released || !held) {
			t.Fatal("failed stop released the generation", iteration, err)
		}
		if !released && g.stop(context.Background()) != nil {
			t.Fatal("retained proof could not be explicitly retried", iteration)
		}
	}
}
