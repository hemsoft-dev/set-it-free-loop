<#
.SYNOPSIS
    Configures folder-based Git and GitHub CLI identity routing.

.DESCRIPTION
    HemSoft checkouts under D:\github\HemSoft use the HemSoft account,
    github-personal1 SSH profile, and ~/.gh-personal.

    Relias checkouts under D:\github\Relias use the fhemmerrelias account,
    github-work1 SSH profile, and ~/.gh-work.
#>

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$hemSoftConfig = Join-Path $HOME '.gitconfig-hemsoft'
$reliasConfig = Join-Path $HOME '.gitconfig-relias'

Write-Host 'Configuring Git folder routing...'

git config --global --unset-all 'url.git@github-work1:.insteadOf' 2>$null
git config --global 'includeIf.gitdir:D:/github/HemSoft/.path' '~/.gitconfig-hemsoft'
git config --global 'includeIf.gitdir:D:/github/hemsoft-dev/.path' '~/.gitconfig-hemsoft'
git config --global 'includeIf.gitdir:D:/github/Relias/.path' '~/.gitconfig-relias'

git config --file $hemSoftConfig 'url.git@github-personal1:.insteadOf' 'git@github.com:'
git config --file $reliasConfig 'user.email' 'fhemmer@relias.com'
git config --file $reliasConfig 'url.git@github-work1:.insteadOf' 'git@github.com:'

Write-Host 'Configuring PowerShell gh wrapper...'

$profilePath = $PROFILE
$profileDir = Split-Path -Parent $profilePath
if (-not (Test-Path $profileDir)) {
    New-Item -ItemType Directory -Path $profileDir -Force | Out-Null
}

$profileContent = if (Test-Path $profilePath) {
    Get-Content $profilePath -Raw
} else {
    ''
}

$block = @'
# BEGIN HemSoft/Relias gh routing
function gh {
    $realGh = 'C:\Program Files\GitHub CLI\gh.exe'
    $previousGhConfigDir = $env:GH_CONFIG_DIR

    try {
        $cwd = (Get-Location).Path

        if ($cwd -like 'D:\github\HemSoft*' -or $cwd -like 'D:\github\hemsoft-dev*') {
            $env:GH_CONFIG_DIR = "$HOME\.gh-personal"
        }
        elseif ($cwd -like 'D:\github\Relias*') {
            $env:GH_CONFIG_DIR = "$HOME\.gh-work"
        }

        & $realGh @args
    }
    finally {
        if ($null -eq $previousGhConfigDir) {
            Remove-Item Env:\GH_CONFIG_DIR -ErrorAction SilentlyContinue
        }
        else {
            $env:GH_CONFIG_DIR = $previousGhConfigDir
        }
    }
}
# END HemSoft/Relias gh routing
'@

$pattern = '(?s)# BEGIN HemSoft/Relias gh routing.*?# END HemSoft/Relias gh routing'
if ($profileContent -match $pattern) {
    $profileContent = [regex]::Replace($profileContent, $pattern, [System.Text.RegularExpressions.MatchEvaluator] { param($m) $block })
} else {
    if ($profileContent.Length -gt 0 -and -not $profileContent.EndsWith("`n")) {
        $profileContent += "`n"
    }
    $profileContent += "`n$block`n"
}

Set-Content -Path $profilePath -Value $profileContent -NoNewline

Write-Host ''
Write-Host 'Done. Open a new PowerShell session, then verify with:'
Write-Host '  cd D:\github\HemSoft\hs-buddy; gh auth status'
Write-Host '  cd D:\github\Relias\set-it-free-loop; gh auth status'
