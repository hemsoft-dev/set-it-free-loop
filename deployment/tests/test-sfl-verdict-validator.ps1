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
        [bool] $ShouldPass,

        [string] $LiveHead = 'abc123',

        [object[]] $Threads = @()
    )

    $outputPath = Join-Path $tempRoot "$Name.json"
    @{ items = $Items; errors = @() } |
        ConvertTo-Json -Depth 10 |
        Set-Content -LiteralPath $outputPath
    $statePath = Join-Path $tempRoot "$Name-state.json"
    @{
        headRefOid = $LiveHead
        reviewThreads = @{ nodes = $Threads }
    } |
        ConvertTo-Json -Depth 10 |
        Set-Content -LiteralPath $statePath

    $previousOutputPath = $env:SFL_AGENT_OUTPUT_PATH
    $previousStatePath = $env:SFL_PR_STATE_PATH
    $previousHead = $env:EXPECTED_HEAD_SHA
    $previousRunId = $env:EXPECTED_RUN_ID
    $previousRepository = $env:GITHUB_REPOSITORY
    $previousPrNumber = $env:PR_NUMBER
    try {
        $env:SFL_AGENT_OUTPUT_PATH = $outputPath
        $env:SFL_PR_STATE_PATH = $statePath
        $env:EXPECTED_HEAD_SHA = 'abc123'
        $env:EXPECTED_RUN_ID = '42'
        $env:GITHUB_REPOSITORY = 'HemSoft/test'
        $env:PR_NUMBER = '384'
        & node $scriptPath *> (Join-Path $tempRoot "$Name.log")
        $passed = $LASTEXITCODE -eq 0
    } finally {
        $env:SFL_AGENT_OUTPUT_PATH = $previousOutputPath
        $env:SFL_PR_STATE_PATH = $previousStatePath
        $env:EXPECTED_HEAD_SHA = $previousHead
        $env:EXPECTED_RUN_ID = $previousRunId
        $env:GITHUB_REPOSITORY = $previousRepository
        $env:PR_NUMBER = $previousPrNumber
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
    param(
        [Parameter(Mandatory)]
        [string] $Event,

        [int] $Critical = 0,
        [int] $High = 0,
        [int] $Medium = 0,
        [int] $Low = 0,
        [int] $Overflow = 0,
        [string] $Body
    )

    $verdict = if ($Event -eq 'REQUEST_CHANGES') {
        'CHANGES_REQUESTED'
    } else {
        'APPROVE'
    }
    if (-not $Body) {
        $Body = @"
## SFL Full-Spectrum Review

SFL run ID: 42
Head SHA: abc123
Verdict: $verdict

| Severity | Count |
| --- | ---: |
| Critical | $Critical |
| High | $High |
| Medium | $Medium |
| Low | $Low |
| Overflow | $Overflow |
"@
    }

    [pscustomobject]@{
        type = 'submit_pull_request_review'
        event = $Event
        body = $Body
    }
}

