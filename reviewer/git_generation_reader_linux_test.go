//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
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

func TestGitGenerationReaderPinsActualTLSAndRetiresBodies(t *testing.T) {
	installation, calls := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		operation := GitHubGitAdvertisement
		if q.Method == http.MethodPost {
			operation = GitHubGitUploadPack
			input, _ := io.ReadAll(q.Body)
			if string(input) != "0008NAK\n0000" {
				t.Error("upload-pack input changed")
			}
		}
		w.Header().Set("Content-Type", gitReadMedia(operation))
		io.WriteString(w, "0008NAK\n0000")
	}))
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	inner := pair.reader.state.inner
	pair.reader.state.inner = installation.state.inner
	defer func() { pair.reader.state.inner = inner }()
	g, stopChild, _ := credentialAcquisitionFixture(t, owner)
	reader := &gitGenerationReader{g}
	for _, protocol := range []int{0, 2} {
		for _, operation := range []GitHubGitOperation{GitHubGitAdvertisement, GitHubGitUploadPack} {
			request := GitHubGitReadRequest{Operation: operation, Protocol: protocol}
			if operation == GitHubGitUploadPack {
				request.Body = io.NopCloser(strings.NewReader("0008NAK\n0000"))
			}
			body, err := reader.Exchange(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			owner.state.mu.Lock()
			active := len(owner.state.active)
			owner.state.mu.Unlock()
			if active != 1 {
				t.Fatal("typed Git body did not retain HTTP borrow", active)
			}
			data, err := io.ReadAll(body)
			if err != nil || string(data) != "0008NAK\n0000" || body.Close() != nil || body.Close() != nil {
				t.Fatal("typed Git wire or exactly-once close", err)
			}
			owner.state.mu.Lock()
			active, owned := len(owner.state.active), len(owner.state.acquisitions)
			owner.state.mu.Unlock()
			if active != 0 || owned != 1 {
				t.Fatal("HTTP EOF lost whole-acquisition ownership", active, owned)
			}
			for _, opaque := range []any{reader, *reader, body} {
				raw, err := json.Marshal(opaque)
				if err != nil || !strings.Contains(string(raw), "redacted") || !strings.Contains(fmt.Sprintf("%#v", opaque), "redacted") {
					t.Fatal("typed generation capability exposed internals")
				}
			}
		}
	}
	if calls.Load() != 4 || g.stop(context.Background()) == nil || credentialPairCleared(pair) {
		t.Fatal("Git EOF became live-process stop evidence")
	}
	stopChild()
	if g.stop(context.Background()) != nil || owner.Close(context.Background()) != nil {
		t.Fatal("actual stopped generation did not release")
	}
	input := &installationBodyProbe{}
	if body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: input}); body != nil || err == nil || input.closed != 1 || calls.Load() != 4 {
		t.Fatal("released generation dispatched or leaked input")
	}
}

type gitGenerationUnreturnedBody struct {
	entered, closed, release chan struct{}
	readOnce, closeOnce      sync.Once
	closes                   atomic.Int32
}

func (b *gitGenerationUnreturnedBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}
func (b *gitGenerationUnreturnedBody) Close() error {
	b.closeOnce.Do(func() { b.closes.Add(1); close(b.closed) })
	return nil
}

func TestGitGenerationReaderCancellationRetainsUnreturnedRead(t *testing.T) {
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	g, stopChild, stops := credentialAcquisitionFixture(t, owner)
	inner := pair.reader.state.inner
	defer func() { pair.reader.state.inner = inner }()
	responseBody := &gitGenerationUnreturnedBody{entered: make(chan struct{}), closed: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(responseBody.release) }) }
	defer release()
	pair.reader.state.inner = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{gitReadMedia(GitHubGitAdvertisement)}}, Body: responseBody, ContentLength: -1}, nil
	})
	reader := &gitGenerationReader{g}
	body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { var one [1]byte; body.Read(one[:]); close(readDone) }()
	select {
	case <-responseBody.entered:
	case <-time.After(time.Second):
		t.Fatal("body read did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err = g.stop(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || stops.Load() != 0 || credentialPairCleared(pair) {
		t.Fatal("unreturned read lost credential ownership", err)
	}
	select {
	case <-responseBody.closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close underlying body")
	}
	if body.Close() != nil || responseBody.closes.Load() != 1 {
		t.Fatal("concurrent close replayed body closure")
	}
	owner.state.mu.Lock()
	active := len(owner.state.active)
	owner.state.mu.Unlock()
	if active != 1 {
		t.Fatal("Close retired an HTTP borrow before Read returned", active)
	}
	stopChild()
	release()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("read did not return")
	}
	if g.stop(context.Background()) != nil || stops.Load() != 1 || owner.Close(context.Background()) != nil {
		t.Fatal("returned read and stopped process did not release ownership")
	}
}

func TestGitGenerationReaderAdmissionFailureClosesInput(t *testing.T) {
	for _, reader := range []*gitGenerationReader{nil, {}} {
		input := &installationBodyProbe{}
		body, err := reader.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: input})
		if body != nil || err == nil || input.closed != 1 {
			t.Fatal("missing generation retained input")
		}
	}
}

func TestGitGenerationReaderExchangeFailureRetiresOnlyHTTPBorrow(t *testing.T) {
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("qualify pair")
	}
	pair := owner.state.current
	g, stopChild, _ := credentialAcquisitionFixture(t, owner)
	reader := &gitGenerationReader{g}
	inner := pair.reader.state.inner
	defer func() { pair.reader.state.inner = inner }()
	calls := 0
	pair.reader.state.inner = githubResponseTransport(func(q *http.Request) (*http.Response, error) {
		calls++
		if q.Body != nil {
			q.Body.Close()
		}
		return nil, errors.New("synthetic-installation-reader private transport failure")
	})
	for _, request := range []GitHubGitReadRequest{
		{Operation: GitHubGitAdvertisement},
		{Operation: "receive-pack", Body: &installationBodyProbe{}},
		{Operation: GitHubGitUploadPack, Protocol: 9, Body: &installationBodyProbe{}},
	} {
		body, err := reader.Exchange(context.Background(), request)
		if body != nil || !errors.Is(err, ErrGitHubGitRead) || strings.Contains(err.Error(), "synthetic-") {
			t.Fatal("exchange failure changed class or leaked authority", err)
		}
		if input, ok := request.Body.(*installationBodyProbe); ok && input.closed != 1 {
			t.Fatal("failed typed exchange did not close input exactly once")
		}
		owner.state.mu.Lock()
		active, owned := len(owner.state.active), len(owner.state.acquisitions)
		owner.state.mu.Unlock()
		if active != 0 || owned != 1 {
			t.Fatal("exchange failure leaked HTTP borrow or whole generation", active, owned)
		}
	}
	if calls != 1 {
		t.Fatal("invalid typed operations dispatched", calls)
	}
	stopChild()
	if g.stop(context.Background()) != nil || owner.Close(context.Background()) != nil {
		t.Fatal("failure cleanup did not retain explicit physical-stop path")
	}
}
