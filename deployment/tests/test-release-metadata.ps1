Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$version = (Get-Content -LiteralPath (Join-Path $repoRoot 'VERSION') -Raw).Trim()
$manifest = Get-Content -LiteralPath (Join-Path $repoRoot 'sfl.json') -Raw | ConvertFrom-Json
$release = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\release-metadata.json') -Raw |
    ConvertFrom-Json
$autoVersion = Get-Content -LiteralPath (Join-Path $repoRoot '.github\workflows\auto-version.yml') -Raw
$failures = [System.Collections.Generic.List[string]]::new()

if ($version -notmatch '^\d+\.\d+\.\d+$') {
    $failures.Add("VERSION is not semantic version metadata: '$version'.")
}
if ($manifest.version -ne $version) {
    $failures.Add("sfl.json version '$($manifest.version)' differs from VERSION '$version'.")
}
if ($release.distribution.version -ne $version) {
    $failures.Add("Release distribution version '$($release.distribution.version)' differs from VERSION '$version'.")
}
if ($release.distribution.repository -ne 'HemSoft/set-it-free-loop' -or
    $release.distribution.visibility -ne 'private') {
    $failures.Add('Release distribution identity is not the private HemSoft repository.')
}
if ($release.reviewerBaseline.repository -ne 'relias-engineering/set-it-free-loop' -or
    $release.reviewerBaseline.releaseVersion -ne '6.5.7' -or
    $release.reviewerBaseline.releaseTag -ne "v$($release.reviewerBaseline.releaseVersion)") {
    $failures.Add('Relias reviewer release identity is inconsistent.')
}
foreach ($property in @('releaseCommit', 'reviewedCommit')) {
    if ([string] $release.reviewerBaseline.$property -notmatch '^[0-9a-f]{40}$') {
        $failures.Add("reviewerBaseline.$property is not an immutable full commit SHA.")
    }
}

$requiredWorkflowPatterns = @(
    "- 'deployment/release-metadata.json'",
    'jq --arg v "$NEW_VERSION" ''.distribution.version = $v'' deployment/release-metadata.json',
    'git add VERSION sfl.json deployment/release-metadata.json'
)
foreach ($pattern in $requiredWorkflowPatterns) {
    if (-not $autoVersion.Contains($pattern)) {
        $failures.Add("Auto Version does not synchronize release metadata with: $pattern")
    }
}

if ($failures.Count -gt 0) {
    throw "Release metadata contract failed:`n - $($failures -join "`n - ")"
}

Write-Output "Release metadata contract passed for HemSoft $version and Relias $($release.reviewerBaseline.releaseVersion)."
