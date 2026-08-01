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
# Deployed from: HemSoft/set-it-free-loop/deployment/workflows/test.md@abc123
# To upgrade: re-run deploy-workflow.ps1 at the desired SHA
'@

$result = Add-SflSourcePin -Content $workflow -PinComment $pin

if ($result -notmatch '\A---\r?\n') {
    throw 'Source pin was inserted before workflow frontmatter.'
}

if ($result -notmatch '(?s)\A---\r?\n.*?\r?\n---\r?\n# Deployed from:') {
    throw 'Source pin was not inserted immediately after workflow frontmatter.'
}

Write-Output 'Source pin placement test passed.'
