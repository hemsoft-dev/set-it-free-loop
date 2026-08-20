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
Assert-True ($workflowContent -match 'if ! MANIFEST=.*contents/\.sfl/sfl\.json' -and
    $workflowContent -match 'MANIFEST=.*contents/sfl\.json') 'Auditor does not use root sfl.json as a canonical-manifest fallback.'
Assert-True ($workflowContent -match 'OBSERVER_STATE.*\.state') 'Auditor does not inspect the observer workflow state.'
Assert-True ($workflowContent -match 'OBSERVER_STATE" != "active"') 'Auditor does not reject disabled observer workflows.'
Assert-True ($workflowContent -match "DEFAULT_BRANCH=.*\.default_branch") 'Auditor does not read the repository default branch.'
Assert-True ($workflowContent -match 'PUSH_FILTER_VALID') 'Auditor does not validate the observer push branch.'
Assert-True ($workflowContent -match 'BASE_ENV_VALID') 'Auditor does not validate the observer base-branch environment.'
Assert-True ($workflowContent -match 'EXPECTED_SOURCE_SHA') 'Auditor does not read the canonical manifest source SHA.'
Assert-True ($workflowContent -match 'OBSERVER_SOURCE_SHA') 'Auditor does not read the observer source pin.'
Assert-True ($workflowContent -match 'OBSERVER_SOURCE_SHA" != "\$EXPECTED_SOURCE_SHA') 'Auditor does not reject an observer source-pin mismatch.'
Assert-True ($workflowContent -match 'declares standalone SFL review in') 'Auditor issue text does not cover both supported reviewer manifest forms.'
Assert-True ($workflowContent -notmatch 'declares \\`sfl-pr-review-auto\\` in') 'Auditor issue text still claims every reviewer manifest uses the component form.'

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

    $reviewScript = Get-WorkflowStepScript "      - name: `"Check: SFL review prerequisites`"`n"
    $manifestPath = Join-Path $temporaryDirectory 'sfl.json'
    $rootManifestPath = Join-Path $temporaryDirectory 'root-sfl.json'
    $manifestLogPath = Join-Path $temporaryDirectory 'manifest-log'
    $observerPath = Join-Path $temporaryDirectory 'observer.yml'
    $reviewOutputPath = Join-Path $temporaryDirectory 'review-output'
    $issueLogPath = Join-Path $temporaryDirectory 'issue-log'
    $sourceSha = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    "{`"components`": [`"sfl-pr-review-auto`"], `"sourceSha`": `"$sourceSha`"}" | Set-Content -LiteralPath $manifestPath
    "{`"components`": [`"sfl-pr-review-auto`"], `"sourceSha`": `"$sourceSha`"}" | Set-Content -LiteralPath $rootManifestPath
    $reviewMock = @'
gh() {
  if [ "$1" = "api" ]; then
    case "$2" in
      repos/HemSoft/example/contents/.sfl/sfl.json)
        printf 'canonical\n' >> "$MANIFEST_LOG"
        base64 "$MANIFEST_FIXTURE"
        return
        ;;
      repos/HemSoft/example/contents/sfl.json)
        printf 'root\n' >> "$MANIFEST_LOG"
        base64 "$ROOT_MANIFEST_FIXTURE"
        return
        ;;
      repos/HemSoft/example/actions/workflows/sfl-pr-review-auto.yml)
        printf 'active\n'
        return
        ;;
      repos/HemSoft/example/contents/.github/workflows/sfl-pr-review-auto.yml)
        base64 "$OBSERVER_FIXTURE"
        return
        ;;
      repos/HemSoft/example)
        printf '%s\n' "$DEFAULT_BRANCH_FIXTURE"
        return
        ;;
    esac
    return 1
  fi
  if [ "$1 $2" = "issue list" ]; then
    return
  fi
  if [ "$1 $2" = "issue create" ]; then
    printf 'created\n' >> "$ISSUE_LOG"
    return
  fi
  if [ "$1 $2" = "issue close" ]; then
    printf 'closed\n' >> "$ISSUE_LOG"
    return
  fi
  return 1
}
'@
    $reviewEnvironment = @{
        GITHUB_OUTPUT = $reviewOutputPath
        DEFAULT_BRANCH_FIXTURE = 'main'
        ISSUE_LOG = $issueLogPath
        MANIFEST_FIXTURE = $manifestPath
        MANIFEST_LOG = $manifestLogPath
        OBSERVER_FIXTURE = $observerPath
        REPO = 'HemSoft/example'
        ROOT_MANIFEST_FIXTURE = $rootManifestPath
    }
    @'
# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
name: SFL Codex Review Observer
on:
  push:
    branches: ['trunk']
env:
  SFL_REVIEW_BASE_BRANCH: 'trunk'
github.event.sender.id == 199175422
name: "SFL Reviewer Gate Runner"
'@ | Set-Content -LiteralPath $observerPath
    $staleBaseRun = Invoke-BashScript -Script "$reviewMock`n$reviewScript" -Environment $reviewEnvironment
    $staleBaseOutput = Get-Content -Raw -LiteralPath $reviewOutputPath
    $staleBaseIssue = Get-Content -Raw -LiteralPath $issueLogPath
    Assert-True ($staleBaseRun.ExitCode -eq 0) "Stale-base prerequisite script failed: $($staleBaseRun.Output)"
    Assert-True ($staleBaseOutput -match 'sfl_review_prerequisites_missing=1') 'Auditor accepted a stale observer base branch.'
    Assert-True ($staleBaseIssue -match 'created') 'Auditor did not create an issue for a stale observer base branch.'

    Remove-Item -LiteralPath $reviewOutputPath, $issueLogPath -Force
    @'
# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
name: SFL Codex Review Observer
on:
  push:
    branches: ['main']
env:
  SFL_REVIEW_BASE_BRANCH: 'main'
github.event.sender.id == 199175422
name: "SFL Reviewer Gate Runner"
'@ | Set-Content -LiteralPath $observerPath
    $currentBaseRun = Invoke-BashScript -Script "$reviewMock`n$reviewScript" -Environment $reviewEnvironment
    $currentBaseOutput = Get-Content -Raw -LiteralPath $reviewOutputPath
    $currentBaseIssue = if (Test-Path -LiteralPath $issueLogPath) { Get-Content -Raw -LiteralPath $issueLogPath } else { '' }
    Assert-True ($currentBaseRun.ExitCode -eq 0) "Current-base prerequisite script failed: $($currentBaseRun.Output)"
    Assert-True ($currentBaseOutput -match 'sfl_review_prerequisites_missing=0') 'Auditor rejected an observer deployed for the current default branch.'
    Assert-True ([string]::IsNullOrEmpty($currentBaseIssue)) 'Auditor opened or closed an issue for a valid observer base branch.'

    Remove-Item -LiteralPath $reviewOutputPath, $issueLogPath -Force -ErrorAction SilentlyContinue
    (Get-Content -Raw -LiteralPath $observerPath).Replace($sourceSha, 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb') |
        Set-Content -LiteralPath $observerPath
    $stalePinRun = Invoke-BashScript -Script "$reviewMock`n$reviewScript" -Environment $reviewEnvironment
    $stalePinOutput = Get-Content -Raw -LiteralPath $reviewOutputPath
    $stalePinIssue = Get-Content -Raw -LiteralPath $issueLogPath
    Assert-True ($stalePinRun.ExitCode -eq 0) "Stale-pin prerequisite script failed: $($stalePinRun.Output)"
    Assert-True ($stalePinOutput -match 'sfl_review_prerequisites_missing=1') 'Auditor accepted an observer with a stale source pin.'
    Assert-True ($stalePinIssue -match 'created') 'Auditor did not create an issue for a stale observer source pin.'

    Remove-Item -LiteralPath $reviewOutputPath, $issueLogPath -Force -ErrorAction SilentlyContinue
    Set-Content -LiteralPath $observerPath -Value ''
    $missingObserverRun = Invoke-BashScript -Script "$reviewMock`n$reviewScript" -Environment $reviewEnvironment
    $missingObserverOutput = Get-Content -Raw -LiteralPath $reviewOutputPath
    $missingObserverIssue = Get-Content -Raw -LiteralPath $issueLogPath
    Assert-True ($missingObserverRun.ExitCode -eq 0) "Missing-observer prerequisite script failed: $($missingObserverRun.Output)"
    Assert-True ($missingObserverOutput -match 'sfl_review_prerequisites_missing=1') 'Auditor did not report a missing observer file.'
    Assert-True ($missingObserverIssue -match 'created') 'Auditor did not create an issue for a missing observer file.'

    Remove-Item -LiteralPath $reviewOutputPath, $issueLogPath -Force -ErrorAction SilentlyContinue
    $reviewEnvironment.DEFAULT_BRANCH_FIXTURE = "release/o'brien"
    @'
# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
name: SFL Codex Review Observer
on:
  push:
    branches: ['release/o''brien']
env:
  SFL_REVIEW_BASE_BRANCH: 'release/o''brien'
github.event.sender.id == 199175422
name: "SFL Reviewer Gate Runner"
'@ | Set-Content -LiteralPath $observerPath
    $quotedBaseRun = Invoke-BashScript -Script "$reviewMock`n$reviewScript" -Environment $reviewEnvironment
    $quotedBaseOutput = Get-Content -Raw -LiteralPath $reviewOutputPath
    $quotedBaseIssue = if (Test-Path -LiteralPath $issueLogPath) { Get-Content -Raw -LiteralPath $issueLogPath } else { '' }
    Assert-True ($quotedBaseRun.ExitCode -eq 0) "Quoted-base prerequisite script failed: $($quotedBaseRun.Output)"
    Assert-True ($quotedBaseOutput -match 'sfl_review_prerequisites_missing=0') 'Auditor rejected a correctly YAML-escaped default branch.'
    Assert-True ([string]::IsNullOrEmpty($quotedBaseIssue)) 'Auditor opened or closed an issue for a correctly YAML-escaped default branch.'

    Remove-Item -LiteralPath $reviewOutputPath, $manifestLogPath -Force -ErrorAction SilentlyContinue
    '{"components":[]}' | Set-Content -LiteralPath $manifestPath
    $canonicalManifestRun = Invoke-BashScript -Script "$reviewMock`n$reviewScript" -Environment $reviewEnvironment
    $canonicalManifestOutput = Get-Content -Raw -LiteralPath $reviewOutputPath
    $manifestLookups = Get-Content -Raw -LiteralPath $manifestLogPath
    Assert-True ($canonicalManifestRun.ExitCode -eq 0) "Canonical-manifest precedence script failed: $($canonicalManifestRun.Output)"
    Assert-True ($canonicalManifestOutput -match 'sfl_review_prerequisites_missing=0') 'Auditor used a stale root reviewer declaration after the canonical manifest removed it.'
    Assert-True ($manifestLookups -eq "canonical`n") 'Auditor read root sfl.json even though .sfl/sfl.json exists.'

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
