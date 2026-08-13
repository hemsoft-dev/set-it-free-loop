[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$modulePath = Join-Path $repoRoot 'deployment\scripts\SflGitHubAppBootstrap.psm1'
$scriptPath = Join-Path $repoRoot 'deployment\scripts\set-sfl-github-app-credentials.ps1'
$failures = [Collections.Generic.List[string]]::new()

Import-Module $modulePath -Force -DisableNameChecking

function Assert-Equal {
    param(
        [object] $Actual,
        [object] $Expected,
        [string] $Message
    )

    if ($Actual -cne $Expected) {
        $failures.Add("${Message}: expected '$Expected', got '$Actual'")
    }
}

function Assert-Throw {
    param(
        [scriptblock] $Action,
        [string] $Pattern,
        [string] $Message
    )

    try {
        & $Action
        $failures.Add("${Message}: expected an exception")
    }
    catch {
        if ($_.Exception.Message -notmatch $Pattern) {
            $failures.Add("${Message}: unexpected exception '$($_.Exception.Message)'")
        }
    }
}

function ConvertFrom-Base64Url([string] $Value) {
    $padded = $Value.Replace('-', '+').Replace('_', '/')
    switch ($padded.Length % 4) {
        2 { $padded += '==' }
        3 { $padded += '=' }
    }
    return [Convert]::FromBase64String($padded)
}

function Get-HealthyIdentity {
    return [pscustomobject]@{
        id        = 123456
        client_id = 'Iv1.testclient'
        owner     = [pscustomobject]@{ login = 'HemSoft' }
    }
}

function Get-HealthyInstallation {
    return [pscustomobject]@{
        id                   = 987654
        app_id               = 123456
        client_id            = 'Iv1.testclient'
        repository_selection = 'selected'
        target_type          = 'User'
        account              = [pscustomobject]@{ login = 'HemSoft' }
        permissions          = [pscustomobject]@{
            actions       = 'write'
            checks        = 'read'
            contents      = 'read'
            issues        = 'write'
            metadata      = 'read'
            pull_requests = 'write'
        }
    }
}

$fixedNow = [DateTimeOffset]::FromUnixTimeSeconds(1800000000)
$rsa = [Security.Cryptography.RSA]::Create(2048)
try {
    $privateKeyPem = $rsa.ExportPkcs8PrivateKeyPem()
    $jwt = ConvertTo-SflGitHubAppJwt -ClientId 'Iv1.testclient' -PrivateKeyPem $privateKeyPem -Now $fixedNow
    $parts = $jwt.Split('.')
    Assert-Equal $parts.Count 3 'JWT segment count'

    if ($parts.Count -eq 3) {
        $header = [Text.Encoding]::UTF8.GetString((ConvertFrom-Base64Url $parts[0])) | ConvertFrom-Json
        $payload = [Text.Encoding]::UTF8.GetString((ConvertFrom-Base64Url $parts[1])) | ConvertFrom-Json
        Assert-Equal $header.alg 'RS256' 'JWT algorithm'
        Assert-Equal $header.typ 'JWT' 'JWT type'
        Assert-Equal $payload.iss 'Iv1.testclient' 'JWT issuer'
        Assert-Equal $payload.iat ($fixedNow.AddSeconds(-60).ToUnixTimeSeconds()) 'JWT issued-at claim'
        Assert-Equal $payload.exp ($fixedNow.AddMinutes(9).ToUnixTimeSeconds()) 'JWT expiry claim'

        $signatureValid = $rsa.VerifyData(
            [Text.Encoding]::UTF8.GetBytes("$($parts[0]).$($parts[1])"),
            (ConvertFrom-Base64Url $parts[2]),
            [Security.Cryptography.HashAlgorithmName]::SHA256,
            [Security.Cryptography.RSASignaturePadding]::Pkcs1
        )
        Assert-Equal $signatureValid $true 'JWT signature'
    }
}
finally {
    $rsa.Dispose()
}

Assert-SflGitHubAppIdentity `
    -Identity (Get-HealthyIdentity) `
    -ExpectedAppId '123456' `
    -ExpectedClientId 'Iv1.testclient' `
    -ExpectedOwner 'HemSoft'

Assert-Throw {
    Assert-SflGitHubAppIdentity `
        -Identity ([pscustomobject]@{
            id = 123456; client_id = 'Iv1.wrong'; owner = [pscustomobject]@{ login = 'Other' }
        }) `
        -ExpectedAppId '123456' `
        -ExpectedClientId 'Iv1.testclient' `
        -ExpectedOwner 'HemSoft'
} 'client ID.*App owner' 'App identity mismatch'

Assert-SflGitHubAppInstallation `
    -Installation (Get-HealthyInstallation) `
    -Repository 'HemSoft/example' `
    -ExpectedAppId '123456' `
    -ExpectedClientId 'Iv1.testclient' `
    -ExpectedOwner 'HemSoft'

$allRepositories = Get-HealthyInstallation
$allRepositories.repository_selection = 'all'
Assert-Throw {
    Assert-SflGitHubAppInstallation -Installation $allRepositories -Repository 'HemSoft/example' `
        -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'HemSoft'
} 'not limited to selected repositories' 'All-repository installation'

