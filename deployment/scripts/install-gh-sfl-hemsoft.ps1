<#
.SYNOPSIS
    Installs a checksum-verified private release or builds the owned gh-sfl source.

.DESCRIPTION
    Pass -ReleaseVersion to download the matching private GitHub release asset
    with the active HemSoft gh authentication and verify it against SHA256SUMS.
    Without -ReleaseVersion, the script validates and builds repository-owned
    source for local development. No path depends on a Relias checkout.
#>
[CmdletBinding()]
param(
    [string] $WorkDir = (Join-Path ([System.IO.Path]::GetTempPath()) "gh-sfl-hemsoft-$([guid]::NewGuid().ToString('N'))"),
    [string] $Version,
    [string] $ReleaseVersion,
    [string] $BuildDate = (Get-Date -Format 'yyyy-MM-dd'),
    [string] $GitHubCliPath = 'gh',
    [switch] $NoInstall,
    [ValidateSet('HemSoft/set-it-free-loop', 'hemsoft-dev/set-it-free-loop')]
    [string] $Repository = 'HemSoft/set-it-free-loop'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$InformationPreference = 'Continue'

$semanticVersionPattern = '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$'
if (-not [string]::IsNullOrWhiteSpace($ReleaseVersion) -and
    -not [string]::IsNullOrWhiteSpace($Version)) {
    throw 'Use -ReleaseVersion for a published binary or -Version for a source build, not both.'
}

New-Item -ItemType Directory -Path $WorkDir -Force | Out-Null
$outputPath = $null
$resolvedVersion = $null

if (-not [string]::IsNullOrWhiteSpace($ReleaseVersion)) {
    if ($ReleaseVersion -notmatch $semanticVersionPattern) {
        throw "Invalid release version: $ReleaseVersion"
    }
    if (-not (Get-Command $GitHubCliPath -ErrorAction SilentlyContinue)) {
        throw 'GitHub CLI is required to download the private HemSoft release.'
    }
    if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne
        [System.Runtime.InteropServices.Architecture]::X64) {
        throw "No HemSoft gh-sfl release artifact supports architecture $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)."
    }

    $artifactName = switch ($true) {
        ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
            [System.Runtime.InteropServices.OSPlatform]::Windows)) {
            "gh-sfl_${ReleaseVersion}_windows_amd64.exe"
            break
        }
        ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
            [System.Runtime.InteropServices.OSPlatform]::Linux)) {
            "gh-sfl_${ReleaseVersion}_linux_amd64"
            break
        }
        default {
            throw 'HemSoft gh-sfl release installation currently supports Windows and Linux amd64.'
        }
    }

    $tag = "v$ReleaseVersion"
    $LASTEXITCODE = 0
    & $GitHubCliPath release download $tag --repo $Repository --pattern $artifactName `
        --pattern SHA256SUMS --dir $WorkDir --clobber
    $downloadSucceeded = $?
    $downloadExitCode = $LASTEXITCODE
    if (-not $downloadSucceeded -or $downloadExitCode -ne 0) {
        throw "Downloading private HemSoft gh-sfl release $tag failed with exit code $downloadExitCode"
    }

    $outputPath = Join-Path $WorkDir $artifactName
    $checksumPath = Join-Path $WorkDir 'SHA256SUMS'
    if (-not (Test-Path -LiteralPath $outputPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $checksumPath -PathType Leaf)) {
        throw "Release $tag did not contain $artifactName and SHA256SUMS."
    }

    $escapedName = [regex]::Escape($artifactName)
    $matches = @(Get-Content -LiteralPath $checksumPath | Where-Object {
        $_ -match "^([0-9a-fA-F]{64})  $escapedName$"
    })
    if ($matches.Count -ne 1) {
        throw "SHA256SUMS must contain exactly one checksum for $artifactName."
    }
    $expectedHash = ([regex]::Match($matches[0], '^([0-9a-fA-F]{64})')).Groups[1].Value
    $actualHash = (Get-FileHash -LiteralPath $outputPath -Algorithm SHA256).Hash
    if (-not $actualHash.Equals($expectedHash, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Checksum mismatch for $artifactName. Expected $expectedHash, got $actualHash."
    }
    $resolvedVersion = $ReleaseVersion
    Write-Information "Verified SHA-256 for $artifactName."
}
else {
    $scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
    $repoRoot = (Resolve-Path (Join-Path $scriptDir '..\..')).ProviderPath
    $sourceDir = Join-Path $repoRoot 'gh-sfl'
    $versionPath = Join-Path $repoRoot 'VERSION'

    if (-not (Test-Path -LiteralPath $sourceDir)) {
        throw "Repository-owned gh-sfl source not found: $sourceDir"
    }
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'Go is required to build gh-sfl but was not found on PATH.'
    }
    if ([string]::IsNullOrWhiteSpace($Version)) {
        $Version = (Get-Content -LiteralPath $versionPath -Raw).Trim()
    }
    if ($Version -notmatch $semanticVersionPattern) {
        throw "Invalid gh-sfl version: $Version"
    }
    if ($BuildDate -notmatch '^\d{4}-\d{2}-\d{2}$') {
        throw "BuildDate must use YYYY-MM-DD: $BuildDate"
    }

    $suffix = if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
        [System.Runtime.InteropServices.OSPlatform]::Windows)) { '.exe' } else { '' }
    $outputPath = Join-Path $WorkDir "gh-sfl$suffix"
    Push-Location $sourceDir
    try {
        go generate ./...
        if ($LASTEXITCODE -ne 0) { throw "go generate contract checks failed with exit code $LASTEXITCODE" }
        $unformatted = @(gofmt -l .)
        if ($LASTEXITCODE -ne 0) { throw "gofmt failed with exit code $LASTEXITCODE" }
        if ($unformatted.Count -gt 0) { throw "gh-sfl contains files that require gofmt: $($unformatted -join ', ')" }
        go vet ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE" }
        go test -count=1 ./...
        if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
        go build -trimpath -buildvcs=false -ldflags "-X main.version=$Version -X main.buildDate=$BuildDate -X main.motherRepoOwner=$($Repository.Split('/')[0])" -o $outputPath .
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    }
    finally {
        Pop-Location
    }
    $resolvedVersion = $Version
}

if (-not $NoInstall) {
    if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
        [System.Runtime.InteropServices.OSPlatform]::Windows)) {
        $extensionDir = Join-Path $env:LOCALAPPDATA 'GitHub CLI\extensions\gh-sfl'
        $installedPath = Join-Path $extensionDir 'gh-sfl.exe'
    }
    elseif ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
        [System.Runtime.InteropServices.OSPlatform]::Linux)) {
        $dataHome = if ([string]::IsNullOrWhiteSpace($env:XDG_DATA_HOME)) {
            Join-Path $HOME '.local/share'
        } else {
            $env:XDG_DATA_HOME
        }
        $extensionDir = Join-Path $dataHome 'gh/extensions/gh-sfl'
        $installedPath = Join-Path $extensionDir 'gh-sfl'
    }
    else {
        throw 'HemSoft gh-sfl installation currently supports Windows and Linux.'
    }
    New-Item -ItemType Directory -Path $extensionDir -Force | Out-Null
    Copy-Item -LiteralPath $outputPath -Destination $installedPath -Force
    if (-not [System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
        [System.Runtime.InteropServices.OSPlatform]::Windows)) {
        & chmod 755 $installedPath
        if ($LASTEXITCODE -ne 0) { throw "chmod failed with exit code $LASTEXITCODE" }
    }
    Write-Information "Installed HemSoft gh-sfl $resolvedVersion. Run: gh sfl version"
}
else {
    Write-Information "Prepared HemSoft gh-sfl $resolvedVersion at $outputPath"
}
