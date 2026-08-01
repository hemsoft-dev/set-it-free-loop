[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
$failures = [System.Collections.Generic.List[string]]::new()

function Assert-FileExists {
    param([string] $RelativePath)

    $path = Join-Path $repoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        $failures.Add("Missing file: $RelativePath")
    }
}

function Assert-FileContains {
    param(
        [string] $RelativePath,
        [string[]] $Patterns
    )

    $path = Join-Path $repoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        $failures.Add("Missing file: $RelativePath")
        return
    }

    $content = Get-Content -LiteralPath $path -Raw
    foreach ($pattern in $Patterns) {
        if ($content -notmatch $pattern) {
            $failures.Add("$RelativePath does not contain pattern: $pattern")
        }
    }
}

function Assert-FileNotContains {
    param(
        [string] $RelativePath,
        [string[]] $Patterns
    )

    $path = Join-Path $repoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        $failures.Add("Missing file: $RelativePath")
        return
    }

    $content = Get-Content -LiteralPath $path -Raw
    foreach ($pattern in $Patterns) {
        if ($content -match $pattern) {
            $failures.Add("$RelativePath contains prohibited pattern: $pattern")
        }
    }
}

$workflowPatterns = @(
    'pull_request:',
    'names:\s*\[sfl-review\]',
    'sfl-review',
    'copilot-requests:\s*write',
    '(?m)^model:[ \t]*gpt-5\.5\?effort=high\r?$',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    'create-pull-request-review-comment',
    'submit-pull-request-review',
    'create-check-run',
    'remove-labels',
    'SFL Reviewer Approval',
    'SFL run ID:',
    'Verdict:\s*APPROVE',
    '\*\*CRITICAL\s',
    '\*\*HIGH\s',
    '\*\*MEDIUM\s',
    '\*\*LOW\s'
)

Assert-FileContains -RelativePath '.github\workflows\sfl-pr-review.md' -Patterns $workflowPatterns
Assert-FileContains -RelativePath 'deployment\workflows\sfl-pr-review.md' -Patterns $workflowPatterns
Assert-FileNotContains -RelativePath '.github\workflows\sfl-pr-review.md' -Patterns @(
    '(?m)^engine:\r?\n[ \t]+id:[ \t]*copilot\r?\n[ \t]+model:',
    'label_command',
    'pr-diff\.patch',
    'pr-review-comments\.json'
)
Assert-FileNotContains -RelativePath 'deployment\workflows\sfl-pr-review.md' -Patterns @(
    '(?m)^engine:\r?\n[ \t]+id:[ \t]*copilot\r?\n[ \t]+model:',
    'label_command',
    'pr-diff\.patch',
    'pr-review-comments\.json'
)
Assert-FileExists -RelativePath '.github\workflows\sfl-pr-review.lock.yml'

$stagingPath = Join-Path $repoRoot '.github\workflows\sfl-pr-review.md'
$deploymentPath = Join-Path $repoRoot 'deployment\workflows\sfl-pr-review.md'
if ((Test-Path -LiteralPath $stagingPath) -and (Test-Path -LiteralPath $deploymentPath)) {
    if ((Get-Content -LiteralPath $stagingPath -Raw) -ne
        (Get-Content -LiteralPath $deploymentPath -Raw)) {
        $failures.Add('Staging and deployment SFL review workflows differ.')
    }
}

$labelsPath = Join-Path $repoRoot 'deployment\governance\labels.json'
$labels = Get-Content -LiteralPath $labelsPath -Raw | ConvertFrom-Json
if ('sfl-review' -notin @($labels.name)) {
    $failures.Add('deployment/governance/labels.json is missing sfl-review.')
}

$schemaPath = Join-Path $repoRoot 'deployment\sfl-manifest.schema.json'
$schema = Get-Content -LiteralPath $schemaPath -Raw | ConvertFrom-Json
$componentEnum = @($schema.properties.components.items.enum)
if ('sfl-pr-review' -notin $componentEnum) {
    $failures.Add('deployment/sfl-manifest.schema.json is missing sfl-pr-review.')
}

$enginePolicyPath = Join-Path $repoRoot 'deployment\engine-policy.json'
$enginePolicy = Get-Content -LiteralPath $enginePolicyPath -Raw | ConvertFrom-Json
$copilotProfile = $enginePolicy.profiles.PSObject.Properties['copilot-gpt-55-high']
if ($null -eq $copilotProfile -or $copilotProfile.Value.provider -ne 'copilot') {
    $failures.Add('deployment/engine-policy.json is missing the Copilot review profile.')
}
$reviewPolicy = $enginePolicy.workflows.PSObject.Properties['sfl-pr-review']
if ($null -eq $reviewPolicy -or $reviewPolicy.Value.profile -ne 'copilot-gpt-55-high') {
    $failures.Add('deployment/engine-policy.json does not map sfl-pr-review to Copilot.')
}

Assert-FileContains -RelativePath 'deployment\scripts\deploy-workflow.ps1' -Patterns @(
    'ValidateSet\([^\)]*"review"',
    '"review"\s*=\s*@\{',
    'sfl-pr-review',
    'gh label list --repo \$TargetRepo',
    'if \(\$labelExists\)',
    '''label'', ''create'', \$SflReviewLabel\.name',
    'Ensuring trigger label',
    'Verifying SFL App credentials',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    'Compiling deployed workflow',
    'gh aw compile \$DestFile --approve',
    '\$modelLine\s*=\s*"model:'
)
Assert-FileContains -RelativePath 'CATALOG.md' -Patterns @(
    '\|\s*\*\*review\*\*',
    '\[sfl-pr-review\]',
    'SFL Reviewer Approval'
)
Assert-FileContains -RelativePath 'deployment\infrastructure\sfl-auditor.yml' -Patterns @(
    'Check: SFL review prerequisites',
    'sfl-pr-review\.lock\.yml',
    'labels/sfl-review',
    'SFL_APP_ID',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    'actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1',
    'permission-actions:\s*write',
    'permission-checks:\s*write',
    'installation/repositories',
    'viewer \{ login \}',
    'sfl-app\[bot\]',
    'Missing SFL review prerequisites',
    'gh issue create',
    'gh issue close'
)
Assert-FileContains -RelativePath '.github\workflows\sfl-auditor.md' -Patterns @(
    'Check: SFL review prerequisites',
    'sfl-pr-review\.lock\.yml',
    'sfl-review',
    'create-issue',
    'github-app:',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    '(?m)^model:[ \t]*gpt-5\.5\?effort=high\r?$'
)
Assert-FileNotContains -RelativePath '.github\workflows\sfl-auditor.md' -Patterns @(
    '(?m)^engine:\r?\n[ \t]+id:[ \t]*codex\r?\n[ \t]+model:'
)

if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Error $_ -ErrorAction Continue }
    throw "SFL review contract failed with $($failures.Count) finding(s)."
}

Write-Output 'SFL review contract passed.'
