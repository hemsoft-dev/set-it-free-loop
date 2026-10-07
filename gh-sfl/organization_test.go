package main

import (
	"bytes"
	"fmt"
	"os"
	"slices"
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
				if strings.Join(args, " ") == "api --method GET repos/hemsoft-dev/consumer" {
					return *bytes.NewBufferString(`{"full_name":"hemsoft-dev/consumer"}`), bytes.Buffer{}, nil
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
	module, err := os.ReadFile("go.mod")
	if err != nil || strings.TrimSpace(strings.SplitN(string(module), "\n", 2)[0]) != "module github.com/HemSoft/set-it-free-loop/gh-sfl" {
		t.Fatalf("incompatible Go module declaration: %v", err)
	}
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
		if strings.Join(args, " ") == "api --method GET repos/hemsoft-dev/consumer" {
			return *bytes.NewBufferString(`{"full_name":"hemsoft-dev/consumer"}`), bytes.Buffer{}, nil
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
	if shouldPreserveSyncAudit(nil, release, "reviewer") {
		t.Fatal("missing manifest preserved audit")
	}
	manifest := &sflManifest{MotherRepo: "HemSoft/set-it-free-loop", Version: release.Version, SourceSHA: release.SHA, Tier: "reviewer"}
	if shouldPreserveSyncAudit(manifest, release, "reviewer") {
		t.Fatal("source cutover retained an earlier deployment audit")
	}
	manifest.MotherRepo = "hemsoft-dev/set-it-free-loop"
	if !shouldPreserveSyncAudit(manifest, release, "reviewer") {
		t.Fatal("unchanged source did not preserve deployment audit")
	}
}

func TestOrganizationStatusAllowsReadAccess(t *testing.T) {
	previous := ghExec
	t.Cleanup(func() { ghExec = previous })
	for _, role := range []string{"read", "triage", "write", "maintain", "admin", "none"} {
		ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
			if strings.Join(args, " ") == "api user --jq .login" {
				return *bytes.NewBufferString("reader"), bytes.Buffer{}, nil
			}
			if strings.Join(args, " ") == "api --method GET repos/hemsoft-dev/consumer" {
				return *bytes.NewBufferString(`{"full_name":"hemsoft-dev/consumer"}`), bytes.Buffer{}, nil
			}
			if strings.Join(args, " ") != "api --method GET repos/hemsoft-dev/consumer/collaborators/reader/permission" {
				t.Fatalf("unexpected call: %v", args)
			}
			return *bytes.NewBufferString(fmt.Sprintf(`{"permission":%q,"user":{"login":"reader"}}`, role)), bytes.Buffer{}, nil
		}
		_, _, err := parseStatusTarget("hemsoft-dev/consumer")
		if (err != nil) != (role == "none") {
			t.Fatalf("status role %s: %v", role, err)
		}
	}
}

func TestAddonRequiresSyncBeforeSourceCutover(t *testing.T) {
	previous := motherRepoOwner
	t.Cleanup(func() { motherRepoOwner = previous })
	motherRepoOwner = "hemsoft-dev"
	manifest := &sflManifest{MotherRepo: "HemSoft/set-it-free-loop"}
	if err := validateAddonSource(manifest); err == nil || !strings.Contains(err.Error(), "gh sfl sync") {
		t.Fatalf("missing cutover preflight: %v", err)
	}
	manifest.MotherRepo = "hemsoft-dev/set-it-free-loop"
	if err := validateAddonSource(manifest); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SFL_SOURCE_REPOSITORY", "HEMSOFT-DEV/set-it-free-loop")
	if err := configureSourceRepository(); err != nil || motherRepoOwner != "hemsoft-dev" {
		t.Fatalf("source not canonical: %s %v", motherRepoOwner, err)
	}
}

func TestRepositoryRedirectRejectedBeforePermissionOrMutation(t *testing.T) {
	previous := ghExec
	t.Cleanup(func() { ghExec = previous })
	for _, owner := range []string{"HemSoft", "hemsoft-dev"} {
		for _, metadata := range []string{`{"full_name":"other/consumer"}`, `{"full_name":"hemsoft-dev/renamed"}`, `{}`, `invalid`} {
			t.Run(owner+"/"+metadata, func(t *testing.T) {
				ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
					switch strings.Join(args, " ") {
					case "api user --jq .login":
						return *bytes.NewBufferString("HemSoft"), bytes.Buffer{}, nil
					case "api --method GET repos/" + owner + "/consumer":
						return *bytes.NewBufferString(metadata), bytes.Buffer{}, nil
					default:
						t.Fatalf("permission or mutation reached after redirect: %v", args)
						return bytes.Buffer{}, bytes.Buffer{}, nil
					}
				}
				if err := validateDeploymentTarget(owner, "consumer"); err == nil {
					t.Fatal("accepted noncanonical repository")
				}
			})
		}
	}
}

