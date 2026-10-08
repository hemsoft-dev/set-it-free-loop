package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/cli/go-gh/v2/pkg/api"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestConsumerSyncPolicyRejectsReviewerAndArbitraryPaths(t *testing.T) {
	for _, body := range []string{
		`{"version":1,"unmanagedWorkflows":["sfl-pr-review-auto.yml"]}`,
		`{"version":1,"unmanagedWorkflows":["../secrets"]}`,
		`{"version":1,"unmanagedWorkflows":["sfl-auditor.yml","sfl-auditor.yml"]}`,
		`{"version":2,"unmanagedWorkflows":["sfl-auditor.yml"]}`,
		`{"version":1,"unmanagedWorkflows":[]}`,
		`{"version":1,"unmanagedWorkflows":["sfl-auditor.yml"],"ignoreReviewer":true}`,
		`{"version":1,"unmanagedWorkflows":["sfl-auditor.yml"]} {}`,
	} {
		if _, err := parseConsumerSyncPolicy([]byte(body)); err == nil {
			t.Fatalf("accepted invalid policy %s", body)
		}
	}
}

func TestConsumerOwnedFilesAreNeitherReplacedNorDeleted(t *testing.T) {
	policy, err := parseConsumerSyncPolicy([]byte(`{"version":1,"unmanagedWorkflows":["sfl-auditor.yml","sfl-dispatcher.yml","issue-processor.md"]}`))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".github/workflows/sfl-auditor.yml":        "on:\n  schedule:\n    - cron: '* * * * *'\n",
		".github/workflows/sfl-dispatcher.yml":     "restore issue processor",
		".github/workflows/issue-processor.md":     "retired automation",
		".github/workflows/sfl-pr-review-auto.yml": "current required observer",
	}
	applyConsumerSyncPolicy(files, policy)
	fake := &fakeREST{fileContents: map[string]string{
		".github/workflows/sfl-auditor.yml":        "on:\n  workflow_dispatch:\n",
		".github/workflows/sfl-dispatcher.yml":     "consumer App-token v3, no issue processing",
		".github/workflows/sfl-pr-review-auto.yml": "old required observer",
	}}
	matches, deletions, err := deploymentFilesState(fake, "hemsoft-dev", "consumer", "main", files, true, consumerPreservedPaths(policy)...)
	if err != nil || matches {
		t.Fatalf("current reviewer update must remain necessary: %v %v", matches, err)
	}
	for _, deletion := range deletions {
		if strings.Contains(deletion.Path, "sfl-auditor") || strings.Contains(deletion.Path, "sfl-dispatcher") {
			t.Fatalf("consumer file deleted: %s", deletion.Path)
		}
	}
	for _, request := range fake.gets {
		for _, name := range policy.UnmanagedWorkflows {
			if strings.Contains(request, "/contents/.github/workflows/"+name+"?") {
				t.Fatalf("consumer-owned path entered reconciliation: %s", request)
			}
		}
	}
	fake.fileContents[".github/workflows/sfl-pr-review-auto.yml"] = "current required observer"
	matches, deletions, err = deploymentFilesState(fake, "hemsoft-dev", "consumer", "main", files, true, consumerPreservedPaths(policy)...)
	if err != nil || !matches || len(deletions) != 0 {
		t.Fatalf("repeat must make no changes: %v %v %v", matches, deletions, err)
	}
}

type policyREST struct {
	fakeREST
	failure error
	body    string
}

func (p *policyREST) Get(path string, response interface{}) error {
	p.gets = append(p.gets, path)
	if p.failure != nil {
		return p.failure
	}
	return decodeTestResponse(response, map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(p.body))})
}
func TestPolicyLookupOnlyTreatsTyped404AsAbsence(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized, http.StatusInternalServerError} {
		f := &policyREST{failure: &api.HTTPError{StatusCode: status}}
		policy, err := readConsumerSyncPolicy(f, "hemsoft-dev", "consumer", strings.Repeat("a", 40))
		if status == 404 {
			if err != nil || policy != nil {
				t.Fatal("404 must be absent")
			}
		} else if err == nil {
			t.Fatalf("status%d must fail", status)
		}
		if !strings.HasSuffix(f.gets[0], "?ref="+strings.Repeat("a", 40)) {
			t.Fatal("policy capture not pinned")
		}
	}
	primary := errors.New("provider failure")
	f := &policyREST{failure: primary}
	_, err := readConsumerSyncPolicy(f, "hemsoft-dev", "consumer", strings.Repeat("a", 40))
	if !errors.Is(err, primary) {
		t.Fatal("lost primary error")
	}
}
func TestPreserveConsumerEngineChoices(t *testing.T) {
	prior := &sflEnginePolicyManifest{Workflows: []sflEngineWorkflowProfile{{Name: "issue-processor", Model: "existing-consumer-model"}}}
	next := &sflEnginePolicyManifest{Workflows: []sflEngineWorkflowProfile{{Name: "pr-fixer", Model: "updated-managed-model"}}}
	policy := &consumerSyncPolicy{Version: 1, UnmanagedWorkflows: []string{"issue-processor.md"}}
	preserveConsumerEngineProfiles(next, prior, policy)
	if len(next.Workflows) != 2 || next.Workflows[1].Model != "existing-consumer-model" || prior.Workflows[0].Model != "existing-consumer-model" {
		t.Fatal("consumer model policy changed")
	}
	if !slices.Contains(consumerPreservedPaths(policy), ".github/workflows/issue-processor.md") {
		t.Fatal("missing preserved path")
	}
}

