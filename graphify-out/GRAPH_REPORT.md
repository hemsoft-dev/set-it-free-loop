# Graph Report - issue-59-own-gh-sfl  (2026-08-13)

## Corpus Check
- 96 files · ~192,491 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 1206 nodes · 1822 edges · 90 communities (87 shown, 3 thin omitted)
- Extraction: 88% EXTRACTED · 12% INFERRED · 0% AMBIGUOUS · INFERRED: 218 edges (avg confidence: 0.8)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `122b9298`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀
- required
- sfl-manifest.schema.json
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
- enum
- HemSoft SFL Pull Request Reviewer
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
- Test-NormalizedTextEqual
- AGENTS.md
- Issue Processor
- deployment_test.go
- repositoryRuleset
- properties
- renderRunTable
- conditionalRESTClient
- review.go
- main.go
- github.go
- fakeREST
- Commands
- version
- addons
- reviewer_contract_test.go
- hemsoft.go
- reviewerFixtureBash
- properties
- requiredSecretsAnyOf
- properties
- enum
- enum
- sfl: Frontmatter Spec
- changelog.go
- Daily Repo Status
- Repo Audit
- github.com/HemSoft/set-it-free-loop/gh-sfl
- SFL reviewer parity baseline
- source
- sourceSha
- review_recovery_fixture_test.go
- PROVENANCE.md

## God Nodes (most connected - your core abstractions)
1. `deployViaPullRequest()` - 26 edges
2. `repositoryRuleset` - 24 edges
3. `runInit()` - 21 edges
4. `restAPI` - 20 edges
5. `runSync()` - 20 edges
6. `PR Promoter` - 20 edges
7. `PR Promoter` - 20 edges
8. `enum` - 18 edges
9. `runAdd()` - 18 edges
10. `readContractFile()` - 17 edges

## Surprising Connections (you probably didn't know these)
- `Deploy-ToRepo()` --calls--> `Add-SflSourcePin()`  [INFERRED]
  deployment/scripts/deploy-workflow.ps1 → deployment/scripts/add-sfl-source-pin.ps1
- `Deploy-ToRepo()` --calls--> `Merge-SflManifest()`  [INFERRED]
  deployment/scripts/deploy-workflow.ps1 → deployment/scripts/merge-sfl-manifest.ps1
- `runAdd()` --calls--> `knownAddonNames()`  [INFERRED]
  gh-sfl/add.go → gh-sfl/addons.go
- `runAdd()` --calls--> `deployViaPullRequest()`  [INFERRED]
  gh-sfl/add.go → gh-sfl/github.go
- `runAdd()` --calls--> `hemSoftEnginePolicyManifestForFileMap()`  [INFERRED]
  gh-sfl/add.go → gh-sfl/hemsoft.go

## Import Cycles
- None detected.

## Communities (90 total, 3 thin omitted)

### Community 0 - "Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀"
Cohesion: 0.04
Nodes (47): 10) Cost Predictions at Scale (Hundreds of Repos), 11) Rollout Strategy (0 → 100+ Repos), 12) KPIs That Prove Value, 13) Final Thesis, 1) Core Thesis, 2) What We Built Here (Concrete, Not Hypothetical), 3) The Set it Free Loop Pattern, 4) Architecture Layers (+39 more)

### Community 1 - "required"
Cohesion: 0.16
Nodes (14): additionalProperties, minLength, required, type, additionalProperties, type, model, profile (+6 more)

### Community 3 - "sfl-manifest.schema.json"
Cohesion: 0.17
Nodes (11): additionalProperties, description, required, $schema, title, type, components, deployedAt (+3 more)

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
Cohesion: 0.11
Nodes (18): enum, daily-repo-status, governance, issue-processor, labels, pr-analyzer-general, pr-analyzer-quality, pr-analyzer-security (+10 more)

### Community 8 - "PR Fixer — Authority"
Cohesion: 0.09
Nodes (21): After all fixes, Blocking Issues, Fix priorities, Guardrails, Handling rebase conflicts, Implementation rules, Non-Blocking Suggestions, PR Fixer — Authority (+13 more)

### Community 9 - "PR Fixer — Authority"
Cohesion: 0.09
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

### Community 30 - "enum"
Cohesion: 0.20
Nodes (10): provider, enum, type, claude, codex, copilot, crush, gemini (+2 more)

### Community 31 - "HemSoft SFL Pull Request Reviewer"
Cohesion: 0.22
Nodes (9): Advisory and gated operation, Automatic and explicit review, Deployment, Distribution boundary, Evidence and approval, HemSoft SFL Pull Request Reviewer, Package, Recovery (+1 more)

