//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func TestCapturedClaudeHeadersAndStrictRouteBudget(t *testing.T) {
	data, err := os.ReadFile("testdata/native-request-headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Requests []struct {
			Headers []string `json:"headers"`
		} `json:"requests"`
	}
	if json.Unmarshal(data, &capture) != nil || len(capture.Requests) != 3 {
		t.Fatal("invalid native header artifact")
	}
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("claude-header-test", snapshot, r.Digest("synthetic"), r.Claude, 1, "packet", nil, nil, r.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	guard, policy, _, closeBackend, err := fixtureEgress(s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("header test reached backend") }))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	defer guard.Close(context.Background())
	if len(policy.Routes) != 1 || policy.Routes[0].RawQuery != "beta=true" || policy.Limits.Requests != 3 || policy.Limits.InferenceRequests != 3 || policy.Limits.RefreshRequests != 0 || policy.Limits.MetadataRequests != 0 || policy.Limits.NegotiationRequests != 0 {
		t.Fatal("fixture route/budget broadened", policy)
	}
	allowed := map[string]bool{"Content-Length": true, "Connection": true}
	for _, name := range policy.Headers {
		allowed[name] = true
	}
	for _, request := range capture.Requests {
		for _, name := range request.Headers {
			if !allowed[name] {
				t.Errorf("captured header missing: %s", name)
			}
		}
	}
}
