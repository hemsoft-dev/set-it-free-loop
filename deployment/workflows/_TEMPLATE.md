---
description: |
  {Replace with a 2-3 sentence description of what this workflow does.}

on:
  # Choose one:
  schedule:
    - cron: "0 9 * * 1"   # Mondays at 9am UTC — replace with your cron expression
  workflow_dispatch:
  # OR for label-driven:
  # issues:
  #   types: [labeled]

permissions:
  contents: read
  issues: write          # change to read if workflow does not create/comment on issues
  pull-requests: write   # change to read if workflow does not create/comment on PRs

network: defaults

tools:
  github:
    lockdown: false

safe-outputs:
  create-issue:
    title-prefix: "[workflow-name] "
    labels: [type:report]   # or type:action-item for actionable output

sfl:
  status: staging           # do not change until acceptance criteria are all met
  version: "1.0.0"
  category: quality         # quality | reporting | intake | security | custom
  risk-class: low           # trivial | low | medium | high | critical
  target-labels: []         # only for label-driven workflows
  outcome-definition: |
    Replace with a human-readable description of success.
    Include measurable KPIs where possible.
  acceptance-criteria:
    - Runs without error on a repo with no prior issues
    - Creates correct output type with correct labels
    - No duplicate outputs on re-run
    - Add workflow-specific criteria here
  source-repo: HemSoft/set-it-free-loop
---

# Workflow Title

One sentence summary of what this workflow does.

## Goals

- Goal 1
- Goal 2
- Goal 3

## Scope

What the workflow looks at / acts on.

## Output Requirements

What the output must contain to be considered complete.

## Process

1. Step 1
2. Step 2
3. Step 3
