<#
.SYNOPSIS
    Builds the checksum-verified HemSoft gh-sfl release bundle.
#>
[CmdletBinding()]
param(
    [string] $RepositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath,
    [string] $OutputDirectory = (Join-Path $RepositoryRoot 'dist'),
    [string] $Version,
    [string] $BuildDate = (Get-Date -Format 'yyyy-MM-dd')
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$semanticVersionPattern = '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$'
if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = (Get-Content -LiteralPath (Join-Path $RepositoryRoot 'VERSION') -Raw).Trim()
}
if ($Version -notmatch $semanticVersionPattern) {
    throw "Invalid semantic version: $Version"
}
if ($BuildDate -notmatch '^\d{4}-\d{2}-\d{2}$') {
    throw "BuildDate must use YYYY-MM-DD: $BuildDate"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is required to build release artifacts but was not found on PATH.'
}

$sourceDirectory = Join-Path $RepositoryRoot 'gh-sfl'
$installerPath = Join-Path $RepositoryRoot 'deployment\scripts\install-gh-sfl-hemsoft.ps1'
if (-not (Test-Path -LiteralPath $sourceDirectory -PathType Container) -or
    -not (Test-Path -LiteralPath $installerPath -PathType Leaf)) {
    throw 'Repository-owned CLI source or installer is missing.'
}

$OutputDirectory = if ([System.IO.Path]::IsPathFullyQualified($OutputDirectory)) {
    [System.IO.Path]::GetFullPath($OutputDirectory)
} else {
    [System.IO.Path]::GetFullPath((Join-Path (Get-Location).ProviderPath $OutputDirectory))
}
if (Test-Path -LiteralPath $OutputDirectory) {
    $existingOutput = @(Get-ChildItem -LiteralPath $OutputDirectory -Force)
    if ($existingOutput.Count -gt 0) {
        throw "Release output directory must be empty: $OutputDirectory"
    }
} else {
    New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
}

Push-Location $sourceDirectory
try {
    go generate ./...
    if ($LASTEXITCODE -ne 0) { throw "go generate failed with exit code $LASTEXITCODE" }
    $unformatted = @(gofmt -l .)
    if ($LASTEXITCODE -ne 0) { throw "gofmt failed with exit code $LASTEXITCODE" }
    if ($unformatted.Count -gt 0) { throw "gh-sfl requires gofmt: $($unformatted -join ', ')" }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE" }
    go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }

    $oldGoOS = $env:GOOS
    $oldGoArch = $env:GOARCH
    $oldCGOEnabled = $env:CGO_ENABLED
    try {
        $env:GOARCH = 'amd64'
        $env:CGO_ENABLED = '0'
        foreach ($target in @(
            @{ OS = 'linux'; Suffix = '' },
            @{ OS = 'windows'; Suffix = '.exe' }
        )) {
            $env:GOOS = $target.OS
            $artifactName = "gh-sfl_${Version}_$($target.OS)_amd64$($target.Suffix)"
            $artifactPath = Join-Path $OutputDirectory $artifactName
            go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$Version -X main.buildDate=$BuildDate" -o $artifactPath .
            if ($LASTEXITCODE -ne 0) {
                throw "go build for $($target.OS)/amd64 failed with exit code $LASTEXITCODE"
            }
        }
    }
    finally {
        $env:GOOS = $oldGoOS
        $env:GOARCH = $oldGoArch
        $env:CGO_ENABLED = $oldCGOEnabled
    }
}
finally {
    Pop-Location
}

Copy-Item -LiteralPath $installerPath -Destination (Join-Path $OutputDirectory 'install-gh-sfl-hemsoft.ps1')
$checksumPath = Join-Path $OutputDirectory 'SHA256SUMS'
$checksumLines = Get-ChildItem -LiteralPath $OutputDirectory -File |
    Where-Object Name -ne 'SHA256SUMS' |
    Sort-Object Name |
    ForEach-Object {
        $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $($_.Name)"
    }
$utf8NoBom = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText($checksumPath, "$($checksumLines -join "`n")`n", $utf8NoBom)

Write-Output "Built HemSoft gh-sfl v$Version release bundle at $OutputDirectory."