function New-Check {
    param(
        [Parameter(Mandatory)]
        [string] $Conclusion,

        [int] $Critical = 0,
        [int] $High = 0,
        [int] $Medium = 0,
        [int] $Low = 0,
        [int] $Overflow = 0,
        [string] $Summary
    )

    $verdict = if ($Conclusion -eq 'failure') {
        'CHANGES_REQUESTED'
    } else {
        'APPROVE'
    }
    if (-not $Summary) {
        $Summary = @"
Verdict: $verdict
Head SHA: abc123
SFL run ID: 42
Critical: $Critical
High: $High
Medium: $Medium
Low: $Low
Overflow: $Overflow
"@
    }

    [pscustomobject]@{
        type = 'create_check_run'
        conclusion = $Conclusion
        title = 'SFL full-spectrum review complete'
        summary = $Summary
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
        (New-Review -Event 'APPROVE' -High 1),
        (New-Check -Conclusion 'success' -High 1)
    )

    Invoke-ValidatorCase -Name 'accept-high-block' -ShouldPass $true -Items @(
        (New-Inventory -NewHigh 1),
        (New-Comment -Severity 'HIGH'),
        (New-Review -Event 'REQUEST_CHANGES' -High 1),
        (New-Check -Conclusion 'failure' -High 1)
    )

    $overflowItems = [System.Collections.Generic.List[object]]::new()
    $overflowItems.Add((New-Inventory -NewMedium 21 -Overflow 1))
    1..20 | ForEach-Object {
        $overflowItems.Add((New-Comment -Severity 'MEDIUM'))
    }
    $overflowItems.Add((New-Review -Event 'REQUEST_CHANGES' -Medium 21 -Overflow 1))
    $overflowItems.Add((New-Check -Conclusion 'failure' -Medium 21 -Overflow 1))
    Invoke-ValidatorCase -Name 'accept-overflow-block' -ShouldPass $true -Items $overflowItems

    $badOverflowItems = [System.Collections.Generic.List[object]]::new()
    $badOverflowItems.Add((New-Inventory -NewLow 21 -Overflow 1))
    1..20 | ForEach-Object {
        $badOverflowItems.Add((New-Comment -Severity 'LOW'))
    }
    $badOverflowItems.Add((New-Review -Event 'APPROVE' -Low 21 -Overflow 1))
    $badOverflowItems.Add((New-Check -Conclusion 'success' -Low 21 -Overflow 1))
    Invoke-ValidatorCase -Name 'reject-overflow-approval' -ShouldPass $false -Items $badOverflowItems

    Invoke-ValidatorCase -Name 'accept-noop' -ShouldPass $true -LiveHead 'def456' -Items @(
        [pscustomobject]@{ type = 'noop'; message = 'stale head' }
    )

    Invoke-ValidatorCase -Name 'reject-noop-current-head' -ShouldPass $false -Items @(
        [pscustomobject]@{ type = 'noop'; message = 'stale head' }
    )

    $unresolvedHighThread = [pscustomobject]@{
        isResolved = $false
        comments = @{
            nodes = @(
                [pscustomobject]@{
                    author = @{ login = 'sfl-app[bot]' }
                    body = '**HIGH Finding** Existing unresolved defect.'
                }
            )
        }
    }
    Invoke-ValidatorCase `
        -Name 'reject-carried-omission' `
        -ShouldPass $false `
        -Threads @($unresolvedHighThread) `
        -Items @(
            (New-Inventory),
            (New-Review -Event 'APPROVE'),
            (New-Check -Conclusion 'success')
        )
    Invoke-ValidatorCase `
        -Name 'accept-carried-high' `
        -ShouldPass $true `
        -Threads @($unresolvedHighThread) `
        -Items @(
            (New-Inventory -CarriedHigh 1),
            (New-Review -Event 'REQUEST_CHANGES' -High 1),
            (New-Check -Conclusion 'failure' -High 1)
        )

    Invoke-ValidatorCase -Name 'reject-rendered-mismatch' -ShouldPass $false -Items @(
        (New-Inventory -NewHigh 1),
        (New-Comment -Severity 'HIGH'),
        (New-Review -Event 'REQUEST_CHANGES' -Body 'Review summary'),
        (New-Check -Conclusion 'failure' -High 1)
    )

    $wrongTargetComment = New-Comment -Severity 'HIGH'
    $wrongTargetComment | Add-Member -NotePropertyName pull_request_number -NotePropertyValue 999
    Invoke-ValidatorCase -Name 'reject-target-override' -ShouldPass $false -Items @(
        (New-Inventory -NewHigh 1),
        $wrongTargetComment,
        (New-Review -Event 'REQUEST_CHANGES' -High 1),
        (New-Check -Conclusion 'failure' -High 1)
    )

    $contradictoryReview = New-Review -Event 'APPROVE'
    $contradictoryReview.body += "`nVerdict: CHANGES_REQUESTED"
    Invoke-ValidatorCase -Name 'reject-contradictory-review' -ShouldPass $false -Items @(
        (New-Inventory),
        $contradictoryReview,
        (New-Check -Conclusion 'success')
    )

    Invoke-ValidatorCase -Name 'reject-incomplete-signal' -ShouldPass $false -Items @(
        (New-Inventory),
        (New-Review -Event 'APPROVE'),
        (New-Check -Conclusion 'success'),
        [pscustomobject]@{ type = 'missing_data'; message = 'incomplete review' }
    )
} finally {
    Remove-Item -LiteralPath $tempRoot -Recurse -Force
}

Write-Output 'SFL verdict validator tests passed.'
