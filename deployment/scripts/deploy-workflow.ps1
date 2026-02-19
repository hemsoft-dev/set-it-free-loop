<#
.SYNOPSIS
    Deploys one or more Set it Free Loop workflows to target repositories via Pull Requests.

.DESCRIPTION
    For each target repo, this script:
      1. Clones the repo (direct clone — no fork; assumes write access within the org)
      2. Creates a feature branch
      3. Copies the workflow file from deployment/workflows/ into .github/workflows/
      4. SHA-pins the source: reference to the current HEAD of set-it-free-loop
      5. Opens a Pull Request via gh pr create

    The consumer repo receives a PR — a human reviews and merges it.
    This script never force-pushes or auto-merges.

.PARAMETER Workflow
    Name of the workflow to deploy (without .md extension).
    Must exist in deployment/workflows/.

.PARAMETER Repos
    Comma-separated list of target repos in "org/repo" format.
    Example: "HemSoft/myapp,HemSoft/otherapp"

.PARAMETER DryRun
    Print what would be done without making any changes or API calls.

.PARAMETER CloneDir
    Temporary directory for cloning target repos. Cleaned up after each PR.
    Defaults to $env:TEMP\sfl-deploy

.EXAMPLE
    .\deploy-workflow.ps1 -Workflow repo-audit -Repos "HemSoft/hs-buddy"
    .\deploy-workflow.ps1 -Workflow daily-repo-status -Repos "HemSoft/app1,HemSoft/app2" -DryRun
#>

[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)]
    [string] $Workflow,

    [Parameter(Mandatory)]
    [string] $Repos,

    [switch] $DryRun,

    [string] $CloneDir = "$env:TEMP\sfl-deploy"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ─── Resolve paths ────────────────────────────────────────────────────────────

$ScriptDir      = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot       = Resolve-Path (Join-Path $ScriptDir "..\..")
$WorkflowSource = Join-Path $RepoRoot "deployment\workflows\$Workflow.md"

if (-not (Test-Path $WorkflowSource)) {
    Write-Error "Workflow not found: $WorkflowSource`nRun 'Get-ChildItem $RepoRoot\deployment\workflows\' to see available workflows."
    exit 1
}

# ─── Resolve current SHA for source pinning ───────────────────────────────────
# Consumers pin to this SHA so upgrades are always explicit.

$CurrentSha = (git -C $RepoRoot rev-parse HEAD 2>$null).Trim()
if (-not $CurrentSha) {
    Write-Error "Could not determine current git SHA. Is this repo initialized?"
    exit 1
}

$SflSourceRef = "HemSoft/set-it-free-loop/deployment/workflows/$Workflow.md@$CurrentSha"

# ─── Helpers ──────────────────────────────────────────────────────────────────

function Write-Status([string]$Emoji, [string]$Message, [ConsoleColor]$Color = "Cyan") {
    Write-Host "$Emoji  $Message" -ForegroundColor $Color
}

