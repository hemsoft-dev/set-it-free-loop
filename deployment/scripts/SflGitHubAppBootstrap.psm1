Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function ConvertTo-SflBase64Url {
    param(
        [Parameter(Mandatory = $true)]
        [byte[]] $Bytes
    )

    return [Convert]::ToBase64String($Bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

function ConvertTo-SflGitHubAppJwt {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string] $ClientId,

        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string] $PrivateKeyPem,

        [Parameter(Mandatory = $false)]
        [DateTimeOffset] $Now = [DateTimeOffset]::UtcNow
    )

    $issuedAt = $Now.AddSeconds(-60).ToUnixTimeSeconds()
    $expiresAt = $Now.AddMinutes(9).ToUnixTimeSeconds()
    $headerJson = @{ alg = 'RS256'; typ = 'JWT' } | ConvertTo-Json -Compress
    $payloadJson = @{ iat = $issuedAt; exp = $expiresAt; iss = $ClientId } | ConvertTo-Json -Compress
    $header = ConvertTo-SflBase64Url -Bytes ([Text.Encoding]::UTF8.GetBytes($headerJson))
    $payload = ConvertTo-SflBase64Url -Bytes ([Text.Encoding]::UTF8.GetBytes($payloadJson))
    $unsignedToken = "$header.$payload"

    $rsa = [Security.Cryptography.RSA]::Create()
    try {
        $rsa.ImportFromPem($PrivateKeyPem)
        $signature = $rsa.SignData(
            [Text.Encoding]::UTF8.GetBytes($unsignedToken),
            [Security.Cryptography.HashAlgorithmName]::SHA256,
            [Security.Cryptography.RSASignaturePadding]::Pkcs1
        )
    }
    finally {
        $rsa.Dispose()
    }

    return "$unsignedToken.$(ConvertTo-SflBase64Url -Bytes $signature)"
}

function Invoke-SflGitHubAppGet {
    param(
        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string] $Path,

        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string] $Jwt
    )

    $headers = @{
        Accept                 = 'application/vnd.github+json'
        Authorization          = "Bearer $Jwt"
        'X-GitHub-Api-Version' = '2026-03-10'
        'User-Agent'           = 'HemSoft-SFL-App-Bootstrap'
    }

    try {
        return Invoke-RestMethod -Method Get -Uri "https://api.github.com$Path" -Headers $headers
    }
    catch {
        throw "GitHub App API GET $Path failed: $($_.Exception.Message)"
    }
}

function Get-SflGitHubAppIdentity {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string] $Jwt
    )

    return Invoke-SflGitHubAppGet -Path '/app' -Jwt $Jwt
}

function Get-SflGitHubAppRepositoryInstallation {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [ValidatePattern('^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')]
        [string] $Repository,

        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string] $Jwt
    )

    return Invoke-SflGitHubAppGet -Path "/repos/$Repository/installation" -Jwt $Jwt
}

function Get-SflObjectPropertyValue {
    param(
        [Parameter(Mandatory = $true)]
        [AllowNull()]
        [object] $Object,

        [Parameter(Mandatory = $true)]
        [string] $Name
    )

    if ($null -eq $Object) {
        return $null
    }

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }

    return $property.Value
}

function Assert-SflGitHubAppIdentity {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [object] $Identity,

        [Parameter(Mandatory = $true)]
        [string] $ExpectedAppId,

        [Parameter(Mandatory = $true)]
        [string] $ExpectedClientId,

        [Parameter(Mandatory = $true)]
        [string] $ExpectedOwner
    )

    $problems = [Collections.Generic.List[string]]::new()
    if ([string](Get-SflObjectPropertyValue -Object $Identity -Name 'id') -cne $ExpectedAppId) {
        $problems.Add('App ID does not match the configured App ID.')
    }
    if ([string](Get-SflObjectPropertyValue -Object $Identity -Name 'client_id') -cne $ExpectedClientId) {
        $problems.Add('App client ID does not match the configured client ID.')
    }

    $owner = Get-SflObjectPropertyValue -Object $Identity -Name 'owner'
    if ([string](Get-SflObjectPropertyValue -Object $owner -Name 'login') -cne $ExpectedOwner) {
        $problems.Add("App owner is not '$ExpectedOwner'.")
    }

    if ($problems.Count -gt 0) {
        throw "GitHub App identity validation failed: $($problems -join ' ')"
    }
}

function Assert-SflGitHubAppInstallation {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [object] $Installation,

        [Parameter(Mandatory = $true)]
        [string] $Repository,

        [Parameter(Mandatory = $true)]
        [string] $ExpectedAppId,

        [Parameter(Mandatory = $true)]
        [string] $ExpectedClientId,

        [Parameter(Mandatory = $true)]
        [string] $ExpectedOwner
    )

    $requiredPermissions = [ordered]@{
        actions       = 'write'
        checks        = 'write'
        contents      = 'read'
        issues        = 'write'
        metadata      = 'read'
        pull_requests = 'write'
    }
    $problems = [Collections.Generic.List[string]]::new()

    if ([string](Get-SflObjectPropertyValue -Object $Installation -Name 'app_id') -cne $ExpectedAppId) {
        $problems.Add('installation App ID does not match')
    }
    if ([string](Get-SflObjectPropertyValue -Object $Installation -Name 'client_id') -cne $ExpectedClientId) {
        $problems.Add('installation client ID does not match')
    }
    $repositorySelection = [string](Get-SflObjectPropertyValue -Object $Installation -Name 'repository_selection')
    if ($repositorySelection -notin @('selected', 'all')) {
        $problems.Add("installation repository selection '$repositorySelection' is invalid")
    }
    if ([string](Get-SflObjectPropertyValue -Object $Installation -Name 'target_type') -cne 'User') {
        $problems.Add('installation target type is not User')
    }

    $account = Get-SflObjectPropertyValue -Object $Installation -Name 'account'
    if ([string](Get-SflObjectPropertyValue -Object $account -Name 'login') -cne $ExpectedOwner) {
        $problems.Add("installation account is not '$ExpectedOwner'")
    }

    $permissions = Get-SflObjectPropertyValue -Object $Installation -Name 'permissions'
    foreach ($permission in $requiredPermissions.GetEnumerator()) {
        $actual = [string](Get-SflObjectPropertyValue -Object $permissions -Name $permission.Key)
        if ($actual -cne $permission.Value) {
            $displayActual = if ([string]::IsNullOrEmpty($actual)) { 'missing' } else { $actual }
            $problems.Add("permission '$($permission.Key)' is '$displayActual'; expected '$($permission.Value)'")
        }
    }

    if ($null -ne $permissions) {
        foreach ($permission in $permissions.PSObject.Properties) {
            if (-not $requiredPermissions.Contains($permission.Name) -and [string]$permission.Value -cne 'none') {
                $problems.Add("unexpected permission '$($permission.Name)' is '$($permission.Value)'")
            }
        }
    }

    if ($problems.Count -gt 0) {
        throw "GitHub App installation validation failed for ${Repository}: $($problems -join '; ')."
    }
}

Export-ModuleMember -Function @(
    'ConvertTo-SflGitHubAppJwt',
    'Get-SflGitHubAppIdentity',
    'Get-SflGitHubAppRepositoryInstallation',
    'Assert-SflGitHubAppIdentity',
    'Assert-SflGitHubAppInstallation'
)
