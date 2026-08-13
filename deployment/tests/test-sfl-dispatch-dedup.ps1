[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
. (Join-Path $PSScriptRoot 'line-ending-test-helpers.ps1')
$canonicalPath = Join-Path $repoRoot '.github\workflows\sfl-pr-review-auto.yml'
$deployedPath = Join-Path $repoRoot 'deployment\infrastructure\sfl-pr-review-auto.yml'
$canonical = Get-Content -LiteralPath $canonicalPath -Raw
$deployed = Get-Content -LiteralPath $deployedPath -Raw

if (-not (Test-NormalizedTextEqual $canonical $deployed)) {
    throw 'Canonical and deployable auto-review dispatchers differ.'
}

$match = [regex]::Match(
    $canonical,
    '(?ms)^\s*# BEGIN TESTABLE DISPATCH DEDUP\r?\n(?<body>.*?)^\s*# END TESTABLE DISPATCH DEDUP'
)
if (-not $match.Success) {
    throw 'Could not extract the testable dispatch deduplication function.'
}

$functionBody = $match.Groups['body'].Value -replace '(?m)^          ', ''
$isWindowsPlatform = [System.IO.Path]::DirectorySeparatorChar -eq '\'
$bashPath = if ($isWindowsPlatform -and (Test-Path -LiteralPath 'C:\Program Files\Git\bin\bash.exe')) {
    'C:\Program Files\Git\bin\bash.exe'
} else {
    (Get-Command bash -ErrorAction Stop).Source
}

$tempDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ("sfl-dispatch-dedup-{0}" -f [guid]::NewGuid())
New-Item -ItemType Directory -Path $tempDirectory | Out-Null
$scriptPath = Join-Path $tempDirectory 'test-dispatch-dedup.sh'

try {
    $script = @"
set -euo pipefail
$functionBody
if [ "`$1" = '--statuses' ]; then
    reusable_review_statuses
    exit 0
fi
find_reusable_review_run "`$1" "`$2"
"@
    Set-Content -LiteralPath $scriptPath -Value $script -Encoding utf8NoBOM

    $expectedTitle = 'SFL PR Review #49 base:head retry='
    $statuses = @(& $bashPath $scriptPath --statuses)
    if ($LASTEXITCODE -ne 0) {
        throw 'Reusable review statuses failed to execute.'
    }
    if (($statuses -join ',') -cne 'queued,in_progress,success') {
        throw "Reusable review statuses were '$($statuses -join ',')'; expected queued,in_progress,success."
    }

    $fixtures = @(
        @{
            Name = 'queued exact-state run'
            LabelPresent = 'false'
            Expected = '102'
            Json = '{"workflow_runs":[{"id":101,"status":"queued","display_title":"SFL PR Review #49 base:head retry=0 dispatch=first"},{"id":102,"status":"queued","display_title":"SFL PR Review #49 base:head retry=0 dispatch=second"}]}'
        },
        @{
            Name = 'in-progress exact-state run'
            LabelPresent = 'false'
            Expected = '201'
            Json = '{"workflow_runs":[{"id":201,"status":"in_progress","display_title":"SFL PR Review #49 base:head retry=0 dispatch=active"}]}'
        },
        @{
            Name = 'successful completed exact-state run'
            LabelPresent = 'false'
            Expected = '301'
            Json = '{"workflow_runs":[{"id":301,"status":"completed","conclusion":"success","display_title":"SFL PR Review #49 base:head retry=0 dispatch=completed"}]}'
        },
        @{
            Name = 'unrelated completed history'
            LabelPresent = 'false'
            Expected = ''
            Json = '{"workflow_runs":[{"id":401,"status":"completed","conclusion":"success","display_title":"SFL PR Review #48 other:head retry=0 dispatch=other"}]}'
        },
        @{
            Name = 'live explicit label bypass from any concurrent event'
            LabelPresent = 'true'
            Expected = ''
            Json = '{"workflow_runs":[{"id":501,"status":"completed","conclusion":"success","display_title":"SFL PR Review #49 base:head retry=0 dispatch=completed"}]}'
        }
    )

    foreach ($fixture in $fixtures) {
        $output = @($fixture.Json | & $bashPath $scriptPath $expectedTitle $fixture.LabelPresent)
        if ($LASTEXITCODE -ne 0) {
            throw "Dispatch dedup fixture failed to execute: $($fixture.Name)"
        }
        $actual = $output -join "`n"
        if ($actual -cne $fixture.Expected) {
            throw "Dispatch dedup fixture '$($fixture.Name)' returned '$actual'; expected '$($fixture.Expected)'."
        }
    }
} finally {
    Remove-Item -LiteralPath $tempDirectory -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'SFL dispatch deduplication fixtures passed.'
