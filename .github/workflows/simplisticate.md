---
description: |
  Daily code simplification audit to identify unnecessary complexity,
  over-engineering, dead code, and opportunities for targeted simplification.
  Creates summary report and individual agent-fixable issues for low-risk
  simplifications.

on:
  schedule: "0 6 * * *"
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
    title-prefix: "[simplisticate] "
    labels: [type:report, audit]
    max: 10
---

<!-- sfl:
  status: active
  version: "1.0.0"
  category: quality
  risk-class: low
  outcome-definition: |
    One summary report issue per run plus up to 3 agent-fixable issues for
    low-risk simplifications. KPI: >70% of agent-fixable findings are merged
    without human rewrites.
  acceptance-criteria:
    - Creates one summary report issue per run
    - Agent-fixable issues capped at 3 per run
    - Only creates agent-fixable issues for risk:trivial or risk:low
    - Agent-fixable issues limited to 1-2 files each
    - Findings table includes severity, risk, and agent-fixability
  source-repo: HemSoft/set-it-free-loop
-->

# Daily Simplisticate Audit

> *"Perfection is achieved not when there is nothing more to add, but when
> there is nothing left to take away."* — Antoine de Saint-Exupéry

Run a high-signal daily code simplification audit. Produce a summary report
issue and individual fixable issues for simplifications an agent can apply
autonomously.

## Goals

- Identify unnecessary complexity that can be safely reduced
- Surface dead code, unused abstractions, and over-engineering
- Find duplicated logic that can be consolidated
- Recommend small, targeted simplifications with risk assessment

## Complexity Signals to Detect

| Signal | Description |
|--------|-------------|
| Deep nesting | >3 levels of indentation |
| Long methods | Functions >30 lines |
| Too many parameters | 4+ parameters |
| Excessive abstractions | Interfaces with single implementations |
| Duplicated logic | Similar code in multiple places |
| Complex conditionals | Nested if/else, long switch statements |
| Over-engineering | Patterns where simpler solutions exist |
| Dead code | Unused variables, methods, imports, files |
| Tangled dependencies | Circular or convoluted dependency chains |
| Magic values | Hardcoded numbers/strings without explanation |

## Output Requirements

### Summary issue (always)

Create one summary issue with labels `type:report` and `audit`:

- Executive summary (overall code simplicity health)
- Findings table with location, signal type, severity, risk, agent-fixability
- "No action required" section if everything looks clean

### Per-finding issues (for agent-fixable findings only)

Create separate issues for findings that are:

- Scoped to 1-2 files
- Deterministic (one clear correct outcome)
- Risk: `risk:trivial` or `risk:low`
- No user-facing behavioral change

Label each: `type:action-item`, `agent:fixable`, appropriate risk label.
Cap at 3 per run.

Issue body must include: Finding, Complexity Signal, Proposed Simplification,
Before/After code sketch, Acceptance Criteria, Risk justification.

## Process

1. Inspect repository file structure
2. Scan source files for complexity signals
3. Cross-reference findings with usage patterns
4. Assess risk of each simplification
5. Create summary report issue
6. For qualifying findings, create scoped agent-fixable issues
