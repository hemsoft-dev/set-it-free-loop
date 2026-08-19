package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestHemSoftEnginePolicyRewriteIsIdempotent(t *testing.T) {
	workflow := "---\nname: Example\nmodel: legacy\nnetwork: defaults\n---\n# Body\n"

	first, err := applyHemSoftEnginePolicyToWorkflow(workflow, "example")
	if err != nil {
		t.Fatalf("first engine-policy rewrite: %v", err)
	}
	second, err := applyHemSoftEnginePolicyToWorkflow(first, "example")
	if err != nil {
		t.Fatalf("second engine-policy rewrite: %v", err)
	}
	if second != first {
		t.Fatalf("engine-policy rewrite is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	for _, required := range []string{
		"engine:\n  id: codex",
		"model: gpt-5.5?effort=high",
		"network: defaults",
	} {
		if !strings.Contains(first, required) {
			t.Errorf("rewritten workflow is missing %q", required)
		}
	}
	if strings.Contains(first, "model: legacy") {
		t.Error("rewritten workflow retained the superseded model")
	}
}

func TestRenderHemSoftWorkflowMatchesDeploymentAndStatus(t *testing.T) {
	markdown := "---\nname: Example\nmodel: legacy\nnetwork: defaults\n---\nVersion " + sflVersionPlaceholder + "\n"
	rendered, err := renderHemSoftWorkflow("example.md", markdown, "2.0.0")
	if err != nil {
		t.Fatalf("render Markdown workflow: %v", err)
	}
	for _, required := range []string{"engine:\n  id: codex", "model: gpt-5.5?effort=high", "Version 2.0.0"} {
		if !strings.Contains(rendered, required) {
			t.Errorf("rendered Markdown workflow is missing %q", required)
		}
	}

	yaml, err := renderHemSoftWorkflow("wrapper.yml", "version: "+sflVersionPlaceholder+"\n", "2.0.0")
	if err != nil {
		t.Fatalf("render YAML workflow: %v", err)
	}
	if yaml != "version: 2.0.0\n" {
		t.Fatalf("rendered YAML workflow = %q", yaml)
	}
}

func TestHemSoftEnginePolicyHasNoOpenRouterReviewerProfile(t *testing.T) {
	policy, err := hemSoftEnginePolicyConfig()
	if err != nil {
		t.Fatalf("load engine policy: %v", err)
	}
	if _, exists := policy.Workflows["sfl-pr-review"]; exists {
		t.Fatal("native Codex reviewer still has a gh-aw engine mapping")
	}
	if strings.Contains(hemSoftEnginePolicyJSON, "OPENROUTER") || strings.Contains(hemSoftEnginePolicyJSON, "openrouter") {
		t.Fatal("embedded engine policy retained OpenRouter configuration")
	}
}

func TestHemSoftOwnershipPreservesConsumerCodeowners(t *testing.T) {
	files := map[string]string{
		".sfl/sfl.json": `{"tier":"full"}`,
		"CODEOWNERS":    "* @consumer-team\n",
	}

	if err := applyHemSoftOwnership(files); err != nil {
		t.Fatalf("apply HemSoft ownership: %v", err)
	}
	if files["CODEOWNERS"] != "* @consumer-team\n" {
		t.Fatalf("consumer CODEOWNERS was overwritten: %q", files["CODEOWNERS"])
	}
}

func TestValidateDeploymentTargetEnforcesIdentity(t *testing.T) {
	oldGHExec := ghExec
	t.Cleanup(func() { ghExec = oldGHExec })

	for _, test := range []struct {
		name      string
		login     string
		wantError string
	}{
		{name: "wrong identity", login: "fhemmerrelias", wantError: "authenticated as HemSoft"},
		{name: "HemSoft identity", login: "HemSoft"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
				var stdout bytes.Buffer
				switch strings.Join(args, " ") {
				case "api user --jq .login":
					fmt.Fprintln(&stdout, test.login)
				default:
					return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("unexpected gh arguments: %v", args)
				}
				return stdout, bytes.Buffer{}, nil
			}

			err := validateDeploymentTarget("HemSoft", "consumer")
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validateDeploymentTarget() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validateDeploymentTarget() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestValidateDeploymentTargetRejectsScopeBeforeGitHubAccess(t *testing.T) {
	oldGHExec := ghExec
	t.Cleanup(func() { ghExec = oldGHExec })
	ghExec = func(args ...string) (bytes.Buffer, bytes.Buffer, error) {
		return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("GitHub should not be called: %v", args)
	}

	for _, test := range []struct {
		owner     string
		repo      string
		wantError string
	}{
		{owner: "relias-engineering", repo: "consumer", wantError: "outside the HemSoft repository scope"},
		{owner: "HemSoft", repo: "set-it-free-loop", wantError: "is protected"},
	} {
		err := validateDeploymentTarget(test.owner, test.repo)
		if err == nil || !strings.Contains(err.Error(), test.wantError) {
			t.Fatalf("validateDeploymentTarget(%q, %q) error = %v, want %q", test.owner, test.repo, err, test.wantError)
		}
	}
}

func TestHemSoftWorkflowSourceRouting(t *testing.T) {
	for name, want := range map[string]string{
		"sfl-pr-review-auto.yml": "deployment/infrastructure/sfl-pr-review-auto.yml",
		"repo-audit.md":          "deployment/workflows/repo-audit.md",
	} {
		if got := sourceWorkflowPath(name); got != want {
			t.Errorf("sourceWorkflowPath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEscapedLabelPathEncodesPathSensitiveCharacters(t *testing.T) {
	if got, want := escapedLabelPath("sfl-state:ready now"), "sfl-state%3Aready%20now"; got != want {
		t.Fatalf("escapedLabelPath() = %q, want %q", got, want)
	}
}

func TestReviewerStatusReadsPinnedFilesThroughHemSoftRouting(t *testing.T) {
	statusSource := string(readContractFile(t, filepath.Join("status.go")))
	if !strings.Contains(statusSource, "sourceWorkflowPath(workflow)") {
		t.Error("reviewer status does not resolve pinned artifacts through HemSoft source routing")
	}
	if !strings.Contains(statusSource, "renderHemSoftWorkflow(workflow, source, manifest.Version)") {
		t.Error("reviewer status does not apply the same engine policy as deployment")
	}
	if strings.Contains(statusSource, `"workflows/"+workflow`) {
		t.Error("reviewer status still assumes the Relias workflow directory layout")
	}
}

func TestUpdateChecksUsePrivateSetItFreeLoopRelease(t *testing.T) {
	for _, file := range []string{"main.go", "version.go"} {
		source := string(readContractFile(t, file))
		if !strings.Contains(source, "fetchLatestReleaseFunc(motherRepoOwner, motherRepoName)") {
			t.Errorf("%s does not check the private set-it-free-loop release", file)
		}
		if strings.Contains(source, "fetchLatestReleaseFunc(motherRepoOwner, extensionName)") {
			t.Errorf("%s still depends on a separate gh-sfl repository", file)
		}
	}
}

func TestDirectMutationsUseHemSoftSSHProfileWithoutTokenURLs(t *testing.T) {
	for _, file := range []string{"github.go", "uninstall.go"} {
		source := string(readContractFile(t, file))
		if !strings.Contains(source, `git@github-personal1:%s/%s.git`) {
			t.Errorf("%s does not use the HemSoft SSH profile", file)
		}
		for _, forbidden := range []string{"x-access-token", `gh.Exec("auth", "token")`} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s retains token-bearing Git transport %q", file, forbidden)
			}
		}
	}
}
