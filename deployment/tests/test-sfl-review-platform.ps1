[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$failures = [System.Collections.Generic.List[string]]::new()

function Read-RepoFile([string] $RelativePath) {
    $path = Join-Path $repoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        $failures.Add("Missing file: $RelativePath")
        return ''
    }
    return Get-Content -LiteralPath $path -Raw
}

function Assert-Patterns(
    [string] $RelativePath,
    [string[]] $Patterns
) {
    $content = Read-RepoFile $RelativePath
    foreach ($pattern in $Patterns) {
        if ($content -notmatch $pattern) {
            $failures.Add("$RelativePath does not contain pattern: $pattern")
        }
    }
}

function Assert-ExactPair(
    [string] $CanonicalPath,
    [string] $StagedPath
) {
    $canonical = Read-RepoFile $CanonicalPath
    $staged = Read-RepoFile $StagedPath
    if ($canonical -ne $staged) {
        $failures.Add("Staged file differs from canonical source: $StagedPath")
    }
}

$reviewer = 'deployment\workflows\sfl-pr-review.md'
$auto = 'deployment\infrastructure\sfl-pr-review-auto.yml'
$recovery = 'deployment\infrastructure\sfl-pr-review-recovery.yml'

Assert-ExactPair $reviewer '.github\workflows\sfl-pr-review.md'
Assert-ExactPair $auto '.github\workflows\sfl-pr-review-auto.yml'
Assert-ExactPair $recovery '.github\workflows\sfl-pr-review-recovery.yml'

Assert-Patterns $reviewer @(
    'source: HemSoft/set-it-free-loop/deployment/workflows/sfl-pr-review\.md@main',
    'COPILOT_PROVIDER_BASE_URL: https://openrouter\.ai/api/v1',
    'COPILOT_MODEL: moonshotai/kimi-k3',
    'item_number:',
    'base_sha:',
    'head_sha:',
    'Validate trusted review context',
    'sfl-review-run-provenance',
    'external_id:',
    'Validate and resolve requested SFL threads',
    'Thread \$\{threadId\} is not an obsolete SFL finding',
    'Could not enumerate unresolved SFL findings',
    'Critical, High, Medium, or Low',
    'SFL Reviewer Approval'
)

Assert-Patterns $auto @(
    'types: \[opened, synchronize, reopened, ready_for_review, edited, review_requested, labeled\]',
    'pull_request_review:\s*\r?\n\s+types: \[submitted\]',
    "github\.event\.label\.name == 'sfl-review'",
    'HEAD_REPOSITORY: \$\{\{ github\.event\.pull_request\.head\.repo\.full_name \}\}',
    'if \[ "\$HEAD_REPOSITORY" != "\$REPOSITORY" \]',
    'if \[ "\$BASE_REF" != "\$DEFAULT_BRANCH" \]',
    'name: Detect installed reviewer contract',
    'reviewer-installed: \$\{\{ steps\.reviewer-contract\.outputs\.installed \}\}',
    "steps\.reviewer-contract\.outputs\.installed == 'true'",
    'Reviewer bootstrap deployment detected',
    'permission-pull-requests: write',
    'consume_review_label',
    'resolve_review_effort',
    'for STATUS in queued in_progress',
    'runs\?status=\$\{STATUS\}',
    'inputs\[base_sha\]',
    'inputs\[head_sha\]',
    'name: SFL Reviewer Gate Runner',
    'checks: write',
    '--arg name "SFL Reviewer Approval"',
    'head_sha: \$head_sha',
    'continue-on-error: true',
    'name: Finalize head approval check',
    'WAIT_OUTCOME: \$\{\{ steps\.wait-review\.outcome \}\}',
    'validate_review_run'
)

$autoContent = Read-RepoFile $auto
if ($autoContent -match 'permission-pull-requests:\s*read') {
    $failures.Add('Auto-review dispatcher cannot consume pull-request labels with a read-only pull-request token.')
}
if ($autoContent -match 'permission-issues:\s*write') {
    $failures.Add('Auto-review dispatcher should not retain issue-wide write access when pull-request write covers its label mutations.')
}

Assert-Patterns $recovery @(
    "github\.event\.workflow_run\.path == '\.github/workflows/sfl-pr-review\.lock\.yml'",
    'decide_review_recovery',
    'newer_run_suppresses_retry',
    'missing_data',
    'missing_tool',
    'report_incomplete',
    '\.retry_count == 0 or \.retry_count == 1',
    'inputs\[retry_count\]=1',
    'suppressing stale retry',
    'produced no formal review after its one trusted retry',
    'Review agent output is unavailable; refusing an unproven retry',
    'Review agent output is malformed; refusing an unproven retry',
    'Last-moment interlock',
    'FINAL_PULL_REQUEST=',
    'FINAL_NEWER_RUN_ID=',
    'changed before recovery dispatch; suppressing stale retry'
)

Assert-Patterns 'deployment\scripts\deploy-workflow.ps1' @(
    'sfl-pr-review-auto',
    'sfl-pr-review-recovery',
    'Assert-HemSoftPrivateRepository',
    "visibility -ne 'PRIVATE'",
    'Add-SflYamlSourcePin',
    'deployment/infrastructure/\$inf\.yml@\$CurrentSha'
)

Assert-Patterns 'deployment\scripts\set-sfl-review-gate.ps1' @(
    "ValidatePattern\('\^HemSoft/",
    "visibility -ne 'PRIVATE'",
    "reviewContext = 'SFL Reviewer Approval'",
    'githubActionsAppId = 15368',
    "activeLogin -ne 'HemSoft'",
    'strict = \$true',
    'protection/required_status_checks',
    'protectionExists',
    'enabling status checks without replacing existing branch protection',
    'Could not inspect full branch protection',
    '--method PUT',
    '--method PATCH',
    'preserving'
)

Assert-Patterns 'deployment\scripts\install-gh-sfl-hemsoft.ps1' @(
    '"reviewer": \{',
    'sfl-pr-review\.lock\.yml',
    'sfl-pr-review-auto\.yml',
    'sfl-pr-review-recovery\.yml',
    'outside the private HemSoft repository scope',
    'GitHub CLI must be authenticated as HemSoft',
    'must be private; visibility is',
    'git@github-personal1:',
    'case "sfl-pr-review\.lock\.yml"',
    "go test \./\.\.\. -run '\^\$'"
)

foreach ($manifestPath in @('deployment\sfl-manifest.schema.json', 'sfl.json')) {
    Assert-Patterns $manifestPath @('sfl-pr-review-auto', 'sfl-pr-review-recovery')
}

Assert-Patterns 'docs\SFL-REVIEWER.md' @(
    'private HemSoft repositories',
    'OPENROUTER_API_KEY',
    'retries exactly once',
    'SFL Reviewer Approval',
    'set-sfl-review-gate\.ps1'
)

$sensitivePatterns = @(
    '(?im)^\s*(?:app-id|github-app-id|installation-id|client-id):\s*(?:\d+|Iv[A-Za-z0-9]+)\s*$',
    '(?im)^\s*source:[ \t]+(?!HemSoft/set-it-free-loop(?:/|@))[^\r\n]*set-it-free-loop'
)
foreach ($path in @($reviewer, $auto, $recovery, 'docs\SFL-REVIEWER.md')) {
    $content = Read-RepoFile $path
    foreach ($pattern in $sensitivePatterns) {
        if ($content -match $pattern) {
            $failures.Add("$path contains prohibited organization-specific content: $pattern")
        }
    }
}

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ }
    exit 1
}

Write-Output 'SFL reviewer platform contract passed.'
