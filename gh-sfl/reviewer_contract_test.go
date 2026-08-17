package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReviewerDeploymentContract(t *testing.T) {
	t.Helper()

	root := filepath.Clean("..")
	assertFilesEqual(t,
		filepath.Join(root, "deployment", "workflows", "sfl-pr-review.md"),
		filepath.Join(root, ".github", "workflows", "sfl-pr-review.md"),
	)
	assertFilesEqual(t,
		filepath.Join(root, "deployment", "infrastructure", "sfl-pr-review-auto.yml"),
		filepath.Join(root, ".github", "workflows", "sfl-pr-review-auto.yml"),
	)
	assertFilesEqual(t,
		filepath.Join(root, "deployment", "infrastructure", "sfl-pr-review-recovery.yml"),
		filepath.Join(root, ".github", "workflows", "sfl-pr-review-recovery.yml"),
	)

	source := readContractFile(t, filepath.Join(root, "deployment", "workflows", "sfl-pr-review.md"))
	sourceText := normalizeLineEndings(source)
	for _, required := range []string{
		"base_sha:\n        description: Expected pull request base commit",
		"head_sha:\n        description: Expected pull request head commit",
		"dispatch_id:\n        description: Unique wrapper or recovery dispatch identifier",
		"retry_count:\n        description: Missing-review-output retry counter",
		"review_effort:\n        description: Audit effort marker retained in provenance and recovery",
		"default: low\n        type: choice",
		"options:\n          - low\n          - medium\n          - high",
		"SFL_REVIEW_EFFORT: ${{ inputs.review_effort || 'low' }}",
		"pre-steps:",
		"Validate trusted review context",
		`--argjson item_number "$ITEM_NUMBER"`,
		`item_type: "pull_request"`,
		`item_number: $item_number`,
		`base_sha: $base_sha`,
		`head_sha: $head_sha`,
		"Untrusted or inconsistent review context",
		`case "$REVIEW_EFFORT" in`,
		`low|medium|high) ;;`,
		"-f review_effort=low",
		`run-name: "SFL PR Review #${{ inputs.item_number }} ${{ inputs.base_sha }}:${{ inputs.head_sha }} retry=${{ inputs.retry_count }} dispatch=${{ inputs.dispatch_id }}"`,
		"publish_review_provenance:",
		"Initialize review evidence",
		"status: 'in_progress'",
		"`sfl-review:${pullNumber}:${baseSha}:${headSha}:${runId}`",
		"SFL_RUN_ID: ${{ github.run_id }}",
		"evidence-check-run-id: ${{ steps.initialize-evidence.outputs.check-run-id }}",
		"name: sfl-review-run-provenance",
		"workflow_run_id: $workflow_run_id",
		"base_sha: $base_sha",
		`[[ "$PR_NUMBER" =~ ^[1-9][0-9]*$ ]]`,
		"continue-on-error: false",
		"commit-id: \"${{ inputs.head_sha }}\"",
		"resolve-sfl-review-thread:",
		"if: needs.safe_outputs.result == 'success'",
		"pull-requests: write",
		"Thread ${threadId} is not an obsolete SFL finding on PR",
		"Verify pull request base and head before safe outputs",
		"still-applicable unresolved SFL",
		"`side: LEFT` for a deleted line",
		"countUnresolvedSflFindings",
		"replace(/\\[bot\\]$/i, '')",
		"name: safe-outputs-items",
		"item => item.type === 'submit_pull_request_review'",
		"submittedReview.metadata?.review_id",
		"Review body contains unresolved template placeholder",
		"const publishFailure = async ({ reason }) =>",
		"name: 'SFL Review Evidence'",
		"github.rest.checks.get",
		"github.rest.checks.update",
		"for (let attempt = 1; attempt <= 3; attempt += 1)",
		"Initialized SFL review evidence does not match this run",
		"external_id: gateExternalId",
		"core.setFailed(summary)",
		"github-token: ${{ steps.app-token.outputs.token }}",
		"github-token: ${{ github.token }}",
		"checks: write",
		"permission-contents: read",
		"'replacement-retry-eligible'",
		"SFL_AGENT_OUTPUT: /tmp/gh-aw/agent_output.json",
		"Download reviewer agent output",
		"fs.readFileSync(process.env.SFL_AGENT_OUTPUT",
		"item.type === 'missing_data'",
		"item.type === 'missing_tool'",
		"item.type === 'report_incomplete'",
		"let replacementRetryEligible = false",
		"Expected exactly one immutable agent submitted-review item",
		"(agentSubmittedReview.repo ?? configuredRepo) !== configuredRepo",
		"(item.repo ?? configuredRepo) !== configuredRepo ||",
		"Published review-comment count ${publishedReviewComments.length} does not match immutable agent count ${agentReviewComments.length}",
		"const originalBody = agentSubmittedReview.body || ''",
		"Published review manifest does not match the immutable agent verdict",
		"const immutableFindingCount = agentReviewComments.length",
		"publishedFindingCount !== immutableFindingCount",
		"replacement_retry_eligible=${replacementRetryEligible}",
		"SFL_REPLACEMENT_RETRY_ELIGIBLE === 'true'",
		"-f base_sha=PULL_REQUEST_BASE_SHA",
		`"base_sha":"PULL_REQUEST_BASE_SHA"`,
		"make exactly one successful `submit_pull_request_review` call",
		"call `missing_data` with the specific blocker",
		"`noop` and never silently stop",
		"Run the embedded Unslop pass on every user-facing sentence",
		"Protocol literals override the Unslop rules",
		`Ask, "What makes this obviously AI generated?"`,
		"immutable-head `SFL Reviewer Approval` check",
	} {
		if !strings.Contains(sourceText, required) {
			t.Errorf("canonical reviewer is missing contract text %q", required)
		}
	}

	for _, placeholder := range reviewerTemplatePlaceholders {
		if !strings.Contains(sourceText, "'"+placeholder+"'") {
			t.Errorf("canonical reviewer does not validate template placeholder %q", placeholder)
		}
	}
	if strings.Contains(sourceText, "require('@actions/github')") {
		t.Error("review metadata dynamically imports an unavailable github-script module")
	}
	if strings.Contains(sourceText, "permission-checks: write") {
		t.Error("shared SFL App token still requests forgeable Checks write access")
	}

	compiled := readContractFile(t, filepath.Join(root, ".github", "workflows", "sfl-pr-review.lock.yml"))
	compiledText := normalizeLineEndings(compiled)
	for _, required := range []string{
		"GH_AW_INPUTS_BASE_SHA: ${{ inputs.base_sha }}",
		"GH_AW_INPUTS_HEAD_SHA: ${{ inputs.head_sha }}",
		"SFL_REVIEW_EFFORT: ${{ inputs.review_effort || 'low' }}",
		"Validate trusted review context",
		"Untrusted or inconsistent review context",
		`run-name: "SFL PR Review #${{ inputs.item_number }} ${{ inputs.base_sha }}:${{ inputs.head_sha }} retry=${{ inputs.retry_count }} dispatch=${{ inputs.dispatch_id }}"`,
		"{{#runtime-import .github/workflows/sfl-pr-review.md}}",
		"publish_review_provenance:",
		"Initialize review evidence",
		"status: 'in_progress'",
		"`sfl-review:${pullNumber}:${baseSha}:${headSha}:${runId}`",
		"SFL_RUN_ID: ${{ github.run_id }}",
		"evidence-check-run-id: ${{ steps.initialize-evidence.outputs.check-run-id }}",
		"name: sfl-review-run-provenance",
		"RETRY_COUNT: ${{ inputs.retry_count }}",
		"\"commit_id\":\"${GH_AW_INPUT_HEAD_SHA}\"",
		"GH_AW_DETECTION_CONTINUE_ON_ERROR: \"false\"",
		"Verify pull request base and head before safe outputs",
		"Could not enumerate unresolved SFL findings",
		"SFL_BASE_SHA: ${{ inputs.base_sha }}",
		"SFL_HEAD_SHA: ${{ inputs.head_sha }}",
		"SFL_SAFE_OUTPUT_ITEMS: /tmp/sfl-review-safe-outputs/safe-output-items.jsonl",
		"const publishFailure = async ({ reason }) =>",
		"name: 'SFL Review Evidence'",
		"github.rest.checks.get",
		"github.rest.checks.update",
		"for (let attempt = 1; attempt <= 3; attempt += 1)",
		"Initialized SFL review evidence does not match this run",
		"external_id: gateExternalId",
		"core.setFailed(summary)",
		"permission-contents: read",
		"'replacement-retry-eligible'",
		"SFL_AGENT_OUTPUT: /tmp/gh-aw/agent_output.json",
		"Download reviewer agent output",
		"fs.readFileSync(process.env.SFL_AGENT_OUTPUT",
		"let replacementRetryEligible = false",
		"Expected exactly one immutable agent submitted-review item",
		"agentSubmittedReview.repo ?? configuredRepo",
		"const originalBody = agentSubmittedReview.body || ''",
		"const immutableFindingCount = agentReviewComments.length",
		"replacement_retry_eligible=${replacementRetryEligible}",
		"SFL_REPLACEMENT_RETRY_ELIGIBLE === 'true'",
	} {
		if !strings.Contains(compiledText, required) {
			t.Errorf("compiled reviewer is missing contract text %q", required)
		}
	}
	if strings.Contains(compiledText, "require('@actions/github')") {
		t.Error("compiled review metadata dynamically imports an unavailable github-script module")
	}
	if strings.Contains(compiledText, "permission-checks: write") {
		t.Error("compiled reviewer still requests forgeable Checks write access for the shared SFL App")
	}
	if !strings.Contains(
		compiledText,
		"agent:\n    needs:\n      - activation\n      - publish_review_provenance",
	) {
		t.Error("reviewer agent can run before current review evidence is initialized")
	}
	validationIndex := strings.Index(compiledText, "name: Validate trusted review context")
	if validationIndex < 0 {
		t.Fatal("compiled reviewer is missing trusted review context validation")
	}
	for _, laterStep := range []string{
		"name: Checkout repository",
		"name: Checkout PR branch",
		"name: Execute GitHub Copilot CLI",
	} {
		stepIndex := strings.Index(compiledText, laterStep)
		if stepIndex < 0 {
			t.Errorf("compiled reviewer is missing ordering target %q", laterStep)
		} else if validationIndex > stepIndex {
			t.Errorf("trusted review context validation runs after %q", laterStep)
		}
	}

	trigger := readContractFile(t, filepath.Join(root, "deployment", "infrastructure", "sfl-pr-review-auto.yml"))
	triggerText := normalizeLineEndings(trigger)
	var triggerWorkflow any
	if err := yaml.Unmarshal(trigger, &triggerWorkflow); err != nil {
		t.Fatalf("parse automatic reviewer workflow: %v", err)
	}
	for _, required := range []string{
		"pull_request_target:",
		"pull_request_review:",
		"BASE_SHA: ${{ github.event.pull_request.base.sha }}",
		"HEAD_SHA: ${{ github.event.pull_request.head.sha }}",
		`if [ "$BASE_REF" != "$DEFAULT_BRANCH" ]; then`,
		"types: [opened, synchronize, reopened, ready_for_review, edited, review_requested, labeled]",
		"github.event.action != 'labeled'",
		"github.event.label.name == 'sfl-review'",
		"vars.SFL_ENABLED != 'false'",
		"cancel-in-progress: false",
		"name: SFL Reviewer Gate Runner",
		"if: always()",
		"SFL_ENABLED: ${{ vars.SFL_ENABLED }}",
		`if [ "$SFL_ENABLED" = "false" ]; then`,
		"required reviewer gate fails closed",
		"timeout-minutes: 120",
		"for ATTEMPT in $(seq 1 440); do",
		"Wait for exact SFL review run",
		"validate_review_run",
		`.path == ".github/workflows/sfl-pr-review.lock.yml"`,
		`.event == "workflow_dispatch"`,
		".head_repository.full_name == $repository",
		".head_branch == $default_branch",
		`actions/runs/${EXPECTED_REVIEW_RUN_ID}`,
		"--paginate --jq '.workflow_runs[]'",
		"Actions run lookup failed",
		"Exact SFL review run concluded",
		"expected-review-run-id:",
		"review-run-id=",
		"permission-contents: read",
		"permission-pull-requests: write",
		"Reset stale SFL review state after a push",
		`issues/${PR_NUMBER}/labels?per_page=100`,
		"--paginate",
		"contains(github.event.pull_request.labels.*.name, 'sfl-pr')",
		`grep -Eq '^(sfl-done|sfl-needs-work)$'`,
		`labels[]=sfl-ready-for-review`,
		"grep -qE '^\\s+base_sha:\\s*$'",
		"dispatch_args+=(-f \"inputs[base_sha]=${BASE_SHA}\")",
		"grep -qE '^\\s+head_sha:\\s*$'",
		"-f \"ref=${TRUSTED_REF}\"",
		"dispatch_args+=(-f \"inputs[head_sha]=${HEAD_SHA}\")",
		"grep -qE '^\\s+retry_count:\\s*$'",
		"dispatch_args+=(-f \"inputs[retry_count]=0\")",
		`EVENT_ACTION: ${{ github.event.action }}`,
		"grep -qE '^\\s+review_effort:\\s*$'",
		`REVIEW_EFFORT=$(resolve_review_effort "$PR_LABELS")`,
		`dispatch_args+=(-f "inputs[review_effort]=${REVIEW_EFFORT}")`,
		`DISPATCH_ID="auto-${GITHUB_RUN_ID}"`,
		`dispatch_args+=(-f "inputs[dispatch_id]=${DISPATCH_ID}")`,
		`select(.display_title | endswith(" dispatch=" + $dispatch_id))`,
		`issues/${PR_NUMBER}/labels/sfl-review`,
		"consume_review_label",
		`consume_review_label "$PR_LABELS"`,
		`grep -Fxq "sfl-review"`,
		`"${EXPECTED_TITLE}" "$REVIEW_LABEL_PRESENT"`,
		"selected at request time, even",
		"stale queued labeled event whose trigger was consumed",
		"cat > /dev/null",
		`grep -q 'HTTP 404'`,
		"sfl-review trigger label is already absent",
		"Could not consume sfl-review trigger label",
		`EXPECTED_TITLE="SFL PR Review #${PR_NUMBER} ${BASE_SHA}:${HEAD_SHA} retry="`,
		`BEFORE_RUN_ID=$(gh api`,
		`--argjson before_run_id "$BEFORE_RUN_ID"`,
		`select(.id > $before_run_id)`,
		"skipping duplicate dispatch",
		"for STATUS in $(reusable_review_statuses)",
		`runs?status=${STATUS}&per_page=100`,
		"correctness hazard worse than an occasional duplicate",
		"# BEGIN TESTABLE DISPATCH DEDUP",
		"find_reusable_review_run",
	} {
		if !strings.Contains(triggerText, required) {
			t.Errorf("automatic reviewer trigger is missing contract text %q", required)
		}
	}
	if strings.Contains(triggerText, "ref=${BASE_REF}") {
		t.Error("automatic reviewer trigger still dispatches from the pull request base ref")
	}
	if strings.Contains(triggerText, "created_at >= $dispatched_at") {
		t.Error("automatic reviewer trigger still trusts the runner clock to resolve a dispatched run")
	}
	if count := strings.Count(triggerText, "github.event.label.name == 'sfl-review'"); count != 2 {
		t.Errorf("sfl-review label gate appears %d times, want concurrency and dispatch guards", count)
	}
	if count := strings.Count(triggerText, "consume_review_label"); count != 3 {
		t.Errorf("sfl-review label consumer appears %d times, want definition plus active and new dispatch paths", count)
	}

	recovery := normalizeLineEndings(readContractFile(t,
		filepath.Join(root, "deployment", "infrastructure", "sfl-pr-review-recovery.yml"),
	))
	for _, required := range []string{
		`workflows: ["SFL PR Review"]`,
		"vars.SFL_ENABLED != 'false'",
		"github.event.workflow_run.conclusion == 'failure'",
		"github.event.workflow_run.conclusion == 'timed_out'",
		"github.event.workflow_run.path == '.github/workflows/sfl-pr-review.lock.yml'",
		"github.event.workflow_run.event == 'workflow_dispatch'",
		"github.event.workflow_run.head_repository.full_name == github.repository",
		"github.event.workflow_run.head_branch == github.event.repository.default_branch",
		"--name sfl-review-run-provenance",
		"--name safe-outputs-items",
		"--name agent",
		"agent_output.json",
		`SFL_APP_SLUG: ${{ steps.app-token.outputs.app-slug }}`,
		`pulls/${PR_NUMBER}/reviews?per_page=100`,
		`contains("/actions/runs/" + $workflow_run_id)`,
		`reconcile_review_count`,
		`PUBLISHED_REVIEW_COUNT`,
		`reconciling published reviews`,
		`select(.type == "submit_pull_request_review")`,
		`.type == "missing_data"`,
		`.type == "report_incomplete"`,
		`terminal_signal_count`,
		`echo "reported"`,
		`if [ "$retry_count" -eq 0 ]; then`,
		`echo "exhausted"`,
		`(.base_sha | test("^[0-9a-f]{40}$"))`,
		`(.review_effort | test("^(low|medium|high)$"))`,
		`inputs[base_sha]=${BASE_SHA}`,
		`inputs[retry_count]=1`,
		`REVIEW_EFFORT=$(jq -r '.review_effort' "$PROVENANCE")`,
		`dispatch_args+=(-f "inputs[review_effort]=${REVIEW_EFFORT}")`,
		`dispatch_args+=(-f "inputs[dispatch_id]=recovery-${GITHUB_RUN_ID}")`,
		`A newer review run already owns PR`,
		`--name sfl-review-run-provenance`,
		`--paginate`,
		`--slurp`,
		`.workflow_runs[]`,
		`NEWER_PROVENANCE_ATTEMPTS=12`,
		`NEWER_PROVENANCE_LOADED="false"`,
		`newer_run_suppresses_retry`,
		"The sealed title is computed first",
		`EXPECTED_REVIEW_TITLE_PREFIX="SFL PR Review #${PR_NUMBER} ${BASE_SHA}:${HEAD_SHA} retry="`,
		`NEWER_TITLE_MATCH`,
		`A newer in-progress review for PR #${PR_NUMBER} at ${BASE_SHA}:${HEAD_SHA} has unavailable provenance`,
		`PR #${PR_NUMBER} is no longer eligible`,
	} {
		if !strings.Contains(recovery, required) {
			t.Errorf("review recovery workflow is missing contract text %q", required)
		}
	}

}
func TestResolverTokenSeparation(t *testing.T) {
	t.Helper()

	root := filepath.Clean("..")
	const tokenStepName = "Generate SFL App token for thread validation"
	const identityStepName = "Resolve SFL App identity"
	const resolverStepName = "Validate and resolve requested SFL threads"

	sourcePath := filepath.Join(root, "deployment", "workflows", "sfl-pr-review.md")
	source := normalizeLineEndings(readContractFile(t, sourcePath))
	if count := strings.Count(source, tokenStepName); count != 1 {
		t.Fatalf("%s contains %d resolver token steps, want 1", sourcePath, count)
	}
	tokenStepStart := strings.Index(source, tokenStepName)
	tokenStepEnd := strings.Index(source[tokenStepStart:], resolverStepName)
	if tokenStepEnd < 0 {
		t.Fatalf("%s resolver token step boundary is missing", sourcePath)
	}
	tokenStep := source[tokenStepStart : tokenStepStart+tokenStepEnd]
	for _, permission := range []string{
		"permission-contents: read",
		"permission-pull-requests: read",
	} {
		if !strings.Contains(tokenStep, permission) {
			t.Errorf("%s resolver token step is missing %q", sourcePath, permission)
		}
	}
	for _, contract := range []string{
		"contents: write",
		"github-token: ${{ steps.validation-token.outputs.token }}",
		"SFL_APP_LOGIN: ${{ steps.app-identity.outputs.login }}",
		"github-token: ${{ github.token }}",
		"isOutdated",
		"(!thread.isResolved && !thread.isOutdated)",
		"const unresolvedOutdatedThreadIds = []",
		"for (const threadId of unresolvedOutdatedThreadIds)",
		"github.rest.repos.getContent",
		"no longer exists at head ${expectedHead}; human resolution is required",
		"const failedThreadIds = []",
		"for (let attempt = 1; attempt <= 3; attempt += 1)",
		"failedThreadIds.push(threadId)",
	} {
		if !strings.Contains(source, contract) {
			t.Errorf("%s resolver is missing %q", sourcePath, contract)
		}
	}

	compiledPath := filepath.Join(root, ".github", "workflows", "sfl-pr-review.lock.yml")
	var compiled struct {
		Jobs map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				ID   string         `yaml:"id"`
				Name string         `yaml:"name"`
				Env  map[string]any `yaml:"env"`
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(readContractFile(t, compiledPath), &compiled); err != nil {
		t.Fatalf("parse compiled reviewer workflow: %v", err)
	}
	job, ok := compiled.Jobs["resolve_sfl_review_thread"]
	if !ok {
		t.Fatal("compiled reviewer is missing resolve_sfl_review_thread job")
	}
	for permission, expected := range map[string]string{
		"contents":      "write",
		"pull-requests": "write",
	} {
		if actual := job.Permissions[permission]; actual != expected {
			t.Errorf("compiled resolver job permission %s = %q, want %q", permission, actual, expected)
		}
	}

	var tokenSteps, identitySteps, resolverSteps int
	for _, step := range job.Steps {
		switch step.Name {
		case tokenStepName:
			tokenSteps++
			if step.ID != "validation-token" {
				t.Errorf("compiled resolver token step id = %q, want validation-token", step.ID)
			}
			const tokenAction = "actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1"
			if step.Uses != tokenAction {
				t.Errorf("compiled resolver token step uses %q, want %q", step.Uses, tokenAction)
			}
			expectedPermissions := map[string]string{
				"permission-contents":      "read",
				"permission-pull-requests": "read",
			}
			var permissionCount int
			for permission, actual := range step.With {
				if !strings.HasPrefix(permission, "permission-") {
					continue
				}
				permissionCount++
				expected, ok := expectedPermissions[permission]
				if !ok {
					t.Errorf("compiled resolver token has unexpected permission %q", permission)
					continue
				}
				if actual != expected {
					t.Errorf("compiled resolver token %s = %v, want %q", permission, actual, expected)
				}
			}
			if permissionCount != len(expectedPermissions) {
				t.Errorf("compiled resolver token has %d permission inputs, want %d", permissionCount, len(expectedPermissions))
			}
			for permission, expected := range expectedPermissions {
				if actual := step.With[permission]; actual != expected {
					t.Errorf("compiled resolver token %s = %v, want %q", permission, actual, expected)
				}
			}
		case identityStepName:
			identitySteps++
			const expectedToken = "${{ steps.validation-token.outputs.token }}"
			if actual := step.With["github-token"]; actual != expectedToken {
				t.Errorf("compiled identity github-token = %v, want %q", actual, expectedToken)
			}
		case resolverStepName:
			resolverSteps++
			const expectedToken = "${{ github.token }}"
			if actual := step.With["github-token"]; actual != expectedToken {
				t.Errorf("compiled resolver github-token = %v, want %q", actual, expectedToken)
			}
			const expectedLogin = "${{ steps.app-identity.outputs.login }}"
			if actual := step.Env["SFL_APP_LOGIN"]; actual != expectedLogin {
				t.Errorf("compiled resolver SFL_APP_LOGIN = %v, want %q", actual, expectedLogin)
			}
		}
	}
	if tokenSteps != 1 {
		t.Errorf("compiled resolver has %d token steps, want 1", tokenSteps)
	}
	if identitySteps != 1 {
		t.Errorf("compiled resolver has %d identity steps, want 1", identitySteps)
	}
	if resolverSteps != 1 {
		t.Errorf("compiled resolver has %d mutation steps, want 1", resolverSteps)
	}
}

func TestAutoTriggerTokenPermissions(t *testing.T) {
	t.Helper()

	root := filepath.Clean("..")
	path := filepath.Join(root, "deployment", "infrastructure", "sfl-pr-review-auto.yml")
	var workflow struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				ID   string         `yaml:"id"`
				Name string         `yaml:"name"`
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(readContractFile(t, path), &workflow); err != nil {
		t.Fatalf("parse auto-trigger workflow: %v", err)
	}
	if len(workflow.Permissions) != 0 {
		t.Errorf("auto-trigger workflow has workflow-level permissions %v, want none", workflow.Permissions)
	}
	job, ok := workflow.Jobs["dispatch"]
	if !ok {
		t.Fatal("auto-trigger workflow is missing dispatch job")
	}
	if actual := job.Permissions["contents"]; actual != "read" {
		t.Errorf("auto-trigger dispatch contents permission = %q, want read", actual)
	}

	const tokenAction = "actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1"
	expectedPermissions := map[string]string{
		"permission-actions":       "write",
		"permission-contents":      "read",
		"permission-pull-requests": "write",
	}
	var tokenSteps int
	for _, step := range job.Steps {
		if step.Name != "Mint SFL GitHub App token" {
			continue
		}
		tokenSteps++
		if step.ID != "app-token" {
			t.Errorf("auto-trigger token step id = %q, want app-token", step.ID)
		}
		if step.Uses != tokenAction {
			t.Errorf("auto-trigger token step uses %q, want %q", step.Uses, tokenAction)
		}
		var permissionCount int
		for permission, actual := range step.With {
			if !strings.HasPrefix(permission, "permission-") {
				continue
			}
			permissionCount++
			expected, ok := expectedPermissions[permission]
			if !ok {
				t.Errorf("auto-trigger token has unexpected permission %q", permission)
				continue
			}
			if actual != expected {
				t.Errorf("auto-trigger token %s = %v, want %q", permission, actual, expected)
			}
		}
		if permissionCount != len(expectedPermissions) {
			t.Errorf("auto-trigger token has %d permission inputs, want %d", permissionCount, len(expectedPermissions))
		}
		for permission, expected := range expectedPermissions {
			if actual := step.With[permission]; actual != expected {
				t.Errorf("auto-trigger token %s = %v, want %q", permission, actual, expected)
			}
		}
	}
	if tokenSteps != 1 {
		t.Errorf("auto-trigger has %d token steps, want 1", tokenSteps)
	}
}

// The approval gate must be published with the repository-scoped github.token,
// never the shared SFL App token. A global substring assertion cannot prove
// this because other compiled steps also receive a github-token input; parse
// the compiled workflow and assert the credential on the exact publication
// steps so the trust-boundary contract fails exactly when either step changes.
func TestCompiledEvidencePublishesWithRepositoryToken(t *testing.T) {
	t.Helper()

	root := filepath.Clean("..")
	compiled := readContractFile(t, filepath.Join(root, ".github", "workflows", "sfl-pr-review.lock.yml"))

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string         `yaml:"name"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(compiled, &workflow); err != nil {
		t.Fatalf("parse compiled reviewer workflow: %v", err)
	}

	expectedSteps := map[string]int{
		"Initialize review evidence":   0,
		"Finalize SFL review evidence": 0,
	}
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if _, ok := expectedSteps[step.Name]; !ok {
				continue
			}
			expectedSteps[step.Name]++
			token, ok := step.With["github-token"].(string)
			if !ok {
				t.Errorf("job %q evidence-publication step has no github-token input", jobName)
				continue
			}
			if token != "${{ github.token }}" {
				t.Errorf(
					"job %q evidence-publication step github-token = %q, want repository-scoped ${{ github.token }}",
					jobName, token,
				)
			}
		}
	}
	for stepName, matches := range expectedSteps {
		if matches != 1 {
			t.Fatalf("expected exactly one %s step, found %d", stepName, matches)
		}
	}
}

func TestIssueBranchCorrelationPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^agent-fix/issue-36(-|$)`)
	for _, branch := range []string{
		"agent-fix/issue-36",
		"agent-fix/issue-36-retry-output",
	} {
		if !pattern.MatchString(branch) {
			t.Errorf("expected branch %q to match issue correlation", branch)
		}
	}
	for _, branch := range []string{
		"agent-fix/issue-360",
		"agent-fix/issue-36retry-output",
		"feature/issue-36",
	} {
		if pattern.MatchString(branch) {
			t.Errorf("expected branch %q not to match issue correlation", branch)
		}
	}
}

