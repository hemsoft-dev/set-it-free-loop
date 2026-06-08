<#
.SYNOPSIS
    Deploys Set it Free Loop workflows to target repositories via Pull Requests.

.DESCRIPTION
    Supports two modes:

    1. Single workflow: Deploy one workflow by name.
       .\deploy-workflow.ps1 -Workflow repo-audit -Repos "org/repo"

    2. Tier-based: Deploy a predefined set of workflows by tier.
       .\deploy-workflow.ps1 -Tier standard -Repos "org/repo"

    For each target repo, this script:
      1. Clones the repo (direct clone — no fork; assumes write access within the org)
      2. Creates a feature branch
      3. Copies workflow files into .github/workflows/
      4. Copies infrastructure files (dispatcher, auditor) for standard/full tiers
      5. Creates/updates sfl.json manifest in the consumer repo
      6. Injects/updates the dynamic SFL badge in README.md
      7. SHA-pins the source reference
      8. Opens a Pull Request via gh pr create

    The consumer repo receives a PR — a human reviews and merges it.
    This script never force-pushes or auto-merges.

.PARAMETER Workflow
    Name of a single workflow to deploy (without .md extension).
    Must exist in deployment/workflows/. Cannot be used with -Tier.

.PARAMETER Tier
    Deploy a predefined tier of workflows. Options: minimal, standard, full.
    Cannot be used with -Workflow.

    Tiers:
      minimal  — Labels, governance, repo-audit, daily-repo-status
      standard — Minimal + SFL Auditor, SFL Dispatcher, issue-processor, simplisticate
      full     — Standard + PR Analyzers A/B/C, PR Fixer, PR Promoter

.PARAMETER Repos
    Comma-separated list of target repos in "org/repo" format.

.PARAMETER DryRun
    Print what would be done without making any changes or API calls.

.PARAMETER CloneDir
    Temporary directory for cloning target repos. Cleaned up after each PR.
    Defaults to $env:TEMP\sfl-deploy

.EXAMPLE
    .\deploy-workflow.ps1 -Workflow repo-audit -Repos "HemSoft/hs-buddy"
    .\deploy-workflow.ps1 -Tier full -Repos "HemSoft/app1,HemSoft/app2"
    .\deploy-workflow.ps1 -Tier standard -Repos "HemSoft/myapp" -DryRun
#>

[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory = $false)]
    [string] $Workflow,

    [Parameter(Mandatory = $false)]
    [ValidateSet("minimal", "standard", "full")]
    [string] $Tier,

    [Parameter(Mandatory)]
    [string] $Repos,

    [switch] $DryRun,

    [string] $CloneDir = "$env:TEMP\sfl-deploy"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ─── Validate parameters ─────────────────────────────────────────────────────

if (-not $Workflow -and -not $Tier) {
    Write-Error "You must specify either -Workflow or -Tier."
    exit 1
}
if ($Workflow -and $Tier) {
    Write-Error "Cannot specify both -Workflow and -Tier. Use one or the other."
    exit 1
}

# ─── Resolve paths ────────────────────────────────────────────────────────────

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "..\..")

# ─── SFL Version (read from VERSION file — single source of truth) ────────────

$VersionFile = Join-Path $RepoRoot "VERSION"
if (-not (Test-Path $VersionFile)) {
    Write-Error "VERSION file not found at $VersionFile. This file is the single source of truth for the SFL version."
    exit 1
}
$SflVersion = (Get-Content $VersionFile -Raw).Trim()

$EnginePolicyPath = Join-Path $RepoRoot "deployment\engine-policy.json"
if (-not (Test-Path $EnginePolicyPath)) {
    Write-Error "Engine policy file not found at $EnginePolicyPath."
    exit 1
}

$EnginePolicy = Get-Content $EnginePolicyPath -Raw | ConvertFrom-Json

function Get-SflObjectProperty([object]$Object, [string]$Name, [string]$Context) {
    if ($null -eq $Object) {
        throw "$Context is null."
    }

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        throw "$Context is missing required property '$Name'."
    }

    return $property.Value
}

$DefaultEngineProfileName = [string](Get-SflObjectProperty $EnginePolicy "defaultProfile" "engine policy")
$EnginePolicyProfiles = Get-SflObjectProperty $EnginePolicy "profiles" "engine policy"

