---
description: |
  This workflow runs a daily code simplification audit to identify unnecessary
  complexity, over-engineering, dead code, and opportunities for targeted
  simplification. It creates one summary report issue and individual
  agent-fixable issues for low-risk simplifications an agent can apply autonomously.

on:
  schedule: "0 6 * * *"   # 1:00 AM EST
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
    labels: [report, audit]
    max: 10
  update-issue:
    target: "*"
    max: 5
---

# Daily Simplisticate Audit

> *"Perfection is achieved not when there is nothing more to add, but when there is nothing left to take away."* — Antoine de Saint-Exupéry

Run a high-signal daily code simplification audit. Produce a summary report
issue and individual fixable issues for simplifications an agent can apply
autonomously.

## Step 0 — Close previous simplisticate summary reports

Before creating today's audit, search for all **open** issues whose title
starts with `[simplisticate] Daily Simplisticate Audit` AND that have both
`report` and `audit` labels. For each one found, close it using
`update_issue` with:

- `issue_number`: the issue number
- `status`: `"closed"`

Do NOT close agent-fixable action-item issues — only the dated summary reports.

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

## Audit Scope

1. **Dead Code & Unused Exports**
   - Unused variables, functions, imports, and type exports
   - Files that are never referenced from any other file
   - Config keys or env vars that appear unused

2. **Over-Engineering**
   - Abstractions with only one implementation
   - Wrapper functions that add no value
   - Unnecessary indirection layers
   - Premature generalization

3. **Duplication**
   - Repeated logic blocks across files
   - Copy-paste patterns that should be consolidated
   - Near-identical utility functions

4. **Excessive Complexity**
   - Deeply nested control flow (>3 levels)
   - Functions with too many responsibilities
   - Complex boolean expressions that could be simplified
   - Long parameter lists

5. **Stale Patterns**
   - Legacy patterns when modern alternatives exist
   - Verbose code that can leverage newer language/framework features
   - Unnecessary defensive coding against impossible scenarios

## Output Requirements

### Summary issue (always)

Create one summary issue with labels `report` and `audit` containing:

- Executive summary (overall code simplicity health)
- Findings table with:
  - Location (file + line range)
  - Complexity signal type
  - Severity (low / medium / high)
  - Risk of simplification (🟢 Low / 🟡 Medium / 🔴 High)
  - Whether the finding is agent-fixable
- A short "No action required" section if everything looks clean

### Per-finding issues (for agent-fixable findings only)

For each finding that meets ALL of the following criteria, create a separate issue:

- The simplification is **scoped to one or two files** — no broad refactors
- The simplification is **deterministic** — there is one clear correct outcome
- Risk is 🟢 Low (`risk:trivial` or `risk:low`)
- No user-facing behavioral change
- The resulting code is strictly simpler (fewer lines, fewer branches, fewer abstractions)

Label each agent-fixable issue with: `action-item`, `agent:fixable`, and the appropriate risk label (`risk:trivial` or `risk:low`).

Issue title format: `[simplisticate] <short description of the specific simplification>`

Issue body must include:

- **Finding**: What complexity was detected and where (file path, line range)
- **Complexity Signal**: Which signal type from the table above
- **Proposed Simplification**: Exactly what change to make
- **Before/After**: Brief code sketch showing the improvement
- **Acceptance Criteria**: How to verify the simplification is correct
- **Risk**: `risk:trivial` or `risk:low` with justification

Do NOT create agent-fixable issues for:

- Findings requiring architectural decisions
- Findings that touch more than 3 files
- Anything with 🟡 Medium or 🔴 High risk
- Simplifications where multiple valid approaches exist
- Changes that would alter external behavior

Cap total agent-fixable issues at 3 per run to avoid noise.

## Process

1. Inspect repository file structure and identify key source directories
2. Scan source files for complexity signals
3. Cross-reference findings with test coverage and usage patterns
4. Assess risk of each potential simplification
5. Compile findings with severity, risk, and agent-fixability assessment
6. Create summary report issue
7. For each qualifying finding, create a scoped agent-fixable issue