func assertRunBlocksWithinLimit(t *testing.T, workflow string, limit int) {
	t.Helper()

	runBlock := regexp.MustCompile(`^\s+run:\s*[|>]`)
	lines := strings.Split(workflow, "\n")
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if !runBlock.MatchString(line) {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))
		end := index + 1
		for ; end < len(lines); end++ {
			candidate := lines[end]
			if strings.TrimSpace(candidate) == "" {
				continue
			}
			candidateIndent := len(candidate) - len(strings.TrimLeft(candidate, " "))
			if candidateIndent <= indent {
				break
			}
		}
		if length := len(strings.Join(lines[index+1:end], "\n")); length > limit {
			t.Errorf(
				"workflow run block at line %d has %d characters, exceeding GitHub's %d-character expression limit",
				index+1,
				length,
				limit,
			)
		}
		index = end - 1
	}
}

var reviewerTemplatePlaceholders = []string{
	"{verdict_icon}",
	"{VERDICT}",
	"{verdict_reason}",
	"{security_critical_or_dash}",
	"{security_high_or_dash}",
	"{security_medium_or_dash}",
	"{security_low_or_dash}",
	"{accuracy_critical_or_dash}",
	"{accuracy_high_or_dash}",
	"{accuracy_medium_or_dash}",
	"{accuracy_low_or_dash}",
	"{quality_maintainability_critical_or_dash}",
	"{quality_maintainability_high_or_dash}",
	"{quality_maintainability_medium_or_dash}",
	"{quality_maintainability_low_or_dash}",
	"{total_critical_or_dash}",
	"{total_high_or_dash}",
	"{total_medium_or_dash}",
	"{total_low_or_dash}",
	`{Top 3 most impactful current findings with their severity prefixes, or "No findings."}`,
}

