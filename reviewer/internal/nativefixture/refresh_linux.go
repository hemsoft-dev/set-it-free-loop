//go:build linux

package nativefixture

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

const rotatedAccess = "sfl-fake-rotated-access"
const rotatedRefresh = "sfl-fake-rotated-refresh"

type refreshBackend struct {
	mu              sync.Mutex
	provider        r.Provider
	reused          bool
	calls, failures int
}

func RefreshAuthority(provider r.Provider) string {
	if provider == r.Codex {
		return "auth.openai.com"
	}
	return "platform.claude.com"
}
func refreshPath(provider r.Provider) string {
	if provider == r.Codex {
		return "/oauth/token"
	}
	return "/v1/oauth/token"
}

func NewRefresh(s r.Session, reused bool) (*Backend, r.EgressPolicy, error) {
	b, p, err := New(s, false)
	if err != nil {
		return nil, p, err
	}
	b.Refresh = &refreshBackend{provider: s.Identity.Provider, reused: reused}
	if b.Codex != nil {
		b.Codex.accessToken = rotatedAccess
	} else {
		b.Claude.accessToken = rotatedAccess
	}
	p.Routes = append(p.Routes, r.EgressRoute{Authority: RefreshAuthority(s.Identity.Provider), Method: "POST", Path: refreshPath(s.Identity.Provider), Kind: "refresh"})
	p.Limits.RefreshRequests = 1
	p.Limits.Requests = p.Limits.InferenceRequests + p.Limits.RefreshRequests + p.Limits.MetadataRequests + p.Limits.NegotiationRequests
	return b, p, nil
}

func ExpiredSeed(provider r.Provider) []byte {
	var value map[string]any
	json.Unmarshal(Seed(provider), &value)
	if provider == r.Codex {
		value["last_refresh"] = time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	} else {
		value["claudeAiOauth"].(map[string]any)["expiresAt"] = int64(1)
	}
	raw, _ := json.Marshal(value)
	return raw
}

func RotatedAuth(provider r.Provider, raw []byte) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	if provider == r.Codex {
		var token map[string]string
		var refreshed string
		if json.Unmarshal(value["tokens"], &token) != nil || json.Unmarshal(value["last_refresh"], &refreshed) != nil {
			return false
		}
		when, err := time.Parse(time.RFC3339, refreshed)
		return err == nil && time.Since(when) >= -time.Minute && time.Since(when) < time.Hour && token["access_token"] == rotatedAccess && token["refresh_token"] == rotatedRefresh
	}
	var token struct {
		Access  string `json:"accessToken"`
		Refresh string `json:"refreshToken"`
		Expires int64  `json:"expiresAt"`
	}
	return json.Unmarshal(value["claudeAiOauth"], &token) == nil && token.Access == rotatedAccess && token.Refresh == rotatedRefresh && token.Expires > time.Now().Add(30*time.Minute).UnixMilli()
}

func (b *refreshBackend) serve(w http.ResponseWriter, q *http.Request) bool {
	if q.URL.Path != refreshPath(b.provider) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	deny := func() { b.failures++; http.Error(w, "synthetic refresh denied", 400) }
	if b.reused || b.calls != 1 || q.Method != "POST" || q.URL.RawQuery != "" || q.Header.Get("Authorization") != "" || q.Header.Get("X-Api-Key") != "" {
		deny()
		return true
	}
	raw, err := io.ReadAll(io.LimitReader(q.Body, 8193))
	q.Body.Close()
	if err != nil || len(raw) > 8192 {
		deny()
		return true
	}
	fields, err := refreshFields(q.Header.Get("Content-Type"), raw)
	clear(raw)
	if err != nil || fields["grant_type"] != "refresh_token" || fields["refresh_token"] != "sfl-fake-refresh" || fields["client_id"] == "" {
		deny()
		return true
	}
	for name := range fields {
		if name != "grant_type" && name != "refresh_token" && name != "client_id" && name != "scope" {
			deny()
			return true
		}
	}
	response := map[string]any{"access_token": rotatedAccess, "refresh_token": rotatedRefresh, "token_type": "Bearer", "expires_in": 3600, "scope": "user:profile user:inference user:sessions:claude_code"}
	if b.provider == r.Codex {
		var seed struct {
			Tokens map[string]string `json:"tokens"`
		}
		json.Unmarshal(Seed(r.Codex), &seed)
		response["id_token"] = seed.Tokens["id_token"]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	return true
}

func refreshFields(content string, raw []byte) (map[string]string, error) {
	if strings.HasPrefix(content, "application/json") {
		return r.DecodeStrict[map[string]string](raw, 8192)
	}
	if !strings.HasPrefix(content, "application/x-www-form-urlencoded") {
		return nil, errors.New("unexpected synthetic refresh format")
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	for name, value := range values {
		if len(value) != 1 {
			return nil, errors.New("duplicate synthetic refresh field")
		}
		fields[name] = value[0]
	}
	return fields, nil
}
