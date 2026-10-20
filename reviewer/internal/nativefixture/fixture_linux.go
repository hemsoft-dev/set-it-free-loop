//go:build linux

// Package nativefixture contains synthetic pinned-client protocol producers.
// It has no actual auth import, provider connection, App or activation route.
package nativefixture

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

const source = "immutable source evidence\n"

var head = strings.Repeat("a", 40)

type Store struct{}

func (Store) Read(_ context.Context, revision, path string, limit int) (r.Object, error) {
	if revision != head || path != "source.go" || limit < len(source) {
		return r.Object{}, errors.New("fake object denied")
	}
	return r.Object{Mode: "100644", Data: []byte(source)}, nil
}

type Backend struct {
	Session r.Session
	Codex   *codexBackend
	Claude  *claudeBackend
}

func New(s r.Session, negative bool) (*Backend, r.EgressPolicy, error) {
	if s.Validate() != nil || s.Round != 1 || s.Snapshot.Head != head {
		return nil, r.EgressPolicy{}, errors.New("invalid fixed fake session")
	}
	b := &Backend{Session: s}
	if s.Identity.Provider == r.Codex {
		mode := "source"
		if negative {
			mode = "guarded-undeclared-patch"
		}
		b.Codex = &codexBackend{session: s, mode: mode}
		return b, codexPolicy(s), nil
	}
	mode := "source"
	if negative {
		mode = "guarded-bash"
	}
	b.Claude = &claudeBackend{s: s, mode: mode}
	return b, claudePolicy(s), nil
}
func (b *Backend) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	if b.Codex != nil {
		b.Codex.ServeHTTP(w, q)
	} else {
		b.Claude.ServeHTTP(w, q)
	}
}

type Counts struct {
	Inference, Metadata, Negotiation, Failures, Telemetry int
	MetadataCaptured                                      bool
}

func (b *Backend) Counts() Counts {
	if b.Codex != nil {
		c := b.Codex
		c.mu.Lock()
		defer c.mu.Unlock()
		metadata := len(c.metadataRoutes) == 3 && (c.metadataRoutes["/v1/models"] == 1 || c.metadataRoutes["/v1/models"] == 2) && c.metadataRoutes["/backend-api/wham/accounts/check"] == 2 && c.metadataRoutes["/backend-api/wham/settings/user"] == 1
		return Counts{Inference: c.calls, Metadata: c.metadata, Negotiation: c.negotiations, Failures: c.failed, Telemetry: c.telemetry, MetadataCaptured: metadata}
	}
	c := b.Claude
	c.mu.Lock()
	defer c.mu.Unlock()
	return Counts{Inference: c.calls, Failures: c.failures}
}

func Seed(provider r.Provider) []byte {
	if provider == r.Claude {
		return []byte(`{"claudeAiOauth":{"accessToken":"sfl-fake-access","refreshToken":"sfl-fake-refresh","expiresAt":4102444800000,"scopes":["user:profile","user:inference","user:sessions:claude_code"],"subscriptionType":"max","rateLimitTier":"default_claude_max_5x"}}`)
	}
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	jwt := encode(map[string]string{"alg": "none", "typ": "JWT"}) + "." + encode(map[string]any{"email": "fixture@example.invalid", "https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": "plus", "chatgpt_user_id": "sfl-fake-user", "chatgpt_account_id": "sfl-fake-account"}}) + "." + encode("fake-signature")
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"id_token": jwt, "access_token": "sfl-fake-access", "refresh_token": "sfl-fake-refresh", "account_id": "sfl-fake-account"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
	return raw
}