### Community 32 - "Issue Processor"
Cohesion: 0.20
Nodes (9): Guardrails, Issue Processor, Step 1 — Find the oldest claimable issue, Step 2 — Claim the issue, Step 3 — Validate the issue body, Step 4 — Inspect the codebase, Step 5 — Implement the fix, Step 6 — Open a pull request via safe output (+1 more)

### Community 33 - "Daily Simplisticate Audit"
Cohesion: 0.20
Nodes (9): Audit Scope, Complexity Signals to Detect, Daily Simplisticate Audit, Goals, Output Requirements, Per-finding issues (for agent-fixable findings only), Process, Step 0 — Close previous simplisticate summary reports (+1 more)

### Community 34 - "engine-policy.schema.json"
Cohesion: 0.22
Nodes (8): additionalProperties, description, defaultProfile, required, $schema, title, type, profiles

### Community 35 - "Daily Simplisticate Audit"
Cohesion: 0.22
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

### Community 43 - "Test-NormalizedTextEqual"
Cohesion: 0.43
Nodes (5): ConvertTo-NormalizedLineEnding(), Test-NormalizedTextEqual(), Assert-ExactPair(), Assert-PatternSet(), Read-RepoFile()

### Community 54 - "Issue Processor"
Cohesion: 0.20
Nodes (10): Guardrails, Issue Processor, Known Limitation: Label Delivery, Step 1 — Find the oldest claimable issue, Step 2 — Claim the issue, Step 3 — Validate the issue body, Step 4 — Inspect the codebase, Step 5 — Implement the fix (+2 more)

### Community 57 - "deployment_test.go"
Cohesion: 0.06
Nodes (87): knownAddonNames(), findAddition(), T, installDeploymentFakes(), legacyReviewerRuleset(), reviewerWorkflowRuleset(), TestAddUsesPinnedSourceAndMergesEnginePolicy(), TestBuildFileAdditionsSortsAndEncodes() (+79 more)

### Community 58 - "repositoryRuleset"
Cohesion: 0.08
Nodes (63): appInstallation, TestClassifyReviewerGate(), collectStaleReviewerRulesets(), createReviewerRuleset(), Writer, isDedicatedLegacyReviewerGate(), mergeReviewerFreshnessRule(), parseGateOptions() (+55 more)

### Community 60 - "properties"
Cohesion: 0.25
Nodes (8): description, format, type, description, type, properties, deployedAt, deployedBy

### Community 61 - "renderRunTable"
Cohesion: 0.12
Nodes (26): ANSIColor, fetchWorkflowRuns(), tableStyler, Time, Writer, parseListOptions(), renderRunTable(), resolveRunStatus() (+18 more)

### Community 62 - "conditionalRESTClient"
Cohesion: 0.27
Nodes (4): Client, conditionalRESTClient, Reader, RESTClient

### Community 63 - "review.go"
Cohesion: 0.21
Nodes (16): dispatchCapabilities, pullRequestShas, detectDispatchInputs(), fetchPullRequestShas(), Time, Writer, isNotFoundError(), isNotFoundMessage() (+8 more)

### Community 64 - "main.go"
Cohesion: 0.23
Nodes (14): Duration, asyncUpdateCheck(), formatVersion(), Writer, main(), printBanner(), resolveCommand(), run() (+6 more)

### Community 65 - "github.go"
Cohesion: 0.05
Nodes (75): ClientOptions, Writer, parseAddOptions(), runAdd(), writeAddUsage(), addonWorkflowFiles(), validAddon(), addOptions (+67 more)

### Community 66 - "fakeREST"
Cohesion: 0.24
Nodes (6): decodeTestResponse(), Reader, splitEvery(), TestConditionalRESTClientUsesEntityTags(), fakeGraphQL, fakeREST

### Community 67 - "Commands"
Cohesion: 0.14
Nodes (13): Building, Commands, `gh sfl add`, `gh sfl gate`, gh-sfl — GitHub CLI Extension for Set it Free Loop, `gh sfl init`, `gh sfl list`, `gh sfl status` (+5 more)

### Community 68 - "version"
Cohesion: 0.50
Nodes (4): version, description, pattern, type

### Community 69 - "addons"
Cohesion: 0.18
Nodes (11): description, items, type, uniqueItems, description, items, type, uniqueItems (+3 more)

