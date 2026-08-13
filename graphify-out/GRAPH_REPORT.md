# Graph Report - issue-21-threat-pricing  (2026-08-12)

## Corpus Check
- 59 files · ~157,340 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 761 nodes · 756 edges · 52 communities (51 shown, 1 thin omitted)
- Extraction: 100% EXTRACTED · 0% INFERRED · 0% AMBIGUOUS · INFERRED: 2 edges (avg confidence: 0.8)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `6db8c466`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀
- properties
- CATALOG.md
- properties
- properties
- Set it Free — Governance Policy
- environment
- enum
- PR Fixer — Authority
- PR Fixer — Authority
- PR Promoter
- Workflow Details
- PR Promoter
- GitHub Agentic Workflows Agent
- Set it Free Loop™ — Vision
- deploy-workflow.ps1
- PR Analyzer General — Full-Spectrum Review
- PR Analyzer Quality
- PR Analyzer Security
- PR Analyzer Testing
- SFL Auditor
- Set it Free Loop™
- HemSoft SFL Reviewer Parity TODO
- PR Analyzer General — Full-Spectrum Review
- PR Analyzer Quality
- PR Analyzer Security
- PR Analyzer Testing
- SFL Auditor
- HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route.
- HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route.
- Issue Processor
- Daily Simplisticate Audit
- engine-policy.schema.json
- Daily Simplisticate Audit
- SFL — Add Issue to Pipeline
- Repo Audit
- Note to SFL: source-first fixes needed for `gh-x` PR #3
- Workflow Title
- Daily Repo Status
- /new-workflow — Set it Free Loop Intake
- test-sfl-review-platform.ps1
- AGENTS.md

## God Nodes (most connected - your core abstractions)
1. `PR Promoter` - 20 edges
2. `PR Promoter` - 20 edges
3. `enum` - 18 edges
4. `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` - 16 edges
5. `SFL Auditor` - 14 edges
6. `PR Fixer — Authority` - 13 edges
7. `PR Fixer — Authority` - 13 edges
8. `SFL Auditor` - 13 edges
9. `Workflow Details` - 12 edges
10. `Set it Free Loop™ — Vision` - 11 edges

## Surprising Connections (you probably didn't know these)
- `Deploy-ToRepo()` --calls--> `Add-SflSourcePin()`  [INFERRED]
  deployment/scripts/deploy-workflow.ps1 → deployment/scripts/add-sfl-source-pin.ps1
- `Deploy-ToRepo()` --calls--> `Merge-SflManifest()`  [INFERRED]
  deployment/scripts/deploy-workflow.ps1 → deployment/scripts/merge-sfl-manifest.ps1

## Import Cycles
- None detected.

## Communities (52 total, 1 thin omitted)

### Community 0 - "Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀"
Cohesion: 0.04
Nodes (47): 10) Cost Predictions at Scale (Hundreds of Repos), 11) Rollout Strategy (0 → 100+ Repos), 12) KPIs That Prove Value, 13) Final Thesis, 1) Core Thesis, 2) What We Built Here (Concrete, Not Hypothetical), 3) The Set it Free Loop Pattern, 4) Architecture Layers (+39 more)

### Community 1 - "properties"
Cohesion: 0.05
Nodes (46): additionalProperties, minLength, properties, required, type, items, type, enum (+38 more)

### Community 2 - "CATALOG.md"
Cohesion: 0.05
Nodes (36): Daily Repo Status, Process, Step 0 — Close previous daily status reports, Style, What to include, Guardrails, Issue Processor, Known Limitation: Label Delivery (+28 more)

### Community 3 - "properties"
Cohesion: 0.05
Nodes (37): additionalProperties, description, format, type, description, properties, deployedAt, source (+29 more)

### Community 4 - "properties"
Cohesion: 0.06
Nodes (31): minLength, type, additionalProperties, description, properties, required, type, additionalProperties (+23 more)

