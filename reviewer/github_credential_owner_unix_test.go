//go:build linux || darwin

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubCredentialOwnerTLSAtomicPairStableHandlesAndDisposal(t *testing.T) {
	journal, _, _ := installationJournalFixture(t)
	readerFixture, readerOptions := installationHTTPFixture(t, GitHubReader)
	publisherFixture, publisherOptions := installationHTTPFixture(t, GitHubPublisher)
	var signs atomic.Int32
	options := GitHubCredentialOwnerOptions{
		Journal:         journal,
		JWT:             func(context.Context) ([]byte, error) { signs.Add(1); return []byte("synthetic-app-jwt"), nil },
		ReaderTransport: readerOptions.Transport, PublisherTransport: publisherOptions.Transport,
		Check: func(context.Context, Enrollment) error { return nil },
	}
	owner, err := OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	reader, publisher := owner.Reader(), owner.Publisher()
	get := func(transport http.RoundTripper) error {
		q, err := http.NewRequest("GET", githubOrigin+githubRepoPath, nil)
		if err != nil {
			return err
		}
		q.Header.Set("Accept", "application/vnd.github+json")
		q.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		response, err := transport.RoundTrip(q)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if response.StatusCode != 200 {
			return fmt.Errorf("unexpected fixture status %d", response.StatusCode)
		}
		return nil
	}
	if owner.Ready(context.Background()) == nil || get(reader) == nil || signs.Load() != 0 || readerFixture.calls != 0 || publisherFixture.calls != 0 {
		t.Fatal("construction or request acquired credentials")
	}
	short, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	other, err := OpenGitHubCredentialOwner(short, options)
	cancel()
	if err == nil || other != nil || signs.Load() != 0 {
		t.Fatal("second owner admitted")
	}
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := owner.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if signs.Load() != 1 || readerFixture.mints != 1 || publisherFixture.mints != 1 {
		t.Fatal("pair qualification missing or signing replayed")
	}
	old := owner.state.current
	if get(reader) != nil || get(publisher) != nil {
		t.Fatal("qualified stable roles failed")
	}
	calls := readerFixture.calls
	request, err := http.NewRequest("POST", githubOrigin+githubIssue(alphaScope())+"/comments", io.NopCloser(strings.NewReader(`{"body":"synthetic acknowledgment"}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if response, err := reader.RoundTrip(request); response != nil || err == nil || readerFixture.calls != calls {
		t.Fatal("reader role gained publisher authority")
	}
	request, err = http.NewRequest("POST", githubOrigin+githubIssue(alphaScope())+"/comments", io.NopCloser(strings.NewReader(`{"body":"synthetic acknowledgment"}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	response, err := publisher.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 {
		t.Fatal("publisher role lost scoped write", err)
	}
	api, err := NewGitHubPublicationHTTP(alphaEnrollment(), liveFake(native(1, controlTime)), publisher)
	if err != nil {
		t.Fatal(err)
	}
	intent := newIntent("ack", alphaScope(), 1, 0, Snapshot{}, "", Document{Summary: "configuration version2", Inline: []InlineComment{}}, controlTime)
	receipt, err := api.Post(context.Background(), intent)
	if err != nil || receipt.ID != 91 {
		t.Fatal("actual publisher producer failed through owner", err)
	}
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if signs.Load() != 2 || readerFixture.mints != 2 || publisherFixture.mints != 2 || old == owner.state.current {
		t.Fatal("pair not replaced exactly once")
	}
	for _, transport := range []*GitHubInstallationTransport{old.reader, old.publisher} {
		transport.state.mu.Lock()
		cleared := len(transport.state.token) == 0
		transport.state.mu.Unlock()
		if !cleared {
			t.Fatal("old token ownership retained after idle handover")
		}
	}
	if get(reader) != nil || get(publisher) != nil {
		t.Fatal("stable handles did not follow new pair")
	}
	copy := *owner
	if err := copy.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls = readerFixture.calls
	if owner.Ready(context.Background()) == nil || get(reader) == nil || readerFixture.calls != calls {
		t.Fatal("copied handle failed to share stopped ownership")
	}
	reopened, err := OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Ready(context.Background()) == nil || signs.Load() != 2 {
		t.Fatal("restart inherited readiness or acquired credentials")
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{owner, *owner, reader, *reader} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(raw)} {
			if !strings.Contains(text, "redacted") || strings.Contains(text, "synthetic-app-jwt") || strings.Contains(text, "synthetic-installation-") {
				t.Fatal("private generation exposed")
			}
		}
	}
}

func credentialOwnerFixture(t *testing.T) (*GitHubCredentialOwner, GitHubCredentialOwnerOptions, *installationFixture, *installationFixture, *atomic.Int32) {
	t.Helper()
	journal, _, _ := installationJournalFixture(t)
	reader, ro := installationHTTPFixture(t, GitHubReader)
	publisher, po := installationHTTPFixture(t, GitHubPublisher)
	signs := &atomic.Int32{}
	options := GitHubCredentialOwnerOptions{Journal: journal, JWT: func(context.Context) ([]byte, error) { signs.Add(1); return []byte("synthetic-app-jwt"), nil }, ReaderTransport: ro.Transport, PublisherTransport: po.Transport, Check: func(context.Context, Enrollment) error { return nil }}
	owner, err := OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return owner, options, reader, publisher, signs
}
func credentialCounts(f *installationFixture) (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.mints
}
func credentialOwnerRequest(t *testing.T, owner *GitHubCredentialOwner) *http.Response {
	t.Helper()
	q := installationQuery(t, "GET", githubRepoPath+"/pulls/183")
	r, err := owner.Reader().RoundTrip(q)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Body.Close() })
	return r
}
func credentialPairCleared(p *githubCredentialPair) bool {
	for _, tr := range []*GitHubInstallationTransport{p.reader, p.publisher} {
		tr.state.mu.Lock()
		empty := len(tr.state.token) == 0
		tr.state.mu.Unlock()
		if !empty {
			return false
		}
	}
	return true
}
func TestGitHubCredentialOwnerRefreshDrainsBodiesWithoutAutomaticRetry(t *testing.T) {
	owner, _, reader, publisher, signs := credentialOwnerFixture(t)
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	pair := owner.state.current
	response := credentialOwnerRequest(t, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := owner.Refresh(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || owner.Ready(context.Background()) == nil || credentialPairCleared(pair) {
		t.Fatal("drain timeout lost ownership or restored admission", err)
	}
	_, rMints := credentialCounts(reader)
	_, pMints := credentialCounts(publisher)
	if signs.Load() != 1 || rMints != 1 || pMints != 1 {
		t.Fatal("drain minted or signed before idle")
	}
	if r, err := owner.Reader().RoundTrip(installationQuery(t, "GET", githubRepoPath)); err == nil || r != nil {
		t.Fatal("failed refresh admitted old generation")
	}
	// EOF, independently of Close, retires the response lease.
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if signs.Load() != 2 || !credentialPairCleared(pair) {
		t.Fatal("explicit retry did not replace drained pair")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestGitHubCredentialOwnerCloseTimeoutRetainsLockAndTokens(t *testing.T) {
	owner, options, _, _, _ := credentialOwnerFixture(t)
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	pair := owner.state.current
	response := credentialOwnerRequest(t, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := owner.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || credentialPairCleared(pair) || owner.Ready(context.Background()) == nil {
		t.Fatal("timeout discarded ownership", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
	other, err := OpenGitHubCredentialOwner(ctx, options)
	cancel()
	if other != nil || err == nil {
		t.Fatal("timeout released sole owner lock")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !credentialPairCleared(pair) {
		t.Fatal("stopped owner retained token buffers")
	}
	other, err = OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if other.Ready(context.Background()) == nil {
		t.Fatal("reopened owner inherited readiness")
	}
}
func TestGitHubCredentialOwnerPartialPairUnknownLatchBlocksSigningAndHTTP(t *testing.T) {
	owner, options, reader, publisher, signs := credentialOwnerFixture(t)
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := owner.state.current
	publisher.mu.Lock()
	publisher.loseMint = true
	publisher.mu.Unlock()
	if err := owner.Refresh(context.Background()); err == nil {
		t.Fatal("unknown publisher mint qualified pair")
	}
	if owner.Ready(context.Background()) == nil || owner.state.current != old {
		t.Fatal("partial pair became ready or replaced old ownership")
	}
	rCalls, rMints := credentialCounts(reader)
	pCalls, pMints := credentialCounts(publisher)
	if signs.Load() != 2 || rMints != 2 || pMints != 2 {
		t.Fatal("partial handshake not exercised")
	}
	latest, err := options.Journal.Latest(context.Background(), GitHubPublisher)
	if err != nil || latest.State != "unknown" {
		t.Fatal("unknown publisher mint not durably latched", latest, err)
	}
	if err := owner.Refresh(context.Background()); err == nil {
		t.Fatal("refresh bypassed unknown latch")
	}
	rAfter, _ := credentialCounts(reader)
	pAfter, _ := credentialCounts(publisher)
	if signs.Load() != 2 || rCalls != rAfter || pCalls != pAfter {
		t.Fatal("preflight latch allowed signing or new reader mint")
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !credentialPairCleared(old) {
		t.Fatal("old pair retained after stop")
	}
	// A new process owner has no readiness and sees the same durable latch.
	other, err := OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err := other.Refresh(context.Background()); err == nil || signs.Load() != 2 {
		t.Fatal("reopen bypassed unknown latch")
	}
	assertJournalContainsNoCredentials(t, options.Journal)
}
func TestGitHubCredentialOwnerConcurrentRefreshAndCloseCancelQualification(t *testing.T) {
	owner, options, reader, publisher, signs := credentialOwnerFixture(t)
	entered := make(chan struct{})
	done := make(chan error, 1)
	owner.state.options.JWT = func(ctx context.Context) ([]byte, error) {
		signs.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() { done <- owner.Refresh(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not reach signer")
	}
	if err := owner.Refresh(context.Background()); err == nil {
		t.Fatal("concurrent refresh queued a second acquisition")
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("close did not cancel qualification", err)
		}
	case <-time.After(time.Second):
		t.Fatal("qualification did not drain")
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if signs.Load() != 1 || rCalls != 0 || pCalls != 0 {
		t.Fatal("canceled signing reached HTTP")
	}
	other, err := OpenGitHubCredentialOwner(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
}

func TestGitHubCredentialOwnerRejectsTypedNilTransports(t *testing.T) {
	journal, _, _ := installationJournalFixture(t)
	var absent *http.Transport
	options := GitHubCredentialOwnerOptions{Journal: journal, JWT: func(context.Context) ([]byte, error) { t.Fatal("constructor signed"); return nil, nil }, ReaderTransport: absent, PublisherTransport: absent, Check: func(context.Context, Enrollment) error { return nil }}
	owner, err := OpenGitHubCredentialOwner(context.Background(), options)
	if owner != nil {
		owner.Close(context.Background())
	}
	if owner != nil || err == nil {
		t.Fatal("typed nil transport accepted")
	}
}
func TestGitHubCredentialOwnerExpiryAndBackwardClockCloseAdmission(t *testing.T) {
	owner, options, reader, publisher, signs := credentialOwnerFixture(t)
	var now atomic.Int64
	now.Store(controlTime.UnixNano())
	options.Journal.now = func() time.Time { return time.Unix(0, now.Load()) }
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{controlTime.Add(-time.Second), controlTime.Add(59 * time.Minute), controlTime.Add(time.Hour)} {
		now.Store(at.UnixNano())
		if owner.Ready(context.Background()) == nil {
			t.Fatal("out-of-interval clock admitted pair", at)
		}
		if r, err := owner.Reader().RoundTrip(installationQuery(t, "GET", githubRepoPath)); err == nil || r != nil {
			t.Fatal("out-of-interval request admitted")
		}
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if signs.Load() != 1 || rCalls != 7 || pCalls != 7 {
		t.Fatal("readiness renewed credentials")
	}
}

func TestGitHubCredentialOwnerRefreshRevalidatesPairAtHandover(t *testing.T) {
	for _, kind := range []string{"reader-during-publisher", "publisher-at-final-check", "backward-clock", "canceled-by-final-clock"} {
		t.Run(kind, func(t *testing.T) {
			owner, options, reader, publisher, signs := credentialOwnerFixture(t)
			var now atomic.Int64
			now.Store(controlTime.UnixNano())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var cancelClock atomic.Bool
			options.Journal.now = func() time.Time {
				if cancelClock.Load() {
					cancel()
				}
				return time.Unix(0, now.Load())
			}
			if owner.Refresh(context.Background()) != nil {
				t.Fatal("initial pair")
			}
			old := owner.state.current
			if kind == "reader-during-publisher" {
				reader.mu.Lock()
				reader.mutate = func(path string, doc map[string]any) {
					if strings.HasSuffix(path, "/access_tokens") {
						doc["expires_at"] = controlTime.Add(130 * time.Second)
					}
				}
				reader.mu.Unlock()
				publisher.mu.Lock()
				publisher.mutate = func(path string, _ map[string]any) {
					if path == "/app" {
						now.Store(controlTime.Add(80 * time.Second).UnixNano())
					}
				}
				publisher.mu.Unlock()
			} else if kind == "publisher-at-final-check" {
				publisher.mu.Lock()
				publisher.mutate = func(path string, doc map[string]any) {
					if strings.HasSuffix(path, "/access_tokens") {
						doc["expires_at"] = controlTime.Add(130 * time.Second)
					}
				}
				publisher.mu.Unlock()
			}
			checks := 0
			owner.state.options.Check = func(context.Context, Enrollment) error {
				checks++
				if checks == 2 {
					switch kind {
					case "publisher-at-final-check":
						now.Store(controlTime.Add(80 * time.Second).UnixNano())
					case "backward-clock":
						now.Store(controlTime.Add(-time.Second).UnixNano())
					case "canceled-by-final-clock":
						cancelClock.Store(true)
					}
				}
				return nil
			}
			if err := owner.Refresh(ctx); err == nil {
				t.Fatal("unusable or canceled new pair reported successful refresh")
			}
			if checks != 2 || owner.state.current != old || owner.Ready(context.Background()) == nil || credentialPairCleared(old) {
				t.Fatal("failed handover replaced ownership, restored readiness or destroyed old tokens", checks)
			}
			rBefore, rMints := credentialCounts(reader)
			pBefore, pMints := credentialCounts(publisher)
			if signs.Load() != 2 || rMints != 2 || pMints != 2 {
				t.Fatal("pair handover was not exercised exactly once", signs.Load(), rMints, pMints)
			}
			for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
				latest, err := options.Journal.Latest(context.Background(), role)
				if err != nil || latest.State != "qualified" {
					t.Fatal("fixture failed before final handover", role, latest, err)
				}
			}
			body := &installationBodyProbe{}
			q := installationQuery(t, "GET", githubRepoPath)
			q.Body = body
			if response, err := owner.Reader().RoundTrip(q); err == nil || response != nil || body.closed != 1 {
				t.Fatal("failed handover admitted reader HTTP")
			}
			rAfter, _ := credentialCounts(reader)
			pAfter, _ := credentialCounts(publisher)
			if rBefore != rAfter || pBefore != pAfter || signs.Load() != 2 {
				t.Fatal("failed handover automatically retried or dispatched")
			}
			if owner.Close(context.Background()) != nil || !credentialPairCleared(old) {
				t.Fatal("retained old ownership could not close")
			}
		})
	}
}

func TestGitHubCredentialOwnerCancellationDuringFinalEligibility(t *testing.T) {
	for _, request := range []bool{false, true} {
		t.Run(fmt.Sprint("request-", request), func(t *testing.T) {
			owner, options, _, _, _ := credentialOwnerFixture(t)
			if owner.Refresh(context.Background()) != nil {
				t.Fatal("qualify pair")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Cancel from the clock during the final locked receipt check,
			// after the earlier context and local eligibility checks passed.
			checks, cancelAt := 0, 2
			if request {
				cancelAt = 3
			}
			clock := options.Journal.now
			options.Journal.now = func() time.Time {
				checks++
				if checks == cancelAt {
					cancel()
				}
				return clock()
			}
			defer func() { options.Journal.now = clock }()
			if !request {
				if owner.Ready(ctx) == nil || ctx.Err() == nil {
					t.Fatal("canceled final eligibility was accepted")
				}
				return
			}
			reader := owner.state.current.reader.state
			inner := reader.inner
			defer func() { reader.inner = inner }()
			calls := 0
			reader.inner = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
				calls++
				q.Body.Close()
				return nil, errors.New("unexpected canceled dispatch")
			})
			body := &installationBodyProbe{}
			q := installationQuery(t, "GET", githubRepoPath).WithContext(ctx)
			q.Body = body
			r, err := owner.Reader().RoundTrip(q)
			if err == nil || r != nil || ctx.Err() == nil || calls != 0 || body.closed != 1 {
				t.Fatal("canceled final HTTP admission reached transport or leaked body", err, calls, body.closed)
			}
		})
	}
}

func TestGitHubCredentialOwnerCloseErrorIsTerminalAfterLockDisposal(t *testing.T) {
	for _, descriptor := range []string{"lock", "root"} {
		t.Run(descriptor, func(t *testing.T) {
			owner, options, _, _, signs := credentialOwnerFixture(t)
			if owner.Refresh(context.Background()) != nil {
				t.Fatal("qualify pair")
			}
			pair := owner.state.current
			// A closed file descriptor makes authLock.close fail. os.Root.Close
			// is idempotent, so the closed-root case must still succeed. Either
			// outcome is terminal once lock disposal has been attempted.
			var err error
			if descriptor == "lock" {
				err = owner.state.held.file.Close()
			} else {
				err = owner.state.held.root.Close()
			}
			if err != nil {
				t.Fatal("close fixture descriptor", err)
			}
			err = owner.Close(context.Background())
			if (err == nil) != (descriptor == "root") {
				t.Fatal("lock error was hidden or idempotent root close failed", err)
			}
			if !owner.state.closed || owner.state.held != nil || owner.state.current != nil || !credentialPairCleared(pair) {
				t.Fatal("closed lock was represented as retained ownership")
			}
			if owner.Close(context.Background()) != nil || owner.Refresh(context.Background()) == nil || owner.Ready(context.Background()) == nil || signs.Load() != 1 {
				t.Fatal("terminal close retry reopened, signed or failed idempotence")
			}
			replacement, err := OpenGitHubCredentialOwner(context.Background(), options)
			if err != nil || replacement.Ready(context.Background()) == nil {
				t.Fatal("released lock could not admit a fresh unqualified owner", err)
			}
			if replacement.Close(context.Background()) != nil {
				t.Fatal("replacement cleanup")
			}
		})
	}
}

func TestGitHubCredentialOwnerRequestLimitAndDeniedBodyClosure(t *testing.T) {
	owner, _, reader, _, _ := credentialOwnerFixture(t)
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	responses := make([]*http.Response, 0, 64)
	for range 64 {
		responses = append(responses, credentialOwnerRequest(t, owner))
	}
	before, _ := credentialCounts(reader)
	body := &installationBodyProbe{}
	q := installationQuery(t, "GET", githubRepoPath)
	q.Body = body
	if r, err := owner.Reader().RoundTrip(q); r != nil || err == nil || body.closed != 1 {
		t.Fatal("capacity refusal leaked caller body", r, err, body.closed)
	}
	after, _ := credentialCounts(reader)
	if before != after {
		t.Fatal("capacity refusal reached upstream")
	}
	for _, r := range responses {
		if err := r.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	owner.state.mu.Lock()
	remaining := len(owner.state.active)
	owner.state.mu.Unlock()
	if remaining != 0 {
		t.Fatal("closed bodies retained leases")
	}
	r := credentialOwnerRequest(t, owner)
	r.Body.Close()
	for _, change := range []func(*http.Request){
		func(q *http.Request) { q.Header.Set("Authorization", "synthetic-caller-secret") },
		func(q *http.Request) { q.URL.Host = "outside.invalid" },
		func(q *http.Request) {
			*q = *q.WithContext(httptrace.WithClientTrace(q.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { t.Error("caller trace invoked") }}))
		},
	} {
		body := &installationBodyProbe{}
		q := installationQuery(t, "GET", githubRepoPath)
		q.Body = body
		change(q)
		before, _ := credentialCounts(reader)
		r, err := owner.Reader().RoundTrip(q)
		after, _ := credentialCounts(reader)
		if r != nil || err == nil || body.closed != 1 || before != after {
			t.Fatal("denied request leaked transport or body", err)
		}
	}
}
func TestGitHubCredentialOwnerCloseCancelsAdmittedHTTPAndWaitsForReturn(t *testing.T) {
	owner, _, reader, _, _ := credentialOwnerFixture(t)
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	inner := owner.state.current.reader.state.inner
	owner.state.current.reader.state.inner = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		close(entered)
		<-q.Context().Done()
		close(canceled)
		<-release
		return nil, errors.New("synthetic-installation-reader private upstream error")
	})
	result := make(chan error, 1)
	go func() { _, err := owner.Reader().RoundTrip(installationQuery(t, "GET", githubRepoPath)); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not admitted")
	}
	pair := owner.state.current
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := owner.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || credentialPairCleared(pair) {
		t.Fatal("close released still-running transport")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("close failed to cancel admitted HTTP")
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, ErrGitHubCredentialOwner) || strings.Contains(err.Error(), "synthetic-installation-") {
			t.Fatal("upstream error escaped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request did not finish")
	}
	// Restore through the retained pair because successful Close clears current.
	pair.reader.state.inner = inner
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !credentialPairCleared(pair) {
		t.Fatal("completed stop retained tokens")
	}
	calls, _ := credentialCounts(reader)
	if calls != 7 {
		t.Fatal("canceled synthetic runtime reached TLS")
	}
}
func TestGitHubCredentialOwnerFailedCallbacksNeverFallbackAndClearJWT(t *testing.T) {
	for _, stage := range []string{"precheck", "signer", "finalcheck"} {
		t.Run(stage, func(t *testing.T) {
			owner, _, reader, publisher, _ := credentialOwnerFixture(t)
			calls := 0
			jwt := []byte("synthetic-app-jwt")
			owner.state.options.Check = func(context.Context, Enrollment) error {
				calls++
				if stage == "precheck" || (stage == "finalcheck" && calls == 2) {
					return errors.New("synthetic-private-policy")
				}
				return nil
			}
			owner.state.options.JWT = func(context.Context) ([]byte, error) {
				if stage == "signer" {
					return jwt, errors.New("synthetic-private-signer")
				}
				return jwt, nil
			}
			err := owner.Refresh(context.Background())
			if err == nil || strings.Contains(err.Error(), "synthetic-") || owner.Ready(context.Background()) == nil || owner.state.current != nil {
				t.Fatal("failed callback qualified pair or leaked error", err)
			}
			if stage != "precheck" {
				for _, b := range jwt {
					if b != 0 {
						t.Fatal("transferred JWT buffer not cleared")
					}
				}
			}
			rCalls, _ := credentialCounts(reader)
			pCalls, _ := credentialCounts(publisher)
			if stage == "finalcheck" {
				if rCalls != 7 || pCalls != 7 {
					t.Fatal("final check did not follow real TLS pair qualification")
				}
			} else if rCalls != 0 || pCalls != 0 {
				t.Fatal("pre-work failure reached HTTP")
			}
		})
	}
}

func TestGitHubCredentialOwnerUnreturnedSignerRetainsStopOwnership(t *testing.T) {
	owner, options, reader, publisher, _ := credentialOwnerFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	jwt := []byte("synthetic-app-jwt")
	owner.state.options.JWT = func(context.Context) ([]byte, error) { close(entered); <-release; return jwt, nil }
	go func() { result <- owner.Refresh(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("signer not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err := owner.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unreturned callback not retained", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
	other, err := OpenGitHubCredentialOwner(ctx, options)
	cancel()
	if other != nil || err == nil {
		t.Fatal("unreturned callback released owner")
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("signer did not return")
	}
	for _, b := range jwt {
		if b != 0 {
			t.Fatal("late transferred JWT not cleared")
		}
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if rCalls != 0 || pCalls != 0 {
		t.Fatal("late signing dispatched acquisition")
	}
}
func TestGitHubCredentialOwnerRealProcessLock(t *testing.T) {
	const childKey = "SFL_CREDENTIAL_OWNER_TEST_CHILD"
	if path := os.Getenv(childKey); path != "" {
		store, err := OpenStore(path, alphaEnrollment(), DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		journal, err := NewGitHubInstallationJournal(context.Background(), store, func() time.Time { return controlTime })
		if err != nil {
			t.Fatal(err)
		}
		options := GitHubCredentialOwnerOptions{Journal: journal, JWT: func(context.Context) ([]byte, error) { t.Fatal("child signed"); return nil, nil }, ReaderTransport: &http.Transport{}, PublisherTransport: &http.Transport{}, Check: func(context.Context, Enrollment) error { t.Fatal("child checked eligibility"); return nil }}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		owner, err := OpenGitHubCredentialOwner(ctx, options)
		if os.Getenv("SFL_CREDENTIAL_OWNER_TEST_EXPECT") == "blocked" {
			if owner != nil || err == nil {
				t.Fatal("live process owner not excluded")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if owner.Ready(context.Background()) == nil {
			t.Fatal("child inherited readiness")
		}
		if err := owner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}
	owner, options, _, _, _ := credentialOwnerFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(expect string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestGitHubCredentialOwnerRealProcessLock$", "-test.count=1")
		command.Env = []string{childKey + "=" + options.Journal.store.path, "SFL_CREDENTIAL_OWNER_TEST_EXPECT=" + expect}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v %s", expect, err, output)
		}
	}
	child("blocked")
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	child("open")
}
func TestGitHubCredentialOwnerNilAndPreflightFailures(t *testing.T) {
	var nilOwner *GitHubCredentialOwner
	for _, owner := range []*GitHubCredentialOwner{nilOwner, {}} {
		if owner.Ready(context.Background()) == nil || owner.Refresh(context.Background()) == nil || owner.Close(context.Background()) == nil {
			t.Fatal("nil or zero owner accepted")
		}
		for _, transport := range []*GitHubCredentialRoleTransport{owner.Reader(), owner.Publisher(), nil, {}} {
			body := &installationBodyProbe{}
			q := installationQuery(t, "GET", githubRepoPath)
			q.Body = body
			if r, err := transport.RoundTrip(q); r != nil || err == nil || body.closed != 1 {
				t.Fatal("nil role body not closed")
			}
		}
	}
	owner, _, reader, publisher, signs := credentialOwnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, ctx, httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{})} {
		if err := owner.Refresh(ctx); err == nil {
			t.Fatal("invalid qualification context admitted")
		}
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if rCalls != 0 || pCalls != 0 || signs.Load() != 0 {
		t.Fatal("failed preflight performed acquisition")
	}
}

func TestGitHubCredentialOwnerActualRSASignerAndBothTLSRoles(t *testing.T) {
	key := newAppJWTTestSigner(t)
	signer, err := NewGitHubAppJWTSigner(alphaEnrollment(), key, func() time.Time { return controlTime })
	if err != nil {
		t.Fatal(err)
	}
	// Fixed claims and RSA PKCS1v15 are deterministic. The fixture expects the
	// exact independently verified bytes, then the owner calls the real producer.
	expected, err := signer.Sign(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(expected)
	verifyAppJWT(t, expected, &key.key.PublicKey, controlTime)
	reader, ro := installationHTTPFixture(t, GitHubReader, string(expected))
	publisher, po := installationHTTPFixture(t, GitHubPublisher, string(expected))
	journal, _, _ := installationJournalFixture(t)
	var transferred []byte
	owner, err := OpenGitHubCredentialOwner(context.Background(), GitHubCredentialOwnerOptions{Journal: journal, JWT: func(ctx context.Context) ([]byte, error) {
		raw, err := signer.Sign(ctx)
		transferred = raw
		return raw, err
	}, ReaderTransport: ro.Transport, PublisherTransport: po.Transport, Check: func(context.Context, Enrollment) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if key.calls != 2 || len(transferred) != len(expected) {
		t.Fatal("actual signer was skipped or replayed")
	}
	for _, b := range transferred {
		if b != 0 {
			t.Fatal("actual producer buffer retained")
		}
	}
	_, rMints := credentialCounts(reader)
	_, pMints := credentialCounts(publisher)
	if rMints != 1 || pMints != 1 {
		t.Fatal("signed JWT did not qualify both TLS roles")
	}
	assertJournalContainsNoCredentials(t, journal, expected)
}

func TestGitHubCredentialOwnerFencesConcurrentPublisherBeforeReader(t *testing.T) {
	owner, options, reader, publisher, signs := credentialOwnerFixture(t)
	other, err := NewGitHubInstallationJournal(context.Background(), options.Journal.store, options.Journal.now)
	if err != nil {
		t.Fatal(err)
	}
	owner.state.options.JWT = func(ctx context.Context) ([]byte, error) {
		signs.Add(1)
		// This runs after the owner's durable preflight. A separate journal
		// handle must not begin publisher work while the reader is pending.
		publisher.mu.Lock()
		publisher.loseMint = true
		publisher.mu.Unlock()
		phase, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
		defer cancel()
		transport, acquireErr := other.Acquire(phase, GitHubInstallationOptions{options.Journal.enrollment, GitHubPublisher, []byte("synthetic-app-jwt"), options.PublisherTransport, options.Journal.now})
		if transport != nil {
			transport.Close()
			t.Error("competing publisher unexpectedly qualified")
		}
		publisher.mu.Lock()
		publisher.loseMint = false
		publisher.mu.Unlock()
		if acquireErr == nil {
			t.Error("competing publisher was not fenced")
		}
		return []byte("synthetic-app-jwt"), nil
	}
	if err := owner.Refresh(context.Background()); err != nil {
		t.Fatal("publisher raced the reader preflight", err)
	}
	_, readerMints := credentialCounts(reader)
	_, publisherMints := credentialCounts(publisher)
	if signs.Load() != 1 || readerMints != 1 || publisherMints != 1 || owner.Ready(context.Background()) != nil {
		t.Fatal("competing publisher dispatched or pair was not qualified", signs.Load(), readerMints, publisherMints)
	}
	latest, err := options.Journal.Latest(context.Background(), GitHubPublisher)
	if err != nil || latest.State != "qualified" {
		t.Fatal("competing publisher latched uncertainty", latest.State, err)
	}
}
