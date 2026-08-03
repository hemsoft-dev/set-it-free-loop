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
    'label_command:',
    'name:\s*sfl-review',
    'remove_label:\s*true',
    'sfl-review',
    '(?m)^engine:\r?\n[ \t]+id:[ \t]*copilot\r?$',
    '(?m)^model:[ \t]*moonshotai/kimi-k3\r?$',
    'COPILOT_PROVIDER_BASE_URL:[ \t]*https://openrouter\.ai/api/v1',
    'COPILOT_PROVIDER_API_KEY:[ \t]*\$\{\{ secrets\.OPENROUTER_API_KEY \}\}',
    'COPILOT_PROVIDER_TYPE:[ \t]*openai',
    'COPILOT_PROVIDER_WIRE_API:[ \t]*responses',
    'COPILOT_MODEL:[ \t]*moonshotai/kimi-k3',
    'default-ai-credits-pricing:',
    'input:[ \t]*3(?:\.0)?',
    'output:[ \t]*15(?:\.0)?',
    'openrouter\.ai',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    'create-pull-request-review-comment',
    'Set `side` to `LEFT` for a deleted line and `RIGHT`',
    'submit-pull-request-review',
    'create-check-run',
    'threat-detection:\s*\r?\n[ \t]+enabled:\s*true',
    'max-ai-credits:\s*-1',
    'commit-id:\s*\$\{\{ github\.event\.pull_request\.head\.sha \}\}',
    'report-as-issue:\s*false',
    'sfl-review-inventory:',
    'SFL_VERDICT_VALIDATOR_START',
    'Mint SFL validation token',
    'actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1',
    'permission-pull-requests:\s*read',
    'Require SFL review output',
    'reviewThreads\(first:100,after:\$after\)',
    'pageInfo\{hasNextPage endCursor\}',
    'GitHub GraphQL returned errors',
    'GitHub returned incomplete review-thread data',
    'output target repo must be',
    'output target \$\{field\} must be',
    'unexpected safe output types',
    "author !== 'sfl-app\[bot\]'",
    'requiredCheckFragments',
    'requireSingleReviewLine',
    'expectedRows',
    'check summary does not match the required exact shape',
    'Overflow:',
    'noop is forbidden while the pull request head is unchanged',
    'actualCarried',
    'exactly one consolidated review and one check run are required',
    'review event must be',
    'check conclusion must be',
    'check title must be SFL full-spectrum review complete',
    'Repository-owner-approved exception',
    'complete finding inventory exceeded the inline-comment limit',
    'did\s+not exceed 20 comments',
    'unresolved SFL findings from earlier',
    'Call `noop` with',
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
    'copilot-requests:\s*write',
    'OPENAI_API_KEY:',
    'CODEX_API_KEY:',
    'remove-labels:',
    'pr-diff\.patch',
    'pr-review-comments\.json'
)
Assert-FileNotContains -RelativePath 'deployment\workflows\sfl-pr-review.md' -Patterns @(
    '(?m)^engine:\r?\n[ \t]+id:[ \t]*copilot\r?\n[ \t]+model:',
    'copilot-requests:\s*write',
    'OPENAI_API_KEY:',
    'CODEX_API_KEY:',
    'remove-labels:',
    'pr-diff\.patch',
    'pr-review-comments\.json'
)
Assert-FileExists -RelativePath '.github\workflows\sfl-pr-review.lock.yml'
Assert-FileNotContains -RelativePath '.github\workflows\sfl-pr-review.lock.yml' -Patterns @(
    'copilot-requests:\s*write'
)

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
$openRouterProfile = $enginePolicy.profiles.PSObject.Properties['openrouter-kimi-k3-high']
$openRouterArguments = if ($null -ne $openRouterProfile -and
    $null -ne $openRouterProfile.Value.PSObject.Properties['arguments']) {
    @($openRouterProfile.Value.arguments)
} else {
    @()
}
if ($null -eq $openRouterProfile -or
    $openRouterProfile.Value.provider -ne 'copilot' -or
    $openRouterProfile.Value.model -ne 'moonshotai/kimi-k3' -or
    'OPENROUTER_API_KEY' -notin @($openRouterProfile.Value.requiredSecretsAnyOf) -or
    @($openRouterArguments).Count -ne 0) {
    $failures.Add('deployment/engine-policy.json is missing the OpenRouter Kimi K3 review profile.')
}
$reviewPolicy = $enginePolicy.workflows.PSObject.Properties['sfl-pr-review']
if ($null -eq $reviewPolicy -or $reviewPolicy.Value.profile -ne 'openrouter-kimi-k3-high') {
    $failures.Add('deployment/engine-policy.json does not map sfl-pr-review to OpenRouter Kimi K3.')
}

Assert-FileContains -RelativePath 'deployment\scripts\deploy-workflow.ps1' -Patterns @(
    'ValidateSet\([^\)]*"review"',
    '"review"\s*=\s*@\{',
    'sfl-pr-review',
    'gh label list --repo \$TargetRepo --limit 1000',
    'if \(\$labelExists\)',
    '''label'', ''create'', \$SflReviewLabel\.name',
    'Ensuring trigger label',
    'Verifying SFL App credentials',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    'Missing AI engine credential',
    'Compiling deployed workflow',
    'gh aw compile \$DestFile --approve',
    'messageHeadline',
    'Unexpected consumer-authored commit',
    '\$CurrentSha\.Substring',
    'git fetch origin \$BranchName',
    'gh pr list',
    "--json 'number,url,headRefName'",
    'Deployed from:',
    'To upgrade:',
    'Existing PR updated',
    '@\(\$EngineProfile\.Arguments\)\.Count',
    '\$modelLine\s*=\s*"model:',
    'label=SFL%20Upstream',
    '\[!\[SFL Upstream\]',
    '\(\?:Set it Free Loop\|SFL Upstream\)'
)
Assert-FileContains -RelativePath 'deployment\scripts\install-gh-sfl-hemsoft.ps1' -Patterns @(
    'hemSoftEngineConfigForWorkflow',
    'Environment',
    'hemSoftEnginePolicyJSON',
    'json\.Unmarshal'
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
    'gh issue close',
    'sfl-pr-review\.md',
    'Deployed from: HemSoft/set-it-free-loop/',
    'Malformed SFL source provenance',
    'non-heading source provenance'
)
Assert-FileContains -RelativePath '.github\workflows\sfl-auditor.md' -Patterns @(
    'Check: SFL review prerequisites',
    'sfl-pr-review\.lock\.yml',
    'sfl-review',
    'create-issue',
    'github-app:',
    'SFL_APP_CLIENT_ID',
    'SFL_APP_PRIVATE_KEY',
    'Deployed from:',
    '# Deployed from:',
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
