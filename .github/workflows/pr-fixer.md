---
description: |
  PR Fixer — reads all analyzer review comments on a draft PR labeled agent:pr,
  implements all blocking and non-blocking fixes, commits to the PR branch,
  increments the cycle label, and exits. Does NOT un-draft the PR.

on:
  workflow_dispatch:

permissions:
  contents: read
  issues: read
  pull-requests: read

engine:
  id: copilot
  model: claude-opus-4.6

network: defaults

tools:
  github:
    lockdown: false

safe-outputs:
  create-pull-request:
    labels: [agent:pr, type:fix]
    draft: true
  update-issue:
    target: "*"
    max: 5
  noop:
    max: 1
---

<!-- sfl:
  status: active
  version: "1.0.0"
  category: quality
  risk-class: low
  target-labels: [agent:pr]
  outcome-definition: |
    Implements all blocking and non-blocking fixes from the three analyzer
    reviews, commits to the PR branch, and increments the cycle counter.
    KPI: >80% of fixes resolve the stated finding without regressions.
  acceptance-criteria:
    - Processes exactly one PR per run
    - Waits for all 3 analyzer markers before acting
    - Implements both blocking and non-blocking fixes
    - Increments cycle label after fixing
    - Posts structured fix summary with idempotency marker
    - Escalates to agent:human-required at cycle 3
    - Never un-drafts the PR
  source-repo: HemSoft/set-it-free-loop
-->

# PR Fixer

Find the oldest draft PR labeled `agent:pr` whose current cycle has all three
analyzer reviews posted. Read every finding, implement all fixes, commit to
the PR branch, increment the cycle label, and post a structured fix summary.
Process exactly one PR per run.

**You do NOT un-draft the PR.** That is the PR Promoter's responsibility.

## Step 1 — Find the target PR

Search for open draft PRs with `agent:pr` and without `agent:human-required`.
Take the single oldest. If none, call `noop` and exit.

## Step 2 — Determine the current review cycle

Check labels for `pr:cycle-N` (N = 1, 2, or 3). No label = cycle 0.

If cycle 3: escalate by adding `agent:human-required`, post explanation, exit.

## Step 3 — Verify all three analyzers have reviewed

Search PR body for all three markers at the current cycle:

- `[MARKER:pr-analyzer-a cycle:N]`
- `[MARKER:pr-analyzer-b cycle:N]`
- `[MARKER:pr-analyzer-c cycle:N]`

If any missing, call `noop` and exit.

## Step 4 — Check if already fixed in this cycle

Search for `[MARKER:pr-fixer cycle:N]`. If found, call `noop` and exit.

## Step 5 — Parse all analyzer findings

Extract all blocking issues and non-blocking suggestions from all three
analyzer comments. If ALL three verdicts say `**PASS**`, call `noop` and exit.

## Step 6 — Read the PR content and codebase

Read PR description, linked issue, diff, and full file contents.

## Step 7 — Check out the PR branch and rebase onto main

```bash
git fetch origin main <head-branch-name>
git checkout <head-branch-name>
```

### Rebase onto latest main

Before implementing any fixes, rebase the branch onto the latest `main`:

```bash
git rebase origin/main
```

If the rebase completes cleanly, continue to Step 8.

### Handling rebase conflicts

If `git rebase` fails with merge conflicts:

1. **Read each conflicting file** — look for `<<<<<<<`, `=======`, `>>>>>>>` markers
2. **Resolve the conflicts** — for `risk:trivial` / `risk:low` fixes, the
   resolution is usually straightforward
3. **Stage resolved files** and continue:
   ```bash
   git add <resolved-file>
   git rebase --continue
   ```
4. Repeat for each conflicting commit

If you **cannot confidently resolve** a conflict:

1. Abort the rebase: `git rebase --abort`
2. Post a comment via `update_issue` explaining the conflict
3. Add `agent:human-required` label
4. Exit

## Step 8 — Implement all fixes

Fix priorities: blocking issues first, then non-blocking suggestions.

Rules:
- Fix the exact issue described in each finding
- Make the minimum change necessary
- Do not refactor surrounding code
- Preserve existing formatting conventions
- When findings conflict across analyzers, prefer security over style

Commit all fixes:
```bash
git add -A
git commit -m "fix: address analyzer findings from cycle N"
```

Do NOT run `git push` — use `create_pull_request` safe output.

## Step 9 — Push fixes via safe output

Call `create_pull_request` to push commits to the existing PR branch.

## Step 10 — Increment the cycle label

Call `update_issue` to swap `pr:cycle-N` → `pr:cycle-N+1`.

## Step 11 — Post the fix summary

```markdown
[MARKER:pr-fixer cycle:N]
## 🔧 PR Fixer — Cycle N Fix Summary

**Cycle**: N → N+1
**PR**: #<number>
**Linked Issue**: #<issue-number>

### Fixes Applied

- **[file:line]** — What was changed and why. (from Analyzer X)

### Unable to Fix

- **[file:line]** — Why this could not be fixed.

_None._

### Summary

- **Blocking issues fixed**: X of Y
- **Non-blocking suggestions fixed**: X of Y
- **Commit**: `<short SHA>`
- **Next cycle**: pr:cycle-N+1
```

## Guardrails

- Fix exactly ONE PR per run
- Never un-draft the PR
- Never close or merge the PR
- Never modify the linked issue's labels
- Never remove `agent:human-required`
- If pushing fails, post a comment and add `agent:human-required`
- Always rebase onto `origin/main` before implementing fixes (Step 7)
- Attempt to resolve rebase conflicts before escalating — only escalate if
  resolution is ambiguous or outside the PR's scope
- At most 5 `update_issue` calls per run
- When findings conflict, prefer security over style
