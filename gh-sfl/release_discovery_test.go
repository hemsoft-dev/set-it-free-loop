package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type releaseDiscoveryREST struct {
	*fakeREST
	pages   [][]map[string]any
	failure error
	routes  []string
}

func (f *releaseDiscoveryREST) Get(path string, response interface{}) error {
	f.routes = append(f.routes, path)
	if f.failure != nil {
		return f.failure
	}
	var page int
	if _, err := fmt.Sscanf(path, "repos/hemsoft-dev/set-it-free-loop/releases?per_page=100&page=%d", &page); err != nil {
		return fmt.Errorf("unexpected source route %s", path)
	}
	if page < 1 || page > len(f.pages) {
		return fmt.Errorf("unexpected page %d", page)
	}
	body, err := json.Marshal(f.pages[page-1])
	if err != nil {
		return err
	}
	return json.Unmarshal(body, response)
}
func configureReleaseDiscovery(t *testing.T, client *releaseDiscoveryREST) {
	t.Helper()
	t.Setenv("SFL_SOURCE_TOKEN", "")
	old := newRESTClient
	newRESTClient = func() (restAPI, error) { return client, nil }
	t.Cleanup(func() { newRESTClient = old })
}
func TestFetchLatestReleaseIncludesPublishedPrerelease(t *testing.T) {
	client := &releaseDiscoveryREST{fakeREST: &fakeREST{}, pages: [][]map[string]any{{
		{"id": 5, "tag_name": "v2.1.0-rc.16", "published_at": "2026-10-08T03:10:00Z", "prerelease": true},
		{"id": 7, "tag_name": "v2.1.0-rc.17", "draft": true, "published_at": "2026-10-09T03:10:00Z"},
		{"id": 6, "tag_name": "release-not-synchronized", "published_at": "2026-10-09T03:10:00Z"},
		{"id": 2, "tag_name": "v2.0.0", "published_at": "2026-10-01T03:10:00Z"},
		{"id": 8, "tag_name": "v2.1.0-rc.18"},
	}}}
	configureReleaseDiscovery(t, client)
	got, err := fetchLatestRelease("hemsoft-dev", "set-it-free-loop")
	if err != nil || got != "v2.1.0-rc.16" {
		t.Fatalf("latest=%q, err=%v", got, err)
	}
}
func TestFetchLatestReleasePaginatesAndUsesPublicationOrder(t *testing.T) {
	first := make([]map[string]any, 100)
	for i := range first {
		first[i] = map[string]any{"id": int64(i), "tag_name": "v2.2.0-rc.1", "draft": true}
	}
	client := &releaseDiscoveryREST{fakeREST: &fakeREST{}, pages: [][]map[string]any{first, {
		{"id": 101, "tag_name": "v2.1.0-rc.16", "published_at": "2026-10-08T03:10:00Z"},
		{"id": 102, "tag_name": "v2.1.0", "published_at": "2026-10-09T03:10:00Z"},
	}}}
	configureReleaseDiscovery(t, client)
	got, err := fetchLatestRelease("hemsoft-dev", "set-it-free-loop")
	if err != nil || got != "v2.1.0" || len(client.routes) != 2 {
		t.Fatalf("latest=%q, routes=%v, err=%v", got, client.routes, err)
	}
}
func TestFetchLatestReleasePreservesSeparatePrivateSourceCredential(t *testing.T) {
	client := &releaseDiscoveryREST{fakeREST: &fakeREST{}, pages: [][]map[string]any{{{"id": 5, "tag_name": "v2.1.0-rc.16", "published_at": "2026-10-08T03:10:00Z"}}}}
	t.Setenv("SFL_SOURCE_TOKEN", "source-read-fixture")
	oldSource, oldDefault := newSourceRESTClient, newRESTClient
	t.Cleanup(func() { newSourceRESTClient, newRESTClient = oldSource, oldDefault })
	newSourceRESTClient = func(token string) (restAPI, error) {
		if token != "source-read-fixture" {
			t.Fatal("wrong source credential")
		}
		return client, nil
	}
	newRESTClient = func() (restAPI, error) { t.Fatal("default target credential used for private source"); return nil, nil }
	got, err := fetchLatestRelease("hemsoft-dev", "set-it-free-loop")
	if err != nil || got != "v2.1.0-rc.16" {
		t.Fatalf("latest=%q, err=%v", got, err)
	}
}
func TestFetchLatestReleasePropagatesAccessFailure(t *testing.T) {
	denied := errors.New("HTTP 403: source access denied")
	client := &releaseDiscoveryREST{fakeREST: &fakeREST{}, failure: denied}
	configureReleaseDiscovery(t, client)
	got, err := fetchLatestRelease("hemsoft-dev", "set-it-free-loop")
	if got != "" || !errors.Is(err, denied) || len(client.routes) != 1 {
		t.Fatalf("latest=%q, err=%v, routes=%v", got, err, client.routes)
	}
}
func TestFetchLatestReleaseRejectsNoPublishedPackage(t *testing.T) {
	client := &releaseDiscoveryREST{fakeREST: &fakeREST{}, pages: [][]map[string]any{{}}}
	configureReleaseDiscovery(t, client)
	got, err := fetchLatestRelease("hemsoft-dev", "set-it-free-loop")
	if got != "" || err == nil || !strings.Contains(err.Error(), "no published semantic-version SFL release") {
		t.Fatalf("latest=%q, err=%v", got, err)
	}
}
