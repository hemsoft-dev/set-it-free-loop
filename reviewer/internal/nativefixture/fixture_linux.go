//go:build linux

// Package nativefixture contains synthetic pinned-client protocol producers.
// It has no actual auth import, provider connection, App or activation route.
package nativefixture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
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
	Session  r.Session
	Codex    *codexBackend
	Claude   *claudeBackend
	Refresh  *refreshBackend
	hostile  *hostileState
	secrets  []string
	scanMu   sync.Mutex
	scanHits int
}

func New(s r.Session, negative bool) (*Backend, r.EgressPolicy, error) {
	if s.Validate() != nil || s.Snapshot.Head != head {
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

// The backend proposes fixture findings only in R1. Later native sessions must
// copy their captured provenance and explicitly examine the frozen peer IDs.
// These canned judgments qualify protocol transport, never review quality.
func sourceReport(s r.Session) r.Report {
	report := r.Report{JobID: s.JobID, PolicyVersion: s.PolicyVersion, PacketHash: s.PacketHash, Provider: s.Identity.Provider, Round: s.Round, SnapshotHash: s.SnapshotHash, ConfigHash: s.ConfigHash, Findings: []r.Finding{}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}}
	if s.Round == 1 {
		report.Findings = append(report.Findings, r.Finding{ID: string(s.Identity.Provider) + ":fixture", Title: "Synthetic finding", Body: "Protocol fixture only.", Severity: 2, Anchor: r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}})
		return report
	}
	focus := map[string]bool{}
	for _, id := range s.FocusIDs {
		focus[id] = true
	}
	for _, peer := range s.PeerReports {
		for _, finding := range peer.Findings {
			if s.Round == 3 && !focus[finding.ID] {
				continue
			}
			verdict := "confirm"
			if peer.Provider == s.Identity.Provider {
				if s.Round != 3 {
					continue
				}
				verdict = "respond"
			}
			report.Judgments = append(report.Judgments, r.Judgment{FindingID: finding.ID, Verdict: verdict, Reason: "Synthetic source-backed protocol disposition.", Evidence: []r.Anchor{finding.Anchor}})
		}
	}
	return report
}
func (b *Backend) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	if len(b.secrets) != 0 {
		raw, err := io.ReadAll(io.LimitReader(q.Body, (2<<20)+1))
		_ = q.Body.Close()
		b.scan(q, raw)
		if err != nil || len(raw) > 2<<20 {
			http.Error(w, "request cap", 413)
			return
		}
		q.Body = io.NopCloser(bytes.NewReader(raw))
	}
	if b.Refresh != nil && b.Refresh.serve(w, q) {
		return
	}
	if b.Codex != nil {
		b.Codex.ServeHTTP(w, q)
	} else {
		b.Claude.ServeHTTP(w, q)
	}
}

type Counts struct {
	Inference, Metadata, Negotiation, Failures, Telemetry int
	Refreshes, RefreshFailures                            int
	MetadataCaptured                                      bool
}

func (b *Backend) Counts() (result Counts) {
	defer func() {
		if b.Refresh != nil {
			b.Refresh.mu.Lock()
			result.Refreshes = b.Refresh.calls
			result.RefreshFailures = b.Refresh.failures
			result.Metadata += b.Refresh.profiles
			b.Refresh.mu.Unlock()
		}
	}()
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

// NewControl emits synthetic control output only after the same native Read and
// Search responses as review. It does not connect to a provider or read auth.
func NewControl(input r.ControlSession) (*Backend, r.EgressPolicy, error) {
	capability, err := r.ControlCapabilitySession(input)
	if err != nil {
		return nil, r.EgressPolicy{}, err
	}
	backend, policy, err := New(capability, false)
	if err != nil {
		return nil, r.EgressPolicy{}, err
	}
	report := sourceControlReport(input)
	if err = r.ValidateControlReport(report, input); err != nil {
		return nil, r.EgressPolicy{}, err
	}
	if backend.Codex != nil {
		backend.Codex.output = report
	} else {
		backend.Claude.output = report
	}
	return backend, policy, nil
}
func sourceControlReport(s r.ControlSession) r.ControlReport {
	report := r.ControlReport{InputHash: r.Digest(s), JobHash: r.Digest(s.Job), Step: s.Step, Findings: []r.Finding{}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}}
	id := string(s.Provider) + ":fixture"
	anchor := r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}
	if s.Step == 1 {
		report.Findings = append(report.Findings, r.Finding{ID: id, Title: "Synthetic finding", Body: "Protocol fixture only.", Severity: 2, Anchor: anchor})
	} else if s.Step == 4 {
		report.Retractions = append(report.Retractions, id)
	} else {
		verdict := "reject"
		if s.Step == 3 {
			verdict = "cannot_verify"
		}
		report.Judgments = append(report.Judgments, r.Judgment{FindingID: id, Verdict: verdict, Reason: "Synthetic single-provider disposition.", Evidence: []r.Anchor{anchor}})
	}
	return report
}
