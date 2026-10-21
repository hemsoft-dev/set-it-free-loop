//go:build linux

package reviewer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gitServiceFixture(t *testing.T) (gitReviewerServiceOptions, gitUnitFixture, *installationFixture, *installationFixture, *atomic.Int32) {
	t.Helper()
	r, f, runtime := gitFactoryAssetsFixture(t)
	helper, err := duplicateGitControlFile(r.files[2])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { helper.Close() })
	if r.stop(context.Background()) != nil || r.close() != nil || f.owner.Close() != nil {
		t.Fatal("prepared fixture disposal")
	}
	unit, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unit.Close() })
	owner, credentials, reader, publisher, signs := credentialOwnerFixture(t)
	if owner.Close(context.Background()) != nil {
		t.Fatal("unqualified fixture credential owner close")
	}
	return gitReviewerServiceOptions{
		Credentials: credentials, Unit: unit, Archive: r.archive, RuntimeRoot: runtime,
		Helper: helper, Manifest: r.manifest, Triggers: TriggerPolicy{EnabledAt: controlTime},
		Release: testJob().Release, ReleaseHash: r.binding.ReleaseHash, Directory: gitFactoryWorkRoot(t),
		Runtime:  func(context.Context, Job, *JobEvidence) (Engine, error) { return Engine{}, ErrLiveBlocked },
		Check:    func(context.Context, Enrollment, string) error { return nil },
		Interval: time.Second, MaxBackoff: time.Minute, TickTimeout: time.Second,
	}, f, reader, publisher, signs
}