function Resolve-SflEngineProfile([string]$WorkflowName) {
    $profileName = $DefaultEngineProfileName
    $workflowsProperty = $EnginePolicy.PSObject.Properties["workflows"]
    if ($null -ne $workflowsProperty -and $null -ne $workflowsProperty.Value) {
        $workflowProperty = $workflowsProperty.Value.PSObject.Properties[$WorkflowName]
        if ($null -ne $workflowProperty -and $null -ne $workflowProperty.Value) {
            $profileProperty = $workflowProperty.Value.PSObject.Properties["profile"]
            if ($null -ne $profileProperty -and $null -ne $profileProperty.Value) {
                $profileName = [string]$profileProperty.Value
            }
        }
    }

    $profileProperty = $EnginePolicyProfiles.PSObject.Properties[$profileName]
    if ($null -eq $profileProperty -or $null -eq $profileProperty.Value) {
        throw "Engine profile '$profileName' was not found in $EnginePolicyPath."
    }

    $profile = $profileProperty.Value
    $provider = [string](Get-SflObjectProperty $profile "provider" "engine profile '$profileName'")
    $model = [string](Get-SflObjectProperty $profile "model" "engine profile '$profileName'")
    $effortProperty = $profile.PSObject.Properties["effort"]
    $effort = if ($null -ne $effortProperty -and $null -ne $effortProperty.Value) {
        [string]$effortProperty.Value
    } else {
        $null
    }

    if ($effort -and @("low", "medium", "high") -notcontains $effort) {
        throw "Engine profile '$profileName' uses unsupported effort '$effort'. Use low, medium, or high."
    }

    $renderedModel = if ($effort) { "$model`?effort=$effort" } else { $model }
    $requiredSecretsProperty = $profile.PSObject.Properties["requiredSecretsAnyOf"]
    $requiredSecretsAnyOf = if ($null -ne $requiredSecretsProperty -and $null -ne $requiredSecretsProperty.Value) {
        @($requiredSecretsProperty.Value)
    } else {
        @()
    }

    return [pscustomobject]@{
        Profile              = $profileName
        Provider             = $provider
        Model                = $model
        Effort               = $effort
        RenderedModel        = $renderedModel
        RequiredSecretsAnyOf = $requiredSecretsAnyOf
    }
}

function ConvertTo-SflWorkflowWithEnginePolicy([string]$Content, [pscustomobject]$EngineProfile, [string]$WorkflowName) {
    $frontmatterMatch = [regex]::Match($Content, '(?s)\A---\r?\n(?<frontmatter>.*?)\r?\n---')
    if (-not $frontmatterMatch.Success) {
        throw "Workflow '$WorkflowName' does not start with YAML frontmatter."
    }

    $frontmatter = $frontmatterMatch.Groups["frontmatter"].Value
    $rest = $Content.Substring($frontmatterMatch.Length)
    $engineBlock = "engine:`n  id: $($EngineProfile.Provider)`n  model: $($EngineProfile.RenderedModel)"

    $frontmatter = [regex]::Replace(
        $frontmatter,
        '(?ms)^engine:[^\r\n]*(?:\r?\n(?:[ \t]+.*(?:\r?\n|$))*)?\r?\n?',
        '',
        1
    ).TrimEnd("`r", "`n")

    if ($frontmatter -match '(?m)^network:') {
        $frontmatter = [regex]::Replace($frontmatter, '(?m)^network:', "$engineBlock`n`nnetwork:", 1)
    } else {
        $frontmatter = "$frontmatter`n`n$engineBlock"
    }

    return "---`n$frontmatter`n---$rest"
}

function New-SflEnginePolicyManifest([string[]]$WorkflowNames) {
    $workflowEntries = @(
        $WorkflowNames | ForEach-Object {
            $profile = Resolve-SflEngineProfile $_
            [ordered]@{
                name                 = $_
                profile              = $profile.Profile
                provider             = $profile.Provider
                model                = $profile.Model
                effort               = $profile.Effort
                renderedModel        = $profile.RenderedModel
                requiredSecretsAnyOf = @($profile.RequiredSecretsAnyOf)
            }
        }
    )

    return [ordered]@{
        defaultProfile = $DefaultEngineProfileName
        workflows      = $workflowEntries
    }
}

# ─── Tier definitions ─────────────────────────────────────────────────────────

