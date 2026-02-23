---
description: |
  PR Promoter — converts clean draft PRs to ready-for-review. If all three
  analyzers found zero blocking issues (PASS verdict) in the current cycle,
  un-drafts the PR and posts a promotion comment. Also merges approved PRs
  per the Merge Authority Matrix. Processes one PR per run.

on:
  workflow_dispatch:

permissions:
  contents: read
  issues: read
  pull-requests: read

engine:
  id: copilot
  model: claude-sonnet-4.6

network: defaults

tools:
  github:
    lockdown: false

safe-outputs:
  noop:
    max: 1
  update-issue:
    target: "*"
    max: 3
---

<!-- sfl:
  status: active
  version: "1.0.0"
  category: quality
  risk-class: low
  target-labels: [agent:pr, human:ready-for-review]
  outcome-definition: |
    Converts clean draft PRs (all 3 analyzers PASS) to ready-for-review.
    Merges approved PRs via squash merge + branch deletion.
    KPI: 100% of promoted PRs have all-PASS verdicts.
  acceptance-criteria:
    - Promotes exactly one PR per run
    - Only promotes when all 3 analyzer verdicts are PASS
    - Uses gh pr ready for draft → non-draft transition
    - Verifies draft state actually changed before applying labels
    - Posts structured promotion comment with idempotency marker
    - Never promotes a PR with blocking issues
  source-repo: HemSoft/set-it-free-loop
-->

# PR Promoter

Find the oldest draft PR labeled `agent:pr` where all three analyzer verdicts
are **PASS** in the current cycle. Convert from draft to ready-for-review and
post a promotion comment. Process exactly one PR per run.

## Step 1 — Find the target PR

Search for open draft PRs with `agent:pr` and without `agent:human-required`.
Take the single oldest. If none, call `noop` and exit.

## Step 2 — Determine the current review cycle

Check labels for `pr:cycle-N`. No label = cycle 0.

## Step 3 — Check if already promoted

If PR is non-draft and has a promoter marker, call `noop` and exit.
If PR is still draft but has a marker, continue (retry promotion).

## Step 4 — Verify all three analyzers have reviewed

Find the correct cycle to check (accounting for fixer cycle increment).
All three markers must be present:

- `[MARKER:pr-analyzer-a cycle:C]`
- `[MARKER:pr-analyzer-b cycle:C]`
- `[MARKER:pr-analyzer-c cycle:C]`

If any missing, call `noop` and exit.

## Step 5 — Check all analyzer verdicts

All three must say `**PASS**`. If any says `**BLOCKING ISSUES FOUND**`,
call `noop` and exit (PR Fixer will handle it).

## Step 6 — Authenticate GitHub CLI

```bash
export GH_TOKEN="${GITHUB_TOKEN:-$COPILOT_GITHUB_TOKEN}"
gh auth status
```

If auth fails, call `noop` and exit.

## Step 7 — Convert PR to ready-for-review

```bash
gh pr ready <number>
```

## Step 8 — Verify draft state changed

Re-read PR state. If still draft, call `noop` and exit.

## Step 9 — Post the promotion comment

```markdown
[MARKER:pr-promoter cycle:C]
## ✅ PR Promoter — Ready for Human Review

**Cycle**: C
**PR**: #<number>
**Linked Issue**: #<issue-number>

### Analyzer Verdicts

| Analyzer | Verdict |
|----------|---------|
| A | **PASS** |
| B | **PASS** |
| C | **PASS** |

### Summary

All three analyzers found zero blocking issues. This PR has been converted
from draft to ready-for-review.

**Next step**: Human review and merge.
```

## Step 10 — Update labels

Add `human:ready-for-review` to the PR. Remove `agent:promoted` if present.

Only valid after Step 8 confirms PR is non-draft.

## Guardrails

- Promote exactly ONE PR per run
- For every skip path, call `noop`
- Never modify PR code, title, or body content
- Never close or merge the PR — only draft → ready-for-review
- Never apply `human:ready-for-review` to a draft PR
- If `gh pr ready` fails, call `noop` and exit
