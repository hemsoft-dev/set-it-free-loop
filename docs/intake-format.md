# sfl: Frontmatter Spec

**Audience**: Maintainers authoring new workflows for the Set it Free Loop library.

---

## Overview

Every workflow in this library includes a standard `sfl:` metadata block alongside the `gh aw`-compatible frontmatter. The `sfl:` block adds only what `gh aw` genuinely lacks — it is **additive and non-breaking** (`gh aw compile` ignores unknown keys).

Fields that duplicate existing `gh aw` capabilities (`on:`, `safe-outputs:`, `permissions:`) are deliberately excluded.

---

## Full Field Reference

```yaml
sfl:
  # ── Lifecycle ────────────────────────────────────────────────────────────────
  status: staging
  # idea | draft | staging | active | retired
  # staging = running on this repo for verification
  # active  = graduated to deployment/, listed in CATALOG.md

  version: "1.0.0"
  # Semver. Increment MINOR on new behavior; MAJOR on breaking changes.
  # Consumers pin to a SHA — version is informational for humans.

  # ── Classification ───────────────────────────────────────────────────────────
  category: quality
  # quality | reporting | intake | security | custom

  risk-class: low
  # trivial | low | medium | high | critical
  # Controls merge authority and retry limits (see deployment/governance/policy.md)

  # ── Trigger metadata ─────────────────────────────────────────────────────────
  target-labels: []
  # Only for label-driven workflows. List the labels this workflow scans for.
  # NOTE: gh aw may add native label-trigger support. If so, retire this field
  # and reference theirs — the sfl: block is always additive, never load-bearing.

  # ── Outcome ──────────────────────────────────────────────────────────────────
  outcome-definition: |
    Human-readable description of what success looks like.
    Include KPIs where measurable.
    Example: "One issue per week surfacing doc drift. KPI: >80% findings are actionable."

  # ── Graduation gate ──────────────────────────────────────────────────────────
  acceptance-criteria:
    - Must run without error on a repo with no prior issues
    - Creates correct output type with correct labels
    - No duplicate outputs on re-run
    # Add workflow-specific criteria here.
    # All criteria must be checked off before graduating from staging → deployment/

  # ── Attribution ──────────────────────────────────────────────────────────────
  source-repo: HemSoft/set-it-free-loop
  # Always set to this. Used by consumers to trace the canonical source.
```

---

## Field Matrix

| Field | Required | gh aw equivalent | Notes |
|-------|----------|-----------------|-------|
| `status` | Yes | None | Lifecycle gating |
| `version` | Yes | None | Human-readable; consumers use SHA |
| `category` | Yes | None | Catalog filtering |
| `risk-class` | Yes | None | Determines merge authority + retry policy |
| `target-labels` | If label-driven | None (for now) | Monitor gh aw for native support |
| `outcome-definition` | Yes | None | Forces explicit success definition |
| `acceptance-criteria` | Yes | None | Graduation gate — never skip |
| `source-repo` | Yes | `source:` (partial) | `source:` uses SHA; this names the repo |

---

## What NOT to add to sfl:

These are already covered by `gh aw` — do not duplicate them:

| Would-be sfl field | Already covered by |
|---|---|
| `trigger-type` | Readable from `on:` |
| `schedule` | Duplicate of `on.schedule.cron` |
| `expected-outputs` | Covered by `safe-outputs:` |
| `safe-write-paths` | Covered by `permissions:` |
| `network-access` | Covered by `network:` |

---

## Minimal Example

```yaml
---
description: |
  Weekly repository audit.

on:
  schedule:
    - cron: "17 14 * * 1"
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

sfl:
  status: staging
  version: "1.0.0"
  category: quality
  risk-class: low
  outcome-definition: |
    One audit issue per week. KPI: >80% of findings are actionable within 2 weeks.
  acceptance-criteria:
    - Runs without error on a repo with no prior issues
    - Creates exactly one issue per run
    - Findings table includes severity and confidence columns
  source-repo: HemSoft/set-it-free-loop
---

# Workflow body starts here...
```

---

## Future-Proofing

The `sfl:` block is designed to shrink over time, not grow. When `gh aw` adds a feature that makes one of our fields redundant:

1. Deprecate the `sfl:` field (add a comment in `format.md`)
2. Bump `version` MINOR in all affected workflows
3. Update `_TEMPLATE.md`
4. Remove the field at the next MAJOR version bump
