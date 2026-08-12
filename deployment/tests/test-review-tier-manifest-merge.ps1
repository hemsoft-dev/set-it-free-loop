[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
. (Join-Path $repoRoot 'deployment\scripts\merge-sfl-manifest.ps1')

$existingFull = [pscustomobject]@{
    version = '1.9.0'
    deployedAt = '2026-01-01T00:00:00Z'
    tier = 'full'
    source = 'HemSoft/set-it-free-loop'
    sourceSha = '1111111111111111111111111111111111111111'
    components = @('labels', 'governance', 'repo-audit', 'pr-fixer')
    enginePolicy = [pscustomobject]@{
        defaultProfile = 'codex-gpt-55-high'
        workflows = @(
            [pscustomobject]@{
                name = 'repo-audit'
                profile = 'codex-gpt-55-high'
                provider = 'codex'
                model = 'gpt-5.5'
                effort = 'high'
                renderedModel = 'gpt-5.5?effort=high'
                requiredSecretsAnyOf = @('OPENAI_API_KEY')
            }
        )
    }
}

$incomingReview = [pscustomobject]@{
    version = '2.1.0'
    deployedAt = '2026-07-31T00:00:00Z'
    tier = 'review'
    source = 'HemSoft/set-it-free-loop'
    sourceSha = '2222222222222222222222222222222222222222'
    components = @(
        'labels',
        'governance',
        'sfl-pr-review',
        'sfl-pr-review-auto',
        'sfl-pr-review-recovery'
    )
    enginePolicy = [pscustomobject]@{
        defaultProfile = 'codex-gpt-55-high'
        workflows = @(
            [pscustomobject]@{
                name = 'sfl-pr-review'
                profile = 'openrouter-kimi-k3-high'
                provider = 'copilot'
                model = 'moonshotai/kimi-k3'
                effort = 'high'
                renderedModel = 'moonshotai/kimi-k3'
                requiredSecretsAnyOf = @('OPENROUTER_API_KEY')
            }
        )
    }
}

$merged = Merge-SflManifest -ExistingManifest $existingFull -IncomingManifest $incomingReview

if ($merged.tier -ne 'full') {
    throw "Expected existing full tier to be preserved; got '$($merged.tier)'."
}

$expectedComponents = @(
    'governance',
    'labels',
    'pr-fixer',
    'repo-audit',
    'sfl-pr-review',
    'sfl-pr-review-auto',
    'sfl-pr-review-recovery'
)
$actualComponents = @($merged.components | Sort-Object)
if (($actualComponents -join ',') -ne ($expectedComponents -join ',')) {
    throw "Unexpected merged components: $($actualComponents -join ', ')"
}

$workflowNames = @($merged.enginePolicy.workflows.name | Sort-Object)
if (($workflowNames -join ',') -ne 'repo-audit,sfl-pr-review') {
    throw "Unexpected merged engine workflows: $($workflowNames -join ', ')"
}

if ($merged.version -ne $incomingReview.version -or
    $merged.sourceSha -ne $incomingReview.sourceSha -or
    $merged.deployedAt -ne $incomingReview.deployedAt) {
    throw 'Incoming release metadata was not applied.'
}

$newInstall = Merge-SflManifest -ExistingManifest $null -IncomingManifest $incomingReview
if ($newInstall.tier -ne 'review') {
    throw "Expected a new install to use review tier; got '$($newInstall.tier)'."
}

$legacyManifest = [pscustomobject]@{
    version = '1.0.0'
    deployedAt = '2025-01-01T00:00:00Z'
    tier = 'standard'
    source = 'HemSoft/set-it-free-loop'
    sourceSha = '3333333333333333333333333333333333333333'
}

$legacyMerged = Merge-SflManifest `
    -ExistingManifest $legacyManifest `
    -IncomingManifest $incomingReview

if ($legacyMerged.tier -ne 'standard') {
    throw "Expected legacy standard tier to be preserved; got '$($legacyMerged.tier)'."
}
foreach ($component in @('sfl-pr-review', 'sfl-pr-review-auto', 'sfl-pr-review-recovery')) {
    if ($component -notin @($legacyMerged.components)) {
        throw "Legacy manifest did not receive the $component component."
    }
}
if (@($legacyMerged.enginePolicy.workflows).Count -ne 1 -or
    $legacyMerged.enginePolicy.workflows[0].name -ne 'sfl-pr-review') {
    throw 'Legacy manifest did not receive the review engine policy.'
}

Write-Output 'Review-tier manifest merge tests passed.'
