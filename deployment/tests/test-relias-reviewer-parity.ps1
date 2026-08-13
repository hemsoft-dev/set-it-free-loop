[CmdletBinding()]
param(
    [string] $ReliasCheckout
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$baselinePath = Join-Path $repoRoot 'deployment\release-metadata.json'
$failures = [System.Collections.Generic.List[string]]::new()

function Get-NormalizedSha256([string] $Text) {
    $normalized = $Text.Replace("`r`n", "`n").Replace("`r", "`n")
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($normalized)
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        return -join ($sha256.ComputeHash($bytes) | ForEach-Object { $_.ToString('x2') })
    }
    finally {
        $sha256.Dispose()
    }
}

function Resolve-RepoFile([string] $RelativePath) {
    if ([IO.Path]::IsPathRooted($RelativePath)) {
        throw "Repository path must be relative: $RelativePath"
    }

    $fullPath = [IO.Path]::GetFullPath((Join-Path $repoRoot $RelativePath))
    $rootPrefix = $repoRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) +
        [IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Repository path escapes the checkout: $RelativePath"
    }
    return $fullPath
}

function Invoke-GitText([string] $Checkout, [string[]] $Arguments) {
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = 'git'
    $startInfo.UseShellExecute = $false
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    [void] $startInfo.ArgumentList.Add('-C')
    [void] $startInfo.ArgumentList.Add($Checkout)
    foreach ($argument in $Arguments) {
        [void] $startInfo.ArgumentList.Add($argument)
    }

    $process = [Diagnostics.Process]::Start($startInfo)
    $stdout = $process.StandardOutput.ReadToEnd()
    $stderr = $process.StandardError.ReadToEnd()
    $process.WaitForExit()
    if ($process.ExitCode -ne 0) {
        throw "git $($Arguments -join ' ') failed: $($stderr.Trim())"
    }
    return $stdout
}

if (-not (Test-Path -LiteralPath $baselinePath -PathType Leaf)) {
    throw "Missing reviewer parity baseline: $baselinePath"
}

$baseline = Get-Content -LiteralPath $baselinePath -Raw | ConvertFrom-Json
$expectedArtifactNames = @('auto-trigger-gate', 'recovery', 'reviewer-lock', 'reviewer-source')
$expectedClasses = @(
    'private-distribution-boundary',
    'provider-configuration',
    'tested-hemsoft-hardening'
)

if ($baseline.schemaVersion -ne 1) {
    $failures.Add("Unsupported schemaVersion '$($baseline.schemaVersion)'.")
}
if ($baseline.reviewerBaseline.repository -ne 'relias-engineering/set-it-free-loop') {
    $failures.Add('The upstream repository is not relias-engineering/set-it-free-loop.')
}
foreach ($property in @('releaseCommit', 'reviewedCommit')) {
    if ([string] $baseline.reviewerBaseline.$property -notmatch '^[0-9a-f]{40}$') {
        $failures.Add("reviewerBaseline.$property is not a full lowercase Git commit SHA.")
    }
}

$actualClasses = @($baseline.allowedDifferenceClasses | Sort-Object)
if (Compare-Object $expectedClasses $actualClasses) {
    $failures.Add('allowedDifferenceClasses does not match the closed parity classification set.')
}
$actualArtifactNames = @($baseline.artifacts.name | Sort-Object)
if (Compare-Object $expectedArtifactNames $actualArtifactNames) {
    $failures.Add('The baseline must inventory exactly the four reviewer platform artifacts.')
}