function Deploy-ToRepo([string]$TargetRepo) {
    $RepoName    = $TargetRepo.Split("/")[-1]
    $BranchName  = "sfl/add-$Workflow"
    $ClonePath   = Join-Path $CloneDir $RepoName
    $DestWorkdir = Join-Path $ClonePath ".github\workflows"

    Write-Status "🚀" "Deploying $Workflow → $TargetRepo"

    if ($DryRun) {
        Write-Status "🔍" "[DRY RUN] Would clone $TargetRepo to $ClonePath" Yellow
        Write-Status "🔍" "[DRY RUN] Would create branch: $BranchName" Yellow
        Write-Status "🔍" "[DRY RUN] Would copy $Workflow.md → .github/workflows/" Yellow
        Write-Status "🔍" "[DRY RUN] Would pin source: $SflSourceRef" Yellow
        Write-Status "🔍" "[DRY RUN] Would open PR via gh pr create" Yellow
        return
    }

    # Clean up any previous clone attempt
    if (Test-Path $ClonePath) { Remove-Item $ClonePath -Recurse -Force }

    try {
        # 1. Clone
        Write-Status "📥" "Cloning $TargetRepo…"
        git clone "git@github-work1:$TargetRepo.git" $ClonePath --depth=1 --quiet
        if ($LASTEXITCODE -ne 0) { throw "git clone failed" }

        # 2. Create branch
        git -C $ClonePath checkout -b $BranchName --quiet
        if ($LASTEXITCODE -ne 0) { throw "git checkout -b failed" }

        # 3. Copy workflow file
        New-Item -ItemType Directory -Force $DestWorkdir | Out-Null
        $DestFile = Join-Path $DestWorkdir "$Workflow.md"
        Copy-Item $WorkflowSource $DestFile

        # 4. Inject source: pin comment at top of the file
        $content = Get-Content $DestFile -Raw
        $pinComment = "# Deployed from: $SflSourceRef`n# To upgrade: re-run deploy-workflow.ps1 at the desired SHA`n"
        if ($content -notmatch "# Deployed from:") {
            Set-Content $DestFile ($pinComment + $content)
        }

        # 5. Commit
        git -C $ClonePath add ".github/workflows/$Workflow.md" | Out-Null
        git -C $ClonePath commit -m "chore: add $Workflow workflow from Set it Free Loop

Workflow: $Workflow
Source: $SflSourceRef

See https://github.com/HemSoft/set-it-free-loop for full documentation.
Run deployment/governance/setup-labels.ps1 if labels are not yet configured." --quiet
        if ($LASTEXITCODE -ne 0) { throw "git commit failed" }

        # 6. Push
        git -C $ClonePath push origin $BranchName --quiet
        if ($LASTEXITCODE -ne 0) { throw "git push failed" }

        # 7. Open PR
        $prUrl = gh pr create `
            --repo $TargetRepo `
            --head $BranchName `
            --base main `
            --title "chore: add $Workflow workflow (Set it Free Loop)" `
            --body "## Set it Free Loop — Workflow Deployment

This PR adds the **$Workflow** workflow from the [Set it Free Loop](https://github.com/HemSoft/set-it-free-loop) library.

### What this workflow does

See [\`deployment/workflows/$Workflow.md\`](https://github.com/HemSoft/set-it-free-loop/blob/main/deployment/workflows/$Workflow.md) for full documentation.

### Source reference

\`\`\`
$SflSourceRef
\`\`\`

### Before merging

- [ ] Verify \`gh aw compile .github/workflows/$Workflow.md\` succeeds
- [ ] Run \`deployment/governance/setup-labels.ps1\` if labels are not yet configured
- [ ] Trigger manually via \`gh aw run $Workflow\` and confirm output
" 2>&1

        if ($LASTEXITCODE -eq 0) {
            Write-Status "✅" "PR created: $prUrl" Green
        } else {
            Write-Status "❌" "gh pr create failed: $prUrl" Red
        }

    } finally {
        # Always clean up clone
        if (Test-Path $ClonePath) { Remove-Item $ClonePath -Recurse -Force }
    }
}

# ─── Main ─────────────────────────────────────────────────────────────────────

Write-Status "🔄" "Set it Free Loop — Workflow Deployer" White
Write-Status "📦" "Workflow: $Workflow"
Write-Status "🎯" "Source SHA: $CurrentSha"

if ($DryRun) {
    Write-Status "🔍" "DRY RUN — no changes will be made" Yellow
}

# Verify gh CLI
if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    Write-Error "GitHub CLI (gh) is not installed. Install via: winget install GitHub.cli"
    exit 1
}

New-Item -ItemType Directory -Force $CloneDir | Out-Null

$TargetList = $Repos -split "," | ForEach-Object { $_.Trim() } | Where-Object { $_ }

$succeeded = 0
$failed    = 0

foreach ($repo in $TargetList) {
    try {
        Deploy-ToRepo $repo
        $succeeded++
    } catch {
        Write-Status "❌" "Failed for $repo`: $_" Red
        $failed++
    }
}

Write-Host ""
Write-Host "─────────────────────────────────────────" -ForegroundColor DarkGray
Write-Status "📊" "Succeeded: $succeeded  |  Failed: $failed"
if ($failed -gt 0) { exit 1 }