func TestConsumerPolicyStaleDefaultHeadFailsBeforeAnyWrite(t *testing.T) {
	rest := &fakeREST{}
	installDeploymentFakes(t, rest, &fakeGraphQL{})
	_, err := deployViaPullRequestAtRevision("HemSoft", "consumer", "main", "sync", map[string]string{".github/workflows/sfl-pr-review-auto.yml": "new observer"}, "sync", true, io.Discard, strings.Repeat("a", 40), []string{".github/workflows/sfl-auditor.yml"})
	if err == nil || !strings.Contains(err.Error(), "default branch changed") {
		t.Fatalf("expected stale-policy refusal: %v", err)
	}
	if len(rest.posts)+len(rest.puts)+len(rest.patches)+len(rest.deletes) != 0 {
		t.Fatal("stale policy produced a write")
	}
}

func TestConsumerPolicyPreservesFilesInActualCommitPayload(t *testing.T) {
	rest := &fakeREST{openPRURL: "https://github.test/pull/existing", openPRBranch: "sfl/sync-existing", fileContents: map[string]string{
		".github/workflows/sfl-auditor.yml":        "manual consumer auditor",
		".github/workflows/sfl-dispatcher.yml":     "consumer dispatcher v3",
		".github/workflows/sfl-pr-review-auto.yml": "old observer",
		".github/workflows/sfl-pr-review.md":       "retired reviewer",
	}}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)
	_, err := deployViaPullRequestAtRevision("owner", "repo", "main", "sync", map[string]string{".github/workflows/sfl-pr-review-auto.yml": "new observer"}, "sync", true, io.Discard, "base-sha", []string{".github/workflows/sfl-auditor.yml", ".github/workflows/sfl-dispatcher.yml", ".github/workflows/issue-processor.md"})
	if err != nil {
		t.Fatal(err)
	}
	changes := graphQL.variables["input"].(map[string]any)["fileChanges"].(map[string]any)
	encoded, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sfl-auditor.yml", "sfl-dispatcher.yml", "issue-processor.md"} {
		if strings.Contains(string(encoded), name) {
			t.Fatalf("consumer-owned file in published changes: %s", name)
		}
	}
	deletions := changes["deletions"].([]fileDeletion)
	if len(deletions) != 1 || deletions[0].Path != ".github/workflows/sfl-pr-review.md" {
		t.Fatalf("normal retired reviewer reconciliation lost: %v", deletions)
	}
	if !strings.Contains(string(encoded), "sfl-pr-review-auto.yml") {
		t.Fatal("required observer not updated")
	}
}

type changingPolicyBaseREST struct {
	fakeREST
	refReads        int
	changedConsumer bool
}

