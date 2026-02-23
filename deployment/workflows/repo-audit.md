---
description: |
  This workflow runs a daily repository audit to detect documentation drift,
  stale artifacts, configuration hygiene issues, and cross-reference mismatches.

on:
  schedule: daily
  workflow_dispatch:

permissions:
  contents: read
  issues: read
  pull-requests: read

network: defaults

tools:
  github:
    lockdown: false

safe-outputs:
  create-issue:
    title-prefix: "[repo-audit] "
    labels: [type:report]
---

<!-- sfl:
  status: active
  version: "1.1.0"
  category: quality
  risk-class: low
  outcome-definition: |
    One audit issue per day surfacing documentation drift, stale artifacts,
    and configuration hygiene risks with prioritized recommendations.
    KPI: >80% of findings are actionable within 2 weeks of creation.
  acceptance-criteria:
    - Runs without error on a repo with no prior issues
    - Creates exactly one issue per run (no duplicates on re-run)
    - Findings table includes severity and confidence columns
    - Tone is practical and signal-focused; avoids speculative findings
  source-repo: HemSoft/set-it-free-loop
-->

# Repo Audit

Run a high-signal repository audit and publish exactly one actionable weekly report issue.

## Goals

- Detect documentation vs implementation drift
- Surface stale/dead artifacts and outdated references
- Identify configuration or dependency hygiene risks
- Recommend small, prioritized next actions

## Audit Scope

1. Documentation Drift
   - README/docs structure and claims vs actual files and behavior
   - Broken or outdated internal references

2. Configuration Hygiene
   - Config/env keys that appear unused or mismatched
   - Potentially stale scripts, settings, or dependency declarations

3. Artifact Staleness
   - Deprecated, orphaned, or no-longer-relevant files/folders

4. Cross-Reference Accuracy
   - Mismatches between type/config docs and real usage patterns

## Output Requirements

- Create one GitHub issue with:
  - Executive summary (overall repo health)
  - Findings table with severity and confidence
  - Recommended actions labeled "Do now", "Do next", "Later"
  - A short "No action required" section if everything looks healthy

- Keep tone concise and practical.
- Prioritize signal over volume; avoid speculative findings.

## Process

1. Inspect repository structure and key docs
2. Cross-check docs/config claims against implementation
3. Compile findings with severity and confidence
4. Create one audit issue via safe output tool
