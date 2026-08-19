[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
. (Join-Path $PSScriptRoot 'line-ending-test-helpers.ps1')

$canonicalPath = Join-Path $repoRoot 'deployment\infrastructure\sfl-pr-review-auto.yml'
$stagedPath = Join-Path $repoRoot '.github\workflows\sfl-pr-review-auto.yml'
foreach ($path in @($canonicalPath, $stagedPath)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Missing subscription-backed Codex observer: $path"
    }
}

$canonical = Get-Content -LiteralPath $canonicalPath -Raw
$staged = Get-Content -LiteralPath $stagedPath -Raw
if (-not (Test-NormalizedTextEqual $canonical $staged)) {
    throw 'Canonical and staged Codex observers differ.'
}

foreach ($pattern in @(
    'name: SFL Codex Review Observer',
    'github.event.sender.id == 199175422',
    'const appId = 1144995',
    'const appSlug = "chatgpt-codex-connector"',
    'const appOwner = "openai"',
    'Date.parse(comment.created_at || "")',
    'pull.data.state !== "open"',
    'SFL reviews require the default branch',
    'SFL_REVIEW_BASE_BRANCH: main',
    'differs from deployed SFL review base',
    'types: [opened, reopened, edited, synchronize]',
    'sfl-codex-review:pull:${pullNumber}:base:${currentBase.toLowerCase()}',
    'sfl-codex-review:pull:${pull.number}:base:${pull.base.sha.toLowerCase()}',
    'const baseMarker = `${headMarker}${currentBase.toLowerCase()};`',
    'const isOwnerRequest = comment =>',
    'comment.user && comment.user.login',
    'The pull request context changed while the Codex result was being published',
    'confirmedOpenPulls.length !== 1',
    'context=${contextToken}',
    'request:${request.comment.id}:at:${requestTime}',
    'This exact Codex request already has a terminal SFL gate',
    'pull-context:at:${contextChangeTime}',
    'base-advance:at:${baseAdvanceTime}',
    'sfl-codex-review-pull-context-${{ github.repository }}-${{ github.event.pull_request.number }}',
    'github.rest.actions.listWorkflowRuns',
    'const activeStatuses = ["requested", "queued", "in_progress", "waiting", "pending"]',
    'status => github.paginate(',
    'actions: read',
    'event: "pull_request_target"',
    'run.status !== "completed"',
    'Allowing a pull-context run time to materialize before publishing the Codex result',
    'const finalContextRuns = await activeContextRuns()',
    'the Codex result cannot supersede its invalidation',
    'sfl-codex-review-base-advance-${{ github.repository }}-${{ github.ref }}',
    'supersedesInvalidation(check.external_id, contextChangeTime, context.runId)',
    'supersedesInvalidation(check.external_id, baseAdvanceTime, context.runId)',
    'already has this exact invalidation; skipping rerun',
    'received a newer context or successful Codex gate while invalidation was running',
    'conflictingBaseRequest',
    'reviewContextToken(confirmedChecks.data.check_runs) !== contextToken',
    ':context:${encodeURIComponent(invalidationId)}:',
    'skipping stale invalidation',
    'github.rest.actions.getWorkflowRun',
    'successful Codex gate completed after this context change',
    'name: "SFL Reviewer Gate Runner"',
    'check.app.id === 15368',
    'action: "success"',
    'action: "failure"',
    'action: "ignore"'
)) {
    if ($canonical -notmatch [regex]::Escape($pattern)) {
        throw "Codex observer is missing contract text: $pattern"
    }
}

$helperMatches = [regex]::Matches(
    $canonical,
    '(?s)// BEGIN TESTABLE INVALIDATION ORDER\s*(.*?)\s*// END TESTABLE INVALIDATION ORDER'
)
if ($helperMatches.Count -ne 2) {
    throw "Expected two testable invalidation-order helpers, found $($helperMatches.Count)."
}
if ($helperMatches[0].Groups[1].Value -ne $helperMatches[1].Groups[1].Value) {
    throw 'Pull-context and base-advance invalidation-order helpers differ.'
}

$helperTest = @"
$($helperMatches[0].Groups[1].Value)
const cases = [
  ["sfl-codex-review:pull-context:at:2000:20", 1000, 10, true],
  ["sfl-codex-review:base-advance:at:1000:20:42", 1000, 10, true],
  ["sfl-codex-review:pull-context:at:999:99", 1000, 10, false],
  ["sfl-codex-review:pull-context:at:1000:10", 1000, 10, false],
  ["sfl-codex-review:pull:42:base:abc", 1000, 10, false],
];
for (const [externalId, eventTime, runId, expected] of cases) {
  const actual = supersedesInvalidation(externalId, eventTime, runId);
  if (actual !== expected) {
    throw new Error("Unexpected ordering for " + externalId + ": " + actual);
  }
}
"@
$helperTest | node -
if ($LASTEXITCODE -ne 0) {
    throw 'Invalidation-order helper tests failed.'
}

foreach ($legacyPath in @(
    'deployment\workflows\sfl-pr-review.md',
    'deployment\infrastructure\sfl-pr-review-recovery.yml',
    '.github\workflows\sfl-pr-review.md',
    '.github\workflows\sfl-pr-review.lock.yml',
    '.github\workflows\sfl-pr-review-recovery.yml'
)) {
    if (Test-Path -LiteralPath (Join-Path $repoRoot $legacyPath)) {
        throw "Retired OpenRouter reviewer artifact remains: $legacyPath"
    }
}

$policy = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\engine-policy.json') -Raw
if ($policy -match '(?i)openrouter|moonshotai/kimi|OPENROUTER_API_KEY') {
    throw 'Engine policy retained the OpenRouter reviewer configuration.'
}

Write-Output 'Subscription-backed Codex reviewer contract tests passed.'
