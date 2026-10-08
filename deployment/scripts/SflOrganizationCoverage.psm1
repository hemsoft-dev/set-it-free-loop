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
        [Parameter(Mandatory)] [ValidateNotNullOrEmpty()] [string] $Jwt
    )
    if (($Target.repository_id -isnot [int] -and $Target.repository_id -isnot [long]) -or
        $Target.repository_id -le 0 -or
        $Target.repository -cnotmatch '^hemsoft-dev/[A-Za-z0-9_.-]+$') {
        throw 'Repository coverage target must identify the approved organization.'
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
        repository_id = $Target.repository_id
        repository = $Target.repository
        method = 'GET'
        http_status = [int]$response.StatusCode
        request_url = $url
        installation = $installation
    }
}

Export-ModuleMember -Function Get-SflOrganizationCoverageTargets, Get-SflOrganizationRepositoryCoverage
