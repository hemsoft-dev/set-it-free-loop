//go:build linux

package nativefixture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func hostileSession(t *testing.T, provider r.Provider) r.Session {
	t.Helper()
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("native-hostile-test", snapshot, r.Digest("config"), provider, 1, source, nil, nil, r.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type handlerTransport struct {
	handler http.Handler
	calls   int
}

func (t *handlerTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	t.calls++
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, q)
	return w.Result(), nil
}

// inferenceRequest is the minimal pinned-client request shape the fake backend
// accepts: captured model and the fixed source tool inventory.
func inferenceRequest(s r.Session, extra string) *http.Request {
	path, body := "/v1/messages?beta=true", `{"model":"`+s.Identity.Model+`","tools":[{"name":"mcp__sfl-source__read"},{"name":"mcp__sfl-source__search"},{"name":"StructuredOutput"}],"messages":[]`+extra+`}`
	if s.Identity.Provider == r.Codex {
		path, body = "/v1/responses", `{"model":"`+s.Identity.Model+`","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"name":"exec","description":"mcp__sfl_source__read mcp__sfl_source__search"},{"name":"wait"}]}]}]`+extra+`}`
	}
	q := httptest.NewRequest("POST", path, strings.NewReader(body))
	q.Host = "fixture.provider.example"
	q.Header.Set("Authorization", "Bearer sfl-fake-access")
	q.Header.Set("Content-Type", "application/json")
	return q
}

func TestHostileCasesAreUniqueAndProviderBound(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range HostileCases() {
		if seen[c.Name] || !strings.HasPrefix(c.Name, string(c.Provider)+"-") {
			t.Fatal("ambiguous hostile case", c)
		}
		seen[c.Name] = true
		if _, ok := LookupHostile(c.Name, c.Provider); !ok {
			t.Fatal("declared case not found", c)
		}
		other := r.Codex
		if c.Provider == r.Codex {
			other = r.Claude
		}
		if _, ok := LookupHostile(c.Name, other); ok {
			t.Fatal("case accepted for the other provider", c)
		}
		if c.Kind == HostileGuard && c.Provider == r.Claude && len(claudeGuardBlocks(c.Name)) == 0 || c.Kind == HostileGuard && c.Provider == r.Codex && codexGuardItem(c.Name) == nil {
			t.Fatal("guard case has no hostile item", c)
		}
	}
	if _, ok := LookupHostile("codex-unknown", r.Codex); ok {
		t.Fatal("unknown case accepted")
	}
}

// Every guard case must be refused by the actual host response policy before a
// byte reaches the client. A source turn through the same harness is admitted,
// so the refusal is not an artifact of the test transport.
func TestHostileGuardTurnsAreRefusedByTheHostResponsePolicy(t *testing.T) {
	canaries, err := NewCanaries()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []r.Provider{r.Claude, r.Codex} {
		s := hostileSession(t, provider)
		control, policy, err := New(s, false)
		if err != nil {
			t.Fatal(err)
		}
		transport := &handlerTransport{handler: control}
		guard, err := r.NewProviderEgress(context.Background(), s, policy, transport)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		guard.ServeHTTP(w, inferenceRequest(s, ""))
		if w.Code != 200 || guard.Snapshot().Failed {
			t.Fatal("source turn refused by test harness", provider, w.Code, w.Body.String())
		}
		guard.Close(context.Background())
		for _, c := range HostileCases() {
			if c.Provider != provider || c.Kind != HostileGuard && !(c.Name == "codex-metadata") {
				continue
			}
			t.Run(c.Name, func(t *testing.T) {
				backend, policy, err := NewHostile(s, c.Name, canaries)
				if err != nil {
					t.Fatal(err)
				}
				transport := &handlerTransport{handler: backend}
				guard, err := r.NewProviderEgress(context.Background(), s, policy, transport)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close(context.Background())
				q := inferenceRequest(s, "")
				if c.Name == "codex-metadata" {
					// Direct tools re-advertised by injected metadata.
					q = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+s.Identity.Model+`","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"read_mcp_resource"}],"input":[]}`))
					q.Host = "fixture.provider.example"
					q.Header.Set("Authorization", "Bearer sfl-fake-access")
				}
				w := httptest.NewRecorder()
				guard.ServeHTTP(w, q)
				observed := backend.Hostile()
				if w.Code != 400 || !strings.Contains(w.Body.String(), "provider tool response denied") || !guard.Snapshot().Failed || transport.calls != 1 || observed.Turns != 1 {
					t.Fatal("hostile turn crossed the host response policy", w.Code, w.Body.String(), observed)
				}
				if c.Name == "codex-metadata" && (CodexInventoryAllowed(observed.Inventory) || len(observed.Problems) == 0) {
					t.Fatal("re-advertised direct tools were not recorded", observed)
				}
			})
		}
	}
}

