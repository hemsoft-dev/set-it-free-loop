[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot '../scripts/SflRepositoryPolicy.psm1') -Force
$script:permission = 'write'
$script:lookupFails = $false
$script:calls = [Collections.Generic.List[string]]::new()
function global:gh {
    $call = $args -join ' '
    $script:calls.Add($call)
    $global:LASTEXITCODE = 0
    switch ($call) {
        'api user --jq .login' { return 'member' }
        'api --method GET repos/hemsoft-dev/example' { return '{"full_name":"hemsoft-dev/example","owner":{"login":"hemsoft-dev"}}' }
        'api --method GET repos/hemsoft-dev/example/collaborators/member/permission' {
            if ($script:lookupFails) { $global:LASTEXITCODE = 1; return '' }
            return "{`"permission`":`"$script:permission`",`"user`":{`"login`":`"member`"}}"
        }
        default { throw "Unexpected API call: $call" }
    }
}
try {
    foreach ($role in @('write','maintain','admin')) {
        $script:permission = $role
        $context = Get-SflRepositoryContext 'hemsoft-dev/example'
        if ($context.Login -cne 'member' -or $context.Permission -cne $role) { throw 'Permission preflight lost caller identity.' }
    }
    foreach ($role in @('read','triage','none')) {
        $script:permission = $role
        $denied = $false
        try { Get-SflRepositoryContext 'hemsoft-dev/example' | Out-Null } catch { $denied = $true }
        if (-not $denied) { throw "Accepted unauthorized role $role." }
    }
    $script:permission = 'write'
    $denied = $false
    try { Get-SflRepositoryContext 'hemsoft-dev/example' -Access admin | Out-Null } catch { $denied = $true }
    if (-not $denied) { throw 'Accepted write for an admin operation.' }
    $script:lookupFails = $true
    $denied = $false
    try { Get-SflRepositoryContext 'hemsoft-dev/example' | Out-Null } catch { $denied = $true }
    if (-not $denied) { throw 'Accepted unavailable permissions.' }
    foreach ($target in @('other/example','hemsoft-dev/set-it-free-loop','HemSoft/set-it-free-loop')) {
        $before = $script:calls.Count
        $denied = $false
        try { Get-SflRepositoryContext $target | Out-Null } catch { $denied = $true }
        if (-not $denied -or $script:calls.Count -ne $before) { throw "Scope guard failed for $target." }
    }
    Assert-SflSourceRepository 'hemsoft-dev/set-it-free-loop'
} finally { Remove-Item function:\global:gh -ErrorAction SilentlyContinue }
Write-Output 'Organization scope and live permission policy passed.'

# A denied mocked API call must not leak its exit code to the CI shell.
$global:LASTEXITCODE = 0

$script:sourceSha = 'a' * 40
$script:dirty = $false
$script:sourceRedirect = $false
$script:sourceMissing = $false
$script:inheritedFails = $false
$script:sourceCalls = [Collections.Generic.List[string]]::new()
function global:git {
    $global:LASTEXITCODE = 0
    if ($args -contains 'rev-parse') { return $script:sourceSha }
    if ($args -contains 'status') { if ($script:dirty) { return '?? deployment/unreviewed.yml' }; return }
    throw "Unexpected git call: $args"
}
function global:gh {
    $call = $args -join ' '
    $script:sourceCalls.Add($call)
    $global:LASTEXITCODE = 0
    if ($call -eq 'api --method GET repos/hemsoft-dev/set-it-free-loop') {
        if ($script:sourceRedirect) { return '{"full_name":"HemSoft/set-it-free-loop"}' }
        return '{"full_name":"hemsoft-dev/set-it-free-loop"}'
    }
    if ($call -eq "api --method GET repos/hemsoft-dev/set-it-free-loop/git/commits/$script:sourceSha") {
        if ($script:sourceMissing) { $global:LASTEXITCODE = 1; return '' }
        return "{`"sha`":`"$script:sourceSha`"}"
    }
    if ($call -like 'secret list --repo *') { return 'REPO_ONLY' }
    if ($call -eq 'api --method GET --paginate repos/hemsoft-dev/example/actions/organization-secrets?per_page=100 --jq .secrets[].name') {
        if ($script:inheritedFails) { $global:LASTEXITCODE = 1; return '' }
        return @('CODEX_API_KEY', 'REPO_ONLY')
    }
    throw "Unexpected gh call: $call"
}
try {
    Assert-SflSourceCheckout -Repository 'hemsoft-dev/set-it-free-loop' -CheckoutRoot fixture -Commit $script:sourceSha
    foreach ($failure in @('dirty', 'sourceRedirect', 'sourceMissing')) {
        Set-Variable -Name $failure -Scope Script -Value $true
        $denied = $false
        try { Assert-SflSourceCheckout -Repository 'hemsoft-dev/set-it-free-loop' -CheckoutRoot fixture -Commit $script:sourceSha }
        catch { $denied = $true }
        if (-not $denied) { throw "Accepted invalid source checkout: $failure" }
        Set-Variable -Name $failure -Scope Script -Value $false
    }
    $names = @(Get-SflActionsSecretNames 'hemsoft-dev/example')
    if (($names -join ',') -ne 'CODEX_API_KEY,REPO_ONLY') { throw 'Eligible organization secret was omitted or duplicated.' }
    $script:inheritedFails = $true
    $denied = $false
    try { Get-SflActionsSecretNames 'hemsoft-dev/example' | Out-Null } catch { $denied = $true }
    if (-not $denied) { throw 'Unavailable inherited secret listing was treated as complete.' }
    $script:sourceCalls.Clear()
    $names = @(Get-SflActionsSecretNames 'HemSoft/example')
    if (($names -join ',') -ne 'REPO_ONLY' -or $script:sourceCalls.Count -ne 1) { throw 'Personal repository made an organization secret lookup.' }
} finally {
    Remove-Item function:\global:gh, function:\global:git -ErrorAction SilentlyContinue
}
$global:LASTEXITCODE = 0
Write-Output 'Source checkout provenance and inherited secret preflight passed.'
