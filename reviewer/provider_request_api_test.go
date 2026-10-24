package reviewer

import (
	"encoding/json"
	"os"
	"testing"
)

// Every package test reaches the request-policy API through these accessors.
// The issue #276 fail-before procedure replaces this one file with a shim, so
// the new tests compile and run against the production code before the
// request policy existed.

const codexRequestPolicy = CodexSourceRequests

var errProviderToolRequest = ErrProviderToolRequest

const requestBytesBound = maxProviderRequestBytes
const measuredWaitParameters = codexWaitParameters

func nativeLaunchPolicies(provider Provider, p EgressPolicy) bool {
	return nativeSourcePolicies(provider, p)
}

func setRequestPolicy(p *EgressPolicy, policy string) { p.RequestPolicy = policy }
func requestPolicyOf(p EgressPolicy) string           { return p.RequestPolicy }
func requestDenials(a ProviderAccess) int             { return a.RequestDenials }
func setRequestDenials(a *ProviderAccess, n int)      { a.RequestDenials = n }
func checkProviderRequest(provider Provider, policy string, body []byte) error {
	return validateProviderRequest(provider, policy, body)
}

// withCodexRequestPolicy adds the request policy a native Codex launch requires.
func withCodexRequestPolicy(p *EgressPolicy) {
	if p.Provider == Codex {
		p.RequestPolicy = CodexSourceRequests
	}
}

// codexMeasuredRequests returns the two sanitized requests captured from the
// pinned Codex 0.163.0 code-mode client: the first turn and the third, which
// carries two exec calls and their outputs. See the provenance file.
func codexMeasuredRequests(t testing.TB) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-0.163.0-code-mode-requests.json")
	if err != nil {
		t.Fatal(err)
	}
	var requests []map[string]any
	if json.Unmarshal(raw, &requests) != nil || len(requests) != 2 {
		t.Fatal("measured Codex requests unreadable")
	}
	return requests
}

// codexMeasuredBody is a measured first-turn request carrying model.
func codexMeasuredBody(t testing.TB, model string) []byte {
	t.Helper()
	request := codexMeasuredRequests(t)[0]
	request["model"] = model
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