func TestHostileStoreKeepsCanariesBehindTheReader(t *testing.T) {
	canaries, err := NewCanaries()
	if err != nil {
		t.Fatal(err)
	}
	store := NewHostileStore(canaries)
	s := hostileSession(t, r.Claude)
	reader, err := r.NewReader(store, s.Snapshot, HostilePaths(), s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	secrets := canaries.Secrets(r.Claude)
	for _, call := range hostileSourceCalls() {
		var text string
		var err error
		if call.Tool == "read" {
			text, err = reader.Read(context.Background(), call.Input["revision"].(string), call.Input["path"].(string))
		} else {
			var anchors []r.Anchor
			anchors, err = reader.Search(context.Background(), call.Input["revision"].(string), call.Input["needle"].(string), 5)
			raw, _ := json.Marshal(anchors)
			text = string(raw)
		}
		if err != nil {
			text = "Source request denied"
		}
		got := classifySource(call, err != nil, text, secrets)
		if want := map[bool]string{true: "allowed", false: "denied"}[call.Allowed]; call.Label == "read-injection" && got != "allowed-as-data" || call.Label != "read-injection" && got != want {
			t.Fatal("reader exposed hostile object", call.Label, got)
		}
	}
	for _, request := range store.Requests() {
		if request != head+":source.go" && request != head+":inject.md" && request != head+":link.go" {
			t.Fatal("reader asked the store for a forbidden object", request)
		}
	}
	// The canaries are real: an unguarded store read would disclose them.
	foreign, _ := store.Read(context.Background(), strings.Repeat("d", 40), "source.go", 1<<20)
	hidden, _ := store.Read(context.Background(), head, "../auth.json", 1<<20)
	link, _ := store.Read(context.Background(), head, "link.go", 1<<20)
	if CanaryHits(foreign.Data, secrets) != 1 || CanaryHits(hidden.Data, secrets) != 1 || CanaryHits(link.Data, secrets) != 1 || link.Mode != "120000" {
		t.Fatal("canary placement is vacuous")
	}
}

func TestHostileSourceClassificationFailsClosed(t *testing.T) {
	canaries, _ := NewCanaries()
	secrets := canaries.Secrets(r.Codex)
	calls := hostileSourceCalls()
	results := map[string]string{}
	for _, call := range calls {
		switch call.Label {
		case "read-source":
			results[call.Label] = classifySource(call, false, source, secrets)
		case "read-injection":
			results[call.Label] = classifySource(call, false, hostileInjection, secrets)
		default:
			results[call.Label] = classifySource(call, true, "throw: Source request denied", secrets)
		}
	}
	if !HostileSourcePassed(results) {
		t.Fatal("expected outcome rejected", results)
	}
	traversal := calls[2]
	for _, sample := range []struct {
		isError bool
		text    string
	}{{false, source}, {true, "Source request active"}, {true, "Source request denied " + canaries.Hidden}, {false, ""}} {
		changed := map[string]string{}
		for k, v := range results {
			changed[k] = v
		}
		changed[traversal.Label] = classifySource(traversal, sample.isError, sample.text, secrets)
		if HostileSourcePassed(changed) {
			t.Fatal("hostile result accepted", sample)
		}
	}
	delete(results, "symlink")
	if HostileSourcePassed(results) {
		t.Fatal("missing hostile call accepted")
	}
	if classifySource(calls[0], false, source+"x", secrets) == "allowed" || classifySource(calls[1], true, hostileInjection, secrets) != "unexpected-error" {
		t.Fatal("allowed result not exact")
	}
}

func TestCanaryScanCountsEncodedValuesAndIgnoresOnlyAuthorization(t *testing.T) {
	canaries, _ := NewCanaries()
	secrets := canaries.Secrets(r.Codex)
	for prefix := 0; prefix < 6; prefix++ {
		for suffix := 0; suffix < 4; suffix++ {
			for _, secret := range []string{canaries.Hidden, "sfl-fake-refresh"} {
				plain := strings.Repeat("x", prefix) + `{"token":"` + secret + `"}` + strings.Repeat("y", suffix)
				encoded := base64.StdEncoding.EncodeToString([]byte(plain))
				if CanaryHits([]byte("data:image/png;base64,"+encoded), secrets) == 0 {
					t.Fatal("encoded disclosure missed", prefix, suffix)
				}
			}
		}
	}
	b := &Backend{secrets: secrets}
	q := httptest.NewRequest("POST", "/v1/responses", nil)
	q.Header.Set("Authorization", "Bearer sfl-fake-access")
	b.scan(q, []byte(`{"input":"clean"}`))
	if b.CanaryRequestHits() != 0 {
		t.Fatal("provider Authorization counted as disclosure")
	}
	q.Header.Set("X-Codex-Turn-Metadata", "sfl-fake-access")
	b.scan(q, []byte(canaries.Host))
	if b.CanaryRequestHits() < 2 {
		t.Fatal("header or body disclosure missed")
	}
	// Only the exact expected bearer is exempt; a refresh token or canary
	// carried in Authorization is a disclosure.
	before := b.CanaryRequestHits()
	for _, value := range []string{"Bearer sfl-fake-refresh", "Bearer " + canaries.Hidden, "Basic " + base64.StdEncoding.EncodeToString([]byte("x:sfl-fake-access"))} {
		leak := httptest.NewRequest("GET", "/v1/models", nil)
		leak.Header.Set("Authorization", value)
		b.scan(leak, nil)
	}
	if b.CanaryRequestHits() < before+3 {
		t.Fatal("Authorization disclosure missed", b.CanaryRequestHits()-before)
	}
}

func TestHostileBackendScansEveryRequestBeforeRouting(t *testing.T) {
	canaries, _ := NewCanaries()
	for _, provider := range []r.Provider{r.Claude, r.Codex} {
		s := hostileSession(t, provider)
		name := "claude-bash"
		if provider == r.Codex {
			name = "codex-code-probe"
		}
		backend, _, err := NewHostile(s, name, canaries)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		backend.ServeHTTP(w, inferenceRequest(s, `,"leak":"`+canaries.Foreign+`"`))
		observed := backend.Hostile()
		if backend.CanaryRequestHits() != 1 || observed.Case != name || observed.Turns != 1 || w.Code != 200 {
			t.Fatal("request canary or observation lost", provider, w.Code, observed)
		}
	}
}

func TestCodexInventoryAndMarkedOutput(t *testing.T) {
	body := map[string]any{}
	_ = json.Unmarshal([]byte(`{"tools":[{"type":"custom","name":"apply_patch"},{"type":"namespace","name":"mcp__sfl_source","tools":[{"name":"read"}]}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"name":"exec"},{"name":"wait"}]}]}]}`), &body)
	names := codexInventory(body)
	if CodexInventoryAllowed(names) || strings.Join(names, ",") != "additional:functions.exec,additional:functions.wait,tools:apply_patch,tools:mcp__sfl_source.read" {
		t.Fatal("inventory flattened incorrectly", names)
	}
	if !CodexInventoryAllowed([]string{"additional:functions.exec", "additional:functions.wait"}) {
		t.Fatal("code-mode pair refused")
	}
	output := []any{map[string]any{"type": "input_text", "text": "Script completed\nOutput:\n"}, map[string]any{"type": "input_text", "text": `{"sfl_probe":{"typeof:require":"undefined"}}`}}
	marked, ok := codexMarked(output, "sfl_probe")
	if !ok || string(marked["typeof:require"]) != `"undefined"` {
		t.Fatal("marked output not found")
	}
	if _, ok := codexMarked([]any{map[string]any{"type": "input_text", "text": `{"other":{}}`}}, "sfl_probe"); ok {
		t.Fatal("unmarked output accepted")
	}
	if mediaInputs(map[string]any{"input": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}}) == 0 {
		t.Fatal("media input missed")
	}
}