### Community 5 - "Set it Free — Governance Policy"
Cohesion: 0.07
Nodes (29): 1. Label Taxonomy, 2. Retry and Fan-Out Policy, 3. Merge Authority Matrix, 4. Safe Write Boundaries, 5. Escalation Paths, 6. Audit Trail Requirements, 7. Opt-Out Mechanisms, 8. Governance Review Cadence (+21 more)

### Community 6 - "environment"
Cohesion: 0.08
Nodes (24): effort, model, provider, requiredSecretsAnyOf, defaultProfile, COPILOT_MODEL, COPILOT_PROVIDER_API_KEY, COPILOT_PROVIDER_BASE_URL (+16 more)

### Community 7 - "enum"
Cohesion: 0.08
Nodes (24): description, items, type, uniqueItems, enum, type, components, daily-repo-status (+16 more)

### Community 8 - "PR Fixer — Authority"
Cohesion: 0.09
Nodes (21): After all fixes, Blocking Issues, Fix priorities, Guardrails, Handling rebase conflicts, Implementation rules, Non-Blocking Suggestions, PR Fixer — Authority (+13 more)

### Community 9 - "PR Fixer — Authority"
Cohesion: 0.10
Nodes (21): After all fixes, Blocking Issues, Fix priorities, Guardrails, Handling rebase conflicts, Implementation rules, Non-Blocking Suggestions, PR Fixer — Authority (+13 more)

### Community 10 - "PR Promoter"
Cohesion: 0.10
Nodes (20): Guardrails, Phase 2 — Merge Job, PR Promoter, Step 10 — Update labels, Step 11 — Find merge candidate, Step 12 — Verify merge eligibility, Step 13 — Authenticate GitHub CLI, Step 14 — Squash merge and delete branch (+12 more)

### Community 11 - "Workflow Details"
Cohesion: 0.10
Nodes (20): Available Workflows, CATALOG.md — Set it Free Loop™ Workflow Library, daily-repo-status, Deployment Tiers, Governance, Infrastructure Components, Infrastructure Details, issue-processor (+12 more)

### Community 12 - "PR Promoter"
Cohesion: 0.10
Nodes (20): Guardrails, Phase 2 — Merge Job, PR Promoter, Step 10 — Update labels, Step 11 — Find merge candidate, Step 12 — Verify merge eligibility, Step 13 — Authenticate GitHub CLI, Step 14 — Squash merge and delete branch (+12 more)

### Community 13 - "GitHub Agentic Workflows Agent"
Cohesion: 0.11
Nodes (17): Available Prompts, Create New Workflow, Create Shared Agentic Workflow, Debug Workflow, Files This Applies To, GitHub Agentic Workflows Agent, GitHub Projects Integration, How to Use (+9 more)

### Community 14 - "Set it Free Loop™ — Vision"
Cohesion: 0.12
Nodes (17): Architecture, Consumer Integration, Core Loop, Executive Summary, Phase 1 — Observe (current), Phase 2 — Assisted Fixing, Phase 3 — Guarded Autonomy, Quality Gates (+9 more)

### Community 15 - "deploy-workflow.ps1"
Cohesion: 0.27
Nodes (12): Add-SflSourcePin(), Assert-HemSoftPrivateRepository(), Assert-SflReviewCredentials(), ConvertTo-SflWorkflowWithEnginePolicy(), Deploy-ToRepo(), Ensure-SflReviewLabel(), Get-SflObjectProperty(), New-SflEnginePolicyManifest() (+4 more)

### Community 16 - "PR Analyzer General — Full-Spectrum Review"
Cohesion: 0.13
Nodes (14): Best Practices, Correctness & Logic, Guardrails, Performance, PR Analyzer General — Full-Spectrum Review, Security, Step 1 — Find the target PR, Step 2 — Determine the current review cycle (+6 more)

### Community 17 - "PR Analyzer Quality"
Cohesion: 0.13
Nodes (14): Best Practices, Correctness & Logic, Guardrails, Performance, PR Analyzer Quality, Security-Relevant Quality, Step 1 — Find the target PR, Step 2 — Determine the current review cycle (+6 more)

