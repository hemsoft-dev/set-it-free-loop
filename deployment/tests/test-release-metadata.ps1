[CmdletBinding()]
param(
    [string] $ExpectedVersion,
    [string] $ExpectedRepository,
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
if (-not $ExpectedRepository) {
    $ExpectedRepository = if ($env:GITHUB_REPOSITORY) { $env:GITHUB_REPOSITORY } else { 'hemsoft-dev/set-it-free-loop' }
}
if ($ExpectedRepository -notin @('HemSoft/set-it-free-loop', 'hemsoft-dev/set-it-free-loop') -or
    $release.distribution.repository -ine $ExpectedRepository -or
    $release.cliSource.repository -ine $ExpectedRepository) {
    throw "Release source mismatch: distribution and CLI must both identify publishing repository '$ExpectedRepository'."
}

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
if ($release.distribution.repository -notin @('HemSoft/set-it-free-loop', 'hemsoft-dev/set-it-free-loop') -or
    $release.distribution.visibility -ne 'private') {
    $failures.Add('Release distribution identity is not the private HemSoft repository.')
}
if ($release.schemaVersion -ne 2 -or
    $release.reviewerRuntime.type -ne 'subscription-backed-codex-github-app' -or
    $release.reviewerRuntime.trigger -ne '@codex review' -or
    $release.reviewerRuntime.app.id -ne 1144995 -or
    $release.reviewerRuntime.app.slug -ne 'chatgpt-codex-connector' -or
    $release.reviewerRuntime.app.owner -ne 'openai' -or
    $release.reviewerRuntime.app.botUserId -ne 199175422 -or
    $release.reviewerRuntime.requiredGate.name -ne 'SFL Reviewer Gate Runner' -or
    $release.reviewerRuntime.requiredGate.appId -ne 15368) {
    $failures.Add('Subscription-backed Codex reviewer identity is incomplete or inconsistent.')
}
if ($release.cliSource.repository -ne $release.distribution.repository -or
    $release.cliSource.path -ne 'gh-sfl' -or
    $release.cliSource.module -ne 'github.com/HemSoft/set-it-free-loop/gh-sfl' -or
    $release.cliSource.buildScript -ne 'deployment/scripts/install-gh-sfl-hemsoft.ps1') {
    $failures.Add('Repository-owned CLI source provenance is incomplete or inconsistent.')
}

$requiredWorkflowPatterns = @(
    "github.repository == 'HemSoft/set-it-free-loop'",
    "github.ref == 'refs/heads/main'",
    'test-release-metadata.ps1 -ExpectedVersion $env:RELEASE_VERSION -ExpectedRepository $env:GITHUB_REPOSITORY -RequirePrerelease',
    'repos/${GITHUB_REPOSITORY}/commits/main',
    'test "$(git rev-parse HEAD)" = "$remote_main_sha"',
    'IMMUTABLE_RELEASES_ATTESTED: ${{ vars.SFL_IMMUTABLE_RELEASES_ENABLED }}',
    'attestations: read',
    'test "$IMMUTABLE_RELEASES_ATTESTED" = "true"',
    'build-release-artifacts.ps1',
    'sha256sum --check SHA256SUMS',
    'version_output=$("./gh-sfl_${RELEASE_VERSION}_linux_amd64" --version)',
    'first_line=${version_output%%$''\n''*}',
    '$versionOutput = @(& $binary --version)',
    'gh release create "$tag"',
    'releases/tags/${tag}" --jq .immutable',
    'gh release delete "$tag"',
    '--cleanup-tag --yes',
    'Mutable release and tag rollback verified.',
    'gh release verify "$tag"',
    'gh release verify-asset "$tag"',
    'for attempt in {1..30}',
    'Immutable release attestation was not available after five minutes',
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
    New-Item -ItemType Directory -Path (Join-Path $fixtureRoot 'docs') -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repoRoot 'docs\ORGANIZATION-ONBOARDING.md') `
        -Destination (Join-Path $fixtureRoot 'docs\ORGANIZATION-ONBOARDING.md')
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
    $fixtureOnboardingPath = Join-Path $fixtureRoot 'docs\ORGANIZATION-ONBOARDING.md'
    $fixtureOnboarding = Get-Content -LiteralPath $fixtureOnboardingPath -Raw
    $expectedInstall = "Install the [checksum-verified canonical release](https://github.com/$($fixtureMetadata.distribution.repository)/releases/tag/v$fixtureVersion)."
    if (-not $fixtureOnboarding.Contains($expectedInstall)) {
        $failures.Add('set-release-version.ps1 did not update the designated onboarding instruction.')
    }
    [System.IO.File]::WriteAllText($fixtureOnboardingPath, "$fixtureOnboarding`n$expectedInstall`n")
    $duplicateFailure = $null
    try {
        & (Join-Path $repoRoot 'deployment\scripts\set-release-version.ps1') `
            -Version '9.8.8' -RepositoryRoot $fixtureRoot | Out-Null
    } catch { $duplicateFailure = $_.Exception.Message }
    if ($duplicateFailure -notlike 'Onboarding install instruction must be unique*' -or
        (Get-Content -LiteralPath (Join-Path $fixtureRoot 'VERSION') -Raw).Trim() -ne $fixtureVersion) {
        $failures.Add('Ambiguous onboarding instructions must fail before changing release metadata.')
    }
    [System.IO.File]::WriteAllText($fixtureOnboardingPath, $fixtureOnboarding)

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

    $otherRepository = if ($ExpectedRepository -ieq 'HemSoft/set-it-free-loop') { 'hemsoft-dev/set-it-free-loop' } else { 'HemSoft/set-it-free-loop' }
    $sourceMismatch = $null
    try { & $PSCommandPath -ExpectedRepository $otherRepository | Out-Null }
    catch { $sourceMismatch = $_.Exception.Message }
    if ($sourceMismatch -notlike 'Release source mismatch:*') { $failures.Add('Publishing repository mismatch was not rejected before building.') }

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
            $repoRoot $relativeOutputName '9.8.7-rc.2' '2026-08-13' `
            -SourceRepository 'hemsoft-dev/set-it-free-loop' | Out-Null
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

Write-Output "Release metadata contract passed for HemSoft $version and subscription-backed Codex review."
