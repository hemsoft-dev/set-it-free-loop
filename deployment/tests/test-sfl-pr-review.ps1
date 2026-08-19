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

$invalidNestedPaginationMapperMatches = [regex]::Matches(
    $canonical,
    'response\s*=>\s*response\.data\.[A-Za-z_][A-Za-z0-9_]*'
)
if ($invalidNestedPaginationMapperMatches.Count -gt 0) {
    @'
const mappedPage = (response => response.data.items)({data: []});
const items = [mappedPage].flat();
items.filter(item => item.id);
'@ | node -
    if ($LASTEXITCODE -eq 0) {
        throw 'The invalid nested pagination mapper no longer reproduces the observer crash.'
    }
    throw "Codex observer has $($invalidNestedPaginationMapperMatches.Count) pagination mapper(s) that read a nested collection from Octokit's normalized response.data array."
}

foreach ($pattern in @(
    'name: SFL Codex Review Observer',
    "format('SFL Codex review request #{0}', github.event.issue.number)",
    'github.event.sender.id == 199175422',
    'const appId = 1144995',
    'const appSlug = "chatgpt-codex-connector"',
    'const appOwner = "openai"',
    'Date.parse(comment.created_at || "")',
    'Date.parse(comment.updated_at || "")',
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
    'Codex artifact and registered request have ambiguous same-second ordering',
    'invalidate-review-request:',
    'statuses: read',
    "vars.SFL_ENABLED != 'false'",
    'SFL Codex Review Request Registry',
    'sfl-codex-review:request-pending:${commentId}',
    'const terminalRequestIdentity = `:request:${commentId}:at:`',
    'This registered Codex review request already has a terminal SFL gate',
    'registrations.has(comment.id)',
    'const publishedCompletedAt = new Date().toISOString()',
    'completed_at: publishedCompletedAt',
    'status: "in_progress"',
    'SFL Codex review validating',
    'conclusion: "success"',
    'GET /repos/{owner}/{repo}/issues/{issue_number}/events',
    'event.event === "closed" || event.event === "reopened"',
    'lifecycleToken !== initialLifecycleToken',
    'state.invalidationRuns.length > 0',
    'const publicationState = async () =>',
    'const postSuccessState = await publicationState()',
    'state.openPulls.length !== 1',
    'registeredRequestIdsFromStatuses(statuses)',
    'let latestPotentialRequestRegistered = false',
    'visibleRegistrations.has(latestPotentialRequest.comment.id)',
    'if (attempt < 11) await new Promise(resolve => setTimeout(resolve, 5000))',
    'No registered Codex review request materialized for this artifact',
    'context=${contextToken}',
    'function requestGateExternalId(',
    'function requestGateExternalIdPrefix(',
    'request:${requestId}:at:${requestTime}',
    ':artifact:${artifactIdentity}',
    'const artifactAlreadyConsumed = artifactChecks.some(',
    'pending-invalidation:${pendingRun.id}:artifact:${artifactIdentity}',
    'This exact Codex artifact already completed a review request',
    'const pendingRequest = candidateRequests[0]',
    'contextMatch = /;context=(.*?) -->/.exec',
    'pendingRequest.comment.id',
    'const artifactBindsCurrentHead = eventName === "issue_comment"',
    'it cannot complete the pending request',
    'const eligibleReviewRequests = (',
    'completedTime < nextRequestTime',
    'const hasNewerMatchingRequest = (comments, checkRuns, statuses) =>',
    'comment.id > request.comment.id',
    'A newer Codex review request superseded this artifact before publication',
    'supersededRequest',
    'This Codex review request already has a terminal SFL gate',
    'sfl-codex-review-serialize-${{ github.repository }}-${{ github.event.issue.number || github.event.pull_request.number }}',
    'const pendingRequestAlreadyTerminal = artifactChecks.some(',
    'const externalIdPrefix = requestGateExternalIdPrefix(',
    'pull-context:at:${contextChangeTime}',
    'base-advance:at:${baseAdvanceTime}',
    'sfl-codex-review-pull-context-${{ github.repository }}-${{ github.event.pull_request.number }}',
    'github.rest.actions.listWorkflowRuns',
    'const initialChecks = await github.paginate(',
    'const checks = await github.paginate(',
    'const activeStatuses = ["requested", "queued", "in_progress", "waiting", "pending"]',
    'const events = ["pull_request_target", "push", "issue_comment"]',
    'activeStatuses.map(status => github.paginate(',
    'events.includes(run.event)',
    'run.display_title === `SFL Codex review request #${pullNumber}`',
    'actions: read',
    'if (run.event === "push") return true',
    'run.actor && run.actor.login',
    'run.status === "completed"',
    'Allowing an invalidation run time to materialize before publishing the Codex result',
    'const finalInvalidationRuns = await activeInvalidationRuns()',
    'the Codex result cannot supersede its invalidation',
    'sfl-codex-review-base-advance-${{ github.repository }}-${{ github.ref }}',
    'const batchSize = 10',
    'Promise.allSettled(batch.map(invalidatePull))',
    'Base-advance invalidation failed after attempting every pull request',
    'supersedesInvalidation(check.external_id, contextChangeTime, context.runId)',
    'supersedesInvalidation(check.external_id, baseAdvanceTime, context.runId)',
    'already has this exact invalidation; skipping rerun',
    'received a newer context or successful Codex gate while invalidation was running',
    'conflictingBaseRequest',
    'reviewContextToken(confirmedChecks) !== contextToken',
    ':context:${encodeURIComponent(invalidationId)}:',
    'skipping stale invalidation',
    'github.rest.actions.getWorkflowRun',
    'successful Codex gate completed after this context change',
    'if (context.payload.deleted)',
    'repository.data.default_branch',
    'Repository default branch changed from deployed SFL review base',
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

if ($canonical -match [regex]::Escape('if (context.payload.deleted) return;')) {
    throw 'Base-branch deletion events are still discarded instead of invalidating gates after a rename.'
}
if ($canonical -match [regex]::Escape('events.flatMap(') -or
    $canonical -match [regex]::Escape('events.map(event => github.paginate(')) {
    throw 'Codex observer still multiplies or unboundedly paginates workflow-run queries by event.'
}
if ($canonical -notmatch '(?s)invalidate-base-advance:.*?timeout-minutes: 30' -or
    $canonical -match [regex]::Escape('for (const pull of openPulls)')) {
    throw 'Base-advance invalidation is still a short, serial repository-wide sweep.'
}

$artifactBindingIndex = $canonical.IndexOf('const artifactBindsCurrentHead')
$pendingRequestIndex = $canonical.IndexOf('const pendingRequest = candidateRequests[0]')
if ($artifactBindingIndex -lt 0 -or $pendingRequestIndex -lt 0 -or
    $artifactBindingIndex -gt $pendingRequestIndex) {
    throw 'Codex artifact binding is not validated before pending request selection.'
}

$inProgressIndex = $canonical.IndexOf('status: "in_progress"')
$confirmationIndex = if ($inProgressIndex -ge 0) {
    $canonical.IndexOf('const confirmedState = await publicationState()', $inProgressIndex)
} else { -1 }
$successIndex = if ($confirmationIndex -ge 0) {
    $canonical.IndexOf('conclusion: "success"', $confirmationIndex)
} else { -1 }
if ($inProgressIndex -lt 0 -or $confirmationIndex -lt 0 -or $successIndex -lt 0 -or
    $inProgressIndex -gt $confirmationIndex -or $confirmationIndex -gt $successIndex) {
    throw 'Codex success is exposed before the final pull-context confirmation.'
}

$eligibilityMatch = [regex]::Match(
    $canonical,
    '(?s)// BEGIN TESTABLE REQUEST ELIGIBILITY\s*(.*?)\s*// END TESTABLE REQUEST ELIGIBILITY'
)
if (-not $eligibilityMatch.Success) {
    throw 'Could not locate the testable request-eligibility block.'
}
$eligibilityTest = @'
const owner = "HemSoft";
const pullNumber = 42;
const currentBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const baseMarker = `<!-- sfl-codex-review:head=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb;base=${currentBase};`;
const isOwnerRequest = comment =>
  ((comment.user && comment.user.login) || "").toLowerCase() === owner.toLowerCase();
const registeredRequestIds = new Set([1, 2, 3, 4]);
function requestGateExternalIdPrefix(pullNumber, currentBase, contextToken, requestId, requestTime) {
  return `sfl-codex-review:pull:${pullNumber}:base:${currentBase.toLowerCase()}:context:${encodeURIComponent(contextToken)}:request:${requestId}:at:${requestTime}`;
}
'@ + "`n" + $eligibilityMatch.Groups[1].Value + "`n" + @'
const comment = (id, created_at, contextToken) => ({
  id,
  created_at,
  updated_at: created_at,
  user: {login: "HemSoft"},
  body: `@codex review\n\n${baseMarker}context=${contextToken} -->`,
});
const first = comment(1, "2026-08-19T00:00:01Z", "none");
const second = comment(2, "2026-08-19T00:00:02Z", "none");
const otherContext = comment(3, "2026-08-19T00:00:02Z", "base-advance");
const recoveredRetry = comment(4, "2026-08-19T00:00:04Z", "none");
const unregistered = comment(5, "2026-08-19T00:00:05Z", "none");
const editedSecond = {...second, updated_at: "2026-08-19T00:00:03Z"};
const sameSecondBodyMutation = {...second, body: `please @codex review\n\n${baseMarker}context=none -->`};
const firstTime = Date.parse(first.created_at);
const firstPrefix = requestGateExternalIdPrefix(pullNumber, currentBase, "none", first.id, firstTime);
const terminal = completed_at => ({
  app: {id: 15368},
  status: "completed",
  completed_at,
  external_id: `${firstPrefix}:artifact:r123`,
});
const cases = [
  {name: "overlap blocked", comments: [first, second], checks: [], want: [1]},
  {name: "terminal predecessor allows retry", comments: [first, second], checks: [terminal("2026-08-19T00:00:01.500Z")], want: [1, 2]},
  {name: "same-second terminal fails closed", comments: [first, second], checks: [terminal("2026-08-19T00:00:02Z")], want: [1]},
  {name: "edited marker is rejected", comments: [first, editedSecond], checks: [terminal("2026-08-19T00:00:01.500Z")], want: [1]},
  {name: "same-second body mutation is rejected", comments: [first, sameSecondBodyMutation], checks: [terminal("2026-08-19T00:00:01.500Z")], want: [1]},
  {name: "late terminal does not authorize retry", comments: [first, second], checks: [terminal("2026-08-19T00:00:02.500Z")], want: [1]},
  {name: "later retry recovers after overlap", comments: [first, second, recoveredRetry], checks: [terminal("2026-08-19T00:00:03Z")], want: [1, 4]},
  {name: "different contexts are independent", comments: [first, otherContext], checks: [], want: [1, 3]},
  {name: "unregistered owner marker is rejected", comments: [first, unregistered], checks: [terminal("2026-08-19T00:00:01.500Z")], want: [1]},
];
const sameSecondCandidates = eligibleReviewRequests([first], [], Date.parse(first.created_at))
  .map(candidate => candidate.comment.id);
if (JSON.stringify(sameSecondCandidates) !== JSON.stringify([1])) {
  throw new Error(`same-second request lookup: got ${JSON.stringify(sameSecondCandidates)}, want [1]`);
}
const refreshedRegistrations = new Set([1, 2, 3, 4, 5]);
const refreshedCandidates = eligibleReviewRequests(
  [first, unregistered],
  [terminal("2026-08-19T00:00:01.500Z")],
  Number.POSITIVE_INFINITY,
  refreshedRegistrations,
).map(candidate => candidate.comment.id);
if (JSON.stringify(refreshedCandidates) !== JSON.stringify([1, 5])) {
  throw new Error(`refreshed registrations: got ${JSON.stringify(refreshedCandidates)}, want [1,5]`);
}
for (const fixture of cases) {
  const got = eligibleReviewRequests(fixture.comments, fixture.checks, Number.POSITIVE_INFINITY)
    .map(candidate => candidate.comment.id);
  if (JSON.stringify(got) !== JSON.stringify(fixture.want)) {
    throw new Error(`${fixture.name}: got ${JSON.stringify(got)}, want ${JSON.stringify(fixture.want)}`);
  }
}
'@
$eligibilityTest | node -
if ($LASTEXITCODE -ne 0) {
    throw 'Request-eligibility fixture tests failed.'
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
