<#
.SYNOPSIS
Sets scoped SFL App credentials for HemSoft and hemsoft-dev repositories.

.DESCRIPTION
Validate the authenticated user's admin access separately from repository and
App ownership. The default stores credentials per repository. Organization scope
requires hemsoft-dev owner access, explicit repository selection, and no existing
shared credentials or overriding repository credentials. Secret values are piped
without being printed. Environment overrides must be reconciled in pre-transfer
and rollout owner verification.

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
    [string] $ExpectedLogin = 'HemSoft',

    [ValidateSet('HemSoft', 'hemsoft-dev')]
    [string] $ExpectedOwner = 'HemSoft',

    [ValidateSet('repository', 'organization')]
    [string] $CredentialScope = 'repository',

    [ValidateSet('', 'HemSoft', 'hemsoft-dev')]
    [string] $ExpectedAppOwner = ''
)

if (-not $ExpectedAppOwner) { $ExpectedAppOwner = $ExpectedOwner }
$InformationPreference = 'Continue'
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'SflGitHubAppBootstrap.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'SflRepositoryPolicy.psm1') -Force
if ($CredentialScope -eq 'organization' -and $ExpectedOwner -cne 'hemsoft-dev') {
    throw 'Organization credential scope requires hemsoft-dev and explicit selected repositories.'
}

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
    $context = Get-SflRepositoryContext -Repository $repo -Access admin -AllowSource -ExpectedLogin $ExpectedLogin
    if ($context.Metadata.owner.login -ine $ExpectedOwner) {
        throw "Repository '$repo' is not owned by '$ExpectedOwner'."
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
    -ExpectedOwner $ExpectedAppOwner

foreach ($repo in $validatedRepos) {
    $installation = Get-SflGitHubAppRepositoryInstallation -Repository $repo -Jwt $appJwt
    Assert-SflGitHubAppInstallation `
        -Installation $installation `
        -Repository $repo `
        -ExpectedAppId $AppId `
        -ExpectedClientId $ClientId `
        -ExpectedOwner $ExpectedOwner
    Write-Information "Verified SFL App installation and permission ceiling on $repo"
}

if ($CredentialScope -eq 'organization') {
    $membership = Invoke-GhCommand -Arguments @('api', '--method', 'GET', "orgs/$ExpectedOwner/memberships/$ExpectedLogin") | ConvertFrom-Json
    if ($membership.state -ne 'active' -or $membership.role -ne 'admin') {
        throw 'Organization credentials require active organization owner access and admin:org authorization.'
    }
    foreach ($repo in $validatedRepos) {
        $variables = @(Invoke-GhCommand -Arguments @('variable', 'list', '--repo', $repo, '--json', 'name', '--jq', '.[].name'))
        $secrets = @(Invoke-GhCommand -Arguments @('secret', 'list', '--repo', $repo, '--app', 'actions', '--json', 'name', '--jq', '.[].name'))
        if (@($variables | Where-Object { $_ -in $requiredVariables }).Count -or
            @($secrets | Where-Object { $_ -in $requiredSecrets }).Count) {
            throw "Repository credential overrides on $repo take precedence over organization values; reconcile them before organization setup."
        }
    }
    foreach ($repo in $validatedRepos) {
        $environments = @(Invoke-GhCommand -Arguments @('api', '--method', 'GET', "repos/$repo/environments?per_page=100", '--paginate', '--jq', '.environments[].name'))
        foreach ($environment in $environments) {
            $variables = @(Invoke-GhCommand -Arguments @('variable', 'list', '--repo', $repo, '--env', $environment, '--json', 'name', '--jq', '.[].name'))
            $secrets = @(Invoke-GhCommand -Arguments @('secret', 'list', '--repo', $repo, '--env', $environment, '--json', 'name', '--jq', '.[].name'))
            if (@($variables | Where-Object { $_ -in $requiredVariables }).Count -or
                @($secrets | Where-Object { $_ -in $requiredSecrets }).Count) {
                throw "Environment credential overrides on $repo/$environment take precedence; reconcile them before organization setup."
            }
        }
    }
    # Refuse to replace existing shared coverage without a separate owner decision.
    $variables = @(Invoke-GhCommand -Arguments @('variable', 'list', '--org', $ExpectedOwner, '--json', 'name', '--jq', '.[].name'))
    $secrets = @(Invoke-GhCommand -Arguments @('secret', 'list', '--org', $ExpectedOwner, '--app', 'actions', '--json', 'name', '--jq', '.[].name'))
    if (@($variables | Where-Object { $_ -in $requiredVariables }).Count -or
        @($secrets | Where-Object { $_ -in $requiredSecrets }).Count) {
        throw 'Existing organization SFL credentials require owner reconciliation of selected coverage before replacement.'
    }
    $selectedNames = ($validatedRepos | ForEach-Object { $_.Split('/')[1] }) -join ','
    if ($PSCmdlet.ShouldProcess($ExpectedOwner, 'Create organization SFL credentials for explicitly selected repositories')) {
        Invoke-GhCommand -Arguments @('variable', 'set', 'SFL_APP_ID', '--org', $ExpectedOwner, '--repos', $selectedNames, '--body', $AppId) | Out-Null
        Invoke-GhCommand -Arguments @('variable', 'set', 'SFL_APP_CLIENT_ID', '--org', $ExpectedOwner, '--repos', $selectedNames, '--body', $ClientId) | Out-Null
        Get-Content -LiteralPath $resolvedPrivateKeyPath -Raw | & gh secret set SFL_APP_PRIVATE_KEY --org $ExpectedOwner --visibility selected --repos $selectedNames
        if ($LASTEXITCODE -ne 0) { throw 'Setting organization App private key failed.' }
        $expectedRepositories = @($validatedRepos | Sort-Object -Unique)
        foreach ($endpoint in @('variables/SFL_APP_ID', 'variables/SFL_APP_CLIENT_ID', 'secrets/SFL_APP_PRIVATE_KEY')) {
            $actual = @(Invoke-GhCommand -Arguments @('api', '--method', 'GET', "orgs/$ExpectedOwner/actions/$endpoint/repositories?per_page=100", '--paginate', '--jq', '.repositories[].full_name') | Sort-Object -Unique)
            if (($actual -join ',') -ine ($expectedRepositories -join ',')) {
                throw "Organization credential repository coverage mismatch for $endpoint."
            }
        }
        Write-Information "Created organization credentials for $($validatedRepos.Count) selected repositories. Repository/environment overrides must remain reconciled."
    }
    return
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
