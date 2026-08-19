$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$workflowPath = Join-Path $repoRoot 'deployment\infrastructure\sfl-auditor.yml'
$failures = [Collections.Generic.List[string]]::new()
$temporaryDirectories = [Collections.Generic.List[string]]::new()

function Assert-True {
    param(
        [bool] $Condition,
        [string] $Message
    )

    if (-not $Condition) {
        $failures.Add($Message)
    }
}

function Get-WorkflowStepScript {
    param([string] $StepMarker)

    $workflow = (Get-Content -Raw -LiteralPath $workflowPath).Replace("`r`n", "`n")
    $stepStart = $workflow.IndexOf($StepMarker, [StringComparison]::Ordinal)
    if ($stepStart -lt 0) {
        throw "Could not find workflow step marker: $StepMarker"
    }

    $runMarker = "        run: |`n"
    $runStart = $workflow.IndexOf($runMarker, $stepStart, [StringComparison]::Ordinal)
    if ($runStart -lt 0) {
        throw "Could not extract workflow script after marker: $StepMarker"
    }
    $nextStep = $workflow.IndexOf("`n      - name:", $runStart + $runMarker.Length, [StringComparison]::Ordinal)
    if ($nextStep -lt 0) {
        $nextStep = $workflow.Length
    }

    $lines = $workflow.Substring($runStart + $runMarker.Length, $nextStep - $runStart - $runMarker.Length) -split "`n"
    return ($lines | ForEach-Object {
        if ($_.Length -ge 10) { $_.Substring(10) } else { '' }
    }) -join "`n"
}

function Get-BashExecutable {
    if ($IsWindows) {
        $gitBash = 'C:\Program Files\Git\bin\bash.exe'
        if (Test-Path -LiteralPath $gitBash) {
            return $gitBash
        }
    }

    $bash = Get-Command bash -ErrorAction Stop
    return $bash.Source
}

function Invoke-BashScript {
    param(
        [string] $Script,
        [hashtable] $Environment
    )

    $previous = @{}
    foreach ($name in $Environment.Keys) {
        $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, [string] $Environment[$name], 'Process')
    }

    try {
        $output = @($Script | & (Get-BashExecutable) -s 2>&1)
        $exitCode = $LASTEXITCODE
    }
    finally {
        foreach ($name in $Environment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
        }
    }

    return [pscustomobject]@{
        ExitCode = $exitCode
        Output = $output -join "`n"
    }
}

$workflowContent = Get-Content -Raw -LiteralPath $workflowPath
Assert-True ($workflowContent -match 'index\("sfl-pr-review-auto"\)') 'Auditor does not activate for the current reviewer component.'
Assert-True ($workflowContent -notmatch 'index\("sfl-pr-review"\)') 'Auditor still activates on the retired reviewer component.'
Assert-True ($workflowContent -match 'any\(\. == "pr-review"\)') 'Auditor does not activate for the string-valued reviewer add-on.'
Assert-True ($workflowContent -notmatch 'any\(\.name == "pr-review"\)') 'Auditor still treats string-valued add-ons as objects.'
Assert-True ($workflowContent -match 'OBSERVER_STATE.*\.state') 'Auditor does not inspect the observer workflow state.'
Assert-True ($workflowContent -match 'OBSERVER_STATE" != "active"') 'Auditor does not reject disabled observer workflows.'

$mockGh = @'
gh() {
  if [ "$1 $2" = "pr list" ]; then
    cat "$DRAFT_PRS_FIXTURE"
    return
  fi
  if [ "$1 $2" = "pr comment" ]; then
    shift 2
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--body" ]; then
        printf '%s\n' "$2" >> "$COMMENTS_LOG"
        return
      fi
      shift
    done
    return 1
  fi
  return 1
}
'@

