[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$sourcePath = Join-Path $repoRoot 'deployment\workflows\sfl-pr-review.md'
$stagedPath = Join-Path $repoRoot '.github\workflows\sfl-pr-review.md'
$lockPath = Join-Path $repoRoot '.github\workflows\sfl-pr-review.lock.yml'
$actionsLockPath = Join-Path $repoRoot '.github\aw\actions-lock.json'
$failures = [System.Collections.Generic.List[string]]::new()

foreach ($path in @($sourcePath, $stagedPath, $lockPath, $actionsLockPath)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        $failures.Add("Missing reviewer artifact: $path")
    }
}

if ($failures.Count -eq 0) {
    $source = Get-Content -LiteralPath $sourcePath -Raw
    $staged = Get-Content -LiteralPath $stagedPath -Raw
    $lock = Get-Content -LiteralPath $lockPath -Raw
    $actionsLock = Get-Content -LiteralPath $actionsLockPath -Raw

    if ($source -ne $staged) {
        $failures.Add('Canonical and staged reviewer Markdown differ.')
    }

    $requiredSourcePatterns = @(
        '(?m)^source: HemSoft/set-it-free-loop/deployment/workflows/sfl-pr-review\.md@main\r?$',
        '(?m)^  workflow_dispatch:\r?$',
        'Validate trusted review context',
        'publish_review_provenance:',
        'Initialize review evidence',
        'sfl-review:\$\{pullNumber\}:\$\{baseSha\}:\$\{headSha\}:\$\{runId\}',
        'Verify pull request base and head before safe outputs',
        'commit-id: "\$\{\{ inputs\.head_sha \}\}"',
        'resolve-sfl-review-thread:',
        "if: needs\.safe_outputs\.result == 'success'",
        'isOutdated',
        '\(!thread\.isResolved && !thread\.isOutdated\)',
        'const unresolvedOutdatedThreadIds = \[\]',
        'for \(const threadId of unresolvedOutdatedThreadIds\)',
        'if \(thread\.isResolved\) \{\s*core\.info\(`SFL review thread \$\{threadId\} is already resolved`\);\s*continue;',
        'whose thread GitHub reports as outdated',
        'Never request resolution for a live unresolved thread',
        'name: safe-outputs-items',
        'Download reviewer agent output',
        'Expected exactly one immutable agent submitted-review item',
        'Published review manifest does not match the immutable agent verdict',
        'Review body contains unresolved template placeholder',
        'const immutableFindingCount = agentReviewComments\.length',
        'const gateApproved =\s*\r?\n\s*verdictApproved',
        'unresolvedFindingCount === 0',
        'Initialized SFL review evidence does not match this run',
        'COPILOT_PROVIDER_BASE_URL: https://openrouter\.ai/api/v1',
        'COPILOT_MODEL: moonshotai/kimi-k3',
        '(?m)^  providers:\r?$',
        '(?m)^    github-copilot:\r?$',
        '(?m)^        "moonshotai/kimi-k3":\r?$',
        '(?m)^            input: "3e-06"\r?$',
        '(?m)^            output: "1\.5e-05"\r?$'
    )
    foreach ($pattern in $requiredSourcePatterns) {
        if ($source -notmatch $pattern) {
            $failures.Add("Reviewer source is missing contract pattern: $pattern")
        }
    }

    $requiredLockPatterns = @(
        '"compiler_version":"v0\.86\.2"',
        '"engine_base_url_customized":true',
        '"agent_model":"moonshotai/kimi-k3"',
        '"OPENROUTER_API_KEY"',
        'GH_AW_INPUTS_BASE_SHA: \$\{\{ inputs\.base_sha \}\}',
        'GH_AW_INPUTS_HEAD_SHA: \$\{\{ inputs\.head_sha \}\}',
        'Validate trusted review context',
        'publish_review_provenance:',
        'Initialize review evidence',
        'Verify pull request base and head before safe outputs',
        'SFL_SAFE_OUTPUT_ITEMS: /tmp/sfl-review-safe-outputs/safe-output-items\.jsonl',
        'Initialized SFL review evidence does not match this run',
        'isOutdated',
        '\(!thread\.isResolved && !thread\.isOutdated\)',
        'const unresolvedOutdatedThreadIds = \[\]',
        'for \(const threadId of unresolvedOutdatedThreadIds\)',
        '\{\{#runtime-import \.github/workflows/sfl-pr-review\.md\}\}'
    )
    foreach ($pattern in $requiredLockPatterns) {
        if ($lock -notmatch $pattern) {
            $failures.Add("Compiled reviewer is missing contract pattern: $pattern")
        }
    }

    $compiledPricingPattern = '\\"providers\\":\{\\"github-copilot\\":\{\\"models\\":\{\\"moonshotai/kimi-k3\\":\{\\"cost\\":\{\\"input\\":\\"3e-06\\",\\"output\\":\\"1\.5e-05\\"\}\}\}\}\}'
    $compiledPricingCount = [regex]::Matches($lock, $compiledPricingPattern).Count
    if ($compiledPricingCount -ne 2) {
        $failures.Add("Expected explicit Kimi pricing in both primary and threat-detection firewall configurations; found $compiledPricingCount occurrence(s).")
    }
    if ($source -match 'default-ai-credits-pricing' -or
        $lock -match 'defaultAiCreditsPricing') {
        $failures.Add('Reviewer still relies on generic fallback pricing instead of explicit Kimi pricing.')
    }

    if ($source -match 'copilot-requests:\s*write' -or
        $lock -match 'copilot-requests:\s*write') {
        $failures.Add('HemSoft OpenRouter reviewer unexpectedly requests Copilot billing permission.')
    }
    if ($source -match 'needs\.detection\.result') {
        $failures.Add('Thread resolution references an undeclared detection dependency.')
    }
    if ($source -match '(?im)^\s*(?:app-id|github-app-id|installation-id|client-id):\s*(?:\d+|Iv[A-Za-z0-9]+)\s*$') {
        $failures.Add('Reviewer source contains a hard-coded GitHub App or installation identity.')
    }
    if ($actionsLock -notmatch 'github/gh-aw-actions/setup@v0\.86\.2') {
        $failures.Add('Action lock does not pin the compiler-matched gh-aw setup action.')
    }

    $eligibilityGuardIndex = $source.IndexOf('(!thread.isResolved && !thread.isOutdated)')
    $resolvedGuardIndex = $source.IndexOf('if (thread.isResolved)')
    $resolutionQueueIndex = $source.IndexOf('unresolvedOutdatedThreadIds.push(threadId)')
    $resolutionMutationIndex = $source.IndexOf('resolveReviewThread(input: { threadId: $threadId })')
    if ($eligibilityGuardIndex -lt 0 -or
        $resolvedGuardIndex -le $eligibilityGuardIndex -or
        $resolutionQueueIndex -le $resolvedGuardIndex -or
        $resolutionMutationIndex -le $resolutionQueueIndex) {
        $failures.Add('Thread eligibility, idempotency, queueing, and mutation are not ordered fail-closed.')
    }
}

function Get-ExpectedThreadResolutionEligibility {
    param(
        [bool] $IsResolved,
        [bool] $IsOutdated
    )

    return $IsResolved -or $IsOutdated
}

$threadStateCases = @(
    @{ Name = 'live unresolved thread'; Resolved = $false; Outdated = $false; Expected = $false },
    @{ Name = 'outdated unresolved thread'; Resolved = $false; Outdated = $true; Expected = $true },
    @{ Name = 'already resolved current thread'; Resolved = $true; Outdated = $false; Expected = $true },
    @{ Name = 'already resolved outdated thread'; Resolved = $true; Outdated = $true; Expected = $true }
)
foreach ($case in $threadStateCases) {
    $actual = Get-ExpectedThreadResolutionEligibility `
        -IsResolved $case.Resolved `
        -IsOutdated $case.Outdated
    if ($actual -ne $case.Expected) {
        $failures.Add("Thread eligibility case failed: $($case.Name)")
    }
}

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ }
    throw "SFL review artifact contract failed with $($failures.Count) finding(s)."
}

Write-Output 'SFL review artifact contract passed.'
