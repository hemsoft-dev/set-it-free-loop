<#
.SYNOPSIS
    Deploys Set it Free Loop workflows to target repositories via Pull Requests.

.DESCRIPTION
    Supports two modes:

    1. Single workflow: Deploy one workflow by name.
       .\deploy-workflow.ps1 -Workflow repo-audit -Repos "org/repo"

    2. Tier-based: Deploy a predefined set of workflows by tier.
       .\deploy-workflow.ps1 -Tier standard -Repos "org/repo"

    3. Local materialization: Apply the central engine policy to local workflow
       author files and optionally compile the staged .github workflows.
       # -Local is retired; use the central App runbook.

    For each target repo, this script:
      1. Clones the repo (direct clone — no fork; assumes write access within the org)
      2. Creates a feature branch
      3. Copies workflow files into .github/workflows/
      4. Copies infrastructure files (dispatcher, auditor, reviewer wrappers)
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
    Deploy a predefined tier of workflows. Options: review, minimal, standard, full.
    Cannot be used with -Workflow.

    Tiers:
      review   — Subscription-backed Codex review observer and immutable-head gate
      minimal  — Labels, governance, repo-audit, daily-repo-status
      standard — Minimal + SFL Auditor, SFL Dispatcher, issue-processor, simplisticate
      full     — Standard + standalone review, focused PR Analyzers, PR Fixer, PR Promoter

.PARAMETER Repos
    Comma-separated list of target repos in "org/repo" format.

.PARAMETER Local
    Retired. Refuses before local workflow materialization or GitHub access.

