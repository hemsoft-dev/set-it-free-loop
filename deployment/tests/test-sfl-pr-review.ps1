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
    'comment.updated_at || comment.created_at',
    'pull.data.state !== "open"',
    'github.rest.actions.getWorkflowRun',
    'Date.parse(check.completed_at || "") > baseAdvanceTime',
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
