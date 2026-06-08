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

engine:
  id: codex
  model: gpt-5.5?effort=high

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
  status: staging
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

### Per-finding issues (grouped by category)

**Group findings by category and fix pattern.** Do NOT create one issue per
individual finding. Instead, create one issue per group of related,
mechanically similar fixes that share the same remediation approach.

For example:
- 50 "add `role` attribute" a11y warnings → ONE issue: "Fix accessibility roles"
- 12 unused exports across 6 files → ONE issue: "Remove unused exports"
- 3 `useState` lazy init fixes → ONE issue: "Fix useState lazy initialization"

Each grouped issue must meet ALL of the following criteria:

- Every fix in the group uses the **same mechanical pattern**
- Each individual fix is **deterministic** — one clear correct outcome
- Risk class is `risk:trivial` or `risk:low`
- No user-facing behavioral change is required

Label each agent-fixable issue with: `type:action-item`, `agent:fixable`, and
the appropriate risk label (`risk:trivial` or `risk:low`).

Issue title format: `[repo-audit] <short description of the group>`

Issue body must include:

- **Category**: The shared finding category
- **Pattern**: The common fix pattern
- **Affected files**: A checklist of every file and line
- **Acceptance criteria**: How to verify the group of fixes is correct
- **Risk**: `risk:trivial` or `risk:low` with justification

Do NOT create agent-fixable issues for:

- Findings requiring architectural decisions
- Findings where multiple valid fixes exist per instance
- Anything with risk:medium or higher
- Groups that would touch more than 15 files (split into smaller groups)

Cap total agent-fixable issues at 3 per run.

- Keep tone concise and practical.
- Prioritize signal over volume; avoid speculative findings.

## Process

1. Inspect repository structure and key docs
2. Cross-check docs/config claims against implementation
3. Compile findings with severity and confidence
4. Create one audit issue via safe output tool
