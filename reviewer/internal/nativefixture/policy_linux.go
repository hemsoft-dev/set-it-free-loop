//go:build linux

package nativefixture

import r "github.com/hemsoft-dev/set-it-free-loop/reviewer"

func codexPolicy(s r.Session) r.EgressPolicy {
	const authority = "fixture.provider.example"
	policy := r.EgressPolicy{Provider: r.Codex, Limits: r.DefaultEgressLimits(s), ResponsePolicy: r.CodexSourceResponses,
		Routes: []r.EgressRoute{
			{Authority: authority, Method: "POST", Path: "/v1/responses", Kind: "inference"},
			{Authority: authority, Method: "GET", Path: "/v1/responses", Kind: "local_negotiation"},
			{Authority: authority, Method: "GET", Path: "/v1/models", RawQuery: "client_version=0.163.0", Kind: "metadata"},
			{Authority: authority, Method: "GET", Path: "/backend-api/wham/accounts/check", Kind: "metadata"},
			{Authority: authority, Method: "GET", Path: "/backend-api/wham/settings/user", Kind: "metadata"},
		},
		Headers: []string{"Accept", "Accept-Encoding", "Authorization", "Chatgpt-Account-Id", "Content-Type", "Originator", "Openai-Beta", "User-Agent", "Version", "Session-Id", "Thread-Id", "X-Codex-Beta-Features", "X-Codex-Turn-Metadata", "X-Codex-Routing-Hint", "X-Codex-Window-Id", "X-Client-Request-Id", "Cache-Control", "X-Openai-Internal-Codex-Responses-Lite"},
	}
	return policy
}

func claudePolicy(s r.Session) r.EgressPolicy {
	const authority = "fixture.provider.example"
	limits := r.DefaultEgressLimits(s)
	limits.Requests, limits.InferenceRequests, limits.RefreshRequests, limits.MetadataRequests = 3, 3, 0, 0
	policy := r.EgressPolicy{Provider: r.Claude, Limits: limits, ResponsePolicy: r.ClaudeSourceMessages,
		Routes:  []r.EgressRoute{{Authority: authority, Method: "POST", Path: "/v1/messages", RawQuery: "beta=true", Kind: "inference"}},
		Headers: []string{"Accept", "Accept-Encoding", "Anthropic-Beta", "Anthropic-Dangerous-Direct-Browser-Access", "Anthropic-Version", "Authorization", "Content-Type", "User-Agent", "X-App", "X-Claude-Code-Session-Id", "X-Stainless-Arch", "X-Stainless-Lang", "X-Stainless-Os", "X-Stainless-Package-Version", "X-Stainless-Retry-Count", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Timeout"},
	}
	return policy
}