foreach ($artifact in @($baseline.artifacts)) {
    try {
        $hemSoftPath = Resolve-RepoFile ([string] $artifact.hemsoftPath)
        if (-not (Test-Path -LiteralPath $hemSoftPath -PathType Leaf)) {
            $failures.Add("$($artifact.name): missing HemSoft artifact $($artifact.hemsoftPath).")
            continue
        }

        $actualHash = Get-NormalizedSha256 ([IO.File]::ReadAllText($hemSoftPath))
        if ($artifact.hemsoftSha256 -notmatch '^[0-9a-f]{64}$' -or
            $actualHash -ne $artifact.hemsoftSha256) {
            $failures.Add("$($artifact.name): HemSoft normalized SHA-256 is $actualHash; baseline records $($artifact.hemsoftSha256).")
        }

        if ($artifact.upstreamSha256 -notmatch '^[0-9a-f]{64}$') {
            $failures.Add("$($artifact.name): upstreamSha256 is not a lowercase SHA-256 value.")
        }
        if (@($artifact.differenceClasses).Count -eq 0) {
            $failures.Add("$($artifact.name): no intentional difference class is recorded.")
        }
        foreach ($differenceClass in @($artifact.differenceClasses)) {
            if ($differenceClass -notin $expectedClasses) {
                $failures.Add("$($artifact.name): unapproved difference class '$differenceClass'.")
            }
        }

        foreach ($stagedPath in @($artifact.stagedPaths)) {
            $resolvedStagedPath = Resolve-RepoFile ([string] $stagedPath)
            if (-not (Test-Path -LiteralPath $resolvedStagedPath -PathType Leaf)) {
                $failures.Add("$($artifact.name): missing staged artifact $stagedPath.")
                continue
            }
            $stagedHash = Get-NormalizedSha256 ([IO.File]::ReadAllText($resolvedStagedPath))
            if ($stagedHash -ne $actualHash) {
                $failures.Add("$($artifact.name): staged artifact $stagedPath differs from $($artifact.hemsoftPath).")
            }
        }

        foreach ($evidenceTest in @($artifact.evidenceTests)) {
            $resolvedEvidenceTest = Resolve-RepoFile ([string] $evidenceTest)
            if (-not (Test-Path -LiteralPath $resolvedEvidenceTest -PathType Leaf)) {
                $failures.Add("$($artifact.name): missing evidence test $evidenceTest.")
            }
        }
    }
    catch {
        $failures.Add("$($artifact.name): $($_.Exception.Message)")
    }
}

if ($ReliasCheckout) {
    try {
        $resolvedReliasCheckout = (Resolve-Path -LiteralPath $ReliasCheckout).ProviderPath
        $origin = (Invoke-GitText $resolvedReliasCheckout @('remote', 'get-url', 'origin')).Trim()
        if ($origin -notmatch 'relias-engineering[/:]set-it-free-loop(?:\.git)?$') {
            $failures.Add("Relias checkout origin is unexpected: $origin")
        }

        $tagExpression = "$($baseline.reviewerBaseline.releaseTag)^{commit}"
        $tagCommit = (Invoke-GitText $resolvedReliasCheckout @('rev-parse', $tagExpression)).Trim()
        if ($tagCommit -ne $baseline.reviewerBaseline.releaseCommit) {
            $failures.Add("$($baseline.reviewerBaseline.releaseTag) resolves to $tagCommit, not $($baseline.reviewerBaseline.releaseCommit).")
        }

        [void] (Invoke-GitText $resolvedReliasCheckout @('cat-file', '-e', "$($baseline.reviewerBaseline.reviewedCommit)^{commit}"))
        $changedPathsText = Invoke-GitText $resolvedReliasCheckout @(
            'diff', '--name-only',
            $baseline.reviewerBaseline.releaseCommit,
            $baseline.reviewerBaseline.reviewedCommit
        )
        $changedPaths = @($changedPathsText -split '\r?\n' | Where-Object { $_ } | Sort-Object)
        $expectedChangedPaths = @($baseline.reviewerBaseline.postReleaseChanges | Sort-Object)
        if (Compare-Object $expectedChangedPaths $changedPaths) {
            $failures.Add('Relias post-release changed paths differ from the recorded baseline.')
        }

        foreach ($artifact in @($baseline.artifacts)) {
            $reviewedText = Invoke-GitText $resolvedReliasCheckout @(
                'show',
                "$($baseline.reviewerBaseline.reviewedCommit):$($artifact.upstreamPath)"
            )
            $reviewedHash = Get-NormalizedSha256 $reviewedText
            if ($reviewedHash -ne $artifact.upstreamSha256) {
                $failures.Add("$($artifact.name): Relias reviewed-commit SHA-256 is $reviewedHash; baseline records $($artifact.upstreamSha256).")
            }

            $releaseText = Invoke-GitText $resolvedReliasCheckout @(
                'show',
                "$($baseline.reviewerBaseline.releaseCommit):$($artifact.upstreamPath)"
            )
            $releaseHash = Get-NormalizedSha256 $releaseText
            if ($releaseHash -ne $reviewedHash) {
                $failures.Add("$($artifact.name): artifact changed between the release tag and reviewed commit.")
            }
        }
    }
    catch {
        $failures.Add($_.Exception.Message)
    }
}

if ($failures.Count -gt 0) {
    throw "Relias reviewer parity audit failed:`n - $($failures -join "`n - ")"
}

if ($ReliasCheckout) {
    Write-Output "Relias reviewer parity baseline and HemSoft artifacts passed at $($baseline.reviewerBaseline.reviewedCommit)."
}
else {
    Write-Output 'HemSoft reviewer parity metadata and artifact hashes passed (Relias checkout not supplied).'
}