.PARAMETER Compile
    With -Local, compile the materialized .github/workflows/*.md files via gh aw.

.PARAMETER ScheduleSeed
    Repository slug passed to gh aw compile when using -Local -Compile.

.PARAMETER DryRun
    Print what would be done without making any changes or API calls.

.PARAMETER CloneDir
    Temporary directory for cloning target repos. Cleaned up after each PR.
    Defaults to $env:TEMP\sfl-deploy

.EXAMPLE
    .\deploy-workflow.ps1 -Workflow repo-audit -Repos "HemSoft/hs-buddy"
    .\deploy-workflow.ps1 -Tier review -Repos "HemSoft/app1,HemSoft/app2"
    .\deploy-workflow.ps1 -Tier full -Repos "HemSoft/app1,HemSoft/app2"
    .\deploy-workflow.ps1 -Tier standard -Repos "HemSoft/myapp" -DryRun
    # -Local is retired; use the central App runbook.
#>

[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory = $false)]
    [string] $Workflow,

    [Parameter(Mandatory = $false)]
    [ValidateSet("review", "minimal", "standard", "full")]
    [string] $Tier,

    [Parameter(Mandatory = $false)]
    [string] $Repos,

    [switch] $DryRun,

    [switch] $Local,

    [switch] $Compile,

    [string] $ScheduleSeed = "hemsoft-dev/set-it-free-loop",

    [string] $CloneDir = "$env:TEMP\sfl-deploy",

    [string] $SourceRepository = 'hemsoft-dev/set-it-free-loop'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# Source repository consumer automation is retired. Stop before reading files,
# invoking GitHub or materializing workflows, including the former dry-run path.
if ($Local) {
    throw 'Source SFL deployment is retired. -Local materialization is disabled; use the central App runbook.'
}

# ─── Validate parameters ─────────────────────────────────────────────────────

if (-not $Workflow -and -not $Tier -and -not $Local) {
    Write-Error "You must specify -Workflow, -Tier, or -Local."
    exit 1
}
if ($Workflow -and $Tier) {
    Write-Error "Cannot specify both -Workflow and -Tier. Use one or the other."
    exit 1
}
if (-not $Local -and [string]::IsNullOrWhiteSpace($Repos)) {
    Write-Error "You must specify -Repos unless using -Local."
    exit 1
}
if ($Compile -and -not $Local) {
    Write-Error "-Compile can only be used with -Local."
    exit 1
}

# ─── Resolve paths ────────────────────────────────────────────────────────────

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "..\..")
. (Join-Path $ScriptDir "merge-sfl-manifest.ps1")
. (Join-Path $ScriptDir "add-sfl-source-pin.ps1")
Import-Module (Join-Path $ScriptDir 'SflRepositoryPolicy.psm1') -Force
Assert-SflSourceRepository $SourceRepository

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
    $environmentProperty = $profile.PSObject.Properties["environment"]
    $environment = if ($null -ne $environmentProperty -and $null -ne $environmentProperty.Value) {
        $environmentProperty.Value
    } else {
        $null
    }
    $argumentsProperty = $profile.PSObject.Properties["arguments"]
    $arguments = if ($null -ne $argumentsProperty -and $null -ne $argumentsProperty.Value) {
        @($argumentsProperty.Value)
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
        Environment          = $environment
        Arguments            = $arguments
    }
}

function ConvertTo-SflWorkflowWithEnginePolicy([string]$Content, [pscustomobject]$EngineProfile, [string]$WorkflowName) {
    $frontmatterMatch = [regex]::Match($Content, '(?s)\A---\r?\n(?<frontmatter>.*?)\r?\n---')
    if (-not $frontmatterMatch.Success) {
        throw "Workflow '$WorkflowName' does not start with YAML frontmatter."
    }

    $frontmatter = $frontmatterMatch.Groups["frontmatter"].Value
    $rest = $Content.Substring($frontmatterMatch.Length)
    $engineLines = @("engine:", "  id: $($EngineProfile.Provider)")
    if (@($EngineProfile.Arguments).Count -gt 0) {
        $engineLines += "  args:"
        foreach ($argument in $EngineProfile.Arguments) {
            $escapedArgument = ([string] $argument).Replace("'", "''")
            $engineLines += "    - '$escapedArgument'"
        }
    }
    if ($null -ne $EngineProfile.Environment) {
        $engineLines += "  env:"
        [string[]] $environmentNames = @($EngineProfile.Environment.PSObject.Properties.Name)
        [Array]::Sort($environmentNames, [StringComparer]::Ordinal)
        foreach ($propertyName in $environmentNames) {
            $propertyValue = $EngineProfile.Environment.PSObject.Properties[$propertyName].Value
            $engineLines += "    ${propertyName}: $propertyValue"
        }
    }
    $engineBlock = $engineLines -join "`n"
    $modelLine = "model: $($EngineProfile.RenderedModel)"

    $frontmatter = [regex]::Replace(
        $frontmatter,
        '(?m)(?:^[ \t]*\r?\n)?^engine:[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*\r?\n?',
        '',
        1
    ).TrimEnd("`r", "`n")

    $frontmatter = [regex]::Replace(
        $frontmatter,
        '(?m)(?:^[ \t]*\r?\n)?^model:[^\r\n]*\r?\n?',
        '',
        1
    ).TrimEnd("`r", "`n")

    if ($frontmatter -match '(?m)^network:') {
        $frontmatter = [regex]::Replace(
            $frontmatter,
            '(?m)^network:',
            "$engineBlock`n`n$modelLine`n`nnetwork:",
            1
        )
    } else {
        $frontmatter = "$frontmatter`n`n$engineBlock`n`n$modelLine"
    }

    $frontmatter = [regex]::Replace($frontmatter, '(\r?\n){3,}(?=(?:engine|model):)', "`n`n")

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
                arguments            = @($profile.Arguments)
                environment          = $profile.Environment
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
    "review"   = @{
        Workflows      = @()
        Infrastructure = @("sfl-pr-review-auto")
        Components     = @("sfl-pr-review-auto")
    }
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
                           "pr-analyzer-general", "pr-analyzer-quality", "pr-analyzer-security",
                           "pr-analyzer-testing", "pr-fixer", "pr-promoter")
        Infrastructure = @("sfl-dispatcher", "sfl-auditor",
                           "sfl-pr-review-auto")
        Components     = @("labels", "governance", "sfl-dispatcher", "sfl-auditor",
                           "daily-repo-status", "repo-audit", "issue-processor", "simplisticate",
                           "sfl-pr-review-auto",
                           "pr-analyzer-general", "pr-analyzer-quality", "pr-analyzer-security",
                           "pr-analyzer-testing", "pr-fixer", "pr-promoter")
    }
}

# ─── Resolve what to deploy ──────────────────────────────────────────────────

if ($Local -and -not $Workflow -and -not $Tier) {
    $WorkflowsToDeploy = @(
        Get-ChildItem (Join-Path $RepoRoot "deployment\workflows") -Filter "*.md" |
            Where-Object { $_.BaseName -ne "_TEMPLATE" } |
            Sort-Object BaseName |
            ForEach-Object { $_.BaseName }
    )
    $InfrastructureToDeploy = @("sfl-pr-review-auto")
    $DeployTier = "local"
    $DeployComponents = @($WorkflowsToDeploy) + @($InfrastructureToDeploy)
} elseif ($Workflow) {
    $WorkflowSource = Join-Path $RepoRoot "deployment\workflows\$Workflow.md"
    if (-not (Test-Path $WorkflowSource)) {
        Write-Error "Workflow not found: $WorkflowSource`nRun 'Get-ChildItem $RepoRoot\deployment\workflows\' to see available workflows."
        exit 1
    }
    $WorkflowsToDeploy      = @($Workflow)
    $InfrastructureToDeploy = @()
    $DeployTier             = "custom"
    $DeployComponents       = @($Workflow) + @($InfrastructureToDeploy)
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
if (-not $Local -and -not $DryRun) {
    Assert-SflSourceCheckout -Repository $SourceRepository -CheckoutRoot $RepoRoot -Commit $CurrentSha
}

$EnginePolicyManifest = New-SflEnginePolicyManifest $WorkflowsToDeploy

# ─── Helpers ──────────────────────────────────────────────────────────────────

function Write-Status([string]$Emoji, [string]$Message, [ConsoleColor]$Color = "Cyan") {
    Write-Host "$Emoji  $Message" -ForegroundColor $Color
}

function Assert-HemSoftRepository([string] $TargetRepo) {
    Assert-SflTargetScope $TargetRepo
    if ($DryRun) {
        return
    }
    $null = Get-SflRepositoryContext -Repository $TargetRepo
}

function Assert-SflReviewCredentials([string]$TargetRepo) {
    if ($WorkflowsToDeploy.Count -eq 0) {
        return
    }

    Write-Status "🔐" "Verifying workflow engine credentials on $TargetRepo"
    if ($DryRun) {
        return
    }

    $secretNames = @(Get-SflActionsSecretNames -Repository $TargetRepo)

    foreach ($workflowName in $WorkflowsToDeploy) {
        $profile = Resolve-SflEngineProfile $workflowName
        $requiredSecrets = @($profile.RequiredSecretsAnyOf)
        if ($requiredSecrets.Count -gt 0 -and
            @($requiredSecrets | Where-Object { $_ -in $secretNames }).Count -eq 0) {
            throw "Missing AI engine credential on ${TargetRepo} for ${workflowName}: one of $($requiredSecrets -join ', ')"
        }
    }
}

function Deploy-ToRepo([string]$TargetRepo) {
    $RepoName    = $TargetRepo.Split("/")[-1]
    $BranchLabel = if ($Tier) { "tier-$Tier" } else { "add-$Workflow" }
    $CanonicalBranchName = "sfl/$BranchLabel"
    Assert-HemSoftRepository $TargetRepo
    $BaseBranch = gh repo view $TargetRepo --json defaultBranchRef --jq '.defaultBranchRef.name'
    if (-not $BaseBranch) {
        throw "Repository $TargetRepo has no default branch."
    }

    $existingPrCandidates = @(
        gh pr list --repo $TargetRepo --state open --json 'number,url,headRefName' |
            ConvertFrom-Json |
            Where-Object { $_.headRefName -like "$CanonicalBranchName*" }
    )
    if ($existingPrCandidates.Count -gt 1) {
        throw "Multiple open SFL deployment PRs match $CanonicalBranchName on $TargetRepo."
    }

    $existingPr = $existingPrCandidates | Select-Object -First 1
    if ($existingPr) {
        $unexpectedCommits = @(
            gh pr view $existingPr.number --repo $TargetRepo --json commits `
                --jq '.commits[].messageHeadline' |
                Where-Object { $_ -notmatch '^chore: deploy Set it Free Loop' }
        )
        if ($unexpectedCommits.Count -gt 0) {
            throw "Unexpected consumer-authored commit on $($existingPr.url): $($unexpectedCommits -join '; ')"
        }
        $BranchName = [string] $existingPr.headRefName
        $existingPrUrl = [string] $existingPr.url
    } else {
        $BranchName = "$CanonicalBranchName-$($CurrentSha.Substring(0, 7))"
        $existingPrUrl = $null
    }

    $ClonePath   = Join-Path $CloneDir $RepoName
    $DestWorkdir = Join-Path $ClonePath ".github\workflows"

    $DeployLabel = if ($Tier) { "tier '$Tier'" } else { "workflow '$Workflow'" }
    Write-Status "🚀" "Deploying $DeployLabel → $TargetRepo"
    Assert-SflReviewCredentials $TargetRepo

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
            Write-Status "🔍" "[DRY RUN] Would compile deployed workflow $wf.md" Yellow
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

        # 2. Create or reuse the open deployment PR branch
        Push-Location $ClonePath
        try {
            $ExpectedRemoteSha = $null
            if ($existingPr) {
                git fetch origin $BranchName --depth=1 --quiet
                if ($LASTEXITCODE -ne 0) { throw "git fetch origin $BranchName failed" }
                $ExpectedRemoteSha = (git rev-parse FETCH_HEAD).Trim()
                git checkout -B $BranchName "origin/$BaseBranch" --quiet
                if ($LASTEXITCODE -ne 0) { throw "git checkout existing branch from latest $BaseBranch failed" }
            } else {
                git checkout -b $BranchName --quiet
                if ($LASTEXITCODE -ne 0) { throw "git checkout -b failed" }
            }
        } finally {
            Pop-Location
        }

        # 3. Copy workflow files
        New-Item -ItemType Directory -Force $DestWorkdir | Out-Null

        if ("sfl-pr-review-auto" -in $DeployComponents) {
            foreach ($retiredReviewerFile in @(
                "sfl-pr-review.md",
                "sfl-pr-review.lock.yml",
                "sfl-pr-review-recovery.yml"
            )) {
                $retiredPath = Join-Path $DestWorkdir $retiredReviewerFile
                if (Test-Path -LiteralPath $retiredPath -PathType Leaf) {
                    Remove-Item -LiteralPath $retiredPath -Force
                    Write-Status "🧹" "  removed retired $retiredReviewerFile"
                }
            }
        }

        foreach ($wf in $WorkflowsToDeploy) {
            $SourceFile = Join-Path $RepoRoot "deployment\workflows\$wf.md"
            $DestFile   = Join-Path $DestWorkdir "$wf.md"
            Copy-Item $SourceFile $DestFile

            $engineProfile = Resolve-SflEngineProfile $wf
            $content = ConvertTo-SflWorkflowWithEnginePolicy `
                -Content (Get-Content $DestFile -Raw) `
                -EngineProfile $engineProfile `
                -WorkflowName $wf
            $SflSourceRef = "$SourceRepository/deployment/workflows/$wf.md@$CurrentSha"
            $pinComment = "<!--`nDeployed from: $SflSourceRef`nTo upgrade: re-run deploy-workflow.ps1 at the desired SHA`n-->`n"
            $content = Add-SflSourcePin -Content $content -PinComment $pinComment
            Set-Content $DestFile -Value $content -NoNewline
            Write-Status "📄" "  $wf.md ($($engineProfile.Provider) $($engineProfile.RenderedModel))"
        }

        foreach ($wf in $WorkflowsToDeploy) {
            $DestFile = Join-Path $DestWorkdir "$wf.md"
            Write-Status "🔧" "Compiling deployed workflow $wf.md"
            Push-Location $ClonePath
            try {
                gh aw compile $DestFile --approve --no-check-update --actionlint --schedule-seed $TargetRepo
                if ($LASTEXITCODE -ne 0) {
                    throw "gh aw compile failed for $DestFile"
                }
            } finally {
                Pop-Location
            }
        }

        # 4. Copy infrastructure files (standard YAML — go directly to .github/workflows/)
        foreach ($inf in $InfrastructureToDeploy) {
            $SourceFile = Join-Path $RepoRoot "deployment\infrastructure\$inf.yml"
            $DestFile   = Join-Path $DestWorkdir "$inf.yml"
            Copy-Item $SourceFile $DestFile
            if ($inf -eq "sfl-pr-review-auto") {
                $sourceRef = "$SourceRepository/deployment/infrastructure/$inf.yml@$CurrentSha"
                $content = Add-SflYamlSourcePin `
                    -Content (Get-Content $DestFile -Raw) `
                    -SourceRef $sourceRef `
                    -DefaultBranch $BaseBranch
                Set-Content $DestFile -Value $content -NoNewline
            }
            Write-Status "⚙️ " "  $inf.yml"
        }

        # 5. Create sfl.json manifest
        $now = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
        $incomingManifest = [pscustomobject] @{
            version    = $SflVersion
            deployedAt = $now
            tier       = $DeployTier
            source     = $SourceRepository
            sourceSha  = $CurrentSha
            components = $DeployComponents
            enginePolicy = $EnginePolicyManifest
        }

        $manifestPath = Join-Path $ClonePath "sfl.json"
        $canonicalManifestPath = Join-Path $ClonePath ".sfl/sfl.json"
        $existingManifestPath = if (Test-Path -LiteralPath $canonicalManifestPath -PathType Leaf) {
            $canonicalManifestPath
        } else {
            $manifestPath
        }
        $existingManifest = if (Test-Path -LiteralPath $existingManifestPath -PathType Leaf) {
            Get-Content -LiteralPath $existingManifestPath -Raw | ConvertFrom-Json
        } else {
            $null
        }
        $manifestObject = Merge-SflManifest `
            -ExistingManifest $existingManifest `
            -IncomingManifest $incomingManifest
        $manifest = $manifestObject | ConvertTo-Json -Depth 6
        Set-Content $manifestPath $manifest
        if (Test-Path -LiteralPath $canonicalManifestPath -PathType Leaf) {
            Set-Content $canonicalManifestPath $manifest
        }
        Write-Status "📋" "  sfl.json (v$SflVersion, tier: $($manifestObject.tier))"

        # 7. Inject/update SFL badge in README.md
        $readmePath = Join-Path $ClonePath "README.md"
        if (Test-Path $readmePath) {
            $readmeContent = Get-Content $readmePath -Raw
            $owner = $TargetRepo.Split("/")[0]
            $repo  = $TargetRepo.Split("/")[1]
            $badgeUrl = "https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fraw.githubusercontent.com%2F$owner%2F$repo%2Fmain%2Fsfl.json&query=%24.version&prefix=v&label=SFL%20Upstream&color=FFD700&style=flat&logo=githubactions&logoColor=white"
            $badgeLine = "[![SFL Upstream]($badgeUrl)](https://github.com/$SourceRepository)"
            $badgeWithMarker = "$badgeLine`n<!-- SFL_BADGE: auto-updated by deploy-workflow.ps1 -->"

            if ($readmeContent -match '(?m)^.*<!-- SFL_BADGE:.*-->.*$') {
                # Find the badge line above the marker and replace both lines
                $readmeContent = $readmeContent -replace '(?m)^\[!\[(?:Set it Free Loop|SFL Upstream)\].*\r?\n.*<!-- SFL_BADGE:.*-->', $badgeWithMarker
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

Source: $SourceRepository@$CurrentSha
Version: $SflVersion
Tier: $DeployTier
Components: $($DeployComponents -join ', ')
Engine policy: $DefaultEngineProfileName

See https://github.com/$SourceRepository for full documentation." --quiet
        if ($LASTEXITCODE -ne 0) { throw "git commit failed" }

        # 8. Push
        if ($existingPr) {
            git -C $ClonePath push origin $BranchName "--force-with-lease=${BranchName}:$ExpectedRemoteSha" --quiet
        } else {
            git -C $ClonePath push origin $BranchName --quiet
        }
        if ($LASTEXITCODE -ne 0) { throw "git push failed" }

        # 9. Open PR
        $prTitle = if ($Tier) {
            "chore: deploy Set it Free Loop ($Tier tier, v$SflVersion)"
        } else {
            "chore: add $Workflow workflow (Set it Free Loop)"
        }

        $componentList = ($DeployComponents | ForEach-Object { "- ``$_``" }) -join "`n"
		$compileChecklist = if ($WorkflowsToDeploy.Count -gt 0) {
			"- [ ] For each ``.md`` workflow: verify ``gh aw compile .github/workflows/<name>.md`` succeeds"
		} else {
			"- [ ] Verify the standard Actions observer with ``actionlint -ignore 'unexpected key `"queue`" for `"concurrency`" section' .github/workflows/sfl-pr-review-auto.yml``"
		}
		$prBody = "## Set it Free Loop — Deployment

**Version**: $SflVersion
**Tier**: $DeployTier
**Source SHA**: ``$CurrentSha``
**Engine policy**: ``$DefaultEngineProfileName``

### Components deployed

$componentList

### What is the Set it Free Loop?

The [Set it Free Loop](https://github.com/$SourceRepository) is a continuous
quality improvement operating model for software repositories. See the
[CATALOG](https://github.com/$SourceRepository/blob/main/CATALOG.md)
for all available workflows.

### Before merging

- [ ] Run ``.\deployment\governance\setup-labels.ps1 -Owner <org> -Repo <repo>`` if labels are not yet configured
$compileChecklist
- [ ] Trigger a workflow manually to confirm output
- [ ] Review ``sfl.json`` manifest in the repo root
"

        if ($existingPrUrl) {
			$existingPrNumber = [int] $existingPr.number
			gh pr edit $existingPrNumber `
				--repo $TargetRepo `
				--title $prTitle `
				--body $prBody | Out-Null
			if ($LASTEXITCODE -ne 0) {
				throw "gh pr edit failed for $existingPrUrl"
			}
            $prUrl = $existingPrUrl
            Write-Status "✅" "Existing PR updated: $prUrl" Green
        } else {
            $prUrl = gh pr create `
                --repo $TargetRepo `
                --head $BranchName `
                --base $BaseBranch `
                --title $prTitle `
                --body $prBody 2>&1

            if ($LASTEXITCODE -eq 0) {
                Write-Status "✅" "PR created: $prUrl" Green
            } else {
                throw "gh pr create failed: $prUrl"
            }
        }

    } finally {
        # Always clean up clone
        if (Test-Path $ClonePath) { Remove-Item $ClonePath -Recurse -Force }
    }
}

# ─── Main ─────────────────────────────────────────────────────────────────────

function Update-LocalWorkflowFiles {
    $DestWorkdir = Join-Path $RepoRoot ".github\workflows"
    Write-Status "🧩" "Materializing local workflows from engine policy"

    if (-not $DryRun) {
        New-Item -ItemType Directory -Force $DestWorkdir | Out-Null
    }

    foreach ($wf in $WorkflowsToDeploy) {
        $engineProfile = Resolve-SflEngineProfile $wf
        $SourceFile = Join-Path $RepoRoot "deployment\workflows\$wf.md"
        $DestFile = Join-Path $DestWorkdir "$wf.md"
        $sourceContent = ConvertTo-SflWorkflowWithEnginePolicy `
            -Content (Get-Content $SourceFile -Raw) `
            -EngineProfile $engineProfile `
            -WorkflowName $wf
        $destInput = if (Test-Path $DestFile) {
            Get-Content $DestFile -Raw
        } else {
            $sourceContent
        }
        $destContent = ConvertTo-SflWorkflowWithEnginePolicy `
            -Content $destInput `
            -EngineProfile $engineProfile `
            -WorkflowName $wf

        if ($DryRun) {
            Write-Status "🔍" "[DRY RUN] Would apply engine policy in deployment/workflows/$wf.md and .github/workflows/$wf.md with $($engineProfile.Provider) $($engineProfile.RenderedModel)" Yellow
            continue
        }

        Set-Content $SourceFile -Value $sourceContent -NoNewline
        Set-Content $DestFile -Value $destContent -NoNewline
        Write-Status "📄" "  $wf.md ($($engineProfile.Provider) $($engineProfile.RenderedModel))"
    }

    foreach ($inf in $InfrastructureToDeploy) {
        $SourceFile = Join-Path $RepoRoot "deployment\infrastructure\$inf.yml"
        $DestFile = Join-Path $DestWorkdir "$inf.yml"
        if ($DryRun) {
            Write-Status "🔍" "[DRY RUN] Would copy $inf.yml to .github/workflows/" Yellow
            continue
        }
        Copy-Item $SourceFile $DestFile
        Write-Status "⚙️ " "  $inf.yml"
    }

    if (-not $Compile) {
        return
    }

    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
        throw "GitHub CLI (gh) is not installed. Install via: winget install GitHub.cli"
    }

    foreach ($wf in $WorkflowsToDeploy) {
        $DestFile = Join-Path $DestWorkdir "$wf.md"
        Write-Status "🔧" "Compiling $wf.md"
        $CompileArgs = @("aw", "compile", $DestFile)
        if (-not [string]::IsNullOrWhiteSpace($ScheduleSeed)) {
            $CompileArgs += @("--schedule-seed", $ScheduleSeed)
        }
        gh @CompileArgs
        if ($LASTEXITCODE -ne 0) {
            throw "gh aw compile failed for $DestFile"
        }
    }
}

Write-Status "🔄" "Set it Free Loop — Workflow Deployer" White
if ($Tier) {
    Write-Status "📦" "Tier: $Tier ($($WorkflowsToDeploy.Count) workflows + $($InfrastructureToDeploy.Count) infrastructure)"
} elseif ($Local) {
    Write-Status "📦" "Local: $($WorkflowsToDeploy.Count) workflows"
} else {
    Write-Status "📦" "Workflow: $Workflow"
}
Write-Status "🎯" "Source SHA: $CurrentSha"
Write-Status "🤖" "Engine policy: $DefaultEngineProfileName"

if ($DryRun) {
    Write-Status "🔍" "DRY RUN — no changes will be made" Yellow
}

if ($Local) {
    try {
        Update-LocalWorkflowFiles
        Write-Status "📊" "Local workflows updated: $($WorkflowsToDeploy.Count)" Green
        exit 0
    } catch {
        Write-Status "❌" "Local workflow update failed: $_" Red
        exit 1
    }
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
