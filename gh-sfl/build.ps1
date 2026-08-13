<#
.SYNOPSIS
    Builds and optionally installs the repository-owned HemSoft gh-sfl CLI.

.DESCRIPTION
    Delegates to the canonical repository build script so local development
    and CI exercise the same format, vet, test, and build pipeline.
#>
[CmdletBinding()]
param(
    [string] $WorkDir,
    [string] $Version,
    [string] $ReleaseVersion,
    [string] $BuildDate,
    [string] $GitHubCliPath,
    [switch] $NoInstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..')
$buildScript = Join-Path $repoRoot 'deployment\scripts\install-gh-sfl-hemsoft.ps1'
& $buildScript @PSBoundParameters
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
