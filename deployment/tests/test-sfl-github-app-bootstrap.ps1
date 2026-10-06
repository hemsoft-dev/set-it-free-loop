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
            checks        = 'write'
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

$organizationInstallation = Get-HealthyInstallation
$organizationInstallation.account.login = 'hemsoft-dev'
$organizationInstallation.target_type = 'Organization'
Assert-SflGitHubAppInstallation -Installation $organizationInstallation -Repository 'hemsoft-dev/example' `
    -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'hemsoft-dev'
$organizationInstallation.target_type = 'User'
Assert-Throw {
    Assert-SflGitHubAppInstallation -Installation $organizationInstallation -Repository 'hemsoft-dev/example' `
        -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'hemsoft-dev'
} 'target type' 'Organization installation target mismatch'

$allRepositories = Get-HealthyInstallation
$allRepositories.repository_selection = 'all'
Assert-SflGitHubAppInstallation `
    -Installation $allRepositories `
    -Repository 'HemSoft/example' `
    -ExpectedAppId '123456' `
    -ExpectedClientId 'Iv1.testclient' `
    -ExpectedOwner 'HemSoft'

$invalidSelection = Get-HealthyInstallation
$invalidSelection.repository_selection = 'unknown'
Assert-Throw {
    Assert-SflGitHubAppInstallation -Installation $invalidSelection -Repository 'HemSoft/example' `
        -ExpectedAppId '123456' -ExpectedClientId 'Iv1.testclient' -ExpectedOwner 'HemSoft'
} 'repository selection.*invalid' 'Invalid repository selection'

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
        if ($call -eq 'api --method GET repos/HemSoft/example') {
            return '{"full_name":"HemSoft/example","owner":{"login":"HemSoft"}}'
        }
        if ($call -eq 'api --method GET repos/HemSoft/example/collaborators/HemSoft/permission') {
            return '{"permission":"admin","user":{"login":"HemSoft"}}'
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
            $installation.permissions.checks = 'read'
            return $installation
        }
        throw "Unexpected API URI: $Uri"
    }

    Assert-Throw {
        & $scriptPath -Repos 'HemSoft/example' -AppId '123456' -ClientId 'Iv1.testclient' `
            -PrivateKeyPath $scriptPrivateKeyPath
    } "permission 'checks' is 'read'; expected 'write'" 'Bootstrap permission preflight'

    $mutationCalls = @($scriptGhCalls | Where-Object { $_ -match '^(variable|secret) set ' })
    Assert-Equal $mutationCalls.Count 0 'Mutation count after failed App preflight'


    # Exercise organization setup and precedence with real script control flow.
    function global:Invoke-RestMethod {
        param([string] $Method, [string] $Uri, [hashtable] $Headers)
        if ($Method -ne 'Get' -or $Headers.Authorization -notmatch '^Bearer [^.]+\.[^.]+\.[^.]+$') {
            throw 'Expected authenticated App GET.'
        }
        if ($Uri -eq 'https://api.github.com/app') { return Get-HealthyIdentity }
        if ($Uri -eq 'https://api.github.com/repos/hemsoft-dev/example/installation') {
            $installation = Get-HealthyInstallation
            $installation.account.login = 'hemsoft-dev'
            $installation.target_type = 'Organization'
            return $installation
        }
        throw "Unexpected App API URI: $Uri"
    }
    function global:gh {
        $call = $args -join ' '
        $scriptGhCalls.Add($call)
        $global:LASTEXITCODE = 0
        switch ($call) {
            'api user --jq .login' { return 'HemSoft' }
            'api --method GET repos/hemsoft-dev/example' { return '{"full_name":"hemsoft-dev/example","owner":{"login":"hemsoft-dev"}}' }
            'api --method GET repos/hemsoft-dev/example/collaborators/HemSoft/permission' { return '{"permission":"admin","user":{"login":"HemSoft"}}' }
            'api --method GET orgs/hemsoft-dev/memberships/HemSoft' {
                if ($global:sflBootstrapTestCase -eq 'member') { return '{"state":"active","role":"member"}' }
                return '{"state":"active","role":"admin"}'
            }
            'api --method GET repos/hemsoft-dev/example/environments?per_page=100 --paginate --jq .environments[].name' { return 'production' }
        }
        if ($call -match '^(variable|secret) list ') {
            if ($global:sflBootstrapTestCase -eq 'repository' -and $call -match '^variable list --repo hemsoft-dev/example --json') { return 'SFL_APP_ID' }
            if ($global:sflBootstrapTestCase -eq 'environment' -and $call -match '^secret list --repo hemsoft-dev/example --env production') { return 'SFL_APP_PRIVATE_KEY' }
            if ($global:sflBootstrapTestCase -eq 'shared' -and $call -match '^variable list --org') { return 'SFL_APP_CLIENT_ID' }
            return ''
        }
        if ($call -match '^(variable|secret) set ') { return '' }
        if ($call -match '^api --method GET orgs/hemsoft-dev/actions/(variables|secrets)/[^/]+/repositories') {
            if ($global:sflBootstrapTestCase -eq 'coverage') { return 'hemsoft-dev/wrong' }
            return 'hemsoft-dev/example'
        }
        throw "Unexpected gh call: $call"
    }
    $parameters = @{
        Repos = 'hemsoft-dev/example'; AppId = '123456'; ClientId = 'Iv1.testclient'
        PrivateKeyPath = $scriptPrivateKeyPath; ExpectedOwner = 'hemsoft-dev'
        ExpectedAppOwner = 'HemSoft'; CredentialScope = 'organization'
    }
    foreach ($case in @(
        @{ Name = 'member'; Pattern = 'organization owner' },
        @{ Name = 'repository'; Pattern = 'Repository credential overrides' },
        @{ Name = 'environment'; Pattern = 'Environment credential overrides' },
        @{ Name = 'shared'; Pattern = 'Existing organization SFL credentials' }
    )) {
        $global:sflBootstrapTestCase = $case.Name
        $scriptGhCalls.Clear()
        Assert-Throw { & $scriptPath @parameters } $case.Pattern "Organization $($case.Name) preflight"
        Assert-Equal @($scriptGhCalls | Where-Object { $_ -match '^(variable|secret) set ' }).Count 0 "No writes on $($case.Name) denial"
    }
    $global:sflBootstrapTestCase = 'success'
    $scriptGhCalls.Clear()
    & $scriptPath @parameters
    Assert-Equal @($scriptGhCalls | Where-Object { $_ -match '^(variable|secret) set ' }).Count 3 'Selected organization credential write count'
    Assert-Equal @($scriptGhCalls | Where-Object { $_ -match '^secret set .*--visibility selected --repos example$' }).Count 1 'Private key uses selected repositories'
    Assert-Equal @($scriptGhCalls | Where-Object { $_ -match '^api --method GET orgs/hemsoft-dev/actions/.*/repositories' }).Count 3 'All credential coverage verified'
    $mixedCase = $parameters.Clone()
    $mixedCase.Repos = 'hemsoft-dev/EXAMPLE'
    & $scriptPath @mixedCase
    $global:sflBootstrapTestCase = 'coverage'
    Assert-Throw { & $scriptPath @parameters } 'coverage mismatch' 'Coverage post-write failure is visible'
    $global:sflBootstrapTestCase = 'success'
    $scriptGhCalls.Clear()
    & $scriptPath @parameters -WhatIf
    Assert-Equal @($scriptGhCalls | Where-Object { $_ -match '^(variable|secret) set ' }).Count 0 'WhatIf leaves credentials untouched'
}
finally {
    Remove-Item function:\global:gh -ErrorAction SilentlyContinue
    Remove-Item function:\global:Invoke-RestMethod -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $scriptPrivateKeyPath) {
        Remove-Item -LiteralPath $scriptPrivateKeyPath -Force
    }
    Remove-Variable -Name sflBootstrapTestCase -Scope Global -ErrorAction SilentlyContinue
    $scriptRsa.Dispose()
}

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ }
    exit 1
}

Write-Output 'SFL GitHub App bootstrap contract passed.'
