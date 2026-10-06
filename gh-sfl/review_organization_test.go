package main

import (
	"fmt"
	"strings"
	"testing"
)

type organizationReviewREST struct {
	*reviewREST
	role, author, responseUser string
	lookupError                bool
	permissionGets             int
}

func (f *organizationReviewREST) Get(path string, response interface{}) error {
	if strings.Contains(path, "/collaborators/") {
		f.permissionGets++
		if f.lookupError {
			return fmt.Errorf("permission service unavailable")
		}
		parts := strings.Split(path, "/")
		login := parts[len(parts)-2]
		if f.responseUser != "" {
			login = f.responseUser
		}
		return decodeTestResponse(response, map[string]any{"permission": f.role, "user": map[string]string{"login": login}})
	}
	if strings.Contains(path, "/issues/comments/") {
		return decodeTestResponse(response, map[string]any{"id": 123, "user": map[string]string{"login": f.author}})
	}
	return f.reviewREST.Get(path, response)
}

func TestOrganizationRequestRegistrationRequiresMatchingAuthorizedAuthor(t *testing.T) {
	for _, tc := range []struct {
		name, role, creator, author, responseUser, host string
		lookupError, registered, wantError              bool
	}{
		{name: "writer", role: "write", creator: "member", author: "member", registered: true},
		{name: "maintainer", role: "maintain", creator: "member", author: "member", registered: true},
		{name: "owner", role: "admin", creator: "HemSoft", author: "HemSoft", registered: true},
		{name: "reader", role: "read", creator: "member", author: "member"},
		{name: "outsider", role: "none", creator: "outsider", author: "outsider"},
		{name: "forged author", role: "write", creator: "member", author: "other"},
		{name: "identity mismatch", role: "write", creator: "member", author: "member", responseUser: "other", wantError: true},
		{name: "lookup failed", role: "write", creator: "member", author: "member", lookupError: true, wantError: true},
		{name: "untrusted URL", role: "write", creator: "member", author: "member", host: "https://attacker.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host
			if host == "" {
				host = "https://github.com"
			}
			status := reviewRequestStatus{Context: codexReviewRequestRegistryContext, TargetURL: host + "/hemsoft-dev/consumer/pull/94#issuecomment-123"}
			status.Creator.Login = tc.creator
			rest := &organizationReviewREST{reviewREST: &reviewREST{statuses: []reviewRequestStatus{status}}, role: tc.role, author: tc.author, responseUser: tc.responseUser, lookupError: tc.lookupError}
			got, err := findRegisteredCodexRequestIDs(rest, "hemsoft-dev", "consumer", 94, strings.Repeat("b", 40))
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if got[123] != tc.registered {
				t.Fatalf("registration=%v want %v", got, tc.registered)
			}
			if tc.registered && rest.permissionGets < 2 {
				t.Fatal("creator and comment author were not independently authorized")
			}
			if rest.posts != 0 || rest.statusPosts != 0 {
				t.Fatal("read-only authorization mutated the repository")
			}
		})
	}
}
