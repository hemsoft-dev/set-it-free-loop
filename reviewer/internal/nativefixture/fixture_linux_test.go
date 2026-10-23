//go:build linux

package nativefixture

import (
	"encoding/json"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeBackendAcceptsCapturedPeerRound(t *testing.T) {
	limits := r.DefaultLimits()
	snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	var peers []r.Report
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		s, err := r.NewSession("native-peer-test", snap, r.Digest("config"), provider, 1, source, nil, nil, limits)
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, r.Report{JobID: s.JobID, PolicyVersion: s.PolicyVersion, PacketHash: s.PacketHash, Provider: provider, Round: 1, SnapshotHash: s.SnapshotHash, ConfigHash: s.ConfigHash, Findings: []r.Finding{{ID: string(provider) + ":fixture", Title: "Synthetic finding", Body: "Protocol fixture only.", Severity: 2, Anchor: r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: head}}}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}})
	}
	var second []r.Report
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		s, err := r.NewSession("native-peer-test", snap, r.Digest("config"), provider, 2, source, peers, nil, limits)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = New(s, false); err != nil {
			t.Fatal("native producer refuses the captured adversarial peer round", provider, err)
		}
		report := sourceReport(s)
		if r.ValidateReport(report, s) != nil || len(report.Findings) != 0 || len(report.Judgments) != 1 || strings.HasPrefix(report.Judgments[0].FindingID, string(provider)+":") {
			t.Fatal("peer round lost provenance or judged its own finding", provider, report)
		}
		second = append(second, report)
	}
	peers = append(peers, second...)
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		s, err := r.NewSession("native-peer-test", snap, r.Digest("config"), provider, 3, source, peers, []string{"claude:fixture", "codex:fixture"}, limits)
		if err != nil || r.ValidateReport(sourceReport(s), s) != nil || len(sourceReport(s).Judgments) != 2 {
			t.Fatal("focused final round lost required peer/own dispositions", provider, err)
		}
	}
}

func TestNativeSyntheticSeedDoesNotTriggerAgeBasedRefreshAtStartup(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	raw := Seed(r.Codex)
	defer clear(raw)
	var seed struct {
		LastRefresh string `json:"last_refresh"`
	}
	if json.Unmarshal(raw, &seed) != nil {
		t.Fatal("synthetic seed malformed")
	}
	refreshed, err := time.Parse(time.RFC3339, seed.LastRefresh)
	if err != nil || refreshed.Before(before) || refreshed.After(time.Now().UTC().Add(time.Second)) {
		t.Fatal("source fixture would start with an aged synthetic seed")
	}
}

func TestNativeMetadataReceiptRequiresEveryCapturedRoute(t *testing.T) {
	for _, routes := range []map[string]int{
		{"/v1/models": 1, "/backend-api/wham/accounts/check": 2, "/backend-api/wham/settings/user": 1},
		{"/v1/models": 5},
		{"/v1/models": 1, "/backend-api/wham/accounts/check": 1, "/backend-api/wham/settings/user": 2},
	} {
		b := &Backend{Codex: &codexBackend{metadataRoutes: routes}}
		want := len(routes) == 3 && routes["/backend-api/wham/accounts/check"] == 2
		if b.Counts().MetadataCaptured != want {
			t.Fatal("metadata receipt replaced a mandatory route", routes)
		}
	}
}

func TestTelemetryCannotCountAsAllowedMetadata(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		b := &codexBackend{logHTTP: func(any) {}}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest(method, "/backend-api/codex/analytics-events/events", nil))
		if w.Code != 400 || b.telemetry != 1 || b.failed != 1 || b.metadata != 0 || b.calls != 0 {
			t.Fatal("telemetry accepted or hidden in metadata", method, w.Code, b)
		}
	}
}

