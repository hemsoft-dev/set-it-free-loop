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
$global:SflCoverageRequests = [Collections.Generic.List[object]]::new()
$global:SflCoverageHttpStatus = 200
$global:SflCoverageInstallation = [ordered]@{
    id=169090497; app_id=4448946; client_id='Iv23liwvwJJUh2bUIKLW'
    account=@{id=338855369;login='hemsoft-dev';type='Organization'}
    repository_selection='all'; target_type='Organization'; suspended_at=$null; suspended_by=$null
    permissions=@{actions='write';checks='write';contents='read';issues='write';metadata='read';pull_requests='write'}
}
$installationBaseline = $global:SflCoverageInstallation | ConvertTo-Json -Depth 10
function global:Invoke-WebRequest {
    param($Method,$Uri,$MaximumRedirection,$Headers)
    $global:SflCoverageRequests.Add(@{method=$Method;uri=$Uri;redirects=$MaximumRedirection;headers=$Headers})
    return [pscustomobject]@{StatusCode=$global:SflCoverageHttpStatus;Content=($global:SflCoverageInstallation | ConvertTo-Json -Depth 10)}
}
try {
    $result = Get-SflOrganizationRepositoryCoverage -Target $extras[0] -Jwt 'fixture-jwt'
    if ($result.http_status -ne 200 -or $result.repository_id -ne 1408025382 -or
        $result.request_url -cne 'https://api.github.com/repos/hemsoft-dev/sfl-migration-pilot-private/installation' -or
        $global:SflCoverageRequests.Count -ne 1) { throw 'Coverage receipt must bind the actual target and response.' }
    $request = $global:SflCoverageRequests[0]
    if ($request.method -cne 'Get' -or $request.redirects -ne 0 -or
        $request.headers.Authorization -cne 'Bearer fixture-jwt') { throw 'Coverage must use one nonredirecting App-authenticated GET.' }
    $global:SflCoverageHttpStatus = 302
    Assert-Rejected { Get-SflOrganizationRepositoryCoverage $extras[0] 'fixture-jwt' } 'actual HTTP 200'
    $global:SflCoverageHttpStatus = 200
    foreach ($field in @('id','account','repository_selection','permissions','suspended_at')) {
        $global:SflCoverageInstallation = $installationBaseline | ConvertFrom-Json
        switch ($field) {
            'id' { $global:SflCoverageInstallation.id=123 }
            'account' { $global:SflCoverageInstallation.account.id=123 }
            'repository_selection' { $global:SflCoverageInstallation.repository_selection='selected' }
            'permissions' { $global:SflCoverageInstallation.permissions.contents='write' }
            'suspended_at' { $global:SflCoverageInstallation.suspended_at='2026-10-08T00:00:00Z' }
        }
        Assert-Rejected { Get-SflOrganizationRepositoryCoverage $extras[0] 'fixture-jwt' } 'installation|permission'
    }
    $count = $global:SflCoverageRequests.Count
    Assert-Rejected { Get-SflOrganizationRepositoryCoverage ([pscustomobject]@{repository_id=1;repository='other-org/private'}) 'fixture-jwt' } 'approved organization'
    if ($global:SflCoverageRequests.Count -ne $count) { throw 'Wrong-owner target must fail before any request.' }
}
finally {
    Remove-Item function:\global:Invoke-WebRequest
    Remove-Variable -Scope Global SflCoverageRequests,SflCoverageHttpStatus,SflCoverageInstallation
}
'Organization App coverage scope and response contracts passed.'
