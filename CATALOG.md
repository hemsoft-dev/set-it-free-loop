# CATALOG.md — Set it Free Loop™ Workflow Library

Workflows listed here have graduated from staging (`.github/workflows/`) and are ready to deploy to consumer repos.

**To deploy a single workflow**:

```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow <name> -Repos "org/repo1,org/repo2"
```

**To deploy a tier**:

```powershell
.\deployment\scripts\deploy-workflow.ps1 -Tier <minimal|standard|full> -Repos "org/repo"
```

---

## Deployment Tiers

| Tier | Workflows | Infrastructure | Use Case |
|------|-----------|---------------|----------|
| **minimal** | daily-repo-status, repo-audit | — | Hygiene-only: daily reports, no AI fixes |
| **standard** | minimal + issue-processor, simplisticate | sfl-dispatcher, sfl-auditor | Detect + claim + fix issues automatically |
| **full** | standard + pr-analyzer-a/b/c, pr-fixer, pr-promoter | sfl-dispatcher, sfl-auditor | Complete autonomous loop with multi-model review |

---

## Available Workflows

| Name | Category | Trigger | Risk | Outcome | Version |
|------|----------|---------|------|---------|---------|
| [daily-repo-status](#daily-repo-status) | reporting | Schedule — daily | trivial | `type:report` issue with daily activity summary | 1.0.0 |
| [repo-audit](#repo-audit) | quality | Schedule — daily | low | `type:report` issue with findings + recommendations | 1.1.0 |
| [issue-processor](#issue-processor) | automation | Dispatched — when `agent:fixable` issues exist | low | Draft PR with scoped fix for the oldest fixable issue | 1.0.0 |
| [simplisticate](#simplisticate) | quality | Schedule — daily | low | Summary report issue + up to 3 `agent:fixable` issues | 1.0.0 |
| [pr-analyzer-a](#pr-analyzer-a) | review | Dispatched — when draft PRs with `agent:pr` exist | trivial | PR comment with full-spectrum review (claude-sonnet-4.6) | 1.0.0 |
| [pr-analyzer-b](#pr-analyzer-b) | review | Dispatched — when draft PRs with `agent:pr` exist | trivial | PR comment with full-spectrum review (gpt-5.3-codex) | 1.0.0 |
| [pr-analyzer-c](#pr-analyzer-c) | review | Dispatched — when draft PRs with `agent:pr` exist | trivial | PR comment with full-spectrum review (claude-opus-4.6) | 1.0.0 |
| [pr-fixer](#pr-fixer) | automation | Dispatched — when all 3 analyzer markers present | low | Implements all analyzer fixes, increments cycle label | 1.0.0 |
| [pr-promoter](#pr-promoter) | automation | Dispatched — when draft PRs have all-PASS verdicts | trivial | Un-drafts PR, adds `human:ready-for-review` label | 1.0.0 |

## Infrastructure Components

| Name | Type | Schedule | Purpose |
|------|------|----------|---------|
| [sfl-dispatcher](#sfl-dispatcher) | Standard YAML | Every 30 min | Gates gh-aw runs; only dispatches when work exists |
| [sfl-auditor](#sfl-auditor) | Standard YAML | :15, :45 every hour | Repairs label/PR state discrepancies |

---

## Workflow Details

### daily-repo-status

**File**: [`deployment/workflows/daily-repo-status.md`](deployment/workflows/daily-repo-status.md)

**What it does**: Creates an upbeat daily status report issue summarizing recent repository activity — commits, PRs, issues, releases.

**Output**: One `type:report` issue per day. Never enters the automation loop.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow daily-repo-status -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Runs without error on repos with no prior issues
- [x] Creates exactly one issue per run
- [x] Labels correctly (`report`, `daily-status`)
- [x] Never sets `type:action-item` — reports are informational only

---

### repo-audit

**File**: [`deployment/workflows/repo-audit.md`](deployment/workflows/repo-audit.md)

**What it does**: Runs a high-signal repository audit daily. Detects documentation drift, stale artifacts, configuration hygiene risks, and cross-reference mismatches.

**Output**: One `type:report` issue per day with an executive summary, findings table (severity + confidence), and prioritized recommendations. A human reviewer can promote findings to `type:action-item` + `agent:fixable` to authorize automated fixes.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow repo-audit -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Runs without error on repos with no prior issues
- [x] Creates exactly one issue per run (no duplicates)
- [x] Findings table includes severity and confidence columns
- [x] Tone is practical and signal-focused

---

### issue-processor

**File**: [`deployment/workflows/issue-processor.md`](deployment/workflows/issue-processor.md)

**What it does**: Claims the oldest `agent:fixable` issue with `risk:trivial` or `risk:low`, implements a targeted fix, and opens a draft PR. Swaps labels from `agent:fixable` → `agent:in-progress` atomically. Branch pattern: `agent-fix/issue-<number>`.

**Output**: One draft PR per run, linked to the claimed issue. PR body includes agent metadata block. Maximum 3 files changed per PR.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow issue-processor -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Claims issue atomically (no two runs claim the same issue)
- [x] PR body includes agent metadata block (idempotency key, risk class)
- [x] Respects safe-write boundaries — skips prohibited paths
- [x] Max 3 file changes per PR

---

### simplisticate

**File**: [`deployment/workflows/simplisticate.md`](deployment/workflows/simplisticate.md)

**What it does**: Runs a daily code simplification audit. Detects deep nesting, long methods, dead code, duplication, and over-engineering. Creates a summary report issue and up to 3 individual `agent:fixable` issues for the highest-impact opportunities.

**Output**: One `type:report` summary issue + up to 3 `type:action-item` issues with `agent:fixable` label.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow simplisticate -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Detects real complexity (not false positives)
- [x] Creates scoped, actionable issues
- [x] Fan-out capped at 3 issues per run
- [x] Summary includes before/after complexity estimates

---

### pr-analyzer-a

**File**: [`deployment/workflows/pr-analyzer-a.md`](deployment/workflows/pr-analyzer-a.md)

**What it does**: Full-spectrum PR review using **claude-sonnet-4.6**. Reviews all dimensions: correctness, security, performance, style, and best practices. Posts a structured comment with BLOCKING/NON-BLOCKING findings and a PASS or BLOCKING ISSUES FOUND verdict.

**Output**: PR comment with `[MARKER:pr-analyzer-a cycle:N]` idempotency marker.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow pr-analyzer-a -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Reviews all PR dimensions
- [x] Structured output with severity levels
- [x] Idempotency via marker comments
- [x] Never blocks human-authored PRs

---

### pr-analyzer-b

**File**: [`deployment/workflows/pr-analyzer-b.md`](deployment/workflows/pr-analyzer-b.md)

**What it does**: Same as pr-analyzer-a but uses **gpt-5.3-codex**. Provides a second independent review perspective.

**Output**: PR comment with `[MARKER:pr-analyzer-b cycle:N]` idempotency marker.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow pr-analyzer-b -Repos "org/your-repo"
```

---

### pr-analyzer-c

**File**: [`deployment/workflows/pr-analyzer-c.md`](deployment/workflows/pr-analyzer-c.md)

**What it does**: Same as pr-analyzer-a but uses **claude-opus-4.6**. Provides a third independent review perspective — the most capable model for nuanced analysis.

**Output**: PR comment with `[MARKER:pr-analyzer-c cycle:N]` idempotency marker.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow pr-analyzer-c -Repos "org/your-repo"
```

---

### pr-fixer

**File**: [`deployment/workflows/pr-fixer.md`](deployment/workflows/pr-fixer.md)

**What it does**: Reads all 3 analyzer comments, implements every BLOCKING and NON-BLOCKING fix, pushes the changes, and increments the cycle label (`pr:cycle-1` → `pr:cycle-2` → `pr:cycle-3`). At cycle 3, escalates with `agent:human-required` instead of continuing.

**Output**: Committed fixes on the PR branch + `[MARKER:pr-fixer cycle:N]` comment. Does NOT un-draft the PR.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow pr-fixer -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Requires all 3 analyzer markers before running
- [x] Implements all findings (not just blocking)
- [x] Cycle label management is correct
- [x] Escalates at cycle 3

---

### pr-promoter

**File**: [`deployment/workflows/pr-promoter.md`](deployment/workflows/pr-promoter.md)

**What it does**: Converts clean draft PRs to ready-for-review after verifying all 3 analyzers posted PASS verdicts. Uses `gh pr ready` to un-draft, then adds `human:ready-for-review` label.

**Output**: PR transitions from draft → ready for review. `[MARKER:pr-promoter cycle:C]` comment posted.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow pr-promoter -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Verifies all 3 PASS verdicts before promoting
- [x] Confirms draft state actually changed
- [x] Adds `human:ready-for-review` label
- [x] Idempotent — won't re-promote already-promoted PRs

---

## Infrastructure Details

### sfl-dispatcher

**File**: [`deployment/infrastructure/sfl-dispatcher.yml`](deployment/infrastructure/sfl-dispatcher.yml)

**What it does**: Standard YAML workflow that gates gh-aw workflow runs. Before dispatching any workflow, it checks whether work actually exists (e.g., are there `agent:fixable` issues? Are there draft PRs?). This eliminates ~240 no-op Copilot inference calls per day.

**Schedule**: Every 30 minutes.

**Dispatches**:
- `issue-processor` — when `agent:fixable` issues exist
- `pr-analyzer-a/b/c` — when draft PRs with `agent:pr` label exist
- `pr-fixer` — when all 3 analyzer markers are present on a PR
- `pr-promoter` — when draft or approved PRs exist

---

### sfl-auditor

**File**: [`deployment/infrastructure/sfl-auditor.yml`](deployment/infrastructure/sfl-auditor.yml)

**What it does**: Standard YAML workflow that detects and repairs label/PR state discrepancies. Runs at :15 and :45 every hour (offset from dispatcher to avoid overlap).

**Checks**:
- Orphaned `agent:in-progress` issues (no matching `agent-fix/` PR)
- Conflicting labels (both `agent:in-progress` and `agent:fixable`)
- Orphaned agent PRs (PR exists but linked issue lacks `agent:in-progress`)
- Unexplained `agent:pause` labels

---

## Governance

Before deploying any workflow, run the label setup script once on the target repo:

```powershell
.\deployment\governance\setup-labels.ps1 -Owner <org> -Repo <repo>
```

See [`deployment/governance/policy.md`](deployment/governance/policy.md) for the full label taxonomy, retry limits, merge authority matrix, and escalation policy.
