//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

func TestSettingsHookObservationRequiresTheCapturedGuestAndHelper(t *testing.T) {
	binding := r.AuthBinding{SessionHash: r.Digest("session"), LaunchID: strings.Repeat("b", 32), ContainmentID: r.Digest("containment")}
	c := config{ImageSourceHead: strings.Repeat("a", 40), DenyHelperSHA256: r.Digest("measured-helper")}
	o := r.NativeVMExecObservation{Version: 1, Binding: binding, RequestHash: r.Digest("captured-request"), ImageSourceHead: c.ImageSourceHead, HelperSHA256: c.DenyHelperSHA256, HelperEntry: "sh-deny-v1", UID: 1202, Complete: true}
	audit := hostileAudit{Binding: binding, RequestHash: o.RequestHash, ExecObservation: &o}
	if !hookObservationBound(audit, c) {
		t.Fatal("captured observer rejected")
	}
	for _, kind := range []string{"missing", "image", "helper", "session", "launch", "request", "uid", "incomplete", "failed"} {
		t.Run(kind, func(t *testing.T) {
			bad := audit
			copy := o
			bad.ExecObservation = &copy
			switch kind {
			case "missing":
				bad.ExecObservation = nil
			case "image":
				copy.ImageSourceHead = strings.Repeat("c", 40)
			case "helper":
				copy.HelperSHA256 = r.Digest("foreign-executable")
			case "session":
				copy.Binding.SessionHash = r.Digest("other-job")
			case "launch":
				copy.Binding.LaunchID = strings.Repeat("c", 32)
			case "request":
				copy.RequestHash = r.Digest("old-request")
			case "uid":
				copy.UID = 1203
			case "incomplete":
				copy.Complete = false
			case "failed":
				copy.Failed = true
			}
			if hookObservationBound(bad, c) {
				t.Fatal("unknown observation accepted", kind)
			}
		})
	}
}

func TestHostileCaseCannotMixModesOrProviders(t *testing.T) {
	for _, c := range []config{{Provider: r.Codex, Hostile: "codex-shell"}, {Provider: r.Claude, Hostile: "claude-source-mcp"}} {
		if !c.validCase() {
			t.Fatal("hostile case rejected", c)
		}
	}
	for _, c := range []config{{Provider: r.Claude, Hostile: "codex-shell"}, {Provider: r.Codex, Hostile: "codex-unknown"}, {Hostile: "codex-shell", ProtocolRounds: 1}, {Provider: r.Codex, Hostile: "codex-shell", Negative: true}, {Provider: r.Codex, Hostile: "codex-shell", Refresh: true}, {Provider: r.Codex, Hostile: "codex-shell", Cancellation: "reader"}, {Purpose: "native-control-fake-only", Provider: r.Codex, Hostile: "codex-shell"}} {
		if c.validCase() {
			t.Fatal("mixed hostile case accepted", c)
		}
	}
}

// Captured from pinned Codex 0.163.0 and its code host inside the native VM.
func TestCapturedCodeModeProbeIsContainedAndMutationsAreNot(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-code-probe.json")
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]string
	if json.Unmarshal(raw, &probe) != nil || !probeContained(probe) {
		t.Fatal("captured contained probe rejected")
	}
	// A present network or compute global is a capability even when sampled
	// calls throw.
	// A new global outside the measured set is an unreviewed capability.
	for _, extra := range []string{"readFile", "connect"} {
		changed := map[string]string{}
		for k, v := range probe {
			changed[k] = v
		}
		changed["globals"] = probe["globals"] + "," + extra
		if probeContained(changed) {
			t.Fatal("unrecognized code-host global accepted", extra)
		}
	}
	for key, value := range map[string]string{"typeof:fetch": "function", "typeof:WebAssembly": "object", "typeof:SharedArrayBuffer": "function", "typeof:navigator": "object", "typeof:require": "function", "typeof:process": "object", "import:node:fs": "loaded:12", "fetch:http://10.0.0.1/": "status:200", "tool:shell": "function", "tools": "mcp__sfl_source__read,mcp__sfl_source__search,shell", "env": "PATH", "image:profile": "returned:undefined", "audio:file-url": "returned:undefined", "audio:lan-url": `returned:"x"`, "load:profile": `returned:"secret"`} {
		changed := map[string]string{}
		for k, v := range probe {
			changed[k] = v
		}
		changed[key] = value
		if probeContained(changed) {
			t.Fatal("capability accepted", key, value)
		}
	}
	// Every probe key must be present, including both audio URL attempts,
	// and no unknown key may replace one.
	for _, drop := range []string{"import:fs", "audio:file-url", "audio:lan-url", "image:lan-url", "globals"} {
		missing := map[string]string{}
		for k, v := range probe {
			if k != drop {
				missing[k] = v
			}
		}
		if probeContained(missing) {
			t.Fatal("incomplete probe accepted", drop)
		}
		missing["audio:other"] = "throw:x"
		if probeContained(missing) {
			t.Fatal("substituted probe key accepted", drop)
		}
	}
	if probeContained(nil) {
		t.Fatal("empty probe accepted")
	}
}