// Sanitized output fragments produced by pinned Codex against the fake backend.
func TestCapturedNativeCodeModeSourceResults(t *testing.T) {
	data, err := os.ReadFile("../../cmd/codexfixture/testdata/native-source-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured []struct {
		ID     string `json:"id"`
		Output any    `json:"output"`
	}
	if json.Unmarshal(data, &captured) != nil || len(captured) != 2 {
		t.Fatal("invalid native fixture")
	}
	for _, item := range captured {
		if !verifiedSourceOutput(item.Output, item.ID, head) {
			t.Fatal("actual source output rejected", item.ID)
		}
	}
	for _, kind := range []string{"swapped", "error", "duplicate", "wrong-revision"} {
		t.Run(kind, func(t *testing.T) {
			var values []struct {
				ID     string `json:"id"`
				Output any    `json:"output"`
			}
			_ = json.Unmarshal(data, &values)
			output := values[1].Output.([]any)
			if kind == "swapped" {
				output = values[0].Output.([]any)
			} else {
				index := len(output) - 1
				part := output[index].(map[string]any)
				var wrapper map[string]any
				_ = json.Unmarshal([]byte(part["text"].(string)), &wrapper)
				switch kind {
				case "error":
					wrapper["isError"] = true
				case "duplicate":
					output = append(output, part)
				case "wrong-revision":
					wrapper["content"].([]any)[0].(map[string]any)["text"] = `[{"path":"source.go","line":1,"quote":"immutable source evidence","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`
				}
				encoded, _ := json.Marshal(wrapper)
				part["text"] = string(encoded)
			}
			if verifiedSourceOutput(output, "fixture_search", head) {
				t.Fatal("mismatched result accepted", kind)
			}
		})
	}
}
func TestSourceOutputIDMustBeOneTopLevelNativeItem(t *testing.T) {
	item := map[string]any{"type": "custom_tool_call_output", "call_id": "fixture_read", "output": "fixture"}
	if _, ok := outputFor(map[string]any{"input": []any{item}}, "fixture_read"); !ok {
		t.Fatal("native output not found")
	}
	if _, ok := outputFor(map[string]any{"input": []any{item, item}}, "fixture_read"); ok {
		t.Fatal("duplicate ID accepted")
	}
	if _, ok := outputFor(map[string]any{"input": []any{map[string]any{"type": "message", "content": []any{item}}}}, "fixture_read"); ok {
		t.Fatal("nested imitation accepted")
	}
}
func TestNativeSourceResultsRemainDistinctAndNonError(t *testing.T) {
	data, err := os.ReadFile("../../cmd/claudefixture/testdata/native-tool-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured []map[string]any
	if json.Unmarshal(data, &captured) != nil || len(captured) != 2 {
		t.Fatal("invalid captured fixture")
	}
	wrap := func(result map[string]any) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{result}}}}
	}
	for _, result := range captured {
		if !claudeResult(wrap(result), result["tool_use_id"].(string), head) {
			t.Fatal("actual native source result rejected")
		}
	}
	for _, kind := range []string{"read-as-search", "search-as-read", "error", "wrong-error-type", "wrong-revision", "duplicate", "wrong-role"} {
		t.Run(kind, func(t *testing.T) {
			var values []map[string]any
			_ = json.Unmarshal(data, &values)
			result := values[1]
			switch kind {
			case "read-as-search":
				result["content"] = values[0]["content"]
			case "search-as-read":
				result["tool_use_id"] = "sfl_fixture_read_1"
			case "error":
				result["is_error"] = true
			case "wrong-error-type":
				result["is_error"] = "false"
			case "wrong-revision":
				blocks := result["content"].([]any)
				block := blocks[0].(map[string]any)
				block["text"] = `[{"path":"source.go","line":1,"quote":"immutable source evidence","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`
			}
			body := wrap(result)
			if kind == "duplicate" {
				body["messages"].([]any)[0].(map[string]any)["content"] = []any{result, result}
			}
			if kind == "wrong-role" {
				body["messages"].([]any)[0].(map[string]any)["role"] = "assistant"
			}
			if claudeResult(body, result["tool_use_id"].(string), head) {
				t.Fatal("mismatched native result accepted")
			}
		})
	}
}

// A source VM acquisition yields a real commit, not the fixed fixture head.
// NewForSnapshot keeps the canned protocol and source text but binds every
// tool call, verification and finding anchor to the captured revision.
func TestNativeBackendFollowsCapturedSnapshotRevision(t *testing.T) {
	captured := strings.Repeat("d", 40)
	snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 2, Head: captured, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		s, err := r.NewSession("native-snapshot-test", snap, r.Digest("config"), provider, 1, source, nil, nil, r.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := New(s, false); err == nil {
			t.Fatal("fixed-head constructor accepted a foreign revision", provider)
		}
		backend, policy, err := NewForSnapshot(s)
		if err != nil || r.Digest(policy) != r.Digest(Policy(s)) || policy.Provider != provider {
			t.Fatal("snapshot backend policy differs", provider, err)
		}
		report := sourceReport(s)
		if r.ValidateReport(report, s) != nil || len(report.Findings) != 1 || report.Findings[0].Anchor.Revision != captured {
			t.Fatal("finding not anchored at the captured revision", provider, report)
		}
		if provider == r.Claude {
			body, _ := json.Marshal(map[string]any{"model": s.Identity.Model, "tools": []any{map[string]any{"name": "mcp__sfl-source__read"}, map[string]any{"name": "mcp__sfl-source__search"}, map[string]any{"name": "StructuredOutput"}}})
			q := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(string(body)))
			q.Header.Set("Authorization", "Bearer sfl-fake-access")
			w := httptest.NewRecorder()
			backend.ServeHTTP(w, q)
			if w.Code != 200 || !strings.Contains(w.Body.String(), captured) || strings.Contains(w.Body.String(), head) {
				t.Fatal("Claude read did not target the captured revision", w.Code)
			}
		}
		matches, _ := json.Marshal([]r.Anchor{{Path: "source.go", Line: 1, Quote: strings.TrimSpace(source), Revision: captured}})
		block := map[string]any{"type": "text", "text": string(matches)}
		result := map[string]any{"type": "tool_result", "tool_use_id": "sfl_fixture_search_1", "content": []any{block}}
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{result}}}}
		if !claudeResult(body, "sfl_fixture_search_1", captured) || claudeResult(body, "sfl_fixture_search_1", head) {
			t.Fatal("search verification ignored the bound revision")
		}
	}
	invalid := r.Session{}
	if _, _, err := NewForSnapshot(invalid); err == nil {
		t.Fatal("invalid session accepted")
	}
}