### Community 18 - "PR Analyzer Security"
Cohesion: 0.13
Nodes (14): Abuse Resistance, Evidence, Guardrails, PR Analyzer Security, Security, Security Maintainability, Security-Relevant Correctness, Step 1 — Find the target PR (+6 more)

### Community 19 - "PR Analyzer Testing"
Cohesion: 0.13
Nodes (14): Behavior Coverage, CI and Maintenance, Gaps, Guardrails, Merge Risk, PR Analyzer Testing, Step 1 — Find the target PR, Step 2 — Determine the current review cycle (+6 more)

### Community 20 - "SFL Auditor"
Cohesion: 0.13
Nodes (14): Guardrails, SFL Auditor, Step 10 — Check: SFL review prerequisites, Step 11 — Check: unexplained agent:pause, Step 12 — Signal completion, Step 1 — Gather current state, Step 2 — Check: orphaned agent:in-progress labels, Step 3 — Check: conflicting labels (+6 more)

### Community 21 - "Set it Free Loop™"
Cohesion: 0.13
Nodes (15): 1. Set up labels, 2. Deploy a tier (or single workflow), 3. Review the catalog, Adding a new workflow, Adding the badge to a consumer repo, Automatic versioning, How it works, How the loop works (+7 more)

### Community 22 - "HemSoft SFL Reviewer Parity TODO"
Cohesion: 0.15
Nodes (13): Deferred until required parity is complete, Definition of done, Definition of parity, HemSoft SFL Reviewer Parity TODO, P0 — Close reviewer contract drift, P0 — Make `gh sfl` independently distributable, P0 — Restore a trustworthy release pipeline, P1 — Converge approved private consumers (+5 more)

### Community 23 - "PR Analyzer General — Full-Spectrum Review"
Cohesion: 0.13
Nodes (14): Best Practices, Correctness & Logic, Guardrails, Performance, PR Analyzer General — Full-Spectrum Review, Security, Step 1 — Find the target PR, Step 2 — Determine the current review cycle (+6 more)

### Community 24 - "PR Analyzer Quality"
Cohesion: 0.13
Nodes (14): Best Practices, Correctness & Logic, Guardrails, Performance, PR Analyzer Quality, Security-Relevant Quality, Step 1 — Find the target PR, Step 2 — Determine the current review cycle (+6 more)

### Community 25 - "PR Analyzer Security"
Cohesion: 0.13
Nodes (14): Abuse Resistance, Evidence, Guardrails, PR Analyzer Security, Security, Security Maintainability, Security-Relevant Correctness, Step 1 — Find the target PR (+6 more)

### Community 26 - "PR Analyzer Testing"
Cohesion: 0.13
Nodes (14): Behavior Coverage, CI and Maintenance, Gaps, Guardrails, Merge Risk, PR Analyzer Testing, Step 1 — Find the target PR, Step 2 — Determine the current review cycle (+6 more)

### Community 27 - "SFL Auditor"
Cohesion: 0.14
Nodes (13): Guardrails, SFL Auditor, Step 10 — Check: unexplained agent:pause, Step 11 — Signal completion, Step 1 — Gather current state, Step 2 — Check: orphaned agent:in-progress labels, Step 3 — Check: conflicting labels, Step 4 — Check: action items missing agent:fixable (+5 more)

### Community 28 - "HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route."
Cohesion: 0.14
Nodes (13): Conditional Evidence Checks, Full-Spectrum PR Review, HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route., HemSoft SFL reviewer platform v2, Pass 1: 🔒 Security Review, Pass 2: ✅ Accuracy & Reliability Review, Pass 3: 🧰 Quality & Maintainability Review, Required Configuration (+5 more)

### Community 29 - "HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route."
Cohesion: 0.14
Nodes (13): Conditional Evidence Checks, Full-Spectrum PR Review, HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route., HemSoft SFL reviewer platform v2, Pass 1: 🔒 Security Review, Pass 2: ✅ Accuracy & Reliability Review, Pass 3: 🧰 Quality & Maintainability Review, Required Configuration (+5 more)

