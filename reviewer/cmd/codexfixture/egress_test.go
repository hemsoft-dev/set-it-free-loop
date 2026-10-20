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

func TestCapturedNativeHeadersRemainInExplicitFixturePolicy(t *testing.T) {
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
		t.Fatal("invalid captured headers")
	}
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("fixture-header-test", snapshot, r.Digest("synthetic"), r.Codex, 1, "packet", nil, nil, r.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	guard, policy, _, closeBackend, err := fixtureEgress(s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("header test contacted backend") }))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	defer guard.Close(context.Background())
	allowed := map[string]bool{}
	for _, name := range policy.Headers {
		allowed[name] = true
	}
	protocol := map[string]bool{"Content-Length": true, "Connection": true, "Upgrade": true, "Sec-Websocket-Key": true, "Sec-Websocket-Version": true, "Sec-Websocket-Extensions": true}
	for _, request := range capture.Requests {
		for _, name := range request.Headers {
			if !allowed[name] && !protocol[name] {
				t.Errorf("captured native header absent from policy: %s", name)
			}
		}
	}
}
