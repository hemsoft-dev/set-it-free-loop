<#
.SYNOPSIS
    Builds and optionally installs the repository-owned HemSoft gh-sfl CLI.

.DESCRIPTION
    Formats, vets, tests, and builds the version-controlled source in gh-sfl/.
    The build has no dependency on a Relias checkout. By default the script
    installs the resulting executable into the GitHub CLI extension directory.
#>
[CmdletBinding()]
param(
    [string] $WorkDir = (Join-Path ([System.IO.Path]::GetTempPath()) 'gh-sfl-hemsoft-build'),
    [string] $Version,
    [string] $BuildDate = (Get-Date -Format 'yyyy-MM-dd'),
    [switch] $NoInstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$InformationPreference = 'Continue'

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Resolve-Path (Join-Path $scriptDir '..\..')
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
if ($Version -notmatch '^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$') {
    throw "Invalid gh-sfl version: $Version"
}
if ($BuildDate -notmatch '^\d{4}-\d{2}-\d{2}$') {
    throw "BuildDate must use YYYY-MM-DD: $BuildDate"
}

New-Item -ItemType Directory -Path $WorkDir -Force | Out-Null
$outputPath = Join-Path $WorkDir 'gh-sfl.exe'

Push-Location $sourceDir
try {
    go generate ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go generate contract checks failed with exit code $LASTEXITCODE"
    }

    $unformatted = @(gofmt -l .)
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt failed with exit code $LASTEXITCODE"
    }
    if ($unformatted.Count -gt 0) {
        throw "gh-sfl contains files that require gofmt: $($unformatted -join ', ')"
    }

    go vet ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go vet failed with exit code $LASTEXITCODE"
    }

    go test ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go test failed with exit code $LASTEXITCODE"
    }

    go build -trimpath -buildvcs=false -ldflags "-X main.version=$Version -X main.buildDate=$BuildDate" -o $outputPath .
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}

if (-not $NoInstall) {
    $extensionDir = Join-Path $env:LOCALAPPDATA 'GitHub CLI\extensions\gh-sfl'
    New-Item -ItemType Directory -Path $extensionDir -Force | Out-Null
    Copy-Item -LiteralPath $outputPath -Destination (Join-Path $extensionDir 'gh-sfl.exe') -Force
    Write-Information "Installed HemSoft gh-sfl $Version. Run: gh sfl version"
}
else {
    Write-Information "Built HemSoft gh-sfl $Version at $outputPath"
}