func (f *changingPolicyBaseREST) Get(path string, response interface{}) error {
	if strings.Contains(path, "/git/ref/heads/") {
		f.refReads++
		sha := "base-sha"
		if f.refReads > 1 {
			sha = "advanced-default-head"
		}
		return decodeTestResponse(response, map[string]any{"object": map[string]string{"sha": sha}})
	}
	if f.changedConsumer && strings.Contains(path, "/contents/.github/workflows/sfl-auditor.yml?ref=sfl%2Fsync-existing") {
		return decodeTestResponse(response, map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("unwanted scheduled auditor"))})
	}
	return f.fakeREST.Get(path, response)
}
func TestConsumerPolicyLateBaseAdvanceDoesNotCreateBranch(t *testing.T) {
	rest := &changingPolicyBaseREST{fakeREST: fakeREST{fileContents: map[string]string{".github/workflows/sfl-pr-review-auto.yml": "old observer"}}}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)
	_, err := deployViaPullRequestAtRevision("owner", "repo", "main", "sync", map[string]string{".github/workflows/sfl-pr-review-auto.yml": "new observer"}, "sync", true, io.Discard, "base-sha", []string{".github/workflows/sfl-auditor.yml"})
	if err == nil || !strings.Contains(err.Error(), "default branch changed") || rest.refReads != 2 {
		t.Fatalf("late advance not rejected: %v reads%d", err, rest.refReads)
	}
	if len(rest.posts)+len(rest.patches)+len(rest.puts)+len(rest.deletes) != 0 || graphQL.variables != nil {
		t.Fatal("late base advance wrote repository state")
	}
}
func TestExistingSyncPRCannotRestoreConsumerOwnedAutomation(t *testing.T) {
	rest := &changingPolicyBaseREST{changedConsumer: true, fakeREST: fakeREST{openPRURL: "https://github.test/pull/existing", openPRBranch: "sfl/sync-existing", fileContents: map[string]string{".github/workflows/sfl-auditor.yml": "manual consumer auditor"}}}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)
	_, err := deployViaPullRequestAtRevision("owner", "repo", "main", "sync", map[string]string{".github/workflows/sfl-pr-review-auto.yml": "new observer"}, "sync", true, io.Discard, "base-sha", []string{".github/workflows/sfl-auditor.yml"})
	if err == nil || !strings.Contains(err.Error(), "existing deployment PR changes consumer-owned workflow") {
		t.Fatalf("unsafe existing PR not rejected: %v", err)
	}
	if len(rest.posts)+len(rest.patches)+len(rest.puts)+len(rest.deletes) != 0 || graphQL.variables != nil {
		t.Fatal("consumer-owned divergence wrote repository state")
	}
}

func TestExistingSyncPRLateBaseAdvanceFailsBeforeCommit(t *testing.T) {
	rest := &changingPolicyBaseREST{fakeREST: fakeREST{openPRURL: "https://github.test/pull/existing", openPRBranch: "sfl/sync-existing", fileContents: map[string]string{".github/workflows/sfl-auditor.yml": "manual consumer auditor"}}}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)
	_, err := deployViaPullRequestAtRevision("owner", "repo", "main", "sync", map[string]string{".github/workflows/sfl-pr-review-auto.yml": "new observer"}, "sync", true, io.Discard, "base-sha", []string{".github/workflows/sfl-auditor.yml"})
	if err == nil || !strings.Contains(err.Error(), "default branch changed before consumer-policy commit") || rest.refReads != 2 {
		t.Fatalf("late existing-PR advance not rejected: %v reads%d", err, rest.refReads)
	}
	if len(rest.posts)+len(rest.patches)+len(rest.puts)+len(rest.deletes) != 0 || graphQL.variables != nil {
		t.Fatal("stale existing-PR policy wrote repository state")
	}
}

func TestConsumerPolicyNoOpRejectsMovedDefaultHead(t *testing.T) {
	for _, existing := range []bool{false, true} {
		rest := &changingPolicyBaseREST{fakeREST: fakeREST{fileContents: map[string]string{".github/workflows/sfl-pr-review-auto.yml": "current observer"}}}
		if existing {
			rest.openPRURL = "https://github.test/pull/existing"
			rest.openPRBranch = "sfl/sync-existing"
		}
		graphQL := &fakeGraphQL{}
		installDeploymentFakes(t, rest, graphQL)
		_, err := deployViaPullRequestAtRevision("owner", "repo", "main", "sync", map[string]string{".github/workflows/sfl-pr-review-auto.yml": "current observer"}, "sync", true, io.Discard, "base-sha", nil)
		if err == nil || !strings.Contains(err.Error(), "default branch changed before consumer-policy no-op") {
			t.Fatalf("existing=%v returned stale no-op: %v", existing, err)
		}
		if len(rest.posts)+len(rest.puts)+len(rest.patches)+len(rest.deletes) != 0 || graphQL.variables != nil {
			t.Fatal("stale no-op mutated repository")
		}
	}
}

