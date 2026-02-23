---
description: |
  This workflow runs every 30 minutes, picks the single oldest open issue
  labelled agent:fixable, claims it, implements the described fix on a new
  branch, and opens a pull request. One issue per run — no fan-out.

on:
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
  create-pull-request:
    title-prefix: "[agent-fix] "
    labels: [agent:pr]
    draft: true
  update-issue:
    target: "*"
    max: 3
---

# Issue Processor

Run every 30 minutes. Find the oldest open `agent:fixable` issue, claim it,
implement the fix, and open a pull request. Process exactly one issue per run.

## Step 1 — Find the oldest claimable issue

Search for open issues in this repository that have ALL of the following labels:

- `agent:fixable`
- `action-item`

And do NOT have any of:

- `agent:in-progress`
- `agent:pause`
- `agent:human-required`
- `no-agent`

Sort results by creation date ascending. Take the **single oldest** result.

If no issue matches, exit immediately — nothing to do.

## Step 2 — Claim the issue

Before doing any other work, call `update_issue` with:

- `issue_number`: the issue number found in Step 1 (always required)
- `labels`: replace with `["agent:in-progress", "report", "action-item", "audit"]`
  (remove `agent:fixable`, add `agent:in-progress` — keep all other existing labels)
- `body`: append "🤖 Issue Processor claimed this issue. Working on a fix."
- `operation`: `"append"`

This prevents a concurrent run from picking up the same issue.

## Step 3 — Validate the issue body

The issue body must contain all three sections: **Finding**, **Fix**, and
**Acceptance criteria**.

Extract:

- **Finding**: the file path(s) and exact problem described
- **Fix**: precisely what change to make
- **Acceptance criteria**: how to verify the fix is correct
- **Risk**: note the risk level for awareness (e.g. `risk:trivial`, `risk:low`, `risk:medium`)

If any of the three required sections (Finding, Fix, Acceptance criteria) is
missing: add label `agent:human-required`, remove `agent:in-progress`, post a
comment explaining the reason, and exit.

## Step 4 — Inspect the codebase

Read the target file(s) in full before writing anything:

- Confirm the problem described in the Finding actually exists as stated
- If the file has already been fixed (the problem no longer exists), close
  the issue with the comment "✅ Already resolved — closing." and exit

## Step 5 — Implement the fix

Checkout a new branch named `agent-fix/issue-<issue-number>` from `main`.

Apply the minimal change that satisfies the acceptance criteria:

- Do not refactor surrounding code
- Do not rename symbols unless that is the stated fix
- Do not add comments, docs, or type annotations to unchanged lines
- Preserve existing formatting conventions exactly
- Touch only the files identified in the Fix section

Commit the changes with a descriptive commit message.

**IMPORTANT**: Do NOT run `git push`. The safe-output tool in the next step
handles branch pushing and PR creation together. A direct `git push` will fail
because the workflow token does not have push permissions.

## Step 6 — Open a pull request via safe output

Call the `create_pull_request` safe output tool. This is the ONLY way to
create a PR — it handles pushing the branch and opening the PR in one step.

The tool will use your committed changes on the current branch. Provide:

- Title: `<issue title, stripped of any [repo-audit] prefix>`
  (the `[agent-fix]` prefix is added automatically by the safe-output config)
- Body:
  - `Closes #<issue number>`
  - One-paragraph summary of what was changed and why
  - Acceptance criteria quoted from the issue
  - Risk confirmation: `risk:trivial` or `risk:low` with one-line justification

Do NOT set labels — they are configured automatically by the safe-output.

## Step 7 — Update the issue

Call `update_issue` with:

- `issue_number`: the issue number from Step 1 (always required)
- `body`: "🔀 Opened PR #<number> — ready for review."
- `operation`: `"append"`

## Guardrails

- Exit after processing exactly one issue per run — never loop over multiple issues
- Never force-push, amend commits, or modify files outside the Fix scope
- Never run `git push` directly — always use the `create_pull_request` safe output
- If any step fails unexpectedly: call `update_issue` with labels that replace
  `agent:in-progress` with `agent:pause` (keep all other labels), and body
  appending the failure reason, then exit cleanly
