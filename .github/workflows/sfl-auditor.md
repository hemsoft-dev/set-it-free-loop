---
description: |
  Runs every 30 minutes (offset 15 min from issue-processor), audits the
  relationship between issue labels and open pull requests, and repairs any
  state discrepancies it finds. Keeps the agentic pipeline self-consistent
  without human intervention.

on:
  schedule:
    - cron: "15 * * * *"
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
    github-app:
      client-id: ${{ vars.SFL_APP_CLIENT_ID }}
      private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}

safe-outputs:
  github-app:
    client-id: ${{ vars.SFL_APP_CLIENT_ID }}
    private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
  noop:
    max: 1
  create-issue:
    title-prefix: "[SFL Auditor] "
    labels: [report, audit]
    max: 1
    deduplicate-by-title: true
  update-issue:
    target: "*"
    max: 10
---

# SFL Auditor

Run every 30 minutes. Audit the relationship between issue labels and open
pull requests. Detect and repair state discrepancies to keep the agentic
pipeline self-consistent. Never modify PR content — only update issues.

## Step 1 — Gather current state

Collect two lists:

**A. In-progress issues**: all open issues that have the label `agent:in-progress`.

**B. Open agent PRs**: all open pull requests (including drafts) whose head
branch name matches the pattern `agent-fix/issue-<number>-<hash>` (e.g.
`agent-fix/issue-5-8508dd6f9171b056`).

For each PR in list B, extract the issue number: it is the digit(s) immediately
after `agent-fix/issue-` and before the next `-`.

**C. Recently merged agent PRs**: all *merged* (closed) pull requests whose head
branch name matches the pattern `agent-fix/issue-<number>-<hash>`, merged within
the last 7 days. For each, extract the issue number the same way as list B.

## Step 2 — Check: orphaned agent:in-progress labels

For each issue in list A, check whether list B contains a PR whose extracted
issue number matches this issue's number.

If NO matching open PR exists for an in-progress issue:

1. Call `update_issue` with ALL of these fields in a **single call**:
   - `issue_number`: the issue number
   - `labels`: the issue's current labels with `agent:in-progress` removed and
     `agent:fixable` added (keep all other existing labels unchanged)
   - `body`: "⚠️ **SFL Auditor**: No open PR found for this issue. Resetting to `agent:fixable` so the issue processor can reclaim it on the next run."
   - `operation`: `"append"`

**IMPORTANT**: Labels and body MUST be in the same `update_issue` call.
Splitting them into two calls causes label changes to be silently dropped.

## Step 3 — Check: conflicting labels

For each open issue that has BOTH `agent:in-progress` AND `agent:fixable`
simultaneously:

1. Call `update_issue` with:
   - `issue_number`: the issue number
   - `labels`: the issue's current labels with `agent:fixable` removed (keep
     `agent:in-progress` and all other existing labels)

## Step 4 — Check: action items missing agent:fixable

Search for open issues that have BOTH `action-item` AND `audit` labels
but do NOT have any of: `agent:fixable`, `agent:in-progress`,
`agent:human-required`, `agent:pause`, `agent:escalated`.

These are action items the repo-audit agent created but forgot to label as
`agent:fixable`, so they will never enter the pipeline.

For each such issue:

1. Call `update_issue` with ALL of these fields in a **single call**:
   - `issue_number`: the issue number
   - `labels`: the issue's current labels with `agent:fixable` added (keep all
     other existing labels unchanged)
   - `body`: "🔧 **SFL Auditor**: This action item was missing `agent:fixable`. Added it so the Issue Processor can claim it on the next cycle."
   - `operation`: `"append"`

## Step 5 — Check: orphaned agent PRs

For each PR in list B, look up the corresponding issue (using the number
extracted in Step 1). If that issue does NOT have `agent:in-progress`:

1. Call `update_issue` with:
   - `issue_number`: the issue number
   - `body`: "ℹ️ **SFL Auditor**: PR #<pr-number> exists on branch `<branch-name>` but this issue is no longer labeled `agent:in-progress`. A human should decide whether to close the PR or restore the label."
   - `operation`: `"append"`

Skip this check if the issue was already processed in Step 2 (to avoid
double-commenting).

## Step 6 — Check: merged PRs with issues left open

For each PR in list C (recently merged agent PRs), look up the corresponding
issue using the extracted issue number. If that issue is still **OPEN**:

1. Call `update_issue` with ALL of these fields in a **single call**:
   - `issue_number`: the issue number
   - `status`: `"closed"`
   - `body`: "✅ **SFL Auditor**: PR #<pr-number> was merged but this issue was left open. Closing it now."
   - `operation`: `"append"`

Skip issues that still have `agent:in-progress` AND a matching open PR in
list B (those are handled by other steps).

## Step 7 — Check: stale report issues

