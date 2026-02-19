# TODO.md — Set it Free Loop™ Ideas Pipeline

These are workflows we want to build and graduate to [CATALOG.md](CATALOG.md).

Items move from here → `.github/workflows/` (staging) → `deployment/workflows/` (production) → CATALOG.md entry.

---

## Ideas Pipeline

| Priority | Name | Category | Trigger | Outcome Goal | Notes |
|----------|------|----------|---------|-------------|-------|
| 🔴 High | issue-to-pr-fixer | quality | Label `agent:fixable` | Auto-generate scoped PRs for low-risk issues | Core loop closer; requires safe-write-paths definition |
| 🔴 High | pr-quality-analyzer | quality | PR opened/synchronized | Severity gate comment; fail gate if critical/high above threshold | Enables auto-merge for trivial/low risk class |
| 🟡 Medium | feature-intake-normalizer | intake | Schedule — daily | Normalize Jira/GitHub → labeled `type:action-item` issues | Builds on `convex/featureIntakes.ts` pattern |
| 🟡 Medium | loop-cost-reporter | telemetry | Schedule — monthly | Issue with run counts, p50/p90 cost, monthly budget burn | KPI visibility for portfolio-level SFL adoption |
| 🟢 Low | onboarding-health-check | quality | Schedule — weekly | Report flagging missing governance artifacts (labels, opt-out file, CATALOG reference) | Consumer repo readiness gate |

---

## Backlog Detail

### issue-to-pr-fixer

**Goal**: When an issue carries `agent:fixable`, claim it, generate a focused fix branch, and open a PR.

**Design constraints**:
- Safe-write paths allowlist (never touches auth, payments, CI config)
- Risk class drives max diff size (≤ 300 lines for `risk:low`, ≤ 150 for `risk:medium`)
- Sets `agent:in-progress` on claim; `agent:review-requested` on PR open
- Max 2 retries for `risk:low`, 1 for `risk:medium`

**Acceptance criteria**:
- [ ] Claims issue atomically (no two runs claim the same issue)
- [ ] PR body includes agent metadata block (idempotency key, risk class, safe-write verification)
- [ ] Fails safely with `agent:pause` comment when prohibited paths are implicated
- [ ] Tested on a synthetic issue in this repo before graduating to deployment/

---

### pr-quality-analyzer

**Goal**: Assess every PR for severity/risk and enforce merge gates.

**Design constraints**:
- Posts findings as a PR review comment, not a separate issue
- Fails the check if critical/high findings exceed threshold
- `risk:trivial` and `risk:low` can auto-merge if CI is green

**Acceptance criteria**:
- [ ] Runs on all PRs opened against main
- [ ] Comment includes: risk class, finding count by severity, merge recommendation
- [ ] Required check integration (blocks merge on failure)
- [ ] Does not block human-authored PRs — analysis only, not veto power

---

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
