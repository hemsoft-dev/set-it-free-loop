//go:build linux || darwin

package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
