Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$installer = Join-Path $repoRoot 'deployment\scripts\install-gh-sfl-hemsoft.ps1'
$fixtureRoot = Join-Path ([System.IO.Path]::GetTempPath()) "sfl-installer-contract-$([guid]::NewGuid().ToString('N'))"
$releaseRoot = Join-Path $fixtureRoot 'release'
$downloadRoot = Join-Path $fixtureRoot 'download'
$failureRoot = Join-Path $fixtureRoot 'failure'
$version = '9.8.7-rc.2'

try {
    New-Item -ItemType Directory -Path $releaseRoot -Force | Out-Null
    foreach ($invalidVersion in @('01.0.0', '1.0.0-rc.01')) {
        $invalidFailure = $null
        try {
            & $installer -ReleaseVersion $invalidVersion -GitHubCliPath 'gh' `
                -WorkDir (Join-Path $fixtureRoot "invalid-$invalidVersion") -NoInstall
        }
        catch {
            $invalidFailure = $_.Exception.Message
        }
        if ($invalidFailure -notlike 'Invalid release version*') {
            throw "Release installer did not reject '$invalidVersion': $invalidFailure"
        }
    }
    $artifactName = if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
        [System.Runtime.InteropServices.OSPlatform]::Windows)) {
        "gh-sfl_${version}_windows_amd64.exe"
    } elseif ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
        [System.Runtime.InteropServices.OSPlatform]::Linux)) {
        "gh-sfl_${version}_linux_amd64"
    } else {
        Write-Output 'Release installer contract skipped on an unsupported OS.'
        return
    }

    $artifactPath = Join-Path $releaseRoot $artifactName
    [System.IO.File]::WriteAllText($artifactPath, 'verified release fixture')
    $hash = (Get-FileHash -LiteralPath $artifactPath -Algorithm SHA256).Hash.ToLowerInvariant()
    [System.IO.File]::WriteAllText(
        (Join-Path $releaseRoot 'SHA256SUMS'),
        "$hash  $artifactName`n",
        [System.Text.UTF8Encoding]::new($false)
    )
    $fakeGh = Join-Path $fixtureRoot 'gh-fixture.ps1'
    @'
$repositoryIndex = [Array]::IndexOf($args, '--repo')
if ($repositoryIndex -lt 0 -or $args[$repositoryIndex + 1] -cne $env:SFL_INSTALLER_EXPECTED_REPOSITORY) {
    throw 'Installer used the wrong release repository.'
}
$destinationIndex = [Array]::IndexOf($args, '--dir')
if ($destinationIndex -lt 0 -or $destinationIndex + 1 -ge $args.Count) {
    throw 'Fake gh did not receive --dir.'
}
$destination = $args[$destinationIndex + 1]
New-Item -ItemType Directory -Path $destination -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $env:SFL_INSTALLER_FIXTURE 'SHA256SUMS') -Destination $destination -Force
Get-ChildItem -LiteralPath $env:SFL_INSTALLER_FIXTURE -File |
    Where-Object Name -ne 'SHA256SUMS' |
    Copy-Item -Destination $destination -Force
'@ | Set-Content -LiteralPath $fakeGh -Encoding utf8NoBOM

    $env:SFL_INSTALLER_FIXTURE = $releaseRoot
    $env:SFL_INSTALLER_EXPECTED_REPOSITORY = 'hemsoft-dev/set-it-free-loop'
    Remove-Variable -Name LASTEXITCODE -Scope Global -ErrorAction SilentlyContinue
    & $installer -ReleaseVersion $version -GitHubCliPath $fakeGh -WorkDir $downloadRoot -NoInstall
    if (-not (Test-Path -LiteralPath (Join-Path $downloadRoot $artifactName) -PathType Leaf)) {
        throw 'Release installer did not retain the verified artifact.'
    }

    $env:SFL_INSTALLER_EXPECTED_REPOSITORY = 'HemSoft/set-it-free-loop'
    & $installer -Repository 'HemSoft/set-it-free-loop' -ReleaseVersion $version `
        -GitHubCliPath $fakeGh -WorkDir (Join-Path $fixtureRoot 'legacy-download') -NoInstall
    $env:SFL_INSTALLER_EXPECTED_REPOSITORY = 'hemsoft-dev/set-it-free-loop'

    [System.IO.File]::WriteAllText($artifactPath, 'tampered release fixture')
    $failure = $null
    try {
        & $installer -Repository 'hemsoft-dev/set-it-free-loop' -ReleaseVersion $version -GitHubCliPath $fakeGh -WorkDir $failureRoot -NoInstall
    }
    catch {
        $failure = $_.Exception.Message
    }
    if ($failure -notlike 'Checksum mismatch*') {
        throw "Release installer did not fail closed on tampering: $failure"
    }
}
finally {
    Remove-Item Env:SFL_INSTALLER_FIXTURE -ErrorAction SilentlyContinue
    Remove-Item Env:SFL_INSTALLER_EXPECTED_REPOSITORY -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $fixtureRoot) {
        Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
    }
}

Write-Output 'Release installer checksum success/failure contract passed.'
