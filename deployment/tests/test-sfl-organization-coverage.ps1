$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
Import-Module (Join-Path $root 'deployment/scripts/SflOrganizationCoverage.psm1') -Force
Import-Module (Join-Path $root 'deployment/scripts/SflGitHubAppBootstrap.psm1') -Force
$raw = Get-Content (Join-Path $root 'docs/organization-migration/inventory.json') -Raw
function Baseline { return $raw | ConvertFrom-Json }
function Assert-Rejected([scriptblock]$Action, [string]$Pattern) {
    $caught = $false
    try { & $Action | Out-Null }
    catch {
        if ($_.Exception.Message -notmatch $Pattern) { throw }
        $caught = $true
    }
    if (-not $caught) { throw 'Expected invalid coverage target rejection.' }
}
$targets = @(Get-SflOrganizationCoverageTargets -Inventory (Baseline))
if ($targets.Count -ne 65 -or @($targets | Where-Object repository_id -In @(1162179521,1169698740)).Count) {
    throw 'Coverage must include exactly the transferred population and exclude both retained IDs.'
}
$extras = @(
    [pscustomobject]@{repository_id=1408025382;repository='hemsoft-dev/sfl-migration-pilot-private'},
    [pscustomobject]@{repository_id=1408029795;repository='hemsoft-dev/sfl-migration-pilot-public'},
    [pscustomobject]@{repository_id=1409692659;repository='hemsoft-dev/sfl-migration-onboarding-proof'}
)
if (@(Get-SflOrganizationCoverageTargets -Inventory (Baseline) -AdditionalTargets $extras).Count -ne 68) {
    throw 'Explicit pilot coverage must preserve the original transfer population.'
}
Assert-Rejected { $x=Baseline; $x.destination_login='another-org'; Get-SflOrganizationCoverageTargets $x } 'original 67'
Assert-Rejected { $x=Baseline; $x.repositories[0].id=$x.repositories[1].id; Get-SflOrganizationCoverageTargets $x } 'original ID'
Assert-Rejected { $x=Baseline; $x.expected_repository_ids[0]=$x.expected_repository_ids[1]; Get-SflOrganizationCoverageTargets $x } 'duplicate original'
Assert-Rejected { $x=Baseline; $x.repositories[0].destination='other-org/private'; Get-SflOrganizationCoverageTargets $x } 'canonical hemsoft-dev'
Assert-Rejected { $x=Baseline; $x.repositories[0].destination=$x.repositories[1].destination; Get-SflOrganizationCoverageTargets $x } 'unique canonical'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) @([pscustomobject]@{repository_id=1162179521;repository='hemsoft-dev/retained'}) } 'Extra coverage'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) @($extras[0],$extras[0]) } 'Extra coverage'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) @([pscustomobject]@{repository_id=1;repository='other-org/private'}) } 'Extra coverage'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) @([pscustomobject]@{repository_id=0;repository='hemsoft-dev/private'}) } 'Extra coverage'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) @([pscustomobject]@{repository_id='1';repository='hemsoft-dev/private'}) } 'Extra coverage'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) @([pscustomobject]@{repository_id=1;repository='hemsoft-dev/private?token=value'}) } 'Extra coverage'
Assert-Rejected { Get-SflOrganizationCoverageTargets (Baseline) (@($extras[0])*9) } 'at most eight'
$global:SflCoverageCleanupFails = $false
$global:SflCoveragePrimaryException = $null
$global:SflCoverageTokenPermissions = @{metadata='read'}
$global:SflCoverageRequests = [Collections.Generic.List[object]]::new()
$global:SflCoverageHttpStatus = 200
$global:SflCoverageInstallation = [ordered]@{
    id=169090497; app_id=4448946; client_id='Iv23liwvwJJUh2bUIKLW'
    account=@{id=338855369;login='hemsoft-dev';type='Organization'}
    repository_selection='all'; target_type='Organization'; suspended_at=$null; suspended_by=$null
    permissions=@{actions='write';checks='write';contents='read';issues='write';metadata='read';pull_requests='write'}
}
$global:SflCoverageIdentity = [pscustomobject]@{
    id=1408025382;full_name='hemsoft-dev/sfl-migration-pilot-private'
    owner=@{id=338855369;login='hemsoft-dev';type='Organization'}
}
$identityBaseline = $global:SflCoverageIdentity | ConvertTo-Json -Depth 10
$installationBaseline = $global:SflCoverageInstallation | ConvertTo-Json -Depth 10
function global:Invoke-WebRequest {
    param($Method,$Uri,$MaximumRedirection,$Headers,$ContentType,$Body)
    $global:SflCoverageRequests.Add(@{method=$Method;uri=$Uri;redirects=$MaximumRedirection;headers=$Headers;body=$Body})
    if ($Method -eq 'Post') {
        return [pscustomobject]@{StatusCode=201;Content=(@{token='fixture-metadata-token';permissions=$global:SflCoverageTokenPermissions} | ConvertTo-Json -Depth 4)}
    }
    if ($Method -eq 'Delete') {
        if ($global:SflCoverageCleanupFails) { throw 'fixture-revocation-failure' }
        return [pscustomobject]@{StatusCode=204;Content=''}
    }
    if ($null -ne $global:SflCoveragePrimaryException) { throw $global:SflCoveragePrimaryException }
    return [pscustomobject]@{StatusCode=$global:SflCoverageHttpStatus;Content=($(if ($Uri.EndsWith('/installation')) {$global:SflCoverageInstallation} else {$global:SflCoverageIdentity}) | ConvertTo-Json -Depth 10)}
}
try {
    $result = Get-SflOrganizationRepositoryCoverage -Target $extras[0] -Jwt 'fixture-jwt' -MetadataToken 'fixture-metadata-token'
    if ($result.http_status -ne 200 -or $result.repository_id -ne 1408025382 -or
        $result.request_url -cne 'https://api.github.com/repos/hemsoft-dev/sfl-migration-pilot-private/installation' -or
        $global:SflCoverageRequests.Count -ne 2) { throw 'Coverage receipt must bind the actual target and response.' }
    $identityRequest = $global:SflCoverageRequests[0]
    if ($identityRequest.uri -cne 'https://api.github.com/repos/hemsoft-dev/sfl-migration-pilot-private' -or
        $identityRequest.headers.Authorization -cne 'Bearer fixture-metadata-token' -or
        $identityRequest.redirects -ne 0 -or $result.identity.repository.id -ne $result.repository_id) {
        throw 'Identity must be verified using a nonredirecting metadata GET before the installation GET.'
    }
    $request = $global:SflCoverageRequests[1]
    if ($request.method -cne 'Get' -or $request.redirects -ne 0 -or
        $request.headers.Authorization -cne 'Bearer fixture-jwt') { throw 'Installation coverage must use a nonredirecting App-authenticated GET.' }
    $global:SflCoverageHttpStatus = 302
    Assert-Rejected { Get-SflOrganizationRepositoryCoverage $extras[0] 'fixture-jwt' 'fixture-metadata-token' } 'actual HTTP 200'
    $global:SflCoverageHttpStatus = 200
    foreach ($field in @('id','full_name','owner')) {
        $global:SflCoverageIdentity = $identityBaseline | ConvertFrom-Json
        switch ($field) {
            'id' { $global:SflCoverageIdentity.id=123 }
            'full_name' { $global:SflCoverageIdentity.full_name='hemsoft-dev/another-repository' }
            'owner' { $global:SflCoverageIdentity.owner.id=123 }
        }
        $count = $global:SflCoverageRequests.Count
        Assert-Rejected { Get-SflOrganizationRepositoryCoverage $extras[0] 'fixture-jwt' 'fixture-metadata-token' } 'Repository identity'
        if ($global:SflCoverageRequests.Count -ne $count + 1) { throw 'Identity mismatch must reject before the installation request.' }
    }
    $global:SflCoverageIdentity = $identityBaseline | ConvertFrom-Json
    foreach ($field in @('id','account','repository_selection','permissions','suspended_at')) {
        $global:SflCoverageInstallation = $installationBaseline | ConvertFrom-Json
        switch ($field) {
            'id' { $global:SflCoverageInstallation.id=123 }
            'account' { $global:SflCoverageInstallation.account.id=123 }
            'repository_selection' { $global:SflCoverageInstallation.repository_selection='selected' }
            'permissions' { $global:SflCoverageInstallation.permissions.contents='write' }
            'suspended_at' { $global:SflCoverageInstallation.suspended_at='2026-10-08T00:00:00Z' }
        }
        Assert-Rejected { Get-SflOrganizationRepositoryCoverage $extras[0] 'fixture-jwt' 'fixture-metadata-token' } 'installation|permission'
    }
    $count = $global:SflCoverageRequests.Count
    Assert-Rejected { Get-SflOrganizationRepositoryCoverage ([pscustomobject]@{repository_id=1;repository='other-org/private'}) 'fixture-jwt' 'fixture-metadata-token' } 'approved organization'
    if ($global:SflCoverageRequests.Count -ne $count) { throw 'Wrong-owner target must fail before any request.' }
    $global:SflCoverageInstallation = $installationBaseline | ConvertFrom-Json
    $global:SflCoverageRequests.Clear()
    $lifecycleResult = @(Invoke-SflOrganizationCoverage -Targets @($extras[0]) -Jwt 'fixture-jwt')
    if ($lifecycleResult.Count -ne 1 -or $global:SflCoverageRequests.Count -ne 4 -or
        ($global:SflCoverageRequests.method -join ',') -cne 'Post,Get,Get,Delete') {
        throw 'Coverage must mint once, verify identity and installation, then revoke once.'
    }
    $mintBody = $global:SflCoverageRequests[0].body | ConvertFrom-Json
    if (@($mintBody.permissions.PSObject.Properties).Count -ne 1 -or $mintBody.permissions.metadata -cne 'read' -or
        @($mintBody.repository_ids).Count -ne 1 -or $mintBody.repository_ids[0] -ne 1408025382) {
        throw 'Runtime credential must be restricted to metadata read and enumerated IDs.'
    }
    $global:SflCoverageCleanupFails = $true
    Assert-Rejected { Invoke-SflOrganizationCoverage -Targets @($extras[0]) -Jwt 'fixture-jwt' } 'revocation failed'
    $global:SflCoverageIdentity.id = 123
    $global:SflCoverageRequests.Clear()
    Assert-Rejected { Invoke-SflOrganizationCoverage -Targets @($extras[0]) -Jwt 'fixture-jwt' } 'Repository identity'
    if (($global:SflCoverageRequests.method -join ',') -cne 'Post,Get,Delete') {
        throw 'Primary identity failure must still revoke once, with no installation GET or retry.'
    }
    $global:SflCoveragePrimaryException = [InvalidOperationException]::new('fixture-primary-failure')
    $global:SflCoverageRequests.Clear()
    $caught = $null
    try { Invoke-SflOrganizationCoverage -Targets @($extras[0]) -Jwt 'fixture-jwt' | Out-Null }
    catch { $caught = $_.Exception }
    if (-not [object]::ReferenceEquals($caught,$global:SflCoveragePrimaryException) -or
        ($global:SflCoverageRequests.method -join ',') -cne 'Post,Get,Delete') {
        throw 'Revocation failure must preserve the exact primary exception with no replay.'
    }
    $global:SflCoveragePrimaryException = $null
    $global:SflCoverageCleanupFails = $false
    $global:SflCoverageTokenPermissions = @{metadata='read';contents='read'}
    $global:SflCoverageRequests.Clear()
    Assert-Rejected { Invoke-SflOrganizationCoverage -Targets @($extras[0]) -Jwt 'fixture-jwt' } 'permission scope'
    if (($global:SflCoverageRequests.method -join ',') -cne 'Post,Delete') {
        throw 'Unexpected token permissions must reject and revoke before repository requests.'
    }
    $count = $global:SflCoverageRequests.Count
    Assert-Rejected { Invoke-SflOrganizationCoverage -Targets @($extras[0],$extras[0]) -Jwt 'fixture-jwt' } 'unique approved'
    if ($global:SflCoverageRequests.Count -ne $count) { throw 'Invalid token scope must reject before minting.' }

}
finally {
    Remove-Item function:\global:Invoke-WebRequest
    Remove-Variable -Scope Global SflCoverageRequests,SflCoverageHttpStatus,SflCoverageInstallation,SflCoverageIdentity,SflCoverageCleanupFails,SflCoveragePrimaryException,SflCoverageTokenPermissions
}
'Organization App coverage scope and response contracts passed.'