try {
    $temporaryDirectory = Join-Path ([IO.Path]::GetTempPath()) "sfl-auditor-contract-$([Guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Path $temporaryDirectory | Out-Null
    $temporaryDirectories.Add($temporaryDirectory)

    $fixturePath = Join-Path $temporaryDirectory 'draft-prs.json'
    $outputPath = Join-Path $temporaryDirectory 'github-output'
    $commentsPath = Join-Path $temporaryDirectory 'comments'
    $stalledScript = Get-WorkflowStepScript "      - name: `"Check: stalled draft PRs without analyzer reviews`"`n"

    $draftPr = [ordered]@{
        body = ''
        comments = @()
        createdAt = '2020-01-01T00:00:00Z'
        headRefName = 'agent-fix/issue-123'
        labels = @()
        number = 42
    }
    ConvertTo-Json -InputObject @($draftPr) -Depth 8 | Set-Content -LiteralPath $fixturePath

    $environment = @{
        COMMENTS_LOG = $commentsPath
        DRAFT_PRS_FIXTURE = $fixturePath
        GITHUB_OUTPUT = $outputPath
        REPO = 'HemSoft/example'
    }
    $firstRun = Invoke-BashScript -Script "$mockGh`n$stalledScript" -Environment $environment
    $firstOutput = if (Test-Path -LiteralPath $outputPath) { Get-Content -Raw -LiteralPath $outputPath } else { '' }
    $firstComment = if (Test-Path -LiteralPath $commentsPath) { Get-Content -Raw -LiteralPath $commentsPath } else { '' }

    Assert-True ($firstRun.ExitCode -eq 0) "Stalled-PR script failed: $($firstRun.Output)"
    Assert-True ($firstOutput -match 'stalled_prs_found=1') 'Stalled-PR counter did not survive the loop.'
    Assert-True ($firstComment -match '<!-- sfl-auditor:stalled-pr-missing-analyzers -->') 'Stalled-PR warning is missing its stable marker.'

    $draftPr.comments = @([ordered]@{ body = $firstComment })
    ConvertTo-Json -InputObject @($draftPr) -Depth 8 | Set-Content -LiteralPath $fixturePath
    Remove-Item -LiteralPath $outputPath, $commentsPath -Force -ErrorAction SilentlyContinue

    $secondRun = Invoke-BashScript -Script "$mockGh`n$stalledScript" -Environment $environment
    $secondOutput = if (Test-Path -LiteralPath $outputPath) { Get-Content -Raw -LiteralPath $outputPath } else { '' }
    $secondComment = if (Test-Path -LiteralPath $commentsPath) { Get-Content -Raw -LiteralPath $commentsPath } else { '' }

    Assert-True ($secondRun.ExitCode -eq 0) "Stalled-PR deduplication run failed: $($secondRun.Output)"
    Assert-True ($secondOutput -match 'stalled_prs_found=0') 'Stalled-PR warning was counted twice.'
    Assert-True ([string]::IsNullOrEmpty($secondComment)) 'Stalled-PR warning was posted twice.'

    $draftPr.comments = @([ordered]@{ body = 'Review note: this test discusses missing analyzer markers.' })
    ConvertTo-Json -InputObject @($draftPr) -Depth 8 | Set-Content -LiteralPath $fixturePath
    Remove-Item -LiteralPath $outputPath, $commentsPath -Force -ErrorAction SilentlyContinue

    $unrelatedCommentRun = Invoke-BashScript -Script "$mockGh`n$stalledScript" -Environment $environment
    $unrelatedCommentOutput = if (Test-Path -LiteralPath $outputPath) { Get-Content -Raw -LiteralPath $outputPath } else { '' }
    $unrelatedCommentWarning = if (Test-Path -LiteralPath $commentsPath) { Get-Content -Raw -LiteralPath $commentsPath } else { '' }

    Assert-True ($unrelatedCommentRun.ExitCode -eq 0) "Stalled-PR unrelated-comment run failed: $($unrelatedCommentRun.Output)"
    Assert-True ($unrelatedCommentOutput -match 'stalled_prs_found=1') 'Unrelated review text suppressed the Auditor warning.'
    Assert-True ($unrelatedCommentWarning -match '<!-- sfl-auditor:stalled-pr-missing-analyzers -->') 'Auditor warning was not posted after unrelated review text.'

    $summaryScript = Get-WorkflowStepScript "      - name: Summary`n"
    $summaryExpressions = @(
        '${{ steps.orphaned-labels.outputs.orphaned_labels_fixed }}',
        '${{ steps.conflicting.outputs.conflicting_fixed }}',
        '${{ steps.orphaned-prs.outputs.orphaned_prs_found }}',
        '${{ steps.stale-unclaimed.outputs.stale_unclaimed_found }}',
        '${{ steps.stalled-prs.outputs.stalled_prs_found }}',
        '${{ steps.paused.outputs.unexplained_pause_found }}'
    )
    foreach ($expression in $summaryExpressions) {
        $summaryScript = $summaryScript.Replace($expression, '0')
    }
    $summaryScript = $summaryScript.Replace(
        '${{ steps.sfl-review-prerequisites.outputs.sfl_review_prerequisites_missing }}',
        '2'
    )

    $summaryPath = Join-Path $temporaryDirectory 'step-summary.md'
    $summaryRun = Invoke-BashScript -Script $summaryScript -Environment @{ GITHUB_STEP_SUMMARY = $summaryPath }
    $summaryFile = if (Test-Path -LiteralPath $summaryPath) { Get-Content -Raw -LiteralPath $summaryPath } else { '' }

    Assert-True ($summaryRun.ExitCode -eq 0) "Summary script failed: $($summaryRun.Output)"
    Assert-True ($summaryFile -match '\| Missing SFL review prerequisites \| 2 \|') 'Summary omitted the reviewer-prerequisite count.'
    Assert-True ($summaryFile -match 'Found or addressed 2 discrepancies\.') 'Summary total is incorrect.'
    Assert-True ($summaryFile.TrimEnd() -eq $summaryRun.Output.TrimEnd()) 'Summary output was not written to GITHUB_STEP_SUMMARY.'
}
finally {
    foreach ($directory in $temporaryDirectories) {
        Remove-Item -LiteralPath $directory -Recurse -Force -ErrorAction SilentlyContinue
    }
}

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ }
    exit 1
}

Write-Output 'SFL Auditor behavioral contract passed.'
