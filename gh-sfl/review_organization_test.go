package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

type organizationReviewREST struct {
	*reviewREST
	role, author, responseUser, authenticatedUser string
	canonicalRepo                                 string
	lookupError                                   bool
	permissionGets                                int
	deniedLogins                                  map[string]int
}

func (f *organizationReviewREST) Get(path string, response interface{}) error {
	if path == "repos/hemsoft-dev/consumer" {
		return decodeTestResponse(response, map[string]string{"full_name": f.canonicalRepo, "default_branch": "main"})
	}
	if path == "user" {
		return decodeTestResponse(response, map[string]string{"login": f.authenticatedUser})
	}
	if strings.Contains(path, "/collaborators/") {
		f.permissionGets++
		if f.lookupError {
			return fmt.Errorf("permission service unavailable")
		}
		parts := strings.Split(path, "/")
		login := parts[len(parts)-2]
		if status := f.deniedLogins[login]; status != 0 {
			return &api.HTTPError{StatusCode: status}
		}
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

func TestOrganizationRequesterRevalidatedBeforePosting(t *testing.T) {
	for _, tc := range []struct {
		role, user string
		denied     bool
	}{
		{"write", "member", false}, {"read", "member", true}, {"write", "other", true},
	} {
		rest := &organizationReviewREST{reviewREST: &reviewREST{}, role: tc.role, authenticatedUser: tc.user}
		err := confirmOrganizationReviewRequester(rest, "hemsoft-dev", "consumer", "member")
		if (err != nil) != tc.denied {
			t.Fatalf("role %s user %s error %v", tc.role, tc.user, err)
		}
		if rest.posts != 0 || rest.statusPosts != 0 {
			t.Fatal("authorization mutated repository")
		}
	}
}

func TestOrganizationRequesterHTTPDenialsDoNotPoisonAuthorizedRegistry(t *testing.T) {
	for _, status := range []int{403, 404} {
		stale := reviewRequestStatus{Context: codexReviewRequestRegistryContext, TargetURL: "https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-122"}
		stale.Creator.Login = "departed"
		current := reviewRequestStatus{Context: codexReviewRequestRegistryContext, TargetURL: "https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-123"}
		current.Creator.Login = "member"
		rest := &organizationReviewREST{reviewREST: &reviewREST{statuses: []reviewRequestStatus{stale, current}}, role: "write", author: "member", deniedLogins: map[string]int{"departed": status}}
		ids, err := findRegisteredCodexRequestIDs(rest, "hemsoft-dev", "consumer", 94, strings.Repeat("b", 40))
		if err != nil || len(ids) != 1 || !ids[123] {
			t.Fatalf("HTTP%d: registry=%v error=%v", status, ids, err)
		}
		if rest.posts != 0 || rest.statusPosts != 0 {
			t.Fatal("registry scan mutated repository")
		}
	}
	rest := &organizationReviewREST{reviewREST: &reviewREST{}, deniedLogins: map[string]int{"member": 500}}
	if allowed, err := authorizeReviewRequester(rest, "hemsoft-dev", "consumer", "member"); err == nil || allowed {
		t.Fatal("server failure must remain an error")
	}
}

func TestOrganizationReviewRejectsRedirectedRepository(t *testing.T) {
	for _, canonical := range []string{"other/consumer", "hemsoft-dev/renamed", "", "HEMSOFT-DEV/Consumer"} {
		rest := &organizationReviewREST{reviewREST: &reviewREST{}, canonicalRepo: canonical}
		branch, err := fetchRepositoryDefaultBranchWithClient(rest, "hemsoft-dev", "consumer")
		allowed := strings.EqualFold(canonical, "hemsoft-dev/consumer")
		if (err == nil) != allowed || (allowed && branch != "main") {
			t.Fatalf("canonical=%q branch=%q error=%v", canonical, branch, err)
		}
		if rest.posts != 0 || rest.statusPosts != 0 {
			t.Fatal("repository metadata check mutated review state")
		}
	}
}

func TestOrganizationRegistryIgnoresUnrelatedTargetsBeforeAuthorization(t *testing.T) {
	targets := []string{
		"https://github.com/hemsoft-dev/consumer/pull/95#issuecomment-122",
		"https://attacker.test/hemsoft-dev/consumer/pull/94#issuecomment-122",
		"https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-not-an-id",
		"https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-0",
		"https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-+122",
		"https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-0122",
		"https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-122?extra=1",
	}
	for _, target := range targets {
		irrelevant := reviewRequestStatus{Context: codexReviewRequestRegistryContext, TargetURL: target}
		irrelevant.Creator.Login = "irrelevant"
		valid := reviewRequestStatus{Context: codexReviewRequestRegistryContext, TargetURL: "https://github.com/hemsoft-dev/consumer/pull/94#issuecomment-123"}
		valid.Creator.Login = "member"
		rest := &organizationReviewREST{reviewREST: &reviewREST{statuses: []reviewRequestStatus{irrelevant, valid}}, role: "write", author: "member", deniedLogins: map[string]int{"irrelevant": 500}}
		ids, err := findRegisteredCodexRequestIDs(rest, "hemsoft-dev", "consumer", 94, strings.Repeat("b", 40))
		if err != nil || len(ids) != 1 || !ids[123] || rest.permissionGets != 2 {
			t.Fatalf("target=%s ids=%v permissions=%d error=%v", target, ids, rest.permissionGets, err)
		}
	}
}