func TestHostileMetadataInjectsDirectToolsOnlyForTheModelCase(t *testing.T) {
	canaries, _ := NewCanaries()
	s := hostileSession(t, r.Codex)
	for _, name := range []string{"codex-metadata", "codex-settings"} {
		backend, _, err := NewHostile(s, name, canaries)
		if err != nil {
			t.Fatal(err)
		}
		q := httptest.NewRequest("GET", "/v1/models?client_version=0.163.0", nil)
		w := httptest.NewRecorder()
		backend.ServeHTTP(w, q)
		var models struct {
			Models []map[string]any `json:"models"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &models)
		injected := len(models.Models) == 1 && models.Models[0]["slug"] == s.Identity.Model && models.Models[0]["tool_mode"] == "direct"
		if injected != (name == "codex-metadata") {
			t.Fatal("model metadata injection selection differs", name, w.Body.String())
		}
		w = httptest.NewRecorder()
		backend.ServeHTTP(w, httptest.NewRequest("GET", "/backend-api/wham/settings/user", nil))
		if !bytes.Contains(w.Body.Bytes(), []byte(`"mcp_servers"`)) {
			t.Fatal("settings injection absent", name)
		}
		// Delivery is counted, so a client that skips a route cannot pass.
		observed := backend.Hostile()
		if observed.InjectedSettings != 1 || observed.InjectedModels != map[bool]int{true: 1, false: 0}[name == "codex-metadata"] {
			t.Fatal("injected delivery not counted", name, observed.InjectedModels, observed.InjectedSettings)
		}
	}
}

// Forbidden egress through the captured fixture policy: private, link-local and
// non-allowlisted public authorities are refused before any transport call.
func TestHostileEgressDestinationsNeverReachTheTransport(t *testing.T) {
	for _, provider := range []r.Provider{r.Claude, r.Codex} {
		s := hostileSession(t, provider)
		_, policy, err := New(s, false)
		if err != nil {
			t.Fatal(err)
		}
		transport := &handlerTransport{handler: http.NotFoundHandler()}
		guard, err := r.NewProviderEgress(context.Background(), s, policy, transport)
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"10.0.0.1", "192.168.1.1:443", "169.254.169.254", "[fd00::1]", "example.com", "fixture.provider.example.evil.test", "localhost"} {
			q := inferenceRequest(s, "")
			q.Host = host
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, q)
			if w.Code != http.StatusForbidden {
				t.Fatal("forbidden destination admitted", provider, host, w.Code)
			}
		}
		access, _ := guard.Close(context.Background())
		// Ingress refusals are counted where they happen, not downstream.
		if transport.calls != 0 || access.Forwarded != 0 || !access.Failed || access.RouteDenials != 7 {
			t.Fatal("forbidden destination reached the transport", provider, transport.calls, access)
		}
	}
}
