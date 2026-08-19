[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$files = @{
    Init = Get-Content -LiteralPath (Join-Path $repoRoot 'gh-sfl\init.go') -Raw
    Review = Get-Content -LiteralPath (Join-Path $repoRoot 'gh-sfl\review.go') -Raw
    Deploy = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\scripts\deploy-workflow.ps1') -Raw
    Observer = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\infrastructure\sfl-pr-review-auto.yml') -Raw
}

foreach ($required in @(
    'codexReviewCommand',
    '"@codex review"',
    'sfl-codex-review:',
    'findCodexReviewTrigger',
    'sfl-pr-review-auto.yml'
)) {
    if ($files.Review -notmatch [regex]::Escape($required)) {
        throw "gh sfl review is missing contract text: $required"
    }
}

if ($files.Init -match 'sfl-pr-review\.lock\.yml|sfl-pr-review-recovery\.yml' -or
    $files.Deploy -match '@\("sfl-pr-review-auto", "sfl-pr-review-recovery"\)') {
    throw 'Deployment tiers still include the retired compiled reviewer or recovery workflow.'
}

foreach ($required in @(
    'Workflows      = @()',
    'Infrastructure = @("sfl-pr-review-auto")',
    'Components     = @("sfl-pr-review-auto")',
    'Remove-Item -LiteralPath $retiredPath -Force',
    'gh pr edit $existingPrNumber',
    '--body $prBody',
    'actionlint .github/workflows/sfl-pr-review-auto.yml'
)) {
    if ($files.Deploy -notmatch [regex]::Escape($required)) {
        throw "Review-tier deployment is missing contract text: $required"
    }
}

foreach ($required in @(
    'issue_comment:',
    'pull_request_review:',
    'Reviewed commit:',
    'github.rest.repos.getCommit',
    'Codex artifact is stale for the current pull request head',
    'Codex reported review findings on the current head'
)) {
    if ($files.Observer -notmatch [regex]::Escape($required)) {
        throw "Observer is missing result contract text: $required"
    }
}

Write-Output 'SFL Codex reviewer platform contract tests passed.'
