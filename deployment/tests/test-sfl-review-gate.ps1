[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$gateScript = Join-Path $PSScriptRoot '../scripts/set-sfl-review-gate.ps1'
$global:sflGateTestBinding = 15368
$global:sflGateTestWrites = 0
$global:sflGateTestPayload = $null
function global:gh {
    $call = $args -join ' '
    $global:LASTEXITCODE = 0
    switch ($call) {
        'api user --jq .login' { return 'HemSoft' }
        'api --method GET repos/hemsoft-dev/consumer' { return '{"full_name":"hemsoft-dev/consumer","owner":{"login":"hemsoft-dev"}}' }
        'api --method GET repos/hemsoft-dev/consumer/collaborators/HemSoft/permission' { return '{"permission":"admin","user":{"login":"HemSoft"}}' }
        'repo view hemsoft-dev/consumer --json owner,defaultBranchRef' { return '{"owner":{"login":"hemsoft-dev"},"defaultBranchRef":{"name":"main"}}' }
        'api --method GET repos/hemsoft-dev/consumer/branches/main/protection/required_status_checks' {
            return (@{strict=$false;contexts=@('CI','SFL Reviewer Approval');checks=@(
                @{context='CI';app_id=9000},@{context='SFL Reviewer Approval';app_id=$global:sflGateTestBinding}
            )} | ConvertTo-Json -Depth 6)
        }
        'api --method PATCH repos/hemsoft-dev/consumer/branches/main/protection/required_status_checks --header Accept: application/vnd.github+json --input -' {
            $global:sflGateTestWrites++
            $raw = @($input) -join "`n"
            $global:sflGateTestPayload = $raw | ConvertFrom-Json
            return $raw
        }
        default { throw "Unexpected gate API call: $call" }
    }
}
try {
    & $gateScript -Repo hemsoft-dev/consumer
    $checks = @($global:sflGateTestPayload.checks)
    if ($global:sflGateTestWrites -ne 1 -or -not $global:sflGateTestPayload.strict -or
        @($checks | Where-Object { $_.context -eq 'SFL Reviewer Approval' }).Count -ne 0 -or
        @($checks | Where-Object { $_.context -eq 'CI' -and $_.app_id -eq 9000 }).Count -ne 1 -or
        @($checks | Where-Object { $_.context -eq 'SFL Reviewer Gate Runner' -and $_.app_id -eq 15368 }).Count -ne 1) {
        throw 'Gate migration did not replace only the legacy Actions-owned gate and preserve CI.'
    }
    foreach ($binding in @(-1,777)) {
        $global:sflGateTestBinding = $binding
        $before = $global:sflGateTestWrites
        $denied = $false
        try { & $gateScript -Repo hemsoft-dev/consumer } catch {
            if ($_.Exception.Message -notlike '*unverified App binding*') { throw }
            $denied = $true
        }
        if (-not $denied -or $global:sflGateTestWrites -ne $before) { throw 'Unverified legacy binding was modified.' }
    }
} finally {
    Remove-Item function:\global:gh -ErrorAction SilentlyContinue
    Remove-Variable -Scope Global -Name sflGateTestBinding,sflGateTestWrites,sflGateTestPayload -ErrorAction SilentlyContinue
    $global:LASTEXITCODE = 0
}
Write-Output 'Legacy gate migration and unrelated-check preservation passed.'