$overprivileged = Get-HealthyInstallation
$overprivileged.permissions.contents = 'write'
$overprivileged.permissions | Add-Member -NotePropertyName workflows -NotePropertyValue write
Assert-Throw {
    Assert-SflGitHubAppInstallation -Installation $overprivileged -Repository 'HemSoft/example' `
        -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'HemSoft'
} "contents.*expected 'read'.*unexpected permission 'workflows'" 'Over-privileged installation'

$missingPermission = Get-HealthyInstallation
$missingPermission.permissions.PSObject.Properties.Remove('checks')
Assert-Throw {
    Assert-SflGitHubAppInstallation -Installation $missingPermission -Repository 'HemSoft/example' `
        -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'HemSoft'
} "permission 'checks' is 'missing'" 'Missing required permission'

$wrongInstallation = Get-HealthyInstallation
$wrongInstallation.app_id = 777
$wrongInstallation.account.login = 'Other'
$wrongInstallation.target_type = 'Organization'
Assert-Throw {
    Assert-SflGitHubAppInstallation -Installation $wrongInstallation -Repository 'HemSoft/example' `
        -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'HemSoft'
} 'App ID.*target type.*installation account' 'Wrong installation identity'

# Exercise the real script with faked read-only dependencies. A permission
# failure must happen before the first variable or secret mutation.
$scriptGhCalls = [Collections.Generic.List[string]]::new()
$scriptPrivateKeyPath = Join-Path ([IO.Path]::GetTempPath()) "sfl-bootstrap-test-$([Guid]::NewGuid().ToString('N')).pem"
$scriptRsa = [Security.Cryptography.RSA]::Create(2048)
try {
    Set-Content -LiteralPath $scriptPrivateKeyPath -Value $scriptRsa.ExportPkcs8PrivateKeyPem() -NoNewline

    function global:gh {
        $call = $args -join ' '
        $scriptGhCalls.Add($call)
        $global:LASTEXITCODE = 0
        if ($call -eq 'api user --jq .login') {
            return 'HemSoft'
        }
        if ($call -match '^repo view HemSoft/example ') {
            return '{"nameWithOwner":"HemSoft/example","owner":{"login":"HemSoft"},"visibility":"PRIVATE"}'
        }
        return ''
    }

    function global:Invoke-RestMethod {
        param([string] $Method, [string] $Uri, [hashtable] $Headers)
        if ($Method -ne 'Get' -or $Headers.Authorization -notmatch '^Bearer [^.]+\.[^.]+\.[^.]+$') {
            throw 'GitHub App API request did not use an authenticated GET.'
        }
        if ($Uri -eq 'https://api.github.com/app') {
            return Get-HealthyIdentity
        }
        if ($Uri -eq 'https://api.github.com/repos/HemSoft/example/installation') {
            $installation = Get-HealthyInstallation
            $installation.permissions.checks = 'write'
            return $installation
        }
        throw "Unexpected API URI: $Uri"
    }

    Assert-Throw {
        & $scriptPath -Repos 'HemSoft/example' -AppId '123456' -ClientId 'Iv1.testclient' `
            -PrivateKeyPath $scriptPrivateKeyPath
    } "permission 'checks' is 'write'; expected 'read'" 'Bootstrap permission preflight'

    $mutationCalls = @($scriptGhCalls | Where-Object { $_ -match '^(variable|secret) set ' })
    Assert-Equal $mutationCalls.Count 0 'Mutation count after failed App preflight'
}
finally {
    Remove-Item function:\global:gh -ErrorAction SilentlyContinue
    Remove-Item function:\global:Invoke-RestMethod -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $scriptPrivateKeyPath) {
        Remove-Item -LiteralPath $scriptPrivateKeyPath -Force
    }
    $scriptRsa.Dispose()
}

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ }
    exit 1
}

Write-Output 'SFL GitHub App bootstrap contract passed.'