### Community 32 - "Issue Processor"
Cohesion: 0.20
Nodes (9): Guardrails, Issue Processor, Step 1 — Find the oldest claimable issue, Step 2 — Claim the issue, Step 3 — Validate the issue body, Step 4 — Inspect the codebase, Step 5 — Implement the fix, Step 6 — Open a pull request via safe output (+1 more)

### Community 33 - "Daily Simplisticate Audit"
Cohesion: 0.20
Nodes (9): Audit Scope, Complexity Signals to Detect, Daily Simplisticate Audit, Goals, Output Requirements, Per-finding issues (for agent-fixable findings only), Process, Step 0 — Close previous simplisticate summary reports (+1 more)

### Community 34 - "engine-policy.schema.json"
Cohesion: 0.11
Nodes (17): additionalProperties, minLength, type, description, defaultProfile, minProperties, type, properties (+9 more)

### Community 35 - "Daily Simplisticate Audit"
Cohesion: 0.20
Nodes (9): Audit Scope, Complexity Signals to Detect, Daily Simplisticate Audit, Goals, Output Requirements, Per-finding issues (for agent-fixable findings only), Process, Step 0 — Close previous simplisticate summary reports (+1 more)

### Community 36 - "SFL — Add Issue to Pipeline"
Cohesion: 0.25
Nodes (7): Guardrails, SFL — Add Issue to Pipeline, Step 1 — Understand what the user wants fixed, Step 2 — Assess risk, Step 3 — Compose the issue, Step 4 — User approval, Step 5 — Create the issue

### Community 37 - "Repo Audit"
Cohesion: 0.29
Nodes (6): Audit Scope, Goals, Output Requirements, Per-finding issues (grouped by category), Process, Repo Audit

### Community 38 - "Note to SFL: source-first fixes needed for `gh-x` PR #3"
Cohesion: 0.29
Nodes (6): Context, Desired end state, Fixes currently made only in the deployed copy, Note to SFL: source-first fixes needed for `gh-x` PR #3, Recommended remediation, Separate `gh-x` issue

### Community 39 - "Workflow Title"
Cohesion: 0.33
Nodes (5): Goals, Output Requirements, Process, Scope, Workflow Title

### Community 40 - "Daily Repo Status"
Cohesion: 0.33
Nodes (5): Daily Repo Status, Process, Step 0 — Close previous daily status reports, Style, What to include

### Community 42 - "/new-workflow — Set it Free Loop Intake"
Cohesion: 0.40
Nodes (4): /new-workflow — Set it Free Loop Intake, Step 1 — Gather Requirements, Step 2 — Generate the Draft Workflow File, Step 3 — Output the TODO.md Entry

### Community 43 - "test-sfl-review-platform.ps1"
Cohesion: 0.83
Nodes (3): Assert-ExactPair(), Assert-Patterns(), Read-RepoFile()

## Knowledge Gaps
- **548 isolated node(s):** `$schema`, `defaultProfile`, `provider`, `model`, `effort` (+543 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **1 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` connect `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` to `CATALOG.md`?**
  _High betweenness centrality (0.046) - this node is a cross-community bridge._
- **Why does `Set it Free — Governance Policy` connect `Set it Free — Governance Policy` to `CATALOG.md`?**
  _High betweenness centrality (0.029) - this node is a cross-community bridge._
- **Why does `PR Fixer — Authority` connect `PR Fixer — Authority` to `CATALOG.md`?**
  _High betweenness centrality (0.021) - this node is a cross-community bridge._
- **What connects `$schema`, `defaultProfile`, `provider` to the rest of the system?**
  _548 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` be split into smaller, more focused modules?**
  _Cohesion score 0.0425531914893617 - nodes in this community are weakly interconnected._
- **Should `properties` be split into smaller, more focused modules?**
  _Cohesion score 0.04927536231884058 - nodes in this community are weakly interconnected._
- **Should `CATALOG.md` be split into smaller, more focused modules?**
  _Cohesion score 0.04591836734693878 - nodes in this community are weakly interconnected._