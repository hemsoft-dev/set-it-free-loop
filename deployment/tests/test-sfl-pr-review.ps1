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
    "format('SFL Codex comment #{0} {1} {2}', github.event.issue.number,",
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
    'statuses: write',
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
    'function requiredGateTarget(pull)',
    'function requiredGateRepair(check, statuses)',
    'const requiredGateSha = requiredGateTarget(confirmedState.pull)',
    'requiredPublished = await github.rest.repos.createCommitStatus({',
    'sha: requiredGateSha',
    'context: "SFL Reviewer Gate Runner"',
    'state: "pending"',
    'state: "success"',
    'state: "failure"',
    'const postSuccessState = await publicationState()',
    'state.openPulls.length !== 1',
    'historicalRequestIdsFromStatuses(statuses, comments)',
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
    'if (!existingInvalidation && !finalChecks.some(check =>',
    'if (!existingPendingCheck)',
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

if ($canonical -match [regex]::Escape('requiredPublished.data.sha')) {
    throw 'Merge-gate invalidation relies on a SHA absent from commit-status responses.'
}
if ($canonical -match [regex]::Escape('merge_commit_sha') -or
    $canonical -match [regex]::Escape('sha: currentMerge')) {
    throw 'The required reviewer gate still depends on GitHub''s regenerable synthetic merge commit.'
}
$statusWritePermissions = [regex]::Matches($canonical, '(?m)^\s+statuses: write\s*$')
if ($statusWritePermissions.Count -ne 4) {
    throw "Expected status-write permission on all four gate jobs, found $($statusWritePermissions.Count)."
}
$requiredStatusWrites = [regex]::Matches(
    $canonical,
    '(?s)github\.rest\.repos\.createCommitStatus\(\{(.*?)\}\);'
)
if ($requiredStatusWrites.Count -ne 11) {
    throw "Expected eleven required-gate status transitions, found $($requiredStatusWrites.Count)."
}
foreach ($statusWrite in $requiredStatusWrites) {
    if ($statusWrite.Groups[1].Value -notmatch '(?m)^\s+sha: (currentHead|requiredGateSha|pull\.head\.sha),\s*$') {
        throw 'A required-gate status transition does not target the immutable pull request head.'
    }
}

$classifierMatch = [regex]::Match(
    $canonical,
    '(?s)// BEGIN TESTABLE CODEX OBSERVER\s*(.*?)\s*// END TESTABLE CODEX OBSERVER'
)
if (-not $classifierMatch.Success) { throw 'Could not locate the Codex classifier.' }
$classifierTest = $classifierMatch.Groups[1].Value + "`n" + @'
const assert = require("node:assert/strict");
const head = "a".repeat(40);
const input = {eventName: "issue_comment", currentHead: head, resolvedSha: head,
  inlineCount: 0, requestMatchesCurrentBase: true, openPullCount: 1,
  artifact: {user: {id:199175422,login:"chatgpt-codex-connector[bot]"},
    performed_via_github_app:{id:1144995,slug:"chatgpt-codex-connector",owner:{login:"openai"}}}};
for (const text of ["Didn't", "Did not"]) {
  const artifact = {...input.artifact, body: `Codex Review: ${text} find any major issues.`};
  assert.equal(classifyCodexArtifact({...input, artifact}).action, "success");
  assert.notEqual(classifyCodexArtifact({...input, artifact, resolvedSha:"b".repeat(40)}).action, "success");
  assert.notEqual(classifyCodexArtifact({...input, artifact, requestMatchesCurrentBase:false}).action, "success");
  assert.notEqual(classifyCodexArtifact({...input, artifact:{...artifact,performed_via_github_app:{id:1}}}).action, "success");
}
for (const body of ["Codex Review: Did not find any major issues without checking.", "Did not find any major issues."]) {
  assert.notEqual(classifyCodexArtifact({...input, artifact:{...input.artifact,body}}).action, "success");
}
'@
$classifierTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Codex clean-result classifier tests failed.' }

$requestProvenance = [regex]::Match($canonical,
    '(?s)// BEGIN TESTABLE REQUEST CONTEXT PROVENANCE\s*(.*?)\s*// END TESTABLE REQUEST CONTEXT PROVENANCE')
$reviewProvenance = [regex]::Match($canonical,
    '(?s)// BEGIN TESTABLE REVIEW CONTEXT PROVENANCE\s*(.*?)\s*// END TESTABLE REVIEW CONTEXT PROVENANCE')
if (-not $requestProvenance.Success -or -not $reviewProvenance.Success -or
    $requestProvenance.Groups[1].Value -ne $reviewProvenance.Groups[1].Value) {
    throw 'Request invalidation and observer must use identical review provenance checks.'
}
$requestPublication = [regex]::Match($canonical,
    '(?s)const requestContextUsable = .*?core\.setFailed\(reason\);')
if (-not $requestPublication.Success) { throw 'Could not locate the pending-request publication block.' }
$requestPublicationTest = $requestProvenance.Groups[1].Value + "`n" + @'
const assert = require("node:assert/strict");
const publish = new (Object.getPrototypeOf(async function(){}).constructor)(
  "reviewContextUnambiguous", "registeredRequestExact", "confirmedChecks", "pullNumber", "currentBase", "commentId",
  "owner", "repo", "currentHead", "externalId", "comment", "github", "core", "existingPendingCheck",
'@ + "`n" + (ConvertTo-Json $requestPublication.Value -Compress) + "`n" + @'
);
(async () => {
  const base = "b".repeat(40);
  const token = "sfl-codex-review:pull-context:at:123:456";
  const context = {app:{id:15368},external_id:token,output:{text:JSON.stringify({schema:1,pull_number:1,action:"opened",base_sha:base})}};
  const variants = [
    ["opened", [context], "pending"],
    ["missing", [], "failure"],
    ["base advanced", [{...context,external_id:"sfl-codex-review:base-advance:at:123:456:1"}], "failure"],
    ["reopened", [{...context,output:{text:JSON.stringify({schema:1,pull_number:1,action:"reopened",base_sha:base})}}], "failure"],
    ["multiple", [context,{...context,external_id:"sfl-codex-review:pull-context:at:124:457"}], "failure"],
  ];
  for (const [name, checks, expected] of variants) {
    const audits = [], statuses = [], failures = [];
    const github = {rest:{checks:{create:async data => audits.push(data)},repos:{createCommitStatus:async data => statuses.push(data)}}};
    await publish(reviewContextUnambiguous,true,checks,1,base,3,"hemsoft-dev","fixture","a".repeat(40),
      "sfl-codex-review:request-pending:3",{data:{html_url:"https://github.com/hemsoft-dev/fixture/pull/1#issuecomment-3"}},
      github,{setFailed:message => failures.push(message)},false);
    assert.equal(audits.length,1,name); assert.equal(audits[0].conclusion,"failure",name);
    assert.equal(statuses.length,1,name); assert.equal(statuses[0].state,expected,name);
    assert.equal(failures.length,1,name);
    if (expected === "failure") {
      assert.match(audits[0].output.summary,/advance the branch to a new head/,name);
      assert.match(statuses[0].description,/new head/,name);
    }
  }
})().catch(error => {console.error(error);process.exitCode=1;});
'@
$requestPublicationTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Pending-request provenance publication tests failed.' }

$scopeStart = $canonical.IndexOf('            const pull = context.payload.pull_request;')
$scopeEnd = $canonical.IndexOf('            const workflowRun = await github.rest.actions.getWorkflowRun(', $scopeStart)
if ($scopeStart -lt 0 -or $scopeEnd -le $scopeStart) { throw 'Could not locate pull invalidation scope guard.' }
$scopeTest = @'
const assert = require("node:assert/strict");
const guard = new (Object.getPrototypeOf(async function(){}).constructor)("context","owner","repo","core","publish",
'@ + "`n" + (ConvertTo-Json ($canonical.Substring($scopeStart, $scopeEnd - $scopeStart) + 'await publish();') -Compress) + "`n" + @'
);
(async () => {
  for (const [base, full_name, expected] of [["main","hemsoft-dev/fixture",1],["develop","hemsoft-dev/fixture",0],["main","external/fork",0],["main",null,0]]) {
    let writes = 0;
    await guard({payload:{repository:{default_branch:"main"},pull_request:{base:{ref:base},head:{repo:full_name ? {full_name} : null}}}},
      "hemsoft-dev","fixture",{info:()=>{}},async()=>writes++);
    assert.equal(writes,expected);
  }
})().catch(error => {console.error(error);process.exitCode=1;});
'@
$scopeTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Unsupported pull-context invalidation tests failed.' }