Search for **open** issues that have the label `report` (or both
`daily-status` and `report`) but do NOT have any of: `action-item`,
`agent:fixable`, `agent:in-progress`.

These are pure informational report issues (daily status, audit summaries,
simplisticate summaries). If more than one issue shares the same title prefix
(e.g., multiple `[repo-status]` or `[repo-audit] Daily Repo Audit` issues),
close all but the most recent one using `update_issue` with:

- `issue_number`: the issue number
- `status`: `"closed"`
- `body`: "🧹 **SFL Auditor**: Closing stale report issue — a newer report exists."
- `operation`: `"append"`

## Step 8 — Check: stale unclaimed issues

Search for **open** issues that have the label `agent:fixable` but do NOT have
`agent:in-progress`, `agent:pause`, `agent:human-required`, or `agent:escalated`.

For each such issue, check whether it was created more than **2 hours ago**.

If an `agent:fixable` issue is older than 2 hours and has not been claimed:

1. Call `update_issue` with ALL of these fields in a **single call**:
   - `issue_number`: the issue number
   - `body`: "⏰ **SFL Auditor**: This issue has been `agent:fixable` for over 2 hours without being claimed by the Issue Processor. This may indicate a pipeline stall. A human should investigate whether the Issue Processor is running correctly."
   - `operation`: `"append"`

Only flag each issue **once** — if the issue already has a comment containing
"over 2 hours without being claimed", skip it.

## Step 9 — Check: stalled draft PRs without analyzer reviews

For each PR in list B (open agent PRs), check whether the PR is a **draft**
and was created more than **2 hours ago**.

For each such stale draft PR, determine the required analyzer set from labels:

- `pr-analyzer-general` is always required.
- `pr-analyzer-quality` is required only when the PR has label `pr-analyzer-quality`.
- `pr-analyzer-security` is required only when the PR has label `pr-analyzer-security`.
- `pr-analyzer-testing` is required only when the PR has label `pr-analyzer-testing`.

Then check the PR body for the matching required analyzer markers, for example:
`[MARKER:pr-analyzer-general cycle:`,
`[MARKER:pr-analyzer-quality cycle:`,
`[MARKER:pr-analyzer-security cycle:`,
`[MARKER:pr-analyzer-testing cycle:`.

If ANY required marker is missing, and the PR does NOT already have a comment
containing "missing required analyzer markers", this PR is stalled — the
requested analyzers are not running on it.

Call `update_issue` with:

- `issue_number`: the PR number
- `body`: "⏰ **SFL Auditor**: Draft PR #<pr-number> has been open for over 2 hours and is missing one or more required analyzer markers. The PR Analyzers may not be dispatching for this PR. A human should investigate."
- `operation`: `"append"`

## Step 10 — Check: SFL review prerequisites

Read `sfl.json` from the repository default branch. If its `components` array
does not contain `sfl-pr-review`, skip this check.

If standalone review is declared, verify both prerequisites exist:

1. `.github/workflows/sfl-pr-review.lock.yml` on the default branch
2. The repository label `sfl-review`

If either prerequisite is missing, call `create_issue` once with:

- `title`: `Missing SFL review prerequisites`
- `body`: A checklist naming every missing prerequisite and explaining that
  `deployment/scripts/deploy-workflow.ps1 -Tier review` plus
  `deployment/governance/setup-labels.ps1` restores the feature

The safe output deduplicates this title, so do not create separate issues for
the workflow and label.

## Step 11 — Check: unexplained agent:pause

For each open issue with `agent:pause`, check whether any comment on that
issue contains the words "pause" or "paused" or "agent:pause".

If NO such comment exists:

1. Call `update_issue` with:
   - `issue_number`: the issue number
   - `body`: "🔍 **SFL Auditor**: This issue has `agent:pause` but no explanation comment was found. A human should add a comment explaining the pause, or remove the label to resume processing."
   - `operation`: `"append"`

## Step 12 — Signal completion

After completing all checks (Steps 2–11), you MUST always call at least one of:

- `update_issue` — if any discrepancy was found and repaired (already called above)
- `create_issue` — if SFL review prerequisites are missing
- `noop` — if ALL checks passed and NO discrepancies were found

Never finish the run without calling at least one safe output. A run with zero
safe outputs is treated as a failure.

## Guardrails

- Never modify PR content or labels — only update issues via `update_issue`
- Never remove `agent:human-required` — that label requires explicit human action
- Never add `agent:fixable` to an issue that already has a matching open PR
- Always post the recovery comment in Step 2 even if a previous auditor run already commented — it documents the recurring issue and aids debugging
- If the GitHub API search fails for any step, skip that step and continue with the rest
- At most 10 `update_issue` calls per run (enforced by safe-outputs max)
- Create at most one SFL review prerequisite issue per run
