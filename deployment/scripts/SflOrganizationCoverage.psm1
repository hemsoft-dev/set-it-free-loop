Set-StrictMode -Version Latest

function Get-SflOrganizationCoverageTargets {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)] [object] $Inventory,
        [object[]] $AdditionalTargets = @()
    )

    if ($Inventory.destination_login -cne 'hemsoft-dev' -or
        @($Inventory.repositories).Count -ne 67 -or
        @($Inventory.expected_repository_ids).Count -ne 67) {
        throw 'Coverage requires the original 67-repository hemsoft-dev inventory.'
    }
    $expected = [Collections.Generic.HashSet[long]]::new()
    foreach ($id in $Inventory.expected_repository_ids) {
        if (($id -isnot [int] -and $id -isnot [long]) -or $id -le 0 -or -not $expected.Add($id)) {
            throw 'Coverage inventory contains invalid or duplicate original IDs.'
        }
    }
    $seen = [Collections.Generic.HashSet[long]]::new()
    $names = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $targets = [Collections.Generic.List[object]]::new()
    foreach ($repository in $Inventory.repositories) {
        $id = $repository.id
        if (($id -isnot [int] -and $id -isnot [long]) -or -not $expected.Contains($id) -or -not $seen.Add($id)) {
            throw 'Coverage inventory must contain each original ID exactly once.'
        }
        # The owner retained these two personal Vercel repositories outside rollout.
        if ($id -in @(1162179521, 1169698740)) { continue }
        $name = [string]$repository.destination
        if ($name -cnotmatch '^hemsoft-dev/[A-Za-z0-9_.-]+$' -or -not $names.Add($name)) {
            throw 'Coverage requires unique canonical hemsoft-dev repository names.'
        }
        $targets.Add([pscustomobject]@{ repository_id = [long]$id; repository = $name })
    }
    if ($targets.Count -ne 65 -or $AdditionalTargets.Count -gt 8) {
        throw 'Coverage requires 65 transfer targets and at most eight explicit extra targets.'
    }
    foreach ($target in $AdditionalTargets) {
        $id = $target.repository_id
        $name = [string]$target.repository
        if (($id -isnot [int] -and $id -isnot [long]) -or $id -le 0 -or
            -not $seen.Add($id) -or $name -cnotmatch '^hemsoft-dev/[A-Za-z0-9_.-]+$' -or
            -not $names.Add($name)) {
            throw 'Extra coverage targets need unique positive IDs and canonical hemsoft-dev names.'
        }
        $targets.Add([pscustomobject]@{ repository_id = [long]$id; repository = $name })
    }
    return $targets.ToArray()
}