func TestHostileOutcomeHelpers(t *testing.T) {
	head := strings.Repeat("a", 40)
	if !hostAnchorOnly(nil, head) || !hostAnchorOnly([]string{head + ":source.go"}, head) || hostAnchorOnly([]string{head + ":inject.md"}, head) || hostAnchorOnly([]string{head + ":source.go", head + ":source.go"}, head) {
		t.Fatal("host anchor audit differs")
	}
	for err, want := range map[error]string{nil: "none", errors.Join(errors.New("x"), r.ErrWorkerStopPending): "stop-pending", errors.Join(errors.New("x"), r.ErrAuthRecoveryRequired): "auth-recovery", context.Canceled: "deadline-or-cancel", r.PhaseError{State: "invalid_output"}: "phase-invalid_output", errors.New("x"): "other"} {
		if got := errorClass(err); got != want {
			t.Fatal("error class differs", err, got)
		}
	}
	if errorText(nil) != "" || len(errorText(errors.New(strings.Repeat("x", 600)))) != 512 || errorText(errors.Join(errors.New("a"), errors.New("b"))) != "a; b" {
		t.Fatal("error diagnostic not bounded")
	}
}

// The returned profile must still hold the original synthetic tokens; any
// other nonempty payload means the client corrupted or replaced it.
func TestHostileAuthReturnRequiresOriginalSyntheticTokens(t *testing.T) {
	codex := []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"x","access_token":"sfl-fake-access","refresh_token":"sfl-fake-refresh","account_id":"sfl-fake-account"},"last_refresh":"2026-10-22T00:00:00Z"}`)
	claude := []byte(`{"claudeAiOauth":{"accessToken":"sfl-fake-access","refreshToken":"sfl-fake-refresh","expiresAt":4102444800000}}`)
	if !syntheticTokensReturned(r.Codex, codex) || !syntheticTokensReturned(r.Claude, claude) {
		t.Fatal("original synthetic profile rejected")
	}
	for provider, bad := range map[r.Provider][][]byte{
		r.Codex:  {[]byte("{}"), []byte("x"), []byte(`{"auth_mode":"apikey","tokens":{"access_token":"sfl-fake-access","refresh_token":"sfl-fake-refresh"}}`), codex[:40], claude},
		r.Claude: {[]byte("{}"), []byte("truncated"), []byte(`{"claudeAiOauth":{"accessToken":"other","refreshToken":"sfl-fake-refresh"}}`), codex},
	} {
		for _, raw := range bad {
			if syntheticTokensReturned(provider, raw) {
				t.Fatal("corrupt or replaced profile accepted", provider, string(raw))
			}
		}
	}
}

// A guard case passes only on the supervisor's response-policy denial, not on
// a failed session caused by transport, read or size errors.
func TestGuardCasesRequireResponsePolicyDenial(t *testing.T) {
	denied := &r.ProviderEvidence{Access: r.ProviderAccess{Failed: true, Forwarded: 3, Responses: 2, ResponseDenials: 1}}
	if !responsePolicyDenied(denied) {
		t.Fatal("response-policy denial rejected")
	}
	for name, access := range map[string]r.ProviderAccess{
		"transport-or-read-failure": {Failed: true, Forwarded: 1},
		"not-failed":                {Forwarded: 1, ResponseDenials: 1},
		"second-unanswered-forward": {Failed: true, Forwarded: 3, Responses: 1, ResponseDenials: 1},
		"two-denials":               {Failed: true, Forwarded: 2, ResponseDenials: 2},
	} {
		if responsePolicyDenied(&r.ProviderEvidence{Access: access}) {
			t.Fatal("non-policy failure accepted as response denial", name)
		}
	}
	if responsePolicyDenied(nil) {
		t.Fatal("missing evidence accepted")
	}
}

// The codex-metadata case passes its refused variant only on the supervisor's
// own request-policy denial of the single inference, with every other reserved
// request forwarded and answered. The measured pre-#276 outcome, a forwarded
// widened request whose response the response policy refused, does not count.
func TestMetadataCaseRequiresRequestPolicyDenial(t *testing.T) {
	// Five metadata requests and the local negotiation, as measured, then the
	// refused inference.
	denied := r.ProviderAccess{Failed: true, Requests: 7, InferenceRequests: 1, MetadataRequests: 5, NegotiationRequests: 1, NegotiationResponses: 1, Forwarded: 5, Responses: 5, RequestDenials: 1}
	if !requestPolicyDenied(&r.ProviderEvidence{Access: denied}) {
		t.Fatal("request-policy denial rejected")
	}
	change := func(edit func(*r.ProviderAccess)) r.ProviderAccess { a := denied; edit(&a); return a }
	for name, access := range map[string]r.ProviderAccess{
		"measured-response-denial": {Failed: true, Requests: 7, InferenceRequests: 1, MetadataRequests: 5, NegotiationRequests: 1, NegotiationResponses: 1, Forwarded: 6, Responses: 5, ResponseDenials: 1},
		"not-failed":               change(func(a *r.ProviderAccess) { a.Failed = false }),
		"no-denial":                change(func(a *r.ProviderAccess) { a.RequestDenials = 0 }),
		"two-denials":              change(func(a *r.ProviderAccess) { a.RequestDenials, a.InferenceRequests, a.Requests = 2, 2, 8 }),
		"second-inference":         change(func(a *r.ProviderAccess) { a.InferenceRequests, a.Requests, a.Forwarded, a.Responses = 2, 8, 6, 6 }),
		"inference-forwarded":      change(func(a *r.ProviderAccess) { a.Forwarded, a.Responses = 6, 6 }),
		"unanswered-forward":       change(func(a *r.ProviderAccess) { a.Responses = 4 }),
		"also-response-denial":     change(func(a *r.ProviderAccess) { a.ResponseDenials = 1 }),
		"also-route-denial":        change(func(a *r.ProviderAccess) { a.RouteDenials = 1 }),
	} {
		if requestPolicyDenied(&r.ProviderEvidence{Access: access}) {
			t.Fatal("other failure accepted as request denial", name)
		}
	}
	if requestPolicyDenied(nil) {
		t.Fatal("missing evidence accepted")
	}
}

// An exhaustion case that returns auth normally must still return the original
// synthetic tokens; a client that rewrote them cannot pass as returned.
func TestExhaustionReturnedAuthRequiresOriginalSyntheticTokens(t *testing.T) {
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		for _, tc := range []struct {
			name    string
			profile []byte
			want    bool
		}{
			{"original", f.Seed(provider), true},
			{"rewritten", []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"rewritten","refresh_token":"rewritten"},"claudeAiOauth":{"accessToken":"rewritten","refreshToken":"rewritten"}}`), false},
			{"corrupt", []byte("corrupt"), false},
		} {
			dir := filepath.Join(t.TempDir(), "auth")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			store, err := r.NewAuthStore(provider, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Refresh(context.Background(), func([]byte) ([]byte, error) { return tc.profile, nil }); err != nil {
				t.Fatal(err)
			}
			returned := hostileAudit{AuthExported: true, AuthReturned: true}
			if got := exhaustionAuthHeld(provider, dir, store, returned, r.PhaseError{State: "invalid_output"}); got != tc.want {
				t.Fatal("returned auth disposition differs", provider, tc.name, got)
			}
			// Unfenced auth never agrees with a recovery error or an audit
			// that did not return it.
			if exhaustionAuthHeld(provider, dir, store, returned, r.ErrAuthRecoveryRequired) || exhaustionAuthHeld(provider, dir, store, hostileAudit{AuthExported: true}, nil) {
				t.Fatal("unfenced auth accepted as fenced", provider, tc.name)
			}
		}
	}
}
