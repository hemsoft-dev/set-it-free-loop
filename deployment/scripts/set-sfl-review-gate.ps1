<#
.SYNOPSIS
    Requires the native SFL Reviewer Approval check on a branch.

.DESCRIPTION
    Adds the GitHub Actions-owned SFL Reviewer Approval check to branch
    protection without removing existing required checks. If the branch is not
    protected, the helper creates a minimal policy containing this check. The
    policy is strict so a base-branch update requires a fresh review of the new
    pull-request head.

    This HemSoft deployment helper supports HemSoft-owned repositories. It
    does not create or modify organization rulesets.

.EXAMPLE
    .\set-sfl-review-gate.ps1 -Repo HemSoft/my-repo

.EXAMPLE
    .\set-sfl-review-gate.ps1 -Repo HemSoft/my-repo -DryRun
#>

[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)]
    [ValidatePattern('^HemSoft/[^/]+$')]
    [string] $Repo,

    [string] $Branch,

    [switch] $DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$reviewContext = 'SFL Reviewer Approval'
$githubActionsAppId = 15368

if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw 'GitHub CLI (gh) is required.'
}

$activeLogin = (gh api user --jq '.login').Trim()
if ($LASTEXITCODE -ne 0 -or $activeLogin -ne 'HemSoft') {
    throw "GitHub CLI must be authenticated as HemSoft; active login is '$activeLogin'."
}

$repoInfo = gh repo view $Repo --json 'owner,defaultBranchRef' |
    ConvertFrom-Json
if ($LASTEXITCODE -ne 0) {
    throw "Could not read repository metadata for $Repo."
}
if ($repoInfo.owner.login -ne 'HemSoft') {
    throw 'SFL reviewer gates may be configured only on HemSoft-owned repositories.'
}
if ([string]::IsNullOrWhiteSpace($Branch)) {
    $Branch = [string] $repoInfo.defaultBranchRef.name
}
if ([string]::IsNullOrWhiteSpace($Branch)) {
    throw "Repository $Repo has no default branch."
}

$encodedBranch = [uri]::EscapeDataString($Branch)
$endpoint = "repos/$Repo/branches/$encodedBranch/protection/required_status_checks"
$currentJson = gh api --method GET $endpoint 2>&1
$statusChecksExist = $LASTEXITCODE -eq 0
$protectionExists = $statusChecksExist
if ($statusChecksExist) {
    $current = $currentJson | ConvertFrom-Json
} elseif (($currentJson -join "`n") -match 'Branch not protected|"status"\s*:\s*"?404') {
    $current = [pscustomobject]@{
        strict   = $false
        contexts = @()
        checks   = @()
    }
    $protectionEndpoint = "repos/$Repo/branches/$encodedBranch/protection"
    $protectionJson = gh api --method GET $protectionEndpoint 2>&1
    if ($LASTEXITCODE -eq 0) {
        # Required checks are absent, but other protection exists. PATCH the
        # status-check subresource below; never replace the full policy.
        $protectionExists = $true
    } elseif (($protectionJson -join "`n") -match 'Branch not protected|"status"\s*:\s*"?404') {
        $protectionExists = $false
    } else {
        throw "Could not inspect full branch protection on ${Repo}:$Branch. GitHub returned: $protectionJson"
    }
} else {
    throw "Could not inspect branch protection on ${Repo}:$Branch. GitHub returned: $currentJson"
}

$checksByIdentity = [ordered]@{}
foreach ($check in @($current.checks)) {
    $appId = if ($null -eq $check.app_id) { -1 } else { [int64] $check.app_id }
    $key = "$($check.context)|$appId"
    $checksByIdentity[$key] = [ordered]@{
        context = [string] $check.context
        app_id  = $appId
    }
}

foreach ($context in @($current.contexts)) {
    $alreadyPresent = @($checksByIdentity.Values) |
        Where-Object { $_.context -eq [string] $context } |
        Select-Object -First 1
    if (-not $alreadyPresent) {
        $checksByIdentity["$context|-1"] = [ordered]@{
            context = [string] $context
            app_id  = -1
        }
    }
}

$checksByIdentity["$reviewContext|$githubActionsAppId"] = [ordered]@{
    context = $reviewContext
    app_id  = $githubActionsAppId
}

$payload = [ordered]@{
    strict = $true
    checks = @($checksByIdentity.Values)
}
$payloadJson = $payload | ConvertTo-Json -Depth 5

$initialProtection = [ordered]@{
    required_status_checks       = [ordered]@{
        strict   = $true
        contexts = @($reviewContext)
    }
    enforce_admins               = $false
    required_pull_request_reviews = $null
    restrictions                 = $null
}
$initialProtectionJson = $initialProtection | ConvertTo-Json -Depth 5

if ($DryRun) {
    $operation = if ($statusChecksExist) {
        "preserving $(@($current.checks).Count) existing check(s)"
    } elseif ($protectionExists) {
        'enabling status checks without replacing existing branch protection'
    } else {
        'initializing minimal branch protection'
    }
    Write-Output "[DRY RUN] Would require '$reviewContext' on ${Repo}:$Branch while $operation."
    if (-not $protectionExists) {
        Write-Output $initialProtectionJson
    }
    Write-Output $payloadJson
    exit 0
}

if ($PSCmdlet.ShouldProcess("${Repo}:$Branch", "Require $reviewContext with strict status checks")) {
    if (-not $protectionExists) {
        $protectionEndpoint = "repos/$Repo/branches/$encodedBranch/protection"
        $null = $initialProtectionJson | gh api `
            --method PUT `
            $protectionEndpoint `
            --header 'Accept: application/vnd.github+json' `
            --input -
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to initialize branch protection on ${Repo}:$Branch."
        }
    }
    $resultJson = $payloadJson | gh api `
        --method PATCH `
        $endpoint `
        --header 'Accept: application/vnd.github+json' `
        --input -
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to configure the SFL reviewer gate on ${Repo}:$Branch."
    }
    $result = $resultJson | ConvertFrom-Json
    $configured = @($result.checks) |
        Where-Object {
            $_.context -eq $reviewContext -and
            $_.app_id -eq $githubActionsAppId
        }
    if (-not $result.strict -or @($configured).Count -ne 1) {
        throw 'GitHub accepted the request but did not return the required strict SFL reviewer gate.'
    }
    Write-Output "Configured strict '$reviewContext' gate on ${Repo}:$Branch."
}
