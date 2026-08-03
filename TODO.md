# TODO.md — Set it Free Loop™ Ideas Pipeline

These are workflows we want to build and graduate to [CATALOG.md](CATALOG.md).

Items move from here → `.github/workflows/` (staging) → `deployment/workflows/` (production) → CATALOG.md entry.

---

## Resume Work — Quick Start

When switching back to this repo, follow these steps to verify everything works and continue where you left off.

### 1. Verify the repo is up-to-date

```powershell
cd D:\github\HemSoft\set-it-free-loop
git pull origin main
```

### 2. Verify workflows are running on GitHub

Check that the SFL Dispatcher and SFL Auditor are active on the **set-it-free-loop** repo itself (dogfooding):

```powershell
# Check recent workflow runs
gh run list --repo HemSoft/set-it-free-loop --limit 10

# Check specific infrastructure workflows
gh run list --repo HemSoft/set-it-free-loop --workflow "SFL Dispatcher" --limit 5
gh run list --repo HemSoft/set-it-free-loop --workflow "SFL Auditor" --limit 5
```

If no runs appear, the workflows may not have been triggered yet (cron-based). Trigger them manually:

```powershell
gh workflow run sfl-dispatcher.yml --repo HemSoft/set-it-free-loop
gh workflow run sfl-auditor.yml --repo HemSoft/set-it-free-loop
```

### 3. Verify the SFL GitHub App credentials exist

Both `sfl-dispatcher.yml` and `sfl-auditor.yml` mint a short-lived
installation token from the SFL GitHub App. Because `HemSoft` is a user account
rather than a GitHub organization, configure these per repository:

```powershell
gh variable list --repo HemSoft/set-it-free-loop
gh secret list --repo HemSoft/set-it-free-loop --app actions
```

Required entries:

- Variable: `SFL_APP_ID`
- Secret: `SFL_APP_PRIVATE_KEY`

For gh-aw safe outputs, also configure:

- Variable: `SFL_APP_CLIENT_ID`

To set these entries for HemSoft repositories after creating the GitHub App:

```powershell
.\deployment\scripts\set-sfl-github-app-credentials.ps1 `
  -Repos HemSoft/set-it-free-loop,HemSoft/hs-buddy `
  -AppId <app-id> `
  -ClientId <client-id> `
  -PrivateKeyPath <path-to-private-key.pem>
```

### 4. Verify governance labels are set up

```powershell
# Run the label setup script against THIS repo (safe to re-run — idempotent)
.\deployment\governance\setup-labels.ps1 -Owner HemSoft -Repo set-it-free-loop
```

### 5. Test the deploy script (dry run)

Confirm the deployment tool works against hs-buddy (or any consumer repo) without making changes:

```powershell
.\deployment\scripts\deploy-workflow.ps1 -Tier full -Repos "HemSoft/hs-buddy" -DryRun
```

### 6. Verify gh-aw workflow compilation (if touching .md workflows)

Each `.md` workflow in `.github/workflows/` and `deployment/workflows/` compiles via `gh aw`:

```powershell
# Test a single workflow
gh aw compile .github/workflows/repo-audit.md

# Test all .md workflows in the repo
Get-ChildItem .github\workflows\*.md | ForEach-Object {
    Write-Host "Compiling $($_.Name)..."
    gh aw compile $_.FullName
    if ($LASTEXITCODE -ne 0) { Write-Host "FAILED: $($_.Name)" -ForegroundColor Red }
    else { Write-Host "OK" -ForegroundColor Green }
}
```

### 7. Check for open issues / PRs in the pipeline

```powershell
# Any agent:fixable issues waiting for processing?
gh issue list --repo HemSoft/set-it-free-loop --label "agent:fixable" --state open

# Any agent:in-progress issues with open PRs?
gh issue list --repo HemSoft/set-it-free-loop --label "agent:in-progress" --state open

# Any open agent PRs?
gh pr list --repo HemSoft/set-it-free-loop --state open --head "agent-fix/"
```

### 8. What to work on next

See the [Ideas Pipeline](#ideas-pipeline) below for upcoming workflows to build.

Key priorities:
- **feature-intake-normalizer** (medium) — normalize external requests into GitHub Issues
- **loop-cost-reporter** (medium) — monthly telemetry on SFL run costs / budget burn
- **onboarding-health-check** (low) — verify consumer repos have governance artifacts

Also consider:
- Adding more consumer repos via `deploy-workflow.ps1 -Tier standard -Repos "org/repo"`
- Reviewing and improving existing workflow prompts based on real-world run results
- Publishing 30-day pilot metrics from hs-buddy back to this repo

---

## Graduated ✅

These items have been built, proven in hs-buddy, and graduated to the deployment library.

| Name | Graduated As | Version | Status |
|------|-------------|---------|--------|
| issue-to-pr-fixer | [issue-processor](deployment/workflows/issue-processor.md) | 1.0.0 | Active — in CATALOG |
| pr-quality-analyzer | [pr-analyzer-general](deployment/workflows/pr-analyzer-general.md), [pr-analyzer-quality](deployment/workflows/pr-analyzer-quality.md), [pr-analyzer-security](deployment/workflows/pr-analyzer-security.md), [pr-analyzer-testing](deployment/workflows/pr-analyzer-testing.md) | 1.1.0 | Active — in CATALOG |
| *(new)* | [pr-fixer](deployment/workflows/pr-fixer.md) | 1.0.0 | Active — in CATALOG |
| *(new)* | [pr-promoter](deployment/workflows/pr-promoter.md) | 1.0.0 | Active — in CATALOG |
| *(new)* | [simplisticate](deployment/workflows/simplisticate.md) | 1.0.0 | Active — in CATALOG |

---

## Ideas Pipeline

| Priority | Name | Category | Trigger | Outcome Goal | Notes |
|----------|------|----------|---------|-------------|-------|
| 🟡 Medium | feature-intake-normalizer | intake | Schedule — daily | Normalize Jira/GitHub → labeled `type:action-item` issues | Builds on `convex/featureIntakes.ts` pattern |
| 🟡 Medium | loop-cost-reporter | telemetry | Schedule — monthly | Issue with run counts, p50/p90 cost, monthly budget burn | KPI visibility for portfolio-level SFL adoption |
| 🟢 Low | onboarding-health-check | quality | Schedule — weekly | Report flagging missing governance artifacts (labels, opt-out file, CATALOG reference) | Consumer repo readiness gate |

---

## Backlog Detail

### feature-intake-normalizer

**Goal**: Pull unprocessed requests from external systems (Jira, GitHub Issues) and normalize them into labeled `type:action-item` issues with acceptance criteria and risk class.

**Design constraints**:
- Idempotency key per source+externalId (no duplicates)
- Requires acceptance criteria before auto-implementation eligibility
- Auto-routes ambiguous requests to `agent:human-required`

**Acceptance criteria**:
- [ ] Deduplicates by canonical key (no double-creation on re-run)
- [ ] Normalized issue body includes all required fields (problem, outcome, AC, risk class)
- [ ] Fan-out cap: max 5 new issues per run

---

## Graduation Checklist

Before moving a workflow from staging to `deployment/`:

- [ ] Ran successfully at least 3 times on this repo without errors
- [ ] Created correct output type (issue/PR/comment) with correct labels
- [ ] `sfl.status` updated from `staging` → `active`
- [ ] `sfl.acceptance-criteria` all checked off
- [ ] Entry added to CATALOG.md
- [ ] `_TEMPLATE.md` used as starting point (no manual frontmatter inconsistencies)
