<#
.SYNOPSIS
    Synchronizes the HemSoft distribution version and operator install instruction.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $Version,

    [string] $RepositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$semanticVersionPattern = '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$'
if ($Version -notmatch $semanticVersionPattern) {
    throw "Invalid semantic version: $Version"
}

$versionPath = Join-Path $RepositoryRoot 'VERSION'
$manifestPath = Join-Path $RepositoryRoot 'sfl.json'
$metadataPath = Join-Path $RepositoryRoot 'deployment\release-metadata.json'
$onboardingPath = Join-Path $RepositoryRoot 'docs\ORGANIZATION-ONBOARDING.md'
foreach ($path in @($versionPath, $manifestPath, $metadataPath, $onboardingPath)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Release metadata file not found: $path"
    }
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
$metadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json
$onboarding = Get-Content -LiteralPath $onboardingPath -Raw
$installPattern = '(?m)^(Install the \[checksum-verified canonical release\]\()[^)\r\n]+(\)\.)'
if ([regex]::Matches($onboarding, $installPattern).Count -ne 1) {
    throw 'Onboarding install instruction must be unique before updating release metadata.'
}
$installUrl = "https://github.com/$($metadata.distribution.repository)/releases/tag/v$Version"
$updatedOnboarding = [regex]::Replace($onboarding, $installPattern, {
    param($match)
    $match.Groups[1].Value + $installUrl + $match.Groups[2].Value
})
$manifest.version = $Version
$metadata.distribution.version = $Version

if ($metadata.distribution.PSObject.Properties.Name -notcontains 'tag') {
    $metadata.distribution | Add-Member -NotePropertyName tag -NotePropertyValue "v$Version"
} else {
    $metadata.distribution.tag = "v$Version"
}
$isPrerelease = $Version.Split('+', 2)[0].Contains('-')
if ($metadata.distribution.PSObject.Properties.Name -notcontains 'prerelease') {
    $metadata.distribution | Add-Member -NotePropertyName prerelease -NotePropertyValue $isPrerelease
} else {
    $metadata.distribution.prerelease = $isPrerelease
}

$utf8NoBom = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText($versionPath, "$Version`n", $utf8NoBom)
[System.IO.File]::WriteAllText($manifestPath, "$(ConvertTo-Json $manifest -Depth 100)`n", $utf8NoBom)
[System.IO.File]::WriteAllText($metadataPath, "$(ConvertTo-Json $metadata -Depth 100)`n", $utf8NoBom)
[System.IO.File]::WriteAllText($onboardingPath, $updatedOnboarding, $utf8NoBom)

Write-Output "Synchronized HemSoft release metadata for v$Version (prerelease=$($isPrerelease.ToString().ToLowerInvariant()))."