func TestGitReviewerServiceConstructionBeforeExplicitRefresh(t *testing.T) {
	o, _, reader, publisher, signs := gitServiceFixture(t)
	s, err := newGitReviewerService(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	rCalls, rMints := credentialCounts(reader)
	pCalls, pMints := credentialCounts(publisher)
	if signs.Load() != 0 || rCalls != 0 || pCalls != 0 || rMints != 0 || pMints != 0 || !s.source.ready || s.controller.options.Source != s.source {
		t.Fatal("construction acquired credentials or skipped concrete startup")
	}
	discovery, ok := s.controller.options.Discovery.(*GitHubTriggerDiscoveryHTTP)
	if !ok || discovery.policy != o.Triggers {
		t.Fatal("selected trigger policy not composed")
	}
	read := discovery.reader.http.client.Transport.(githubReadTransport).inner.(*GitHubCredentialRoleTransport)
	write := s.controller.options.Publisher.Remote.(*GitHubPublicationHTTP)
	publish := write.client.Transport.(*GitHubCredentialRoleTransport)
	if read.owner != s.credentials.state || read.role != GitHubReader || publish.owner != s.credentials.state || publish.role != GitHubPublisher || write.reader != discovery {
		t.Fatal("source/publication roles or trigger provenance mixed")
	}
	if s.Run(context.Background()) == nil || signs.Load() != 0 {
		t.Fatal("Run implicitly acquired credentials")
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, rMints = credentialCounts(reader)
	_, pMints = credentialCounts(publisher)
	if signs.Load() != 1 || rMints != 1 || pMints != 1 {
		t.Fatal("explicit qualification replayed or omitted a role")
	}
	response, err := read.RoundTrip(installationQuery(t, "GET", githubRepoPath))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var acknowledgment struct{ ID int64 }
	if _, err := write.request(context.Background(), "POST", githubIssue(alphaScope())+"/comments", map[string]string{"body": "model-free service fixture"}, http.StatusCreated, &acknowledgment); err != nil || acknowledgment.ID != 91 {
		t.Fatal("composed publisher failed its qualified synthetic write", err)
	}
	_, pMints = credentialCounts(publisher)
	if signs.Load() != 1 || pMints != 1 {
		t.Fatal("publisher silently refreshed credentials")
	}
	if s.Close(context.Background()) != nil || !s.source.closed || !s.credentials.state.closed {
		t.Fatal("successful shutdown lost an owner")
	}
	reopened, err := OpenGitHubCredentialOwner(context.Background(), o.Credentials)
	if err != nil {
		t.Fatal("credential lock retained after successful service close", err)
	}
	defer reopened.Close(context.Background())
	if reopened.Ready(context.Background()) == nil || signs.Load() != 1 {
		t.Fatal("replacement inherited readiness or signed")
	}
}

func TestGitReviewerServiceInvalidFactoryKeepsCallerOwnership(t *testing.T) {
	o, _, _, _, signs := gitServiceFixture(t)
	o.Directory = "relative-invalid-work-root"
	if s, err := newGitReviewerService(context.Background(), o); err == nil || s != nil || signs.Load() != 0 || o.Unit.closed || o.Archive.closed {
		t.Fatal("invalid factory transferred inputs or kept a credential owner", err)
	}
	owner, err := OpenGitHubCredentialOwner(context.Background(), o.Credentials)
	if err != nil {
		t.Fatal("constructor leaked its otherwise unqualified credential lock", err)
	}
	defer owner.Close(context.Background())
	fd, err := o.RuntimeRoot.directoryFD()
	if err != nil {
		t.Fatal("invalid constructor disposed the caller's runtime", err)
	}
	fd.Close()
}

func TestGitReviewerServiceRevokedEligibilityDoesNotSign(t *testing.T) {
	o, _, reader, publisher, signs := gitServiceFixture(t)
	o.Check = func(context.Context, Enrollment, string) error { return ErrLiveBlocked }
	s, err := newGitReviewerService(context.Background(), o)
	if err != nil {
		t.Fatal("local construction should not activate gates", err)
	}
	defer s.Close(context.Background())
	if s.Refresh(context.Background()) == nil {
		t.Fatal("revoked release acquired credentials")
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if signs.Load() != 0 || rCalls != 0 || pCalls != 0 {
		t.Fatal("release denial reached signing or HTTP")
	}
}

func TestGitReviewerServiceUnknownStartupRetainsOwners(t *testing.T) {
	o, f, reader, publisher, signs := gitServiceFixture(t)
	r, err := o.Unit.read(f.binding.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	r.Started, r.Stopped, r.Revoked = true, false, false
	r.StartJob = "/org/freedesktop/systemd1/job/1"
	if o.Unit.write(r) != nil {
		t.Fatal("synthetic unknown start receipt")
	}
	f.manager.observeErr = errors.New("synthetic missing old unit")
	s, err := newGitReviewerService(context.Background(), o)
	if err == nil || s == nil || s.source == nil || s.source.ready || s.controller != nil {
		t.Fatal("uncertain startup abandoned ownership or started polling", err)
	}
	if s.Refresh(context.Background()) == nil || signs.Load() != 0 {
		t.Fatal("unrecovered service acquired credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if other, err := OpenGitHubCredentialOwner(ctx, o.Credentials); err == nil || other != nil {
		t.Fatal("failed startup released its credential owner lock")
	}
	if s.Close(context.Background()) == nil || s.source.unit.closed || s.credentials.state.closed {
		t.Fatal("uncertain close disposed later owners")
	}
	// Explicit fake changed-boot proof retires only this fixture's old record.
	// It is not evidence that a real worker or host reboot was qualified.
	f.manager.kernel.boot = "synthetic new boot for explicit service recovery"
	if s.Recover(context.Background()) != nil || s.Refresh(context.Background()) == nil || s.controller != nil {
		t.Fatal("shutdown recovery reopened admission")
	}
	if s.Close(context.Background()) != nil || !s.closed {
		t.Fatal("recovered shutdown could not finish")
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if signs.Load() != 0 || rCalls != 0 || pCalls != 0 {
		t.Fatal("local startup/recovery dispatched credentials")
	}
}

func TestGitReviewerServiceCloseCancelsRefreshButRetainsUnreturnedCallback(t *testing.T) {
	o, _, reader, publisher, signs := gitServiceFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	o.Credentials.JWT = func(context.Context) ([]byte, error) {
		signs.Add(1)
		close(entered)
		<-release
		return []byte("synthetic-app-jwt"), nil
	}
	s, err := newGitReviewerService(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.Refresh(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("signing callback not reached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if s.source.enter(ctx) {
		s.source.leave()
		t.Fatal("refresh released factory fence before qualification")
	}
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	if !errors.Is(s.Close(ctx), ErrWorkerStopPending) || s.source.closed || s.credentials.state.closed {
		t.Fatal("unreturned callback lost source/credential ownership")
	}
	cancel()
	unblock()
	if err := <-result; err == nil {
		t.Fatal("canceled refresh reported success")
	}
	if s.Close(context.Background()) != nil {
		t.Fatal("explicit close retry failed")
	}
	rCalls, _ := credentialCounts(reader)
	pCalls, _ := credentialCounts(publisher)
	if signs.Load() != 1 || rCalls != 0 || pCalls != 0 {
		t.Fatal("late callback caused credential HTTP or replay")
	}
}

func TestGitReviewerServiceRefreshDrainsGenerationWithoutFactoryDeadlock(t *testing.T) {
	o, _, _, _, signs := gitServiceFixture(t)
	s, err := newGitReviewerService(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if s.Refresh(context.Background()) != nil {
		t.Fatal("initial explicit refresh")
	}
	stops := 0
	g, err := s.credentials.retainReaderGeneration(context.Background(), func(context.Context) error { stops++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.credentials.state.mu.Lock()
	changed := s.credentials.state.changed
	s.credentials.state.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- s.Refresh(context.Background()) }()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("replacement did not begin draining")
	}
	if g.stop(context.Background()) != nil {
		t.Fatal("generation stop needed the outer factory token")
	}
	select {
	case err := <-result:
		if err != nil || signs.Load() != 2 || stops != 1 {
			t.Fatal("drained refresh did not commit exactly once", err, signs.Load(), stops)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh deadlocked while holding source ownership")
	}
}

func TestGitReviewerServiceModelFreeRunAndExplicitClose(t *testing.T) {
	for _, viaService := range []bool{false, true} {
		t.Run(fmt.Sprint("service-close-", viaService), func(t *testing.T) {
			o, _, _, _, signs := gitServiceFixture(t)
			transport := o.Credentials.ReaderTransport
			listed := make(chan struct{})
			var once sync.Once
			var modelCalls atomic.Int32
			o.Credentials.ReaderTransport = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
				if q.URL.Path == githubRepoPath+"/pulls" {
					if q.Header.Get("Authorization") != "Bearer synthetic-installation-reader" {
						t.Error("polling did not use the reader role")
					}
					once.Do(func() { close(listed) })
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]")), Request: q}, nil
				}
				return transport.RoundTrip(q)
			})
			o.Runtime = func(context.Context, Job, *JobEvidence) (Engine, error) {
				modelCalls.Add(1)
				return Engine{}, ErrLiveBlocked
			}
			s, err := newGitReviewerService(context.Background(), o)
			if err != nil || s.Refresh(context.Background()) != nil {
				t.Fatal("synthetic service qualification", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- s.Run(ctx) }()
			select {
			case <-listed:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("composed polling reader did not list empty fixture scope")
			}
			if viaService {
				if err := s.Close(context.Background()); err != nil {
					t.Fatal("Close could not join Run", err)
				}
			} else {
				cancel()
			}
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatal("service run did not retain cancellation", err)
			}
			if s.source.closed != viaService || s.credentials.state.closed != viaService || signs.Load() != 1 || modelCalls.Load() != 0 {
				t.Fatal("model-free Run renewed or discarded retained owners")
			}
			if s.Close(context.Background()) != nil || s.Run(context.Background()) == nil {
				t.Fatal("explicit shutdown failed or reopened the service")
			}
		})
	}
}
