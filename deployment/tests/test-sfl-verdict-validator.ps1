[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$workflowPath = Join-Path $repoRoot 'deployment\workflows\sfl-pr-review.md'
$workflow = Get-Content -LiteralPath $workflowPath -Raw
$validatorMatch = [regex]::Match(
    $workflow,
    "(?s)# SFL_VERDICT_VALIDATOR_START\r?\n\s*node <<'NODE'\r?\n(?<script>.*?)\r?\n\s*NODE"
)
if (-not $validatorMatch.Success) {
    throw 'Unable to extract the SFL verdict validator from the workflow.'
}

$tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) "sfl-verdict-$([guid]::NewGuid())"
New-Item -ItemType Directory -Path $tempRoot | Out-Null
$scriptPath = Join-Path $tempRoot 'validator.cjs'
Set-Content -LiteralPath $scriptPath -Value $validatorMatch.Groups['script'].Value

function Invoke-ValidatorCase {
    param(
        [Parameter(Mandatory)]
        [string] $Name,

        [Parameter(Mandatory)]
        [object[]] $Items,

        [Parameter(Mandatory)]
        [bool] $ShouldPass
    )

    $outputPath = Join-Path $tempRoot "$Name.json"
    @{ items = $Items; errors = @() } |
        ConvertTo-Json -Depth 10 |
        Set-Content -LiteralPath $outputPath

    $previousOutputPath = $env:SFL_AGENT_OUTPUT_PATH
    $previousHead = $env:EXPECTED_HEAD_SHA
    try {
        $env:SFL_AGENT_OUTPUT_PATH = $outputPath
        $env:EXPECTED_HEAD_SHA = 'abc123'
        & node $scriptPath *> (Join-Path $tempRoot "$Name.log")
        $passed = $LASTEXITCODE -eq 0
    } finally {
        $env:SFL_AGENT_OUTPUT_PATH = $previousOutputPath
        $env:EXPECTED_HEAD_SHA = $previousHead
    }

    if ($passed -ne $ShouldPass) {
        $log = Get-Content -LiteralPath (Join-Path $tempRoot "$Name.log") -Raw
        throw "Validator case '$Name' expected pass=$ShouldPass but got pass=$passed.`n$log"
    }
}

function New-Inventory {
    param(
        [int] $NewCritical = 0,
        [int] $NewHigh = 0,
        [int] $NewMedium = 0,
        [int] $NewLow = 0,
        [int] $CarriedCritical = 0,
        [int] $CarriedHigh = 0,
        [int] $CarriedMedium = 0,
        [int] $CarriedLow = 0,
        [int] $Overflow = 0
    )

    [pscustomobject]@{
        type = 'sfl_review_inventory'
        head_sha = 'abc123'
        new_critical = $NewCritical
        new_high = $NewHigh
        new_medium = $NewMedium
        new_low = $NewLow
        carried_critical = $CarriedCritical
        carried_high = $CarriedHigh
        carried_medium = $CarriedMedium
        carried_low = $CarriedLow
        overflow = $Overflow
    }
}

function New-Comment {
    param([Parameter(Mandatory)][string] $Severity)

    [pscustomobject]@{
        type = 'create_pull_request_review_comment'
        body = "**$Severity Finding** Evidence and fix."
    }
}

function New-Review {
    param([Parameter(Mandatory)][string] $Event)

    [pscustomobject]@{
        type = 'submit_pull_request_review'
        event = $Event
        body = 'Review summary'
    }
}

function New-Check {
    param([Parameter(Mandatory)][string] $Conclusion)

    [pscustomobject]@{
        type = 'create_check_run'
        conclusion = $Conclusion
        title = 'SFL review'
        summary = 'Review summary'
    }
}

try {
    Invoke-ValidatorCase -Name 'approve-empty' -ShouldPass $true -Items @(
        (New-Inventory),
        (New-Review -Event 'APPROVE'),
        (New-Check -Conclusion 'success')
    )

    Invoke-ValidatorCase -Name 'reject-high-approval' -ShouldPass $false -Items @(
        (New-Inventory -NewHigh 1),
        (New-Comment -Severity 'HIGH'),
        (New-Review -Event 'APPROVE'),
        (New-Check -Conclusion 'success')
    )

    Invoke-ValidatorCase -Name 'accept-high-block' -ShouldPass $true -Items @(
        (New-Inventory -NewHigh 1),
        (New-Comment -Severity 'HIGH'),
        (New-Review -Event 'REQUEST_CHANGES'),
        (New-Check -Conclusion 'failure')
    )

    $overflowItems = [System.Collections.Generic.List[object]]::new()
    $overflowItems.Add((New-Inventory -NewMedium 21 -Overflow 1))
    1..20 | ForEach-Object {
        $overflowItems.Add((New-Comment -Severity 'MEDIUM'))
    }
    $overflowItems.Add((New-Review -Event 'REQUEST_CHANGES'))
    $overflowItems.Add((New-Check -Conclusion 'failure'))
    Invoke-ValidatorCase -Name 'accept-overflow-block' -ShouldPass $true -Items $overflowItems

    $badOverflowItems = [System.Collections.Generic.List[object]]::new()
    $badOverflowItems.Add((New-Inventory -NewLow 21 -Overflow 1))
    1..20 | ForEach-Object {
        $badOverflowItems.Add((New-Comment -Severity 'LOW'))
    }
    $badOverflowItems.Add((New-Review -Event 'APPROVE'))
    $badOverflowItems.Add((New-Check -Conclusion 'success'))
    Invoke-ValidatorCase -Name 'reject-overflow-approval' -ShouldPass $false -Items $badOverflowItems

    Invoke-ValidatorCase -Name 'accept-noop' -ShouldPass $true -Items @(
        [pscustomobject]@{ type = 'noop'; message = 'stale head' }
    )
} finally {
    Remove-Item -LiteralPath $tempRoot -Recurse -Force
}

Write-Output 'SFL verdict validator tests passed.'
