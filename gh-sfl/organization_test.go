package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestOrganizationPermissionPreflight(t *testing.T) {
	previous := ghExec
	t.Cleanup(func() { ghExec = previous })
	for _, tc := range []struct {
		permission, responseUser string
		lookupFail, wantError    bool
	}{
		{"admin", "member", false, false}, {"maintain", "member", false, false},
		{"write", "member", false, false}, {"read", "member", false, true},
		{"triage", "member", false, true}, {"", "member", false, true},
		{"write", "other", false, true}, {"write", "member", true, true},
	} {
		t.Run(fmt.Sprintf("%s/%s/%t", tc.permission, tc.responseUser, tc.lookupFail), func(t *testing.T) {
			ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
				if strings.Join(args, " ") == "api user --jq .login" {
					return *bytes.NewBufferString("member"), bytes.Buffer{}, nil
				}
				if strings.Join(args, " ") != "api --method GET repos/hemsoft-dev/consumer/collaborators/member/permission" {
					t.Fatalf("unexpected authorization call %v", args)
				}
				if tc.lookupFail {
					return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("403")
				}
				return *bytes.NewBufferString(fmt.Sprintf(`{"permission":%q,"user":{"login":%q}}`, tc.permission, tc.responseUser)), bytes.Buffer{}, nil
			}
			if err := validateDeploymentTarget("hemsoft-dev", "consumer"); (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%t", err, tc.wantError)
			}
		})
	}
}

func TestOrganizationSourceIsProtectedBeforeAPIAccess(t *testing.T) {
	previous := ghExec
	t.Cleanup(func() { ghExec = previous })
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		t.Fatalf("unexpected API call %v", args)
		return bytes.Buffer{}, bytes.Buffer{}, nil
	}
	if err := validateDeploymentTarget("hemsoft-dev", "set-it-free-loop"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("source protection error=%v", err)
	}
}

func TestSourceCutoverPinsProvenanceWithoutChangingGoModule(t *testing.T) {
	previous := motherRepoOwner
	t.Cleanup(func() { motherRepoOwner = previous })
	t.Setenv("SFL_SOURCE_REPOSITORY", "hemsoft-dev/set-it-free-loop")
	if err := configureSourceRepository(); err != nil {
		t.Fatal(err)
	}
	source := reviewerSourcePlaceholder + "\nname: SFL Codex Review Observer\ngithub.event.sender.id == 199175422\n" + reviewerPushBranchPlaceholder + "\n" + reviewerBaseBranchPlaceholder + "\n"
	got, err := prepareWorkflowSource("sfl-pr-review-auto.yml", source, strings.Repeat("a", 40), "hemsoft-dev/consumer", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "hemsoft-dev/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@"+strings.Repeat("a", 40)) {
		t.Fatalf("wrong provenance: %s", got)
	}
	for _, invalid := range []string{"other/set-it-free-loop", "hemsoft-dev/other", "hemsoft-dev/set-it-free-loop/extra"} {
		t.Setenv("SFL_SOURCE_REPOSITORY", invalid)
		if configureSourceRepository() == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestOrganizationGateRequiresAdminBeforeMutation(t *testing.T) {
	previous := ghExec
	t.Cleanup(func() { ghExec = previous })
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		if strings.Join(args, " ") == "api user --jq .login" {
			return *bytes.NewBufferString("member"), bytes.Buffer{}, nil
		}
		if strings.Join(args, " ") != "api --method GET repos/hemsoft-dev/consumer/collaborators/member/permission" {
			t.Fatalf("unexpected call %v", args)
		}
		return *bytes.NewBufferString(`{"permission":"write","user":{"login":"member"}}`), bytes.Buffer{}, nil
	}
	if err := runGate([]string{"--repo", "hemsoft-dev/consumer"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("gate did not fail with admin preflight: %v", err)
	}
}

func TestSourceCutoverRefreshesSyncAudit(t *testing.T) {
	previous := motherRepoOwner
	t.Cleanup(func() { motherRepoOwner = previous })
	motherRepoOwner = "hemsoft-dev"
	release := deploymentRelease{Version: "2.1.0-rc.13", SHA: strings.Repeat("a", 40)}
	manifest := &sflManifest{MotherRepo: "HemSoft/set-it-free-loop", Version: release.Version, SourceSHA: release.SHA, Tier: "reviewer"}
	if shouldPreserveSyncAudit(manifest, release, "reviewer") {
		t.Fatal("source cutover retained an earlier deployment audit")
	}
	manifest.MotherRepo = "hemsoft-dev/set-it-free-loop"
	if !shouldPreserveSyncAudit(manifest, release, "reviewer") {
		t.Fatal("unchanged source did not preserve deployment audit")
	}
}
