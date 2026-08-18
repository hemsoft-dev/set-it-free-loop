[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
. (Join-Path $PSScriptRoot 'line-ending-test-helpers.ps1')
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

    if (-not (Test-NormalizedTextEqual $source $staged)) {
        $failures.Add('Canonical and staged reviewer Markdown differ.')
    }

    $deployScriptPath = Join-Path $repoRoot 'deployment\scripts\deploy-workflow.ps1'
    $deployScript = Get-Content -LiteralPath $deployScriptPath -Raw
    $rendererStart = $deployScript.IndexOf('function ConvertTo-SflWorkflowWithEnginePolicy')
    $rendererEnd = $deployScript.IndexOf('function New-SflEnginePolicyManifest', $rendererStart)
    if ($rendererStart -lt 0 -or $rendererEnd -le $rendererStart) {
        $failures.Add('Could not load the PowerShell engine-policy renderer for behavioral testing.')
    } else {
        Invoke-Expression $deployScript.Substring($rendererStart, $rendererEnd - $rendererStart)

        $reviewerProfile = [pscustomobject]@{
            Provider = 'copilot'
            Arguments = @()
            Environment = [pscustomobject][ordered]@{
                COPILOT_PROVIDER_WIRE_API = 'responses'
                COPILOT_PROVIDER_TYPE = 'openai'
                COPILOT_PROVIDER_BASE_URL = 'https://openrouter.ai/api/v1'
                COPILOT_PROVIDER_API_KEY = '${{ secrets.OPENROUTER_API_KEY }}'
                COPILOT_MODEL = 'moonshotai/kimi-k3'
            }
            RenderedModel = 'moonshotai/kimi-k3'
        }
        $renderedReviewer = ConvertTo-SflWorkflowWithEnginePolicy `
            -Content $source `
            -EngineProfile $reviewerProfile `
            -WorkflowName 'sfl-pr-review'
        if (-not (Test-NormalizedTextEqual $source $renderedReviewer)) {
            $failures.Add('PowerShell engine-policy rendering changes the canonical reviewer and invalidates its generated lock.')
        }

        # Go uses sort.Strings, so PowerShell must preserve the same ordinal order.
        $mixedCaseProfile = [pscustomobject]@{
            Provider = 'copilot'
            Arguments = @()
            Environment = [pscustomobject][ordered]@{
                dKey = 'lower-d'
                aKey = 'lower-a'
                CKey = 'upper-c'
                BKey = 'upper-b'
            }
            RenderedModel = 'example'
        }
        $mixedCaseRendered = ConvertTo-SflWorkflowWithEnginePolicy `
            -Content "---`nname: Mixed case`nnetwork: defaults`n---`n" `
            -EngineProfile $mixedCaseProfile `
            -WorkflowName 'mixed-case'
        $renderedEnvironmentNames = @(
            [regex]::Matches($mixedCaseRendered, '(?m)^    (?<name>[A-Za-z]+):') |
                ForEach-Object { $_.Groups['name'].Value }
        )
        if (($renderedEnvironmentNames -join ',') -cne 'BKey,CKey,aKey,dKey') {
            $failures.Add("PowerShell engine environment order is not ordinal: $($renderedEnvironmentNames -join ',')")
        }
    }

    $requiredSourcePatterns = @(
        '(?m)^source: HemSoft/set-it-free-loop/deployment/workflows/sfl-pr-review\.md@main\r?$',
        '(?m)^  workflow_dispatch:\r?$',
        'Validate trusted review context',
        'Install ripgrep with bounded diagnostics',
        'timeout --signal=TERM --kill-after=15s 180s',
        'Retain ripgrep setup diagnostics',
        'sfl-ripgrep-setup-\$\{\{ github\.run_id \}\}-\$\{\{ github\.run_attempt \}\}',
        'Install threat-detection ripgrep with bounded diagnostics',
        'Retain threat-detection ripgrep setup diagnostics',
        'sfl-threat-detection-ripgrep-setup-\$\{\{ github\.run_id \}\}-\$\{\{ github\.run_attempt \}\}',
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
        '(?m)^max-turn-cache-misses: 10\r?$',
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
        '\\"maxCacheMisses\\":10',
        '"OPENROUTER_API_KEY"',
        'GH_AW_INPUTS_BASE_SHA: \$\{\{ inputs\.base_sha \}\}',
        'GH_AW_INPUTS_HEAD_SHA: \$\{\{ inputs\.head_sha \}\}',
        'Validate trusted review context',
        'Install ripgrep with bounded diagnostics',
        'timeout --signal=TERM --kill-after=15s 180s',
        'Retain ripgrep setup diagnostics',
        'sfl-ripgrep-setup-\$\{\{ github\.run_id \}\}-\$\{\{ github\.run_attempt \}\}',
        'Install threat-detection ripgrep with bounded diagnostics',
        'Retain threat-detection ripgrep setup diagnostics',
        'sfl-threat-detection-ripgrep-setup-\$\{\{ github\.run_id \}\}-\$\{\{ github\.run_attempt \}\}',
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