$TierComponents = @{
    "minimal"  = @{
        Workflows      = @("daily-repo-status", "repo-audit")
        Infrastructure = @()
        Components     = @("labels", "governance", "daily-repo-status", "repo-audit")
    }
    "standard" = @{
        Workflows      = @("daily-repo-status", "repo-audit", "issue-processor", "simplisticate")
        Infrastructure = @("sfl-dispatcher", "sfl-auditor")
        Components     = @("labels", "governance", "sfl-dispatcher", "sfl-auditor",
                           "daily-repo-status", "repo-audit", "issue-processor", "simplisticate")
    }
    "full"     = @{
        Workflows      = @("daily-repo-status", "repo-audit", "issue-processor", "simplisticate",
                           "pr-analyzer-a", "pr-analyzer-b", "pr-analyzer-c", "pr-fixer", "pr-promoter")
        Infrastructure = @("sfl-dispatcher", "sfl-auditor")
        Components     = @("labels", "governance", "sfl-dispatcher", "sfl-auditor",
                           "daily-repo-status", "repo-audit", "issue-processor", "simplisticate",
                           "pr-analyzer-a", "pr-analyzer-b", "pr-analyzer-c", "pr-fixer", "pr-promoter")
    }
}

# ─── Resolve what to deploy ──────────────────────────────────────────────────

if ($Workflow) {
    $WorkflowSource = Join-Path $RepoRoot "deployment\workflows\$Workflow.md"
    if (-not (Test-Path $WorkflowSource)) {
        Write-Error "Workflow not found: $WorkflowSource`nRun 'Get-ChildItem $RepoRoot\deployment\workflows\' to see available workflows."
        exit 1
    }
    $WorkflowsToDeploy      = @($Workflow)
    $InfrastructureToDeploy = @()
    $DeployTier             = "custom"
    $DeployComponents       = @($Workflow)
} else {
    $tierDef                = $TierComponents[$Tier]
    $WorkflowsToDeploy      = $tierDef.Workflows
    $InfrastructureToDeploy = $tierDef.Infrastructure
    $DeployTier             = $Tier
    $DeployComponents       = $tierDef.Components

    # Validate all workflow files exist
    foreach ($wf in $WorkflowsToDeploy) {
        $wfPath = Join-Path $RepoRoot "deployment\workflows\$wf.md"
        if (-not (Test-Path $wfPath)) {
            Write-Error "Workflow not found: $wfPath"
            exit 1
        }
    }
    foreach ($inf in $InfrastructureToDeploy) {
        $infPath = Join-Path $RepoRoot "deployment\infrastructure\$inf.yml"
        if (-not (Test-Path $infPath)) {
            Write-Error "Infrastructure file not found: $infPath"
            exit 1
        }
    }
}

# ─── Resolve current SHA for source pinning ───────────────────────────────────

$CurrentSha = (git -C $RepoRoot rev-parse HEAD 2>$null).Trim()
if (-not $CurrentSha) {
    Write-Error "Could not determine current git SHA. Is this repo initialized?"
    exit 1
}

$EnginePolicyManifest = New-SflEnginePolicyManifest $WorkflowsToDeploy

# ─── Helpers ──────────────────────────────────────────────────────────────────

function Write-Status([string]$Emoji, [string]$Message, [ConsoleColor]$Color = "Cyan") {
    Write-Host "$Emoji  $Message" -ForegroundColor $Color
}