function Get-SflOrganizationRepositoryCoverage {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)] [object] $Target,
        [Parameter(Mandatory)] [ValidateNotNullOrEmpty()] [string] $Jwt,
        [Parameter(Mandatory)] [ValidateNotNullOrEmpty()] [string] $MetadataToken
    )
    if (($Target.repository_id -isnot [int] -and $Target.repository_id -isnot [long]) -or
        $Target.repository_id -le 0 -or
        $Target.repository -cnotmatch '^hemsoft-dev/[A-Za-z0-9_.-]+$') {
        throw 'Repository coverage target must identify the approved organization.'
    }
    $identityUrl = "https://api.github.com/repos/$($Target.repository)"
    $identityResponse = Invoke-WebRequest -Method Get -Uri $identityUrl -MaximumRedirection 0 -Headers @{
        Accept = 'application/vnd.github+json'
        Authorization = "Bearer $MetadataToken"
        'X-GitHub-Api-Version' = '2026-03-10'
        'User-Agent' = 'HemSoft-SFL-App-Coverage'
    }
    if ([int]$identityResponse.StatusCode -ne 200) {
        throw 'Repository identity requires an actual HTTP 200 response.'
    }
    $identity = $identityResponse.Content | ConvertFrom-Json
    if ($identity.id -ne $Target.repository_id -or $identity.full_name -cne $Target.repository -or
        $identity.owner.id -ne 338855369 -or $identity.owner.login -cne 'hemsoft-dev' -or
        $identity.owner.type -cne 'Organization') {
        throw 'Repository identity does not match the approved target ID, name and organization.'
    }
    $url = "https://api.github.com/repos/$($Target.repository)/installation"
    $response = Invoke-WebRequest -Method Get -Uri $url -MaximumRedirection 0 -Headers @{
        Accept = 'application/vnd.github+json'
        Authorization = "Bearer $Jwt"
        'X-GitHub-Api-Version' = '2026-03-10'
        'User-Agent' = 'HemSoft-SFL-App-Coverage'
    }
    if ([int]$response.StatusCode -ne 200) {
        throw 'Repository coverage requires an actual HTTP 200 installation response.'
    }
    $installation = $response.Content | ConvertFrom-Json
    Assert-SflGitHubAppInstallation -Installation $installation -Repository $Target.repository `
        -ExpectedAppId '4448946' -ExpectedClientId 'Iv23liwvwJJUh2bUIKLW' -ExpectedOwner 'hemsoft-dev'
    if ($installation.id -ne 169090497 -or $installation.account.id -ne 338855369 -or
        $installation.target_type -cne 'Organization' -or $installation.repository_selection -cne 'all') {
        throw 'Repository coverage does not match the approved organization installation.'
    }
    return [pscustomobject]@{
        observed_at = [DateTimeOffset]::UtcNow.ToString('o')
        repository_id = [long]$identity.id
        repository = $Target.repository
        method = 'GET'
        http_status = [int]$response.StatusCode
        request_url = $url
        identity = [pscustomobject]@{
            method = 'GET'
            http_status = [int]$identityResponse.StatusCode
            request_url = $identityUrl
            repository = $identity
        }
        installation = $installation
    }
}

function Invoke-SflOrganizationCoverage {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)] [object[]] $Targets,
        [Parameter(Mandatory)] [ValidateNotNullOrEmpty()] [string] $Jwt
    )
    $seen = [Collections.Generic.HashSet[long]]::new()
    if ($Targets.Count -lt 1 -or $Targets.Count -gt 73) { throw 'Coverage token target count is invalid.' }
    foreach ($target in $Targets) {
        if (($target.repository_id -isnot [int] -and $target.repository_id -isnot [long]) -or
            $target.repository_id -le 0 -or -not $seen.Add($target.repository_id) -or
            $target.repository -cnotmatch '^hemsoft-dev/[A-Za-z0-9_.-]+$' -or
            $target.repository_id -in @(1162179521,1169698740)) {
            throw 'Coverage token targets must be unique approved organization repositories.'
        }
    }
    $metadataToken = $null
    $primaryFailure = $null
    try {
        # This ephemeral token can only read metadata on the enumerated IDs.
        # It remains in RAM and is revoked below; no repository or organization secret is written.
        $tokenResponse = Invoke-WebRequest -Method Post -MaximumRedirection 0 `
          -Uri 'https://api.github.com/app/installations/169090497/access_tokens' `
          -Headers @{Accept='application/vnd.github+json';Authorization="Bearer $Jwt";'X-GitHub-Api-Version'='2026-03-10'} `
          -ContentType 'application/json' -Body (@{
            repository_ids = @($Targets.repository_id)
            permissions = @{metadata='read'}
          } | ConvertTo-Json -Depth 4)
        $tokenData = $tokenResponse.Content | ConvertFrom-Json
        $metadataToken = [string]$tokenData.token
        if (-not [string]::IsNullOrWhiteSpace($metadataToken)) { Write-Host "::add-mask::$metadataToken" }
        if ([int]$tokenResponse.StatusCode -ne 201 -or [string]::IsNullOrWhiteSpace($metadataToken) -or
            @($tokenData.permissions.PSObject.Properties).Count -ne 1 -or
            $tokenData.permissions.metadata -cne 'read') {
          throw 'Metadata token did not match the requested read-only permission scope.'
        }
        $tokenData = $null
        $tokenResponse = $null
        $coverage = [Collections.Generic.List[object]]::new()
        foreach ($target in $Targets) {
            $coverage.Add((Get-SflOrganizationRepositoryCoverage -Target $target -Jwt $Jwt -MetadataToken $metadataToken))
        }
        return $coverage.ToArray()
    }
    catch { $primaryFailure = $_; throw }
    finally {
        $cleanupFailed = $false
        if (-not [string]::IsNullOrWhiteSpace($metadataToken)) {
            try {
                $revoked = Invoke-WebRequest -Method Delete -MaximumRedirection 0 `
                  -Uri 'https://api.github.com/installation/token' `
                  -Headers @{Accept='application/vnd.github+json';Authorization="Bearer $metadataToken";'X-GitHub-Api-Version'='2026-03-10'}
                if ([int]$revoked.StatusCode -ne 204) { throw 'Unexpected token revocation status.' }
            }
            catch { $cleanupFailed = $true }
        }
        $metadataToken = $null
        $tokenData = $null
        $tokenResponse = $null
        if ($cleanupFailed) {
            if ($null -ne $primaryFailure) { Write-Warning 'Metadata token revocation also failed; the original verification failure is preserved.' }
            else { throw 'Metadata token revocation failed; coverage qualification is incomplete.' }
        }
    }
}

Export-ModuleMember -Function Get-SflOrganizationCoverageTargets, Get-SflOrganizationRepositoryCoverage, Invoke-SflOrganizationCoverage
