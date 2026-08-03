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
<!-- Deployed from: HemSoft/set-it-free-loop/deployment/workflows/test.md@abc123 -->
<!-- To upgrade: re-run deploy-workflow.ps1 at the desired SHA -->
'@

$result = Add-SflSourcePin -Content $workflow -PinComment $pin

if ($result -notmatch '\A---\r?\n') {
    throw 'Source pin was inserted before workflow frontmatter.'
}

if ($result -notmatch '(?s)\A---\r?\n.*?\r?\n---\r?\n<!-- Deployed from:') {
    throw 'Source pin was not inserted immediately after workflow frontmatter.'
}

if ($result -match '(?m)^# (?:Deployed from|To upgrade):') {
    throw 'Source pin was rendered as a Markdown heading.'
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
    ([regex]::Matches($upgraded, '<!-- Deployed from:').Count -ne 1)) {
    throw 'Legacy source pin was not replaced cleanly.'
}

Write-Output 'Source pin placement test passed.'