function Deploy-ToRepo([string]$TargetRepo) {
    $RepoName    = $TargetRepo.Split("/")[-1]
    $BranchLabel = if ($Tier) { "tier-$Tier" } else { "add-$Workflow" }
    $BranchName  = "sfl/$BranchLabel"
    $ClonePath   = Join-Path $CloneDir $RepoName
    $DestWorkdir = Join-Path $ClonePath ".github\workflows"

    $DeployLabel = if ($Tier) { "tier '$Tier'" } else { "workflow '$Workflow'" }
    Write-Status "🚀" "Deploying $DeployLabel → $TargetRepo"

    if ($DryRun) {
        Write-Status "🔍" "[DRY RUN] Would clone $TargetRepo to $ClonePath" Yellow
        Write-Status "🔍" "[DRY RUN] Would create branch: $BranchName" Yellow
        foreach ($wf in $WorkflowsToDeploy) {
            $engineProfile = Resolve-SflEngineProfile $wf
            $sourceFile = Join-Path $RepoRoot "deployment\workflows\$wf.md"
            [void](ConvertTo-SflWorkflowWithEnginePolicy `
                -Content (Get-Content $sourceFile -Raw) `
                -EngineProfile $engineProfile `
                -WorkflowName $wf)
            Write-Status "🔍" "[DRY RUN] Would copy $wf.md → .github/workflows/ with $($engineProfile.Provider) $($engineProfile.RenderedModel)" Yellow
        }
        foreach ($inf in $InfrastructureToDeploy) {
            Write-Status "🔍" "[DRY RUN] Would copy $inf.yml → .github/workflows/" Yellow
        }
        Write-Status "🔍" "[DRY RUN] Would create/update sfl.json manifest" Yellow
        Write-Status "🔍" "[DRY RUN] Would inject/update SFL badge in README.md" Yellow
        Write-Status "🔍" "[DRY RUN] Would open PR via gh pr create" Yellow
        return
    }

    # Clean up any previous clone attempt
    if (Test-Path $ClonePath) { Remove-Item $ClonePath -Recurse -Force }

    try {
        # 1. Clone
        Write-Status "📥" "Cloning $TargetRepo…"
        git clone "git@github-personal1:$TargetRepo.git" $ClonePath --depth=1 --quiet
        if ($LASTEXITCODE -ne 0) { throw "git clone failed" }

        # 2. Create branch
        git -C $ClonePath checkout -b $BranchName --quiet
        if ($LASTEXITCODE -ne 0) { throw "git checkout -b failed" }

        # 3. Copy workflow files
        New-Item -ItemType Directory -Force $DestWorkdir | Out-Null

        foreach ($wf in $WorkflowsToDeploy) {
            $SourceFile = Join-Path $RepoRoot "deployment\workflows\$wf.md"
            $DestFile   = Join-Path $DestWorkdir "$wf.md"
            Copy-Item $SourceFile $DestFile

            $engineProfile = Resolve-SflEngineProfile $wf
            $content = ConvertTo-SflWorkflowWithEnginePolicy `
                -Content (Get-Content $DestFile -Raw) `
                -EngineProfile $engineProfile `
                -WorkflowName $wf
            $SflSourceRef = "HemSoft/set-it-free-loop/deployment/workflows/$wf.md@$CurrentSha"
            $pinComment = "# Deployed from: $SflSourceRef`n# To upgrade: re-run deploy-workflow.ps1 at the desired SHA`n"
            if ($content -notmatch "# Deployed from:") {
                Set-Content $DestFile -Value ($pinComment + $content) -NoNewline
            } else {
                Set-Content $DestFile -Value $content -NoNewline
            }
            Write-Status "📄" "  $wf.md ($($engineProfile.Provider) $($engineProfile.RenderedModel))"
        }

        # 4. Copy infrastructure files (standard YAML — go directly to .github/workflows/)
        foreach ($inf in $InfrastructureToDeploy) {
            $SourceFile = Join-Path $RepoRoot "deployment\infrastructure\$inf.yml"
            $DestFile   = Join-Path $DestWorkdir "$inf.yml"
            Copy-Item $SourceFile $DestFile
            Write-Status "⚙️ " "  $inf.yml"
        }

        # 5. Create sfl.json manifest
        $now = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
        $manifest = @{
            version    = $SflVersion
            deployedAt = $now
            tier       = $DeployTier
            source     = "HemSoft/set-it-free-loop"
            sourceSha  = $CurrentSha
            components = $DeployComponents
            enginePolicy = $EnginePolicyManifest
        } | ConvertTo-Json -Depth 6

        $manifestPath = Join-Path $ClonePath "sfl.json"
        Set-Content $manifestPath $manifest
        Write-Status "📋" "  sfl.json (v$SflVersion, tier: $DeployTier)"

        # 7. Inject/update SFL badge in README.md
        $readmePath = Join-Path $ClonePath "README.md"
        if (Test-Path $readmePath) {
            $readmeContent = Get-Content $readmePath -Raw
            $owner = $TargetRepo.Split("/")[0]
            $repo  = $TargetRepo.Split("/")[1]
            $badgeUrl = "https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fraw.githubusercontent.com%2F$owner%2F$repo%2Fmain%2Fsfl.json&query=%24.version&prefix=v&label=Set%20it%20Free%20Loop&color=FFD700&style=flat&logo=githubactions&logoColor=white"
            $badgeLine = "[![Set it Free Loop]($badgeUrl)](https://github.com/HemSoft/set-it-free-loop)"
            $badgeWithMarker = "$badgeLine`n<!-- SFL_BADGE: auto-updated by deploy-workflow.ps1 -->"

            if ($readmeContent -match '(?m)^.*<!-- SFL_BADGE:.*-->.*$') {
                # Find the badge line above the marker and replace both lines
                $readmeContent = $readmeContent -replace '(?m)^\[!\[Set it Free Loop\].*\n.*<!-- SFL_BADGE:.*-->', $badgeWithMarker
                Set-Content $readmePath $readmeContent -NoNewline
                Write-Status "🏷️ " "  README.md badge updated"
            } else {
                # Insert badge after the last consecutive badge line ([![...)
                $lines = $readmeContent -split "`n"
                $insertIndex = 0
                for ($i = 0; $i -lt $lines.Count; $i++) {
                    if ($lines[$i] -match '^\[!\[') {
                        $insertIndex = $i + 1
                    } elseif ($insertIndex -gt 0 -and $lines[$i] -notmatch '^\[!\[') {
                        break
                    }
                }
                $before = $lines[0..($insertIndex - 1)] -join "`n"
                $after  = $lines[$insertIndex..($lines.Count - 1)] -join "`n"
                $newContent = "$before`n$badgeWithMarker`n$after"
                Set-Content $readmePath $newContent -NoNewline
                Write-Status "🏷️ " "  README.md badge injected"
            }
        } else {
            Write-Status "⚠️ " "  No README.md found — skipping badge injection" Yellow
        }

        # 7. Stage and commit
        git -C $ClonePath add -A | Out-Null

        $commitMsg = if ($Tier) {
            "chore: deploy Set it Free Loop ($Tier tier, v$SflVersion)"
        } else {
            "chore: add $Workflow workflow from Set it Free Loop"
        }

        git -C $ClonePath commit -m "$commitMsg

Source: HemSoft/set-it-free-loop@$CurrentSha
Version: $SflVersion
Tier: $DeployTier
Components: $($DeployComponents -join ', ')
Engine policy: $DefaultEngineProfileName

See https://github.com/HemSoft/set-it-free-loop for full documentation." --quiet
        if ($LASTEXITCODE -ne 0) { throw "git commit failed" }

        # 8. Push
        git -C $ClonePath push origin $BranchName --quiet
        if ($LASTEXITCODE -ne 0) { throw "git push failed" }

        # 9. Open PR
        $prTitle = if ($Tier) {
            "chore: deploy Set it Free Loop ($Tier tier, v$SflVersion)"
        } else {
            "chore: add $Workflow workflow (Set it Free Loop)"
        }

        $componentList = ($DeployComponents | ForEach-Object { "- ``$_``" }) -join "`n"

        $prUrl = gh pr create `
            --repo $TargetRepo `
            --head $BranchName `
            --base main `
            --title $prTitle `
            --body "## Set it Free Loop — Deployment

**Version**: $SflVersion
**Tier**: $DeployTier
**Source SHA**: ``$CurrentSha``
**Engine policy**: ``$DefaultEngineProfileName``

### Components deployed

$componentList

### What is the Set it Free Loop?

The [Set it Free Loop](https://github.com/HemSoft/set-it-free-loop) is a continuous
quality improvement operating model for software repositories. See the
[CATALOG](https://github.com/HemSoft/set-it-free-loop/blob/main/CATALOG.md)
for all available workflows.

### Before merging

- [ ] Run ``.\deployment\governance\setup-labels.ps1 -Owner <org> -Repo <repo>`` if labels are not yet configured
- [ ] For each ``.md`` workflow: verify ``gh aw compile .github/workflows/<name>.md`` succeeds
- [ ] Trigger a workflow manually to confirm output
- [ ] Review ``sfl.json`` manifest in the repo root
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
if ($Tier) {
    Write-Status "📦" "Tier: $Tier ($($WorkflowsToDeploy.Count) workflows + $($InfrastructureToDeploy.Count) infrastructure)"
} else {
    Write-Status "📦" "Workflow: $Workflow"
}
Write-Status "🎯" "Source SHA: $CurrentSha"
Write-Status "🤖" "Engine policy: $DefaultEngineProfileName"

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