### Community 70 - "reviewer_contract_test.go"
Cohesion: 0.24
Nodes (13): T, TestReviewTriggerConcurrencyGroups(), assertFilesEqual(), assertRunBlocksWithinLimit(), T, normalizeLineEndings(), TestAutoTriggerTokenPermissions(), TestCompiledEvidencePublishesWithRepositoryToken() (+5 more)

### Community 71 - "hemsoft.go"
Cohesion: 0.14
Nodes (29): validateDeploymentTarget(), applyHemSoftEnginePolicy(), applyHemSoftEnginePolicyToWorkflow(), applyHemSoftOwnership(), cloneHemSoftEnvironment(), hemSoftEngineBlock(), hemSoftEngineConfigForWorkflow(), hemSoftEnginePolicyConfig() (+21 more)

### Community 72 - "reviewerFixtureBash"
Cohesion: 0.24
Nodes (7): T, TestDispatchDedupFixtures(), T, TestReviewEffortResolutionFixtures(), T, reviewerFixtureBash(), TestAutoGateRunValidationFixtures()

### Community 73 - "properties"
Cohesion: 0.22
Nodes (9): properties, type, minLength, type, minLength, type, arguments, model (+1 more)

### Community 74 - "requiredSecretsAnyOf"
Cohesion: 0.33
Nodes (7): items, minLength, type, requiredSecretsAnyOf, items, type, uniqueItems

### Community 75 - "properties"
Cohesion: 0.22
Nodes (9): minLength, type, minProperties, type, properties, defaultProfile, profiles, $schema (+1 more)

### Community 77 - "enum"
Cohesion: 0.33
Nodes (6): enum, type, effort, high, low, medium

### Community 78 - "enum"
Cohesion: 0.20
Nodes (10): tier, description, enum, type, custom, full, minimal, review (+2 more)

### Community 79 - "sfl: Frontmatter Spec"
Cohesion: 0.29
Nodes (7): Field Matrix, Full Field Reference, Future-Proofing, Minimal Example, Overview, sfl: Frontmatter Spec, What NOT to add to sfl:

### Community 80 - "changelog.go"
Cohesion: 0.57
Nodes (6): extractVersion(), Writer, parseChangelogOptions(), runChangelog(), writeChangelogUsage(), changelogOptions

### Community 82 - "Daily Repo Status"
Cohesion: 0.33
Nodes (5): Daily Repo Status, Process, Step 0 — Close previous daily status reports, Style, What to include

### Community 83 - "Repo Audit"
Cohesion: 0.33
Nodes (5): Audit Scope, Goals, Output Requirements, Process, Repo Audit

### Community 85 - "SFL reviewer parity baseline"
Cohesion: 0.40
Nodes (4): CLI source ownership, Intentional differences, Running the audit, SFL reviewer parity baseline

### Community 87 - "source"
Cohesion: 0.50
Nodes (4): source, default, description, type

### Community 88 - "sourceSha"
Cohesion: 0.50
Nodes (4): sourceSha, description, pattern, type

### Community 90 - "review_recovery_fixture_test.go"
Cohesion: 0.67
Nodes (3): T, TestNewerRunSuppressionFixtures(), TestReviewRecoveryPolicyFixtures()

## Knowledge Gaps
- **571 isolated node(s):** `$schema`, `defaultProfile`, `provider`, `model`, `effort` (+566 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **3 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `readContractFile()` connect `deployment_test.go` to `reviewer_contract_test.go`, `hemsoft.go`?**
  _High betweenness centrality (0.014) - this node is a cross-community bridge._
- **Why does `fetchFileRaw()` connect `github.go` to `changelog.go`, `repositoryRuleset`, `conditionalRESTClient`, `review.go`?**
  _High betweenness centrality (0.014) - this node is a cross-community bridge._
- **Why does `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` connect `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` to `CATALOG.md`?**
  _High betweenness centrality (0.011) - this node is a cross-community bridge._
- **Are the 14 inferred relationships involving `deployViaPullRequest()` (e.g. with `runAdd()` and `TestDeployViaPullRequest()`) actually correct?**
  _`deployViaPullRequest()` has 14 INFERRED edges - model-reasoned connections that need verification._
- **Are the 15 inferred relationships involving `runInit()` (e.g. with `addonWorkflowFiles()` and `deployViaGit()`) actually correct?**
  _`runInit()` has 15 INFERRED edges - model-reasoned connections that need verification._
- **What connects `$schema`, `defaultProfile`, `provider` to the rest of the system?**
  _571 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `Solving Software Engineering Development through the art of the Agentic Loop — The Set it Free Loop™ 🚀` be split into smaller, more focused modules?**
  _Cohesion score 0.0425531914893617 - nodes in this community are weakly interconnected._