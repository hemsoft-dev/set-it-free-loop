[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$workflowPath = Join-Path $repoRoot 'deployment\workflows\sfl-pr-review.md'
$workflow = Get-Content -LiteralPath $workflowPath -Raw
$failures = [System.Collections.Generic.List[string]]::new()

$requiredPatterns = @(
    'Review body must contain exactly one recognized SFL verdict',
    "\? 'APPROVE'\s*\r?\n\s*: 'COMMENT'",
    "\? 'APPROVED'\s*\r?\n\s*: 'COMMENTED'",
    'Published review manifest does not match the immutable agent verdict',
    'Published finding count \$\{publishedFindingCount\} does not match immutable agent count',
    'const gateApproved =\s*\r?\n\s*verdictApproved &&\s*\r?\n\s*immutableFindingCount === 0 &&\s*\r?\n\s*unresolvedFindingCount === 0',
    'SFL_GATE_APPROVED: \$\{\{ steps\.finalize-review\.outputs\.gate-approved \}\}',
    'const baseMatchesExpected = pullRequest\.base\.sha === baseSha',
    'const headMatchesExpected = pullRequest\.head\.sha === headSha',
    "initializedEvidence\.name !== 'SFL Review Evidence'",
    'initializedEvidence\.head_sha !== headSha',
    'initializedEvidence\.external_id !== gateExternalId',
    'initializedEvidence\.app\?\.id !== 15368',
    "conclusion: approved \? 'success' : 'failure'",
    'if \(!baseMatchesExpected \|\| !headMatchesExpected\)',
    'if \(!gateApproved\)'
)
foreach ($pattern in $requiredPatterns) {
    if ($workflow -notmatch $pattern) {
        $failures.Add("Reviewer verdict contract is missing pattern: $pattern")
    }
}

function Get-ExpectedGateApproval(
    [bool] $VerdictApproved,
    [int] $NewFindingCount,
    [int] $UnresolvedFindingCount
) {
    return $VerdictApproved -and
        $NewFindingCount -eq 0 -and
        $UnresolvedFindingCount -eq 0
}

$cases = @(
    @{ Name = 'zero findings approve'; Verdict = $true; New = 0; Open = 0; Expected = $true },
    @{ Name = 'needs work blocks'; Verdict = $false; New = 0; Open = 0; Expected = $false },
    @{ Name = 'new low finding blocks'; Verdict = $true; New = 1; Open = 0; Expected = $false },
    @{ Name = 'carried finding blocks'; Verdict = $true; New = 0; Open = 1; Expected = $false },
    @{ Name = 'new and carried findings block'; Verdict = $true; New = 2; Open = 3; Expected = $false }
)
foreach ($case in $cases) {
    $actual = Get-ExpectedGateApproval `
        -VerdictApproved $case.Verdict `
        -NewFindingCount $case.New `
        -UnresolvedFindingCount $case.Open
    if ($actual -ne $case.Expected) {
        $failures.Add("Verdict case failed: $($case.Name)")
    }
}

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ }
    throw "SFL verdict contract failed with $($failures.Count) finding(s)."
}

Write-Output 'SFL verdict contract passed.'
