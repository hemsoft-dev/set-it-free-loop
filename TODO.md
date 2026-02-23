# TODO.md — Set it Free Loop™ Ideas Pipeline

These are workflows we want to build and graduate to [CATALOG.md](CATALOG.md).

Items move from here → `.github/workflows/` (staging) → `deployment/workflows/` (production) → CATALOG.md entry.

---

## Graduated ✅

These items have been built, proven in hs-buddy, and graduated to the deployment library.

| Name | Graduated As | Version | Status |
|------|-------------|---------|--------|
| issue-to-pr-fixer | [issue-processor](deployment/workflows/issue-processor.md) | 1.0.0 | Active — in CATALOG |
| pr-quality-analyzer | [pr-analyzer-a](deployment/workflows/pr-analyzer-a.md), [pr-analyzer-b](deployment/workflows/pr-analyzer-b.md), [pr-analyzer-c](deployment/workflows/pr-analyzer-c.md) | 1.0.0 | Active — in CATALOG |
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
