# CATALOG.md — Set it Free Loop™ Workflow Library

Workflows listed here have graduated from staging (`.github/workflows/`) and are ready to deploy to consumer repos.

**To deploy**: `.\deployment\scripts\deploy-workflow.ps1 -Workflow <name> -Repos "org/repo1,org/repo2"`

---

## Available Workflows

| Name | Category | Trigger | Risk | Outcome | Version |
|------|----------|---------|------|---------|---------|
| [daily-repo-status](#daily-repo-status) | reporting | Schedule — daily | trivial | `type:report` issue with daily activity summary | 1.0.0 |
| [weekly-repo-audit](#weekly-repo-audit) | quality | Schedule — Mondays 14:17 UTC | low | `type:report` issue with findings + recommendations | 1.0.0 |

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

### weekly-repo-audit

**File**: [`deployment/workflows/weekly-repo-audit.md`](deployment/workflows/weekly-repo-audit.md)

**What it does**: Runs a high-signal repository audit every Monday. Detects documentation drift, stale artifacts, configuration hygiene risks, and cross-reference mismatches.

**Output**: One `type:report` issue per week with an executive summary, findings table (severity + confidence), and prioritized recommendations. A human reviewer can promote findings to `type:action-item` + `agent:fixable` to authorize automated fixes.

**Deploy command**:
```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow weekly-repo-audit -Repos "org/your-repo"
```

**Acceptance criteria met**:
- [x] Runs without error on repos with no prior issues
- [x] Creates exactly one issue per run (no duplicates)
- [x] Findings table includes severity and confidence columns
- [x] Tone is practical and signal-focused

---

## Governance

Before deploying any workflow, run the label setup script once on the target repo:

```powershell
.\deployment\governance\setup-labels.ps1 -Owner <org> -Repo <repo>
```

See [`deployment/governance/policy.md`](deployment/governance/policy.md) for the full label taxonomy, retry limits, merge authority matrix, and escalation policy.