func TestCanonicalRepositoryLookupFailsClosed(t *testing.T) {
	previous := ghExec
	t.Cleanup(func() { ghExec = previous })
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("metadata service unavailable")
	}
	if verifyCanonicalRepository("hemsoft-dev", "consumer") == nil {
		t.Fatal("accepted failed lookup")
	}
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		return *bytes.NewBufferString(`{"full_name":"HEMSOFT-DEV/Consumer"}`), bytes.Buffer{}, nil
	}
	if err := verifyCanonicalRepository("hemsoft-dev", "consumer"); err != nil {
		t.Fatalf("rejected case-insensitive canonical target: %v", err)
	}
}

func TestLegacyRootManifestPreservesExistingDeployment(t *testing.T) {
	var paths []string
	manifest, err := readRemoteManifestWithFetcher("hemsoft-dev", "consumer", func(owner, repo, path, ref string) (string, error) {
		paths = append(paths, path)
		if path == ".sfl/sfl.json" {
			return "", fmt.Errorf("HTTP 404")
		}
		return `{"version":"2.1.0-rc.13","tier":"full","source":"HemSoft/set-it-free-loop","addons":["pr-review"]}`, nil
	})
	if err != nil || manifest == nil || manifest.Tier != "full" || len(manifest.Addons) != 1 || manifest.Addons[0] != "pr-review" {
		t.Fatalf("legacy deployment not preserved: %v %v", manifest, err)
	}
	if strings.Join(paths, ",") != ".sfl/sfl.json,sfl.json" {
		t.Fatalf("wrong lookup sequence %v", paths)
	}
	if err := validateInitTierTransition(manifest, initOptions{tier: "reviewer"}, "hemsoft-dev/consumer"); err == nil {
		t.Fatal("legacy full deployment mistaken for fresh reviewer consumer")
	}
}

func TestCanonicalManifestPreventsStaleRootFallback(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		err       error
		wantError bool
	}{
		{name: "current canonical", raw: `{"tier":"minimal","addons":[]}`},
		{name: "malformed canonical", raw: `invalid`, wantError: true},
		{name: "forbidden canonical", err: fmt.Errorf("HTTP 403"), wantError: true},
		{name: "server failure", err: fmt.Errorf("HTTP 500"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := readRemoteManifestWithFetcher("hemsoft-dev", "consumer", func(owner, repo, path, ref string) (string, error) {
				if path != ".sfl/sfl.json" {
					if tc.wantError {
						t.Fatalf("read stale root after failed canonical response: %s", path)
					}
					return `{"tier":"full"}`, nil
				}
				return tc.raw, tc.err
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("manifest=%v error=%v", manifest, err)
			}
			if !tc.wantError && manifest.Tier != "minimal" {
				t.Fatal("canonical tier not preserved")
			}
		})
	}
}

func TestManifestMutationsKeepLegacyCopiesConsistent(t *testing.T) {
	for _, canonicalExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("canonical=%t", canonicalExists), func(t *testing.T) {
			manifest, err := readRemoteManifestWithFetcher("hemsoft-dev", "consumer", func(_, _, path, _ string) (string, error) {
				if path == ".sfl/sfl.json" && !canonicalExists {
					return "", fmt.Errorf("HTTP 404")
				}
				return `{"tier":"review","version":"old","source":"HemSoft/set-it-free-loop"}`, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest.Version = "new"
			manifest.MotherRepo = "hemsoft-dev/set-it-free-loop"
			files := map[string]string{}
			if err := writeManifestFiles(files, manifest); err != nil {
				t.Fatal(err)
			}
			if len(files) != 2 || files["sfl.json"] != files[".sfl/sfl.json"] ||
				!strings.Contains(files["sfl.json"], `"version": "new"`) ||
				!strings.Contains(files["sfl.json"], `"source": "hemsoft-dev/set-it-free-loop"`) ||
				strings.Contains(files["sfl.json"], "RemotePaths") {
				t.Fatalf("inconsistent manifest writes: %v", files)
			}
			removed, err := uninstallFiles(manifest, false)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(removed, "sfl.json") || !slices.Contains(removed, ".sfl/sfl.json") {
				t.Fatalf("uninstall leaves manifest copy: %v", removed)
			}
		})
	}
	files := map[string]string{}
	if err := writeManifestFiles(files, &sflManifest{Tier: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[".sfl/sfl.json"] == "" {
		t.Fatalf("wrong fresh manifest paths: %v", files)
	}
}
