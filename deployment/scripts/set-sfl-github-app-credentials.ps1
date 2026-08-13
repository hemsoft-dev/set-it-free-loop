<#
.SYNOPSIS
Sets per-repository SFL GitHub App credentials for HemSoft-owned repositories.

.DESCRIPTION
HemSoft is a GitHub user account, not an organization, so Actions credentials
must be configured on each repository. Before writing anything, this script
authenticates as the App and proves that every target is a selected private
HemSoft repository with the exact reviewer permission contract. It then sets
the SFL GitHub App Actions variables and stores the app private key as an
Actions secret without echoing the key value.

.PARAMETER Repos
Repository names in OWNER/REPO format.

.PARAMETER AppId
Numeric GitHub App ID.

.PARAMETER ClientId
GitHub App client ID used by gh-aw safe outputs.

.PARAMETER PrivateKeyPath
Path to the GitHub App private-key PEM file.

.PARAMETER ExpectedLogin
GitHub CLI account expected to be active before any credentials are written.

.EXAMPLE
.\deployment\scripts\set-sfl-github-app-credentials.ps1 `
  -Repos HemSoft/set-it-free-loop,HemSoft/hs-buddy `
  -AppId 123456 `
  -ClientId Iv1.abcdef1234567890 `
  -PrivateKeyPath C:\secure\sfl-app.private-key.pem
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')]
    [string[]] $Repos,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+$')]
    [string] $AppId,

    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string] $ClientId,

    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string] $PrivateKeyPath,

    [Parameter(Mandatory = $false)]
    [ValidateNotNullOrEmpty()]
    [string] $ExpectedLogin = 'HemSoft'
)

$InformationPreference = 'Continue'
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'SflGitHubAppBootstrap.psm1') -Force

function Invoke-GhCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string[]] $Arguments
    )

    $output = & gh @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "gh $($Arguments -join ' ') failed: $output"
    }

    return $output
}

Get-Command -Name gh -ErrorAction Stop | Out-Null

$activeLogin = Invoke-GhCommand -Arguments @('api', 'user', '--jq', '.login')
if ($activeLogin -ne $ExpectedLogin) {
    throw "Active gh login is '$activeLogin'; expected '$ExpectedLogin'."
}

$resolvedPrivateKeyPath = (Resolve-Path -LiteralPath $PrivateKeyPath).ProviderPath
$requiredVariables = @('SFL_APP_ID', 'SFL_APP_CLIENT_ID')
$requiredSecrets = @('SFL_APP_PRIVATE_KEY')
$validatedRepos = [Collections.Generic.List[string]]::new()

foreach ($repo in $Repos) {
    $repositoryJson = Invoke-GhCommand -Arguments @(
        'repo', 'view', $repo,
        '--json', 'nameWithOwner,owner,visibility'
    )
    $repository = $repositoryJson | ConvertFrom-Json
    if ($repository.nameWithOwner -cne $repo) {
        throw "Resolved repository '$($repository.nameWithOwner)' does not match requested repository '$repo'."
    }
    if ($repository.owner.login -cne $ExpectedLogin) {
        throw "Repository '$repo' is not owned by '$ExpectedLogin'."
    }
    if ($repository.visibility -cne 'PRIVATE') {
        throw "Repository '$repo' must be private; visibility is '$($repository.visibility)'."
    }
    $validatedRepos.Add($repo)
}

$privateKeyPem = Get-Content -LiteralPath $resolvedPrivateKeyPath -Raw
$appJwt = ConvertTo-SflGitHubAppJwt -ClientId $ClientId -PrivateKeyPem $privateKeyPem
$appIdentity = Get-SflGitHubAppIdentity -Jwt $appJwt
Assert-SflGitHubAppIdentity `
    -Identity $appIdentity `
    -ExpectedAppId $AppId `
    -ExpectedClientId $ClientId `
    -ExpectedOwner $ExpectedLogin

foreach ($repo in $validatedRepos) {
    $installation = Get-SflGitHubAppRepositoryInstallation -Repository $repo -Jwt $appJwt
    Assert-SflGitHubAppInstallation `
        -Installation $installation `
        -Repository $repo `
        -ExpectedAppId $AppId `
        -ExpectedClientId $ClientId `
        -ExpectedOwner $ExpectedLogin
    Write-Information "Verified selected-repository SFL App installation and permission ceiling on $repo"
}

foreach ($repo in $validatedRepos) {
    if (-not $PSCmdlet.ShouldProcess($repo, 'Set SFL GitHub App credentials')) {
        continue
    }

    Write-Information "Setting SFL_APP_ID variable on $repo"
    Invoke-GhCommand -Arguments @(
        'variable', 'set', 'SFL_APP_ID',
        '--repo', $repo,
        '--body', $AppId
    ) | Out-Null

    Write-Information "Setting SFL_APP_CLIENT_ID variable on $repo"
    Invoke-GhCommand -Arguments @(
        'variable', 'set', 'SFL_APP_CLIENT_ID',
        '--repo', $repo,
        '--body', $ClientId
    ) | Out-Null

    Write-Information "Setting SFL_APP_PRIVATE_KEY Actions secret on $repo"
    $secretOutput = Get-Content -LiteralPath $resolvedPrivateKeyPath -Raw |
        & gh secret set SFL_APP_PRIVATE_KEY --repo $repo --app actions 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "gh secret set SFL_APP_PRIVATE_KEY --repo $repo --app actions failed: $secretOutput"
    }

    $variableNames = @(
        Invoke-GhCommand -Arguments @(
            'variable', 'list',
            '--repo', $repo,
            '--json', 'name',
            '--jq', '.[].name'
        )
    )
    $missingVariables = @($requiredVariables | Where-Object { $_ -notin $variableNames })
    if ($missingVariables.Count -gt 0) {
        throw "Missing Actions variable(s) on ${repo}: $($missingVariables -join ', ')"
    }

    $secretNames = @(
        Invoke-GhCommand -Arguments @(
            'secret', 'list',
            '--repo', $repo,
            '--app', 'actions',
            '--json', 'name',
            '--jq', '.[].name'
        )
    )
    $missingSecrets = @($requiredSecrets | Where-Object { $_ -notin $secretNames })
    if ($missingSecrets.Count -gt 0) {
        throw "Missing Actions secret(s) on ${repo}: $($missingSecrets -join ', ')"
    }

    Write-Information "Verified SFL GitHub App credential metadata on $repo"
}
