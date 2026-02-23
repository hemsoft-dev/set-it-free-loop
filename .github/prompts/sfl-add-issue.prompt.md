---
mode: agent
description: Create a pipeline-ready issue for the Set it Free Loop.
---

# SFL — Add Issue to Pipeline

Interactively help the user create a well-formed issue that the SFL Issue
Processor will pick up automatically on its next 30-minute cron cycle.

---

## Step 1 — Understand what the user wants fixed

Ask the user what they want done. They may describe it in one sentence or
paste a code snippet. Gather enough detail to fill in the issue template below.

If the user's description is vague, ask clarifying questions until you can
answer all of these:

1. **What files are affected?** (paths and approximate line numbers)
2. **What is the current behavior / problem?**
3. **What should the fix look like?** (the mechanical change)
4. **Is the fix deterministic?** (one clear correct outcome per file)

If the fix is NOT deterministic, or requires architectural decisions, or is
risk:medium or higher — tell the user this is not a good candidate for SFL
automation and suggest they create a regular issue instead.

---

## Step 2 — Assess risk

Determine the risk class:

- **risk:trivial** — cosmetic, formatting, typo, dead code removal, no
  behavioral change possible
- **risk:low** — mechanical refactor, adding missing attributes, removing
  unused imports; clear single correct outcome; no user-facing behavior change

If the fix would change runtime behavior, touch more than 15 files, or require
judgment calls, it does NOT qualify. Tell the user and stop.

---

## Step 3 — Compose the issue

Build the issue using this exact structure. Show it to the user for approval
before creating it.

**Title format**: `[sfl] <short description of the fix>`

**Body**:

```markdown
## Category

<shared finding category, e.g., Dead Code, Accessibility, Configuration Hygiene>

## Pattern

<the common fix pattern, e.g., "remove unused export", "add role attribute to
clickable div">

## Affected files

- [ ] `<file-path>:<line>` — <what to change>
- [ ] `<file-path>:<line>` — <what to change>

## Acceptance criteria

<how to verify the fix is correct — e.g., "no TypeScript errors", "eslint
passes", "export is no longer referenced anywhere">

## Risk

`risk:<trivial|low>` — <one-line justification>
```

**Labels**: `agent:fixable`, `type:action-item`, `risk:<trivial|low>`

---

## Step 4 — User approval

Show the composed title, body, and labels to the user. Ask:

> Does this look good? I'll create the issue and the pipeline will pick it up
> on its next cycle.

Wait for confirmation. If the user wants changes, revise and re-show.

---

## Step 5 — Create the issue

Run in the terminal:

```bash
gh issue create --repo "<owner>/<repo>" \
  --title "[sfl] <title>" \
  --body "<body>" \
  --label "agent:fixable" \
  --label "type:action-item" \
  --label "risk:<trivial|low>"
```

After creation, confirm the issue number and tell the user:

> Issue #N created. The Issue Processor runs every 30 minutes — it will claim
> this issue, create a draft PR, and send it through analyzer review
> automatically.

---

## Guardrails

- Never create an issue without user confirmation
- Never apply `agent:fixable` to anything risk:medium or higher
- The body MUST contain all five sections (Category, Pattern, Affected files,
  Acceptance criteria, Risk) — the Issue Processor validates this
- Max 15 affected files per issue; if more, split into multiple issues
- Do not add `agent:in-progress` — that is the Issue Processor's job
