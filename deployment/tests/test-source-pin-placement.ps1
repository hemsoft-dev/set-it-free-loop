[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath
. (Join-Path $repoRoot 'deployment\scripts\add-sfl-source-pin.ps1')

$workflow = @'
---
description: Test workflow
on:
  workflow_dispatch:
---

# Test Workflow
'@

$pin = @'
<!--
Deployed from: HemSoft/set-it-free-loop/deployment/workflows/sfl-pr-review.md@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
To upgrade: re-run deploy-workflow.ps1 at the desired SHA
-->
'@

$result = Add-SflSourcePin -Content $workflow -PinComment $pin

if ($result -notmatch '\A---\r?\n') {
    throw 'Source pin was inserted before workflow frontmatter.'
}

if ($result -notmatch '(?s)\A---\r?\n.*?\r?\n---\r?\n<!--\r?\nDeployed from:') {
    throw 'Source pin was not inserted immediately after workflow frontmatter.'
}

if ($result -match '(?m)^# (?:Deployed from|To upgrade):') {
    throw 'Source pin was rendered as a Markdown heading.'
}

if (($result -split '\r?\n' | Measure-Object -Maximum Length).Maximum -gt 120) {
    throw 'Source pin exceeded the consumer Markdown line-length limit.'
}

$legacyWorkflow = @'
---
description: Test workflow
on:
  workflow_dispatch:
---
# Deployed from: HemSoft/set-it-free-loop/deployment/workflows/test.md@old
# To upgrade: re-run deploy-workflow.ps1 at the desired SHA

# Test Workflow
'@

$upgraded = Add-SflSourcePin -Content $legacyWorkflow -PinComment $pin
if ($upgraded -match '(?m)^# (?:Deployed from|To upgrade):' -or
    ([regex]::Matches($upgraded, '(?m)^Deployed from:').Count -ne 1)) {
    throw 'Legacy source pin was not replaced cleanly.'
}

$yamlWorkflow = @'
# HemSoft SFL reviewer platform v2
name: SFL PR Review Auto Trigger
on:
  pull_request_target:
'@
$yamlRef = 'HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
$yamlPinned = Add-SflYamlSourcePin -Content $yamlWorkflow -SourceRef $yamlRef
if ($yamlPinned -notmatch "\A# Deployed from: $([regex]::Escape($yamlRef))\r?\n# To upgrade:") {
    throw 'YAML source pin was not placed at the beginning of the workflow.'
}

$yamlUpdatedRef = 'HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@cccccccccccccccccccccccccccccccccccccccc'
$yamlRepinned = Add-SflYamlSourcePin -Content $yamlPinned -SourceRef $yamlUpdatedRef
if (([regex]::Matches($yamlRepinned, '(?m)^# Deployed from:').Count -ne 1) -or
    $yamlRepinned -notmatch [regex]::Escape($yamlUpdatedRef)) {
    throw 'Existing YAML source pin was not replaced idempotently.'
}

Write-Output 'Source pin placement test passed.'
