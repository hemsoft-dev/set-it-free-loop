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