$gateTargetMatch = [regex]::Match(
    $canonical,
    '(?s)// BEGIN TESTABLE REQUIRED GATE TARGET\s*(.*?)\s*// END TESTABLE REQUIRED GATE TARGET'
)
if (-not $gateTargetMatch.Success) {
    throw 'Could not locate the testable required-gate target helper.'
}
$gateTargetTest = @"
$($gateTargetMatch.Groups[1].Value)
const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const before = {head: {sha: head}, merge_commit_sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"};
const after = {head: {sha: head}, merge_commit_sha: "cccccccccccccccccccccccccccccccccccccccc"};
if (requiredGateTarget(before) !== head || requiredGateTarget(after) !== head) {
  throw new Error("Required gate target changed when only the synthetic merge SHA changed");
}
"@
$gateTargetTest | node -
if ($LASTEXITCODE -ne 0) {
    throw 'Required-gate target fixture tests failed.'
}

$gateRepairMatch = [regex]::Match(
    $canonical,
    '(?s)// BEGIN TESTABLE REQUIRED GATE REPAIR\s*(.*?)\s*// END TESTABLE REQUIRED GATE REPAIR'
)
if (-not $gateRepairMatch.Success) {
    throw 'Could not locate the testable required-gate repair helper.'
}
$gateRepairTest = @"
$($gateRepairMatch.Groups[1].Value)
const actions = state => ({context: "SFL Reviewer Gate Runner", state, creator: {login: "github-actions[bot]"}});
const successCheck = {status: "completed", conclusion: "success"};
const failureCheck = {status: "completed", conclusion: "failure"};
const cases = [
  ["partial success publication", successCheck, [actions("pending")], "success"],
  ["already repaired success", successCheck, [actions("success")], null],
  ["partial failure publication", failureCheck, [actions("pending")], "failure"],
  ["manual status cannot suppress repair", successCheck, [{...actions("success"), creator: {login: "HemSoft"}}], "success"],
  ["in-progress audit is not terminal", {status: "in_progress", conclusion: null}, [actions("pending")], null],
];
for (const [name, check, statuses, expected] of cases) {
  const repair = requiredGateRepair(check, statuses);
  const actual = repair && repair.state;
  if (actual !== expected) throw new Error(name + ": got " + actual + ", want " + expected);
}
"@
$gateRepairTest | node -
if ($LASTEXITCODE -ne 0) {
    throw 'Required-gate repair fixture tests failed.'
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

$pullContextInvalidation = [regex]::Match(
    $canonical,
    '(?s)  invalidate-pull-context:.*?(?=\r?\n  invalidate-base-advance:)'
)
if (-not $pullContextInvalidation.Success) {
    throw 'Could not locate the pull-context invalidation job.'
}
if ($pullContextInvalidation.Value -match [regex]::Escape('core.setFailed(reason);')) {
    throw 'Pull-context invalidation still fails its workflow job after publishing the blocking gate.'
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
const secondTerminal = completed_at => ({
  ...terminal(completed_at),
  external_id: `${requestGateExternalIdPrefix(pullNumber, currentBase, "none", second.id, Date.parse(second.created_at))}:artifact:r124`,
});
const cases = [
  {name: "overlap blocked", comments: [first, second], checks: [], want: [1]},
  {name: "revoked predecessor still blocks overlap", comments: [{...first,user:{login:"departed"}}, second], checks: [], want: [1]},
  {name: "terminal predecessor allows retry", comments: [first, second], checks: [terminal("2026-08-19T00:00:01.500Z")], want: [1, 2]},
  {name: "same-second terminal fails closed", comments: [first, second], checks: [terminal("2026-08-19T00:00:02Z")], want: [1]},
  {name: "edited marker blocks the head", comments: [first, editedSecond], checks: [terminal("2026-08-19T00:00:01.500Z")], want: []},
  {name: "same-second body mutation blocks the head", comments: [first, sameSecondBodyMutation], checks: [terminal("2026-08-19T00:00:01.500Z")], want: []},
  {name: "late terminal does not authorize retry", comments: [first, second], checks: [terminal("2026-08-19T00:00:02.500Z")], want: [1]},
  {name: "third request cannot skip unresolved overlapping predecessor", comments: [first, second, recoveredRetry], checks: [terminal("2026-08-19T00:00:03Z")], want: [1]},
  {name: "serial completed requests allow third request", comments: [first, second, recoveredRetry], checks: [terminal("2026-08-19T00:00:01.500Z"), secondTerminal("2026-08-19T00:00:03Z")], want: [1, 2, 4]},
  {name: "same-second immediate predecessor terminal fails closed", comments: [first, second, recoveredRetry], checks: [terminal("2026-08-19T00:00:01.500Z"), secondTerminal(recoveredRetry.created_at)], want: [1, 2]},
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
const pendingFirst = {comment: first, requestTime: firstTime};
const supersessionCases = [
  {name: "overlap supersedes before first terminal", comments: [first, second], registrations: registeredRequestIds, want: true},
  {name: "same-second overlap uses comment order", comments: [first, {...second, created_at: first.created_at, updated_at: first.created_at}], registrations: registeredRequestIds, want: true},
  {name: "multiple overlapping requests supersede", comments: [first, second, recoveredRetry], registrations: registeredRequestIds, want: true},
  {name: "different context does not supersede", comments: [first, otherContext], registrations: registeredRequestIds, want: false},
  {name: "edited overlap still supersedes", comments: [first, editedSecond], registrations: registeredRequestIds, want: true},
  {name: "mutated overlap still supersedes", comments: [first, sameSecondBodyMutation], registrations: registeredRequestIds, want: true},
  {name: "unregistered overlap cannot supersede", comments: [first, second], registrations: new Set([1]), want: false},
  {name: "newly observed registration supersedes", comments: [first, unregistered], registrations: refreshedRegistrations, want: true},
  {name: "replayed original request does not supersede", comments: [first], registrations: registeredRequestIds, want: false},
];
for (const fixture of supersessionCases) {
  const got = hasNewerRegisteredRequest(fixture.comments, fixture.registrations, "none", pendingFirst);
  if (got !== fixture.want) throw new Error(`${fixture.name}: got ${got}, want ${fixture.want}`);
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

# Run the exact authorization helper embedded in both workflow jobs.
$authorization = [regex]::Matches($canonical, '(?s)// BEGIN TESTABLE REQUESTER AUTHORIZATION\s*(.*?)\s*// END TESTABLE REQUESTER AUTHORIZATION')
if ($authorization.Count -ne 2 -or $authorization[0].Groups[1].Value -cne $authorization[1].Groups[1].Value) {
    throw 'Request invalidation and result observation do not share the same authorization policy.'
}
$authorizationTest = @'
const assert = require("node:assert/strict");
let owner = "hemsoft-dev";
const repo = "consumer";
let role = "write", responseUser = "member", fails = false, failureStatus=500, calls = 0;
const github = {rest:{repos:{getCollaboratorPermissionLevel: async ({username}) => {
  calls++;
  if (fails) throw Object.assign(new Error("lookup failure"), {status:failureStatus});
  return {data:{permission:role,user:{login:responseUser}}};
}}}};
'@ + "`n" + $authorization[0].Groups[1].Value + "`n" + @'
(async () => {
  for (const permission of ["admin", "maintain", "write"]) {
    role=permission; assert.equal(await requesterAllowed("member"),true);
  }
  for (const permission of ["read", "triage", "none", ""]) {
    role=permission; assert.equal(await requesterAllowed("member"),false);
  }
  role="write"; responseUser="other";
  await assert.rejects(requesterAllowed("member"), /identity mismatch/);
  responseUser="member"; fails=true;
  await assert.rejects(requesterAllowed("member"), /lookup failure/);
  for (const status of [403,404]) { failureStatus=status; assert.equal(await requesterAllowed("member"),false); }
  fails=false;
  const before=calls;
  assert.equal(await requesterAllowed("github-actions[bot]"),false);
  assert.equal(await requesterAllowed("member/permission"),false);
  assert.equal(calls,before);
  owner="HemSoft";
  assert.equal(await requesterAllowed("HemSoft"),true);
  assert.equal(await requesterAllowed("other"),false);
  assert.equal(calls,before);
})().catch(error => { console.error(error); process.exitCode=1; });
'@
$authorizationTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Live permission authorization fixtures failed.' }

$registration = [regex]::Match($canonical, '(?s)// BEGIN TESTABLE REQUEST REGISTRATION\s*(.*?)\s*// END TESTABLE REQUEST REGISTRATION')
if (-not $registration.Success) { throw 'Missing testable request registration.' }
$registrationTest = @'
const assert = require("node:assert/strict");
const owner="hemsoft-dev", pullNumber=42, currentBase="a".repeat(40);
const baseMarker=`<!-- sfl-codex-review:head=${"b".repeat(40)};base=${currentBase};`;
const requestTargetPrefix = "https://github.com/hemsoft-dev/consumer/pull/42#issuecomment-";
const triggerComments = [{id:1,user:{login:"member"}},{id:2,user:{login:"other"}}];
const requesterPermissions = new Map([["member",true],["other",true],["reader",false]]);
const isOwnerRequest = comment => Boolean(comment) && requesterPermissions.get(comment.user.login.toLowerCase()) === true;
'@ + "`n" + $registration.Groups[1].Value + "`n" + @'
const registration = (creator,id,host="https://github.com") => ({
 context:"SFL Codex Review Request Registry", creator:{login:creator},
 target_url:`${host}/hemsoft-dev/consumer/pull/42#issuecomment-${id}`,
});
assert.deepEqual([...registeredRequestIdsFromStatuses([registration("member",1)])],[1]);
assert.equal(registeredRequestIdsFromStatuses([registration("member",2)]).size,0);
assert.equal(registeredRequestIdsFromStatuses([registration("reader",1)]).size,0);
assert.equal(registeredRequestIdsFromStatuses([registration("member",1,"https://attacker.test")]).size,0);
assert.equal(registeredRequestIdsFromStatuses([registration("member",99)]).size,0);
assert.equal(hasHistoricalBaseConflict(triggerComments,[registration("member",99)]),true);
assert.deepEqual([...registeredRequestIdsFromStatuses([registration("member",1),registration("member",1)])],[1]);
requesterPermissions.set("member",false);
assert.equal(registeredRequestIdsFromStatuses([registration("member",1)]).size,0);
assert.deepEqual([...historicalRequestIdsFromStatuses([registration("member",1)])],[1]);
assert.equal(historicalRequestIdsFromStatuses([registration("reader",1)]).size,0);
const edited={id:1,user:{login:"member"},body:"marker removed",created_at:"2026-08-19T00:00:00Z",updated_at:"2026-08-19T00:00:01Z"};
assert.equal(hasHistoricalBaseConflict([edited],[registration("member",1)]),true);
const durable={...registration("member",1),description:`SFL Codex request comment 1 for PR #42 base ${currentBase}`};
assert.equal(hasHistoricalBaseConflict([edited],[durable]),false);
assert.equal(hasHistoricalBaseConflict([edited],[{...durable,description:durable.description.replace(currentBase,"c".repeat(40))}]),true);
for (const id of ["01","+1","1?x=1"]) assert.equal(historicalRequestIdsFromStatuses([registration("member",id)]).size,0);
'@
$registrationTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Registry creator/author binding fixtures failed.' }

Write-Output 'Subscription-backed Codex reviewer contract tests passed.'

# Exercise candidate-only lookups and live authorization of pending invalidations.
$refreshHelper = [regex]::Match($canonical, '(?s)// BEGIN TESTABLE REQUESTER REFRESH\s*(.*?)\s*// END TESTABLE REQUESTER REFRESH')
$activeHelper = [regex]::Match($canonical, '(?s)// BEGIN TESTABLE ACTIVE INVALIDATIONS\s*(.*?)\s*// END TESTABLE ACTIVE INVALIDATIONS')
if (-not $refreshHelper.Success -or -not $activeHelper.Success) { throw 'Missing request selection helpers.' }
$selectionTest = @'
const assert = require("node:assert/strict");
let requesterPermissions = new Map();
const requestTargetPrefix = "https://github.com/hemsoft-dev/consumer/pull/42#issuecomment-";
const headMarker = "<!-- sfl-codex-review:head=abc;base=";
let calls=[];
const requesterAllowed = async login => { calls.push(login); return login === "member"; };
const owner="hemsoft-dev", repo="consumer", pullNumber=42,currentHead="abc";
const context={runId:99};
const runs=[
 {id:1,event:"issue_comment",actor:{login:"outsider"}},
 {id:2,event:"issue_comment",actor:{login:"member"}},
 {id:3,event:"push"},
 {id:99,event:"push"}
];
const github={rest:{actions:{listWorkflowRuns:{}}},paginate:async (_method,args) => args.status === "queued" ? runs : []};
const isActiveInvalidationRun = () => true;
const invalidationRequestCommentId = () => null;
'@ + "`n" + $refreshHelper.Groups[1].Value + "`n" + $activeHelper.Groups[1].Value + "`n" + @'
(async () => {
 const comments=[
  {id:1,user:{login:"irrelevant"},body:"ordinary comment"},
  {id:2,user:{login:"member"},body:"@codex review\n"+headMarker+"xyz; -->"},
  {id:3,user:{login:"outsider"},body:"registered comment"}
 ];
 const statuses=[{context:"SFL Codex Review Request Registry",target_url:requestTargetPrefix+3,creator:{login:"forged"}}];
 await refreshRequesterPermissions(comments,statuses);
 assert.deepEqual(calls,["member","outsider"]);
 assert.equal(requesterPermissions.get("member"),true);
 assert.equal(requesterPermissions.get("outsider"),false);
 assert.equal(requesterPermissions.has("forged"),false);
 calls=[];
 assert.deepEqual((await activeInvalidationRuns()).map(r=>r.id),[2,3]);
 assert.deepEqual(calls,["outsider","member"]);
})().catch(error => { console.error(error); process.exitCode=1; });
'@
$selectionTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Request candidate and invalidation authorization fixtures failed.' }

if ([regex]::Matches($canonical, '(?m)^\s*queue:').Count -ne 1) { throw 'Only the observer has an explicitly validated queue setting.' }
$observerConcurrency = [regex]::Match($canonical, '(?s)  observe:\s*.*?    concurrency:\s*(.*?)    runs-on:')
if (-not $observerConcurrency.Success -or $observerConcurrency.Groups[1].Value -notmatch '(?m)^\s*queue: max\s*$' -or
    $observerConcurrency.Groups[1].Value -notmatch 'cancel-in-progress: false') {
    throw 'Observer must serialize artifacts without replacing queued events.'
}

if ($canonical -notmatch [regex]::Escape('update this PR branch from ${baseRef} to a new head, then request a Codex review')) {
    throw 'Base-advance recovery must explain the new-head prerequisite.'
}
if ($canonical -match [regex]::Escape('advanced to ${pull.base.sha}; request a new Codex review with gh sfl review --retry')) {
    throw 'Base-advance recovery must not recommend an unsupported same-head retry.'
}

# Execute production admission and final-state predicates, including their writes.
$contextMatch = [regex]::Match($canonical, '(?s)// BEGIN TESTABLE SUPPORTED PULL CONTEXT\s*(.*?)\s*// END TESTABLE SUPPORTED PULL CONTEXT')
$terminalMatch = [regex]::Match($canonical, '(?s)// BEGIN TESTABLE TERMINAL PUBLICATION PREFLIGHT\s*(.*?)\s*// END TESTABLE TERMINAL PUBLICATION PREFLIGHT')
$changedMatch = [regex]::Match($canonical, '(?s)const publicationChanged = state =>(.*?);\s*const publicationChangeReason')
if (-not $contextMatch.Success -or -not $terminalMatch.Success -or -not $changedMatch.Success) {
    throw 'Missing cutover publication guards.'
}
$cutoverTest = @'
const assert = require('node:assert/strict');
const AsyncFunction = Object.getPrototypeOf(async function(){}).constructor;
const testContext = new Function('owner','repo','headRepo','pull','context',
'@ + "`n" + ($contextMatch.Groups[1].Value + "`nreturn supportedPullContext;" | ConvertTo-Json -Compress) + @'
);
const event = (base, action, changes, head='hemsoft-dev/test') => testContext('hemsoft-dev','test',head,{base:{ref:base}},{payload:{action,changes,repository:{default_branch:'main'}}});
assert.equal(event('main','opened',undefined),true);
assert.equal(event('release','opened',undefined),false);
assert.equal(event('release','edited',{base:{ref:{from:'main'}}}),true);
assert.equal(event('release','edited',{title:{from:'old'}}),false);
assert.equal(event('release-b','edited',{base:{ref:{from:'release-a'}}}),false);
assert.equal(event('main','edited',{base:{ref:{from:'release'}}}),true);
assert.equal(event('release','edited',{base:{ref:{from:'main'}}},'fork/test'),false);
const preflight = new AsyncFunction('publicationState','core','publish',
'@ + "`n" + (
    'const pullNumber=42,currentHead="head",currentBase="base",contextToken="none";' +
    'const publicationChangeReason=()=>"changed";const publicationChanged = state =>' +
    $changedMatch.Groups[1].Value + ";`n" + $terminalMatch.Groups[1].Value + "`nawait publish();" | ConvertTo-Json -Compress) + @'
);
(async()=>{
const valid={requestStillAuthorized:true,pull:{state:'open',head:{sha:'head'},base:{sha:'base'}},openPulls:[{number:42}],supersededRequest:false,lifecycleChanged:false,invalidationRuns:[],contextToken:'none',contextUnambiguous:true};
const changedStates=[{...valid,requestStillAuthorized:false},{...valid,pull:{...valid.pull,head:{sha:'new'}}},{...valid,pull:{...valid.pull,base:{sha:'new'}}},{...valid,pull:{...valid.pull,state:'closed'}},{...valid,openPulls:[]},{...valid,supersededRequest:true},{...valid,lifecycleChanged:true},{...valid,invalidationRuns:[{id:1}]},{...valid,contextToken:'new'},{...valid,contextUnambiguous:false}];
for (const [state,want] of [[valid,1],...changedStates.map(state=>[state,0])]) {
 let writes=0, snapshots=0;
 await preflight(async()=>{snapshots++;return state},{info:()=>{}},async()=>{writes++});
 assert.equal(snapshots,1);assert.equal(writes,want);
}
})().catch(error=>{console.error(error);process.exitCode=1});
'@
$cutoverTest | node -
if ($LASTEXITCODE -ne 0) { throw 'Cutover admission or terminal publication preflight regression failed.' }

& node (Join-Path $PSScriptRoot "test-final-review-history.cjs") $canonicalPath
if ($LASTEXITCODE -ne 0) { throw "Final registered-history production regression failed." }

& node (Join-Path $PSScriptRoot "test-invalidation-recovery.cjs") $canonicalPath
if ($LASTEXITCODE -ne 0) { throw "Production invalidation recovery regression failed." }
