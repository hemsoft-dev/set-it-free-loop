<#
.SYNOPSIS
    Enables GitHub immutable releases for the private HemSoft SFL repository.
#>
[CmdletBinding()]
param(
    [string] $Repository = 'hemsoft-dev/set-it-free-loop',
    [switch] $Plan
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($Repository -notin @('HemSoft/set-it-free-loop', 'hemsoft-dev/set-it-free-loop')) {
    throw "Release protection is restricted to HemSoft/set-it-free-loop, got $Repository."
}
if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw 'GitHub CLI is required to configure release protection.'
}

Import-Module (Join-Path $PSScriptRoot 'SflRepositoryPolicy.psm1') -Force
$context = Get-SflRepositoryContext -Repository $Repository -Access admin -AllowSource
if (-not $context.Metadata.private) {
    throw 'Release protection requires the private SFL source repository.'
}

$immutableState = & gh api "repos/$Repository/immutable-releases" | ConvertFrom-Json
if ($LASTEXITCODE -ne 0) {
    throw 'Could not inspect immutable-release state.'
}
if ($Plan) {
    [ordered]@{
        repository = $Repository
        immutableReleasesEnabled = [bool] $immutableState.enabled
        action = if ([bool] $immutableState.enabled) { 'none' } else { 'enable' }
        repositoryVariable = 'SFL_IMMUTABLE_RELEASES_ENABLED=true'
    } | ConvertTo-Json
    return
}

if (-not [bool] $immutableState.enabled) {
    & gh api --method PUT -H 'X-GitHub-Api-Version: 2026-03-10' "repos/$Repository/immutable-releases" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Enabling immutable releases failed.' }
    Write-Output 'Enabled immutable releases.'
} else {
    Write-Output 'Immutable releases already enabled.'
}

$verifiedState = & gh api "repos/$Repository/immutable-releases" | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or -not [bool] $verifiedState.enabled) {
    throw 'Immutable releases did not remain enabled after configuration.'
}
& gh variable set SFL_IMMUTABLE_RELEASES_ENABLED --repo $Repository --body true
if ($LASTEXITCODE -ne 0) {
    throw 'Recording the immutable-release preflight variable failed.'
}
$recordedState = (& gh variable get SFL_IMMUTABLE_RELEASES_ENABLED --repo $Repository).Trim()
if ($LASTEXITCODE -ne 0 -or $recordedState -ne 'true') {
    throw 'The immutable-release preflight variable was not recorded as true.'
}
Write-Output 'Recorded SFL_IMMUTABLE_RELEASES_ENABLED=true for release preflight.'
