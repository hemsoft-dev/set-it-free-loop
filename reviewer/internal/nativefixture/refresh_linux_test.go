//go:build linux

package nativefixture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func TestSyntheticRefreshPolicyFitsTheCapturedPhaseBudget(t *testing.T) {
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
		s, err := r.NewSession("synthetic-refresh-policy", snapshot, r.Digest("config"), provider, 1, source, nil, nil, r.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		_, policy, err := NewRefresh(s, false)
		if err != nil {
			t.Fatal(err)
		}
		guard, err := r.NewProviderEgress(context.Background(), s, policy, &http.Transport{})
		if err != nil {
			t.Fatal("producer policy cannot be admitted before VM work", provider, err)
		}
		if _, err := guard.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyntheticRefreshProducesRotatedAuthAndRejectsReplay(t *testing.T) {
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		t.Run(string(provider), func(t *testing.T) {
			b := &refreshBackend{provider: provider}
			request := func() *httptest.ResponseRecorder {
				q := httptest.NewRequest("POST", "https://"+RefreshAuthority(provider)+refreshPath(provider), strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"sfl-fake-refresh","client_id":"synthetic-client"}`))
				q.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				if !b.serve(w, q) {
					t.Fatal("refresh route absent")
				}
				return w
			}
			first := request()
			if first.Code != 200 {
				t.Fatal("synthetic refresh refused", first.Code)
			}
			var response map[string]any
			if json.Unmarshal(first.Body.Bytes(), &response) != nil || response["access_token"] != rotatedAccess || response["refresh_token"] != rotatedRefresh {
				t.Fatal("rotation absent")
			}
			if second := request(); second.Code != 400 || b.calls != 2 || b.failures != 1 {
				t.Fatal("refresh replay accepted")
			}
		})
	}
}

func TestRefreshCannotTreatAnExpiredSeedAsAReturnedRotation(t *testing.T) {
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		seed := ExpiredSeed(provider)
		if RotatedAuth(provider, seed) || RotatedAuth(provider, Seed(provider)) {
			t.Fatal("unchanged seed qualified as rotated", provider)
		}
		var value map[string]any
		json.Unmarshal(seed, &value)
		if provider == r.Codex {
			token := value["tokens"].(map[string]any)
			token["access_token"] = rotatedAccess
			token["refresh_token"] = rotatedRefresh
			value["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
		} else {
			token := value["claudeAiOauth"].(map[string]any)
			token["accessToken"] = rotatedAccess
			token["refreshToken"] = rotatedRefresh
			token["expiresAt"] = time.Now().Add(time.Hour).UnixMilli()
		}
		rotated, _ := json.Marshal(value)
		if !RotatedAuth(provider, rotated) {
			t.Fatal("valid returned rotation refused", provider)
		}
	}
}

func TestSyntheticRefreshRejectsDuplicateFieldsInBothWireFormats(t *testing.T) {
	for _, sample := range []struct{ content, body string }{
		{"application/json", `{"refresh_token":"first","refresh_token":"second"}`},
		{"application/x-www-form-urlencoded", "refresh_token=first&refresh_token=second"},
	} {
		if _, err := refreshFields(sample.content, []byte(sample.body)); err == nil {
			t.Fatal("ambiguous refresh accepted")
		}
	}
}
