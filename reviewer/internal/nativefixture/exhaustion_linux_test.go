//go:build linux

package nativefixture

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func TestExhaustionCasesAreSeparateAndProviderBound(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range HostileCases() {
		seen[c.Name] = true
		if c.Kind == HostileExhaust {
			t.Fatal("exhaustion case leaked into the capability matrix", c)
		}
	}
	for _, c := range ExhaustionCases() {
		if seen[c.Name] || c.Kind != HostileExhaust || !strings.HasPrefix(c.Name, string(c.Provider)+"-") {
			t.Fatal("ambiguous exhaustion case", c)
		}
		seen[c.Name] = true
		if got, ok := LookupHostile(c.Name, c.Provider); !ok || got != c {
			t.Fatal("declared exhaustion case not found", c)
		}
		other := r.Codex
		if c.Provider == r.Codex {
			other = r.Claude
		}
		if _, ok := LookupHostile(c.Name, other); ok {
			t.Fatal("case accepted for the other provider", c)
		}
	}
}

// The flood must actually pass the per-response bound, and the host guard,
// not the fixture, must refuse it before a byte reaches the client.
func TestResponseFloodIsRefusedByTheEgressBound(t *testing.T) {
	canaries, _ := NewCanaries()
	for _, provider := range []r.Provider{r.Claude, r.Codex} {
		s := hostileSession(t, provider)
		backend, policy, err := NewHostile(s, string(provider)+"-response-flood", canaries)
		if err != nil {
			t.Fatal(err)
		}
		guard, err := r.NewProviderEgress(context.Background(), s, policy, &handlerTransport{handler: backend})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		guard.ServeHTTP(w, inferenceRequest(s, ""))
		access, _ := guard.Close(context.Background())
		observed := backend.Hostile()
		if w.Code != 502 || w.Body.Len() > 256 || !access.Failed || !access.Exhausted || access.Responses != 0 || access.ResponseBytes != policy.Limits.ResponseBytes+1 || observed.FloodBytes <= policy.Limits.ResponseBytes || len(observed.Problems) != 0 {
			t.Fatal("flood crossed the egress bound", provider, w.Code, access, observed.FloodBytes)
		}
	}
}

// A stalled model is released by the guard's own cancellation, and receipts
// stay readable while it waits.
func TestStallIsReleasedByGuardCancellation(t *testing.T) {
	canaries, _ := NewCanaries()
	for _, provider := range []r.Provider{r.Claude, r.Codex} {
		s := hostileSession(t, provider)
		backend, policy, err := NewHostile(s, string(provider)+"-stall", canaries)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		guard, err := r.NewProviderEgress(ctx, s, policy, &handlerTransport{handler: backend})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			guard.ServeHTTP(httptest.NewRecorder(), inferenceRequest(s, ""))
		}()
		deadline := time.Now().Add(5 * time.Second)
		for backend.Hostile().Results["stall"] != "entered" {
			if time.Now().After(deadline) {
				t.Fatal("stall never entered", provider)
			}
			time.Sleep(time.Millisecond)
		}
		_ = backend.Counts() // must not wait for the stalled handler
		cancelled := time.Now()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("guard cancellation did not release the stall", provider)
		}
		access, _ := guard.Close(context.Background())
		// The upstream's own cancellation time lets the runner tell the phase
		// deadline from the fixture's outer watchdog.
		if at := backend.Hostile().StallCancelled; at.Before(cancelled) || at.After(time.Now()) {
			t.Fatal("stall cancellation time not recorded", provider, at)
		}
		if backend.Hostile().Results["stall"] != "cancelled-by-guard" || !access.Failed || access.Forwarded != 1 || access.Responses != 0 {
			t.Fatal("stall outcome differs", provider, backend.Hostile().Results, access)
		}
	}
}

// The fake model never ends a turn flood itself; only a bound can.
func TestTurnFloodKeepsRequestingSourceUntilTheFixtureCap(t *testing.T) {
	canaries, _ := NewCanaries()
	for _, provider := range []r.Provider{r.Claude, r.Codex} {
		s := hostileSession(t, provider)
		backend, _, err := NewHostile(s, string(provider)+"-turn-flood", canaries)
		if err != nil {
			t.Fatal(err)
		}
		for turn := 1; turn <= 30; turn++ {
			w := httptest.NewRecorder()
			backend.ServeHTTP(w, inferenceRequest(s, ""))
			want := fmt.Sprintf("h_flood_%d", turn-1)
			if w.Code != 200 || !strings.Contains(w.Body.String(), want) || !strings.Contains(w.Body.String(), "source.go") {
				t.Fatal("turn flood stopped early", provider, turn, w.Code)
			}
		}
		if got := backend.Hostile(); got.Turns != 30 || len(got.Problems) != 0 {
			t.Fatal("turn flood observation differs", provider, got)
		}
	}
}

func TestCodexExhaustionProgramsTargetTheirBounds(t *testing.T) {
	canaries, _ := NewCanaries()
	s := hostileSession(t, r.Codex)
	want := map[string]string{
		"codex-source-flood":      fmt.Sprintf("%d;i++", SourceFloodCalls),
		"codex-source-oversize":   `path:"big.go"`,
		"codex-code-memory":       "new Uint8Array(32",
		"codex-code-output-flood": "repeat(9",
		"codex-code-spin":         "for(;;)",
	}
	for name, fragment := range want {
		backend, _, err := NewHostile(s, name, canaries)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		backend.ServeHTTP(w, inferenceRequest(s, ""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"exec"`) || !strings.Contains(w.Body.String(), strings.ReplaceAll(fragment, `"`, `\"`)) {
			t.Fatal("exhaustion program differs", name, w.Body.String())
		}
		// A missing exec result is recorded, and the model still reports so
		// the host, not the fixture, must refuse the exchange.
		w = httptest.NewRecorder()
		backend.ServeHTTP(w, inferenceRequest(s, ""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "msg_fixture") || backend.Hostile().Results["exec-output"] != "missing" {
			t.Fatal("second exhaustion turn differs", name, w.Code)
		}
	}
	if SourceFloodCalls <= r.DefaultLimits().ReaderCalls || FloodBytes <= r.DefaultEgressLimits(s).ResponseBytes || 2*BigObjectBytes <= r.DefaultLimits().ReaderBytes || BigObjectBytes > r.DefaultLimits().ReaderBytes {
		t.Fatal("exhaustion constants no longer exceed their bounds")
	}
}

func TestBigObjectClassificationAndStore(t *testing.T) {
	canaries, _ := NewCanaries()
	secrets := canaries.Secrets(r.Claude)
	store := NewHostileStore(canaries)
	object, err := store.Read(context.Background(), head, "big.go", 1)
	if err != nil || len(object.Data) != BigObjectBytes || object.Mode != "100644" {
		t.Fatal("big object must ignore the store limit", err, len(object.Data))
	}
	for text, want := range map[string]string{bigText: "full", bigText[:100]: "prefix:100", "Error: result exceeds maximum allowed tokens": "other:44: Error: result exceeds maximum allowed tokens", canaries.Hidden: "canary-disclosed"} {
		if got := classifyBig(false, text, secrets); got != want {
			t.Fatal("big result classification differs", got, want)
		}
	}
	if classifyBig(true, "Source request denied", secrets) != "denied" || !strings.HasPrefix(classifyBig(true, "Source request active", secrets), "error-other") {
		t.Fatal("big denial classification differs")
	}
}