func TestReviewerTemplatePlaceholderDetection(t *testing.T) {
	t.Helper()

	validBody := `Key finding: use {side: "LEFT"} for deleted lines.`
	if placeholder := unresolvedReviewerTemplatePlaceholder(validBody); placeholder != "" {
		t.Fatalf("ordinary brace-delimited code was treated as a placeholder: %s", placeholder)
	}

	for _, placeholder := range reviewerTemplatePlaceholders {
		if actual := unresolvedReviewerTemplatePlaceholder("Review body " + placeholder); actual != placeholder {
			t.Errorf("placeholder %q was not detected; got %q", placeholder, actual)
		}
	}
}

func unresolvedReviewerTemplatePlaceholder(body string) string {
	for _, placeholder := range reviewerTemplatePlaceholders {
		if strings.Contains(body, placeholder) {
			return placeholder
		}
	}
	return ""
}

func assertFilesEqual(t *testing.T, sourcePath, deployedPath string) {
	t.Helper()

	source := readContractFile(t, sourcePath)
	deployed := readContractFile(t, deployedPath)
	if !bytes.Equal(source, deployed) {
		t.Errorf("%s differs from %s", deployedPath, sourcePath)
	}
}

func readContractFile(t *testing.T, path string) []byte {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}

func normalizeLineEndings(content []byte) string {
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}
