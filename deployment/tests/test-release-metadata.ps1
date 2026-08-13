[CmdletBinding()]
param(
    [string] $ExpectedVersion,
    [switch] $RequirePrerelease
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$version = (Get-Content -LiteralPath (Join-Path $repoRoot 'VERSION') -Raw).Trim()
$manifest = Get-Content -LiteralPath (Join-Path $repoRoot 'sfl.json') -Raw | ConvertFrom-Json
$release = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\release-metadata.json') -Raw |
    ConvertFrom-Json
$workflow = Get-Content -LiteralPath (Join-Path $repoRoot '.github\workflows\publish-private-prerelease.yml') -Raw
$failures = [System.Collections.Generic.List[string]]::new()
$semanticVersionPattern = '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$'

if ($version -notmatch $semanticVersionPattern) {
    $failures.Add("VERSION is not semantic version metadata: '$version'.")
}
if (-not [string]::IsNullOrWhiteSpace($ExpectedVersion) -and $version -ne $ExpectedVersion) {
    $failures.Add("VERSION '$version' differs from requested release '$ExpectedVersion'.")
}
if ($manifest.version -ne $version) {
    $failures.Add("sfl.json version '$($manifest.version)' differs from VERSION '$version'.")
}
if ($release.distribution.version -ne $version) {
    $failures.Add("Release distribution version '$($release.distribution.version)' differs from VERSION '$version'.")
}
if ($release.distribution.tag -ne "v$version") {
    $failures.Add("Release distribution tag '$($release.distribution.tag)' differs from v$version.")
}
$isPrerelease = $version.Split('+', 2)[0].Contains('-')
if ([bool] $release.distribution.prerelease -ne $isPrerelease) {
    $failures.Add("Release prerelease metadata does not match version '$version'.")
}
if ($RequirePrerelease -and -not $isPrerelease) {
    $failures.Add("Release workflow only publishes prereleases, but '$version' is stable.")
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
if ($release.cliSource.repository -ne 'HemSoft/set-it-free-loop' -or
    $release.cliSource.path -ne 'gh-sfl' -or
    $release.cliSource.module -ne 'github.com/HemSoft/set-it-free-loop/gh-sfl' -or
    $release.cliSource.upstreamRepository -ne 'relias-engineering/set-it-free-loop' -or
    $release.cliSource.upstreamRelease -ne 'v6.5.7' -or
    [string] $release.cliSource.upstreamCommit -notmatch '^[0-9a-f]{40}$' -or
    $release.cliSource.buildScript -ne 'deployment/scripts/install-gh-sfl-hemsoft.ps1') {
    $failures.Add('Repository-owned CLI source provenance is incomplete or inconsistent.')
}

$requiredWorkflowPatterns = @(
    "github.repository == 'HemSoft/set-it-free-loop'",
    "github.ref == 'refs/heads/main'",
    'test-release-metadata.ps1 -ExpectedVersion $env:RELEASE_VERSION -RequirePrerelease',
    'repos/${GITHUB_REPOSITORY}/commits/main',
    'test "$(git rev-parse HEAD)" = "$remote_main_sha"',
    'IMMUTABLE_RELEASES_ATTESTED: ${{ vars.SFL_IMMUTABLE_RELEASES_ENABLED }}',
    'test "$IMMUTABLE_RELEASES_ATTESTED" = "true"',
    'build-release-artifacts.ps1',
    'sha256sum --check SHA256SUMS',
    'gh release create "$tag"',
    'gh release verify "$tag"',
    'gh release verify-asset "$tag"',
    '--prerelease',
    'install-gh-sfl-hemsoft.ps1',
    '-ReleaseVersion $env:RELEASE_VERSION'
)
foreach ($pattern in $requiredWorkflowPatterns) {
    if (-not $workflow.Contains($pattern)) {
        $failures.Add("Private prerelease workflow is missing contract fragment: $pattern")
    }
}

$scriptPaths = @(
    'deployment\scripts\set-release-version.ps1',
    'deployment\scripts\build-release-artifacts.ps1',
    'deployment\scripts\install-gh-sfl-hemsoft.ps1',
    'deployment\scripts\set-release-protection.ps1',
    'deployment\tests\test-release-metadata.ps1',
    'deployment\tests\test-release-installer.ps1'
)
foreach ($relativePath in $scriptPaths) {
    $tokens = $null
    $parseErrors = $null
    [void] [System.Management.Automation.Language.Parser]::ParseFile(
        (Join-Path $repoRoot $relativePath),
        [ref] $tokens,
        [ref] $parseErrors
    )
    if ($parseErrors.Count -gt 0) {
        $failures.Add("$relativePath has PowerShell parse errors: $($parseErrors.Message -join '; ')")
    }
}

$installer = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\scripts\install-gh-sfl-hemsoft.ps1') -Raw
foreach ($pattern in @('release download', 'SHA256SUMS', 'Get-FileHash', 'Checksum mismatch')) {
    if (-not $installer.Contains($pattern)) {
        $failures.Add("Release installer is missing checksum contract fragment: $pattern")
    }
}
$protection = Get-Content -LiteralPath (Join-Path $repoRoot 'deployment\scripts\set-release-protection.ps1') -Raw
foreach ($pattern in @(
    'repos/$Repository/immutable-releases',
    'gh variable set SFL_IMMUTABLE_RELEASES_ENABLED',
    'gh variable get SFL_IMMUTABLE_RELEASES_ENABLED'
)) {
    if (-not $protection.Contains($pattern)) {
        $failures.Add("Release protection script is missing attested-state bridge: $pattern")
    }
}

$fixtureRoot = Join-Path ([System.IO.Path]::GetTempPath()) "sfl-release-contract-$([guid]::NewGuid().ToString('N'))"
try {
    New-Item -ItemType Directory -Path (Join-Path $fixtureRoot 'deployment') -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repoRoot 'VERSION') -Destination (Join-Path $fixtureRoot 'VERSION')
    Copy-Item -LiteralPath (Join-Path $repoRoot 'sfl.json') -Destination (Join-Path $fixtureRoot 'sfl.json')
    Copy-Item -LiteralPath (Join-Path $repoRoot 'deployment\release-metadata.json') `
        -Destination (Join-Path $fixtureRoot 'deployment\release-metadata.json')
    & (Join-Path $repoRoot 'deployment\scripts\set-release-version.ps1') `
        -Version '9.8.7-rc.2+contract' -RepositoryRoot $fixtureRoot | Out-Null
    $fixtureVersion = (Get-Content -LiteralPath (Join-Path $fixtureRoot 'VERSION') -Raw).Trim()
    $fixtureManifest = Get-Content -LiteralPath (Join-Path $fixtureRoot 'sfl.json') -Raw | ConvertFrom-Json
    $fixtureMetadata = Get-Content -LiteralPath (Join-Path $fixtureRoot 'deployment\release-metadata.json') -Raw |
        ConvertFrom-Json
    if ($fixtureVersion -ne '9.8.7-rc.2+contract' -or
        $fixtureManifest.version -ne $fixtureVersion -or
        $fixtureMetadata.distribution.version -ne $fixtureVersion -or
        $fixtureMetadata.distribution.tag -ne "v$fixtureVersion" -or
        -not $fixtureMetadata.distribution.prerelease) {
        $failures.Add('set-release-version.ps1 did not synchronize the fixture metadata.')
    }

    foreach ($invalidVersion in @('9.8', '01.0.0', '1.0.0-rc.01', '1.0.0-')) {
        $invalidVersionFailure = $null
        try {
            & (Join-Path $repoRoot 'deployment\scripts\set-release-version.ps1') `
                -Version $invalidVersion -RepositoryRoot $fixtureRoot | Out-Null
        }
        catch {
            $invalidVersionFailure = $_.Exception.Message
        }
        if ($invalidVersionFailure -notlike 'Invalid semantic version*') {
            $failures.Add("set-release-version.ps1 did not reject '$invalidVersion': $invalidVersionFailure")
        }
    }

    $nonEmptyOutput = Join-Path $fixtureRoot 'non-empty-output'
    New-Item -ItemType Directory -Path $nonEmptyOutput -Force | Out-Null
    Set-Content -LiteralPath (Join-Path $nonEmptyOutput 'sentinel.txt') -Value 'must remain'
    $nonEmptyFailure = $null
    try {
        & (Join-Path $repoRoot 'deployment\scripts\build-release-artifacts.ps1') `
            -Version '9.8.7-rc.2' -OutputDirectory $nonEmptyOutput | Out-Null
    }
    catch {
        $nonEmptyFailure = $_.Exception.Message
    }
    if ($nonEmptyFailure -notlike 'Release output directory must be empty*' -or
        -not (Test-Path -LiteralPath (Join-Path $nonEmptyOutput 'sentinel.txt') -PathType Leaf)) {
        $failures.Add("Release artifact builder did not preserve a non-empty output directory: $nonEmptyFailure")
    }

    $relativeOutputName = "relative-release-$([guid]::NewGuid().ToString('N'))"
    Push-Location $fixtureRoot
    try {
        & (Join-Path $repoRoot 'deployment\scripts\build-release-artifacts.ps1') `
            -Version '9.8.7-rc.2' -BuildDate '2026-08-13' `
            -OutputDirectory $relativeOutputName -RepositoryRoot $repoRoot | Out-Null
    }
    finally {
        Pop-Location
    }
    if (-not (Test-Path -LiteralPath (Join-Path $fixtureRoot "$relativeOutputName\SHA256SUMS") -PathType Leaf)) {
        $failures.Add('Release artifact builder resolved a relative output path after changing location.')
    }
}
finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}

if ($failures.Count -gt 0) {
    throw "Release metadata contract failed:`n - $($failures -join "`n - ")"
}

Write-Output "Release metadata contract passed for HemSoft $version and Relias $($release.reviewerBaseline.releaseVersion)."
