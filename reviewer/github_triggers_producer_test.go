package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
)

// This positive fixture is whitelisted from the real closed model-free PR221,
// not hand-invented event fields. Mutable GitHub state is not contacted by tests.
func TestPollingTriggersAcceptActualGitHubPullMetadata(t *testing.T) {
	raw, err := os.ReadFile("testdata/github-polling-pr221.json")
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Source string
		SHA    string `json:"fixture_sha256"`
	}
	b, err := os.ReadFile("testdata/github-polling-pr221.provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &provenance); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != provenance.SHA || provenance.Source != "https://api.github.com/repos/hemsoft-dev/set-it-free-loop/pulls/221" {
		t.Fatal("producer provenance changed")
	}
	var p githubPull
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	store, _ := openFixtureStore(t)
	scope := Scope{RepositoryID: RepositoryID, InstallationID: 9001, PR: 221}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method != "GET" {
			t.Error("metadata fixture wrote to remote")
			w.WriteHeader(400)
			return
		}
		switch q.URL.Path {
		case githubPR(scope):
			w.Write(raw)
		case githubRepoPath + "/git/ref/heads/main":
			json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": p.Base.SHA, "type": "commit"}})
		default:
			t.Error("unexpected metadata destination", q.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	g, err := NewGitHubTriggerDiscoveryHTTP(alphaEnrollment(), store, githubFixtureRoute{base, server.Client().Transport, t}, TriggerPolicy{EnabledAt: controlTime})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := g.reader.captureTrigger(context.Background(), scope, true)
	if err != nil || captured.Open || captured.PullID != p.ID || captured.AuthorID != OwnerID || captured.Snapshot.Head != p.Head.SHA || captured.Snapshot.Base != p.Base.SHA || !captured.CreatedAt.Equal(p.CreatedAt) {
		t.Fatal(captured, err)
	}
}