func TestConsumerManifestSnapshotRejectsLegacyPresenceRace(t *testing.T) {
	body := `{"version":"2.1.0-rc.18","tier":"full","motherRepo":"hemsoft-dev/set-it-free-loop"}`
	fetch := func(legacy bool) func(string, string, string, string) (string, error) {
		return func(_, _, path, _ string) (string, error) {
			if path == "sfl.json" && !legacy {
				return "", &api.HTTPError{StatusCode: 404}
			}
			return body, nil
		}
	}
	initial, err := readRemoteManifestWithFetcher("owner", "repo", fetch(true))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := readRemoteManifestWithFetcher("owner", "repo", fetch(false))
	if err != nil {
		t.Fatal(err)
	}
	initialJSON, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	boundJSON, err := json.Marshal(bound)
	if err != nil {
		t.Fatal(err)
	}
	if string(initialJSON) != string(boundJSON) {
		t.Fatal("counterexample must change only unencoded legacy presence")
	}
	if err := validateConsumerManifestSnapshot(initialJSON, initial.RemotePaths, bound); err == nil {
		t.Fatal("legacy manifest removal accepted and would be restored")
	}
	if err := validateConsumerManifestSnapshot(initialJSON, initial.RemotePaths, initial); err != nil {
		t.Fatalf("unchanged snapshot rejected: %v", err)
	}
	if err := validateConsumerManifestSnapshot(boundJSON, bound.RemotePaths, initial); err == nil {
		t.Fatal("concurrent legacy manifest addition accepted")
	}
}

func TestAbsentPolicyCannotOverwriteAConcurrentlyAddedPolicyViaGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git shim integration")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	remote := filepath.Join(directory, "remote.git")
	seed := filepath.Join(directory, "seed")
	run := func(dir string, args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	run(directory, "init", "--bare", "--initial-branch=main", remote)
	run(directory, "init", "--initial-branch=main", seed)
	run(seed, "config", "user.name", "SFL regression fixture")
	run(seed, "config", "user.email", "fixture@example.invalid")
	write := func(path, body string) {
		target := filepath.Join(seed, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(".github/workflows/sfl-auditor.yml", "old auditor")
	run(seed, "add", ".")
	run(seed, "commit", "-m", "initial no-policy state")
	captured := run(seed, "rev-parse", "HEAD")
	write(".sfl/sync-policy.json", `{"version":1,"unmanagedWorkflows":["sfl-auditor.yml"]}`)
	write(".github/workflows/sfl-auditor.yml", "manual consumer auditor")
	run(seed, "add", ".")
	run(seed, "commit", "-m", "add consumer policy and manual auditor")
	advanced := run(seed, "rev-parse", "HEAD")
	run(seed, "remote", "add", "origin", remote)
	run(seed, "push", "origin", "main")
	shim := filepath.Join(directory, "bin")
	if err := os.Mkdir(shim, 0755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = clone ]; then
 exec "$SFL_TEST_REAL_GIT" clone --branch main "$SFL_TEST_REMOTE" "$6"
fi
exec "$SFL_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SFL_TEST_REAL_GIT", git)
	t.Setenv("SFL_TEST_REMOTE", remote)
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = deployViaGit("owner", "repo", "main", map[string]string{".github/workflows/sfl-auditor.yml": "unwanted upstream schedule"}, "sync", true, io.Discard, captured)
	if err == nil || !strings.Contains(err.Error(), "default branch changed after consumer policy capture") {
		t.Fatalf("concurrent policy addition not rejected: %v", err)
	}
	if actual := run(directory, "--git-dir", remote, "rev-parse", "main"); actual != advanced {
		t.Fatalf("remote changed: %s", actual)
	}
	if actual := run(directory, "--git-dir", remote, "show", "main:.github/workflows/sfl-auditor.yml"); actual != "manual consumer auditor" {
		t.Fatalf("consumer choice overwritten: %s", actual)
	}
}

func TestAbsentConsumerPolicyStillBindsPRDeliveryToCapturedRevision(t *testing.T) {
	rest := &changingPolicyBaseREST{fakeREST: fakeREST{fileContents: map[string]string{".github/workflows/sfl-auditor.yml": "manual consumer auditor"}}}
	policy, revision, err := captureConsumerSyncPolicy(rest, "owner", "repo", "main")
	if err != nil || policy != nil || revision != "base-sha" {
		t.Fatalf("absence lost its immutable revision: policy=%v revision=%s err=%v", policy, revision, err)
	}
	graphQL := &fakeGraphQL{}
	installDeploymentFakes(t, rest, graphQL)
	_, err = deployViaPullRequestAtRevision("owner", "repo", "main", "sync", map[string]string{".github/workflows/sfl-auditor.yml": "unwanted upstream schedule"}, "sync", true, io.Discard, revision, consumerPreservedPaths(policy))
	if err == nil || !strings.Contains(err.Error(), "default branch changed") {
		t.Fatalf("new policy/default revision not refused: %v", err)
	}
	if len(rest.posts)+len(rest.patches)+len(rest.puts)+len(rest.deletes) != 0 || graphQL.variables != nil {
		t.Fatal("stale absence overwrote consumer workflow")
	}
}
