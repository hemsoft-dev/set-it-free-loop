---
mode: agent
description: Guided intake for authoring a new Set it Free Loop workflow. Generates a draft .md file in .github/workflows/ (staging) and outputs a TODO.md entry.
tools:
  - createFile
  - editFile
---

# /new-workflow — Set it Free Loop Intake

You are helping the user author a new agentic workflow for the Set it Free Loop library.

Work through the questions below **one at a time**, waiting for answers before proceeding.
After collecting all answers, create the draft workflow file and output the TODO.md row.

---

## Step 1 — Gather Requirements

Ask the user each question in sequence:

1. **Name**: What is the short kebab-case name for this workflow? (e.g. `weekly-repo-audit`, `issue-to-pr-fixer`)

2. **Purpose**: In one or two sentences, what does this workflow do?

3. **Trigger type**: How is it triggered?
   - `schedule` — runs on a cron schedule
   - `label-driven` — runs when a GitHub Issue or PR receives a specific label
   - `manual` — `workflow_dispatch` only
   - `hybrid` — schedule + manual dispatch

4. **Trigger detail**:
   - If `schedule`: what cron expression? (e.g. `0 9 * * 1` = Mondays at 9am UTC)
   - If `label-driven`: which label(s) trigger it? (e.g. `agent:fixable`)
   - If `manual` or `hybrid`: any additional schedule?

5. **Output**: What does a successful run produce? (select all that apply)
   - `creates-issue` — opens a GitHub Issue
   - `creates-pr` — opens a Pull Request
   - `posts-comment` — comments on an existing issue or PR
   - `runs-command` — executes a shell command (no GitHub output)

6. **Outcome definition**: In plain language, what does success look like? Include any measurable KPIs.
   (e.g. "One issue per week surfacing doc drift. >80% of findings are actionable.")

7. **Risk class**: How risky are the changes this workflow makes?
   - `trivial` — formatting, typos, doc updates; can auto-merge
   - `low` — dependency bumps, safe refactors; can auto-merge with CI green
   - `medium` — logic changes, new features; requires Copilot review
   - `high` — auth, payments, data migrations; requires human review
   - `critical` — security, PII, compliance; requires two human reviewers

8. **Acceptance criteria**: List 2–4 things that must be true before this workflow graduates from staging to `deployment/`.
   (e.g. "Runs without error on a repo with no prior issues", "Creates exactly one issue per run")

---

## Step 2 — Generate the Draft Workflow File

Using the answers above, create a new file at:

```
.github/workflows/{name}.md
```

Use this template structure:

```markdown
---
description: |
  {2-3 sentence description of what this workflow does}

on:
  {trigger block — use schedule/cron or workflow_dispatch as appropriate}

permissions:
  contents: read
  issues: {read or write depending on output type}
  pull-requests: {read or write depending on output type}

network: defaults

tools:
  github:
    lockdown: false

safe-outputs:
  {create-issue or create-pr block if applicable, with title-prefix and labels}

sfl:
  status: staging
  version: "1.0.0"
  category: {derived from purpose}
  risk-class: {from answer 7}
  target-labels: {from answer 4 if label-driven, else []}
  outcome-definition: |
    {from answer 6}
  acceptance-criteria:
    {from answer 8, one bullet per criterion}
  source-repo: HemSoft/set-it-free-loop
---

# {Title — human readable name}

{1 sentence stating what this workflow does}

## Goals

{3-5 bullet goals}

## Process

1. {step 1}
2. {step 2}
3. {step 3}
```

---

## Step 3 — Output the TODO.md Entry

After creating the file, output the following for the user to paste into `TODO.md`:

```markdown
| 🔴 High | {name} | {category} | {trigger summary} | {outcome goal in ≤10 words} | {any notable constraints} |
```

Then tell the user:

> Draft created at `.github/workflows/{name}.md`.
>
> **Next steps**:
> 1. Run `gh aw compile .github/workflows/{name}.md` to verify the frontmatter compiles
> 2. Trigger the workflow manually via `gh aw run {name}` on this repo
> 3. Verify it meets all acceptance criteria
> 4. Once passing, copy to `deployment/workflows/` and add a CATALOG.md entry
