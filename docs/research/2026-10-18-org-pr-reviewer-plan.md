# Personal organization PR reviewer: revised plan

Superseded by the owner's dual-provider v1 requirement in the [adversarial alpha plan](2026-10-19-adversarial-alpha-plan.md) and the published [specification issue](https://github.com/hemsoft-dev/set-it-free-loop/issues/172). The Claude-first/private-target staging below is historical, not the current implementation scope.

Revision 3, October 19, 2026 EDT. Prepared after two rounds with GPT-6 Astra and Claude Opus 5.5. Both approved Revision 1 as a sound staged feasibility plan; this revision incorporates their final amendments. Approval concerns the plan, not demonstrated runtime behavior.

This document authorizes no deployment. SFL remains frozen. Research, documentation, and model consultation have occurred; no account, App, runner, credentials, workflow, or inference prototype was configured.

The [research report](2026-10-18-subscription-backed-org-pr-reviewer.md) retains the primary-source background. The two review rounds are summarized in this historical plan. Full local consultation records remain outside the repository; the current public contract is [issue #172](https://github.com/hemsoft-dev/set-it-free-loop/issues/172).

## Intended result and limits

Franz requests a review through GitHub's native Reviewers UI on a private `hemsoft-dev` PR. An owner-operated reviewer uses Franz's existing coding subscription and posts a concise summary with useful inline findings. The reviewed revisions and any incomplete coverage are explicit.

The preferred identity remains a custom GitHub App. Its picker eligibility is not established. The project is ready for a gated feasibility investigation, not a claim that the exact experience already works. The service must initially accept only requests attributable to Franz's immutable GitHub account ID. Other repository collaborators are not granted access to his model subscription.

## What changed after the first review round

- Both reviewers recommend Claude Code first, through its unmodified official CLI. This is an operational preference because its documented CI token avoids Codex's mutable login-cache management; no comparative quality advantage has been demonstrated.
- Both recommend a polling service rather than Actions plus public webhooks for the first version. The documented platform interfaces make that plausible; polling discovery and requester attribution still require proof for the chosen identity.
- Astra identified missing recovery semantics, the publication race, account-level paid overflow, and a need for a concrete credential boundary. Those objections are adopted.
- Opus identified a machine-account fallback and automatic execution of repository hooks/MCP configuration in ordinary Claude print mode. Both are incorporated, with the fallback kept optional.
- Opus's proposed `--allowedTools` list is not accepted as a tool-availability boundary. Anthropic documents `--tools` for that purpose. An empty built-in tool list also does not disable MCP. [CLI reference](https://code.claude.com/docs/en/cli-reference).
- A pending GitHub review is not accepted as a proven two-phase commit, and a body marker alone does not establish exactly-once publication. Uncertain writes require reconciliation.
- Base-tip movement invalidates unpublished results even when the merge base is unchanged. This conservative first-version policy avoids overstating the relevance of old integration context.

## Gate 1: prove the reviewer identity without inference

After a future explicit instruction permits a test App and private test repository, verify all of the following:

1. The actual custom App is selectable in GitHub's native picker.
2. A persisted Bot review request identifies that App.
3. The request event identifies its requester, target, event ID, and time.
4. The App can discover that outstanding request through the proposed polling route. Do not assume a user-only search qualifier supports App bots.
5. A canned App-authored COMMENT review completes the request as expected, and GitHub permits a new native review request.
6. Removal, repeated request, and requests arriving while the service is offline can be distinguished using durable GitHub state and paginated timeline data.

GitHub's schema includes bot review-request inputs, but does not document general enrollment criteria for privately registered Apps. If this test fails, prepare a GitHub Support evidence package asking about non-partner private Apps, picker visibility, API eligibility, and review-request completion. Do not send it without authorization. [Bot inputs and request events](https://docs.github.com/en/graphql/reference/pulls#requestreviewsbylogininput).

An optional fallback is a dedicated machine account. GitHub permits human-created machine accounts and documents native review requests for users with repository read access. This supports a candidate native-UI route, not a tested account-specific outcome. It changes the review identity to a User, introduces a personal-token/seat lifecycle, and requires Franz's explicit acceptance. The selected identity must also author the completing review; an App posting on behalf of a machine-user request is not assumed to clear it. [Account terms](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service#3-account-requirements), [Requesting reviews](https://docs.github.com/en/pull-requests/how-tos/create-pull-requests/requesting-a-pull-request-review).

Neither a machine account nor a PR comment is silently substituted for the requested App experience. An unsuccessful identity gate stops the exact App plan until GitHub enables it or Franz changes the requirement.

## Gate 2: establish the actual subscription and billing boundary

Prefer an eligible Claude subscription already owned by Franz, using the pinned unmodified CLI and its documented CI token. This is an owner-use interpretation of documented functionality, not blanket policy clearance for a service offered to other people. Do not embed OAuth in a custom Agent SDK service or emulate private provider endpoints. [Subscription token](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token), [Credential-use rules](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use).

Before an unattended pilot, record the actual account, plan, allowed model, billing mode, expiry, and available quota controls without storing secret values in evidence. Read-only account and billing inspection can precede isolation work. Credential-bearing inference may run only in Gate 3's restricted probe environment after its configuration and network controls have been checked; authenticate and validate isolation together there. Use an allowlisted process environment; omit API/provider credentials and proxy overrides. Verify the account and authentication mode actually selected by the client. An authentication failure must not trigger another provider.

Before every job, query the pinned client's JSON authentication status in the same controlled environment and refuse any method other than the expected subscription method. This corroborates routing; it does not prove overflow is disabled. Disable automatic client updates and gate explicit version changes on the auth/isolation suite. [CLI status and version controls](https://code.claude.com/docs/en/cli-reference).

Claude can continue on paid usage credits when enabled, including from Claude Code. Disable extra usage at account level or prove an enforceable worker-specific boundary. A spending cap above zero bounds spending but does not meet a subscription-allowance-only promise. If interactive sessions need overflow and no worker-specific block exists, that conflict remains an explicit owner choice. CLI dollar estimates, including `--max-budget-usd`, timeouts, and turn limits are not authoritative billing caps. [Usage-credit behavior](https://support.claude.com/en/articles/12429409-manage-usage-credits-for-paid-claude-plans), [Client budget estimate](https://code.claude.com/docs/en/cli-reference).

Codex remains the second adapter. Its documented account-auth CI path is for trusted private infrastructure, with serialized use and persistence of refreshed credentials; public/open-source targets are outside that pattern. Business/Enterprise Codex access tokens may be an alternative only if Franz has those entitlements. No upgrade purchase is proposed. Purchased-credit overflow controls must be checked separately. [Codex CI authentication](https://learn.chatgpt.com/docs/auth/ci-cd-auth), [Workspace access tokens](https://learn.chatgpt.com/docs/enterprise/access-tokens), [Usage and credits](https://learn.chatgpt.com/docs/pricing).

Prove clean-environment unattended execution and credential reuse. Simulate quota/auth errors rather than deliberately exhausting the account. Validate the pinned client's structured signals for quota, authentication, and transient transport failures; a nonzero exit alone is not a quota signal. Unknown errors become visible terminal failures. Only recognized transient transport errors receive bounded retries, and quota wait needs a controlled retry/reset policy. Observe billing/usage after the probe, accounting for reporting delays. Quota exhaustion queues or defers work; it never silently enables paid overflow.

## First implementation, conditional on those gates

Use a supervised private service, one durable SQLite ledger, one inference job at a time, and a separate deterministic publisher. Use an isolated environment on mini only after its virtualization and operations readiness are verified. No public webhook ingress, Vercel plan, target-repository workflow, or Actions runner is necessary for this initial design.

The controller periodically discovers outstanding requests and reconciles request events. Start discovery on a small explicitly allowed repository set. Prefer stable repository, installation, actor, and event IDs over mutable names. Resolve current pending state and the latest relevant request/removal sequence; ambiguous provenance defers the job. Do not infer authorization from PR authorship or from current requested-reviewer membership alone. Once a requested review is complete, later pushes do not authorize another review; a fresh native request is required.

A provisional 60-120 second poll interval is a target for a small pilot, not a guarantee across the whole organization. Paginate, use conditional requests where supported, account for search indexing lag and rate limits, and reconcile directly against repository state. Never rely solely on search results for correctness. On startup, enumerate current pending requests and reconcile unfinished local jobs; do not discard requests because their timestamps predate the restart. An exhausted API budget pauses discovery without marking requests completed.

If polling cannot reliably expose the required bot/requester state, stop and reassess the trigger design. Webhooks plus reconciliation are a possible later choice. GitHub does not automatically redeliver failed webhooks, so webhooks alone do not provide offline recovery. [Failed-delivery behavior](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries).

## Gate 3: concrete process and credential separation

The trusted controller collects GitHub metadata and immutable Git objects. The model process receives no GitHub token or App key. The publisher owns the write credential and accepts only a controller-owned job ID plus validated findings. It resolves the repository, PR, revision, and identity from the trusted ledger, never from model-authored destinations.

Run each inference job in a disposable guest from a pinned, audited image, with only the chosen provider credential and the job input. Do not expose the host home, SSH agent, Docker socket, fleet/LAN/Tailscale services, cloud metadata, unrelated files, or writable shared caches. Network policy permits required provider traffic and the one job-local source-reader connection described below. The exact virtualization mechanism and endpoint allowlist are implementation proof items; a rootless container by itself is not claimed to supply this boundary.

Start the official client in an empty trusted directory, not a target checkout. Supply approved policy explicitly. Preserve reviewed repository configuration as inert source data; do not delete it and then claim full coverage. Collect source directly from immutable Git objects rather than following repository symlinks or executing checkout hooks, submodule setup, dependency installation, or package scripts.

The first auth/isolation probe exposes no model-callable tools. For the quality pilot, remove the CLI's built-in file, shell, browser, and other capability tools. The only source-access capability is a trusted MCP reader/search service under a separate OS identity, confined to that job's immutable Git objects. The controller launches it independently with a clean environment before injecting the provider credential into the CLI. The CLI connects to its job-local endpoint; do not spawn it as a child inheriting the CLI's credential-bearing environment. The reader has no provider or GitHub credentials, no general network egress, and no cross-job access. Bound object/path selection, line ranges, output sizes, and calls. The CLI's model cannot read arbitrary guest files or paths through this service. Any remaining protocol-only tools must be inventoried and proven unable to read files, execute code, or contact other services.

Only the controller-approved reader may enter the strict MCP configuration. Exact compatibility of its transport and the pinned client's restricted/customization-suppression settings remains a Gate 3 test. A tool-poor review that fails the quality gate is not declared adequate just because it is isolated.

The later Codex adapter requires a separate protected authentication-state store with serialized lease, refresh write-back, and recovery outside disposable job state. It must not copy an old seed over refreshed credentials or permit overlapping users of the same credential copy. That adapter is outside the initial Claude pilot.

Claude's ordinary non-bare `-p` mode can execute repository hooks and connect MCP servers. Bare mode conflicts with subscription-token authentication. Pin the client version and verify an explicit configuration that suppresses untrusted customizations and hooks, constrains the actual tool set and MCP configuration, and denies unanswered permission requests. Current safe/restricted modes are candidate controls, not a substitute for OS enforcement or a tested command combination. Managed policies also require auditing. [Programmatic execution](https://code.claude.com/docs/en/headless), [CLI controls](https://code.claude.com/docs/en/cli-reference), [Authentication](https://code.claude.com/docs/en/authentication).

Use hostile fixtures to attempt hooks, MCP startup, helper/env overrides, credential and canary reads, symlink escapes, shell execution, cross-job access, and forbidden network access. Require zero unauthorized access/execution. Repeat the auth and isolation suite before each CLI/image/tool-policy upgrade. Provider calls remain cloud inference; no local GPU is implied.

## Gate 4: revisions, publication, and recovery

Persist a request generation and record repository/installation IDs, request-event ID, requester ID, head SHA, base tip SHA, merge-base SHA, model/client version, and policy version. The three-dot diff uses the captured merge base; integration context uses the captured base tip. Record omitted files, truncation, missing history, binary data, and other coverage limits.

Model output contains findings and coverage, not an executable publication instruction. Validate schema, sizes, source paths, anchors, and evidence; remove duplicate findings. Render controlled GitHub Markdown. Block generated mentions, remote images, raw HTML, and unapproved links. A verified finding that cannot anchor may be summarized with a clear file reference; malformed output is an error, not a clean review.

Immediately before posting, the single publisher checks current access, request generation, cancellation, open PR state, head, and base tip. Known changes discard unpublished results. Coalesce to the latest eligible request with bounded retries; repeated movement defers the review. Track deferral count and reason in local operational status and on GitHub when permissions permit. Measure whether base-tip invalidation starves reviews before considering any relaxed policy. Use COMMENT reviews only, no automatic approval or required-check gate.

GitHub accepts a review commit SHA but provides no documented atomic compare-and-publish condition for both PR revisions. A push or cancellation can race an already dispatched API request. The honest contract is snapshot-bound publication with checks before sending and reconciliation afterward. Clearly identify the captured head and base. Record supersession durably in local operational status. When permissions permit, update the existing review summary while retaining its marker and captured revisions. Access loss may prevent a GitHub annotation. Do not promise that a stale review can never briefly appear. [Review API](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request), [Update a review](https://docs.github.com/en/rest/pulls/reviews#update-a-review-for-a-pull-request).

Persist an outbox entry and stable job marker before making a review POST. After a response, record GitHub's review ID. After a crash/timeout, reconcile the selected identity's reviews and markers before any retry. If the first POST's outcome is ambiguous, keep the job in `publication_unknown` and do not blindly send another. A pending-review staging experiment may inform implementation, but it is not presumed to be a transaction or native idempotency mechanism.

Cancellation advances the durable generation and removes publication eligibility. Only the publisher can post, and it checks its generation immediately before dispatch. Kill workers and reject late results. A cancellation arriving after dispatch cannot retract that request; post-dispatch reconciliation must record supersession locally and annotate GitHub when authorized. Request removal, PR closure, access loss, identity suspension, and uninstall each have explicit states. Distinguish quota wait, authentication failure, invalid output, partial coverage, cancellation, and confirmed publication.

Explicitly test request A starting, A being removed, request B arriving, and A's already dispatched review completing. GitHub's API does not bind a submitted review to a request generation. Determine whether A can consume B's pending request. Never mark B fulfilled just because pending membership disappeared or an older review exists; retain B unresolved and pause for reconciliation if attribution is ambiguous.

Fault tests cover repeated discovery, request/remove/re-request including the A/B race above, interrupted collection, crashes around POST and response persistence, delayed visibility of a posted review, head/base movement, in-flight cancellation, hour-long downtime, and permission/identity loss. Require no blind retry after an uncertain write, no new dispatch after cancellation known to the publisher, and recovery of still-pending authorized requests. Do not advertise exactly-once behavior beyond tested and documented bounds.

## Gate 5: demonstrate useful quality and sustainable usage

Use a held-out set of at least 20 representative historical PRs, with at least 10 clean controls and an independently adjudicated list of material defects. Freeze those labels before evaluating the final prompt. Do not tune on the held-out set or count unverified model allegations as defects.

Provisional targets, not measured results: at least 90% of published findings actionable, at least 70% recall on designated material defects, no false finding on at least 90% of clean controls, valid source references throughout, and explicit incomplete coverage. Report counts and denominators, including when too few findings make precision inconclusive. Small-sample success supports a limited pilot, not commercial parity.

Measure end-to-end and inference latency separately, queue delay, context/tool use, quota consumption where exposed, and estimate uncertainty. A provisional p50 under 10 minutes and p95 under 20 minutes applies when quota is available and mini is online; deferred cases must remain visible. Do not claim a reviews-per-week capacity from dollar estimates or simulated quota errors. Measure subscription use over a limited pilot while protecting normal interactive capacity.

Start with one private repository, one provider, and Franz as requester. After evaluation, a two-week owner-only pilot tests real usefulness, recovery, and quota pressure. Expand only within the originally accepted owner-operated scope. Any automated-on-push mode, public-target adapter, second provider, executable tests, or shared-user access is a separate later decision.

## Remaining choices and proof status

No model agreement establishes GitHub eligibility, subscription entitlement, account-level billing settings, virtualization readiness, security isolation, or review quality. All remain untested in Franz's accounts/environment.

The immediate next implementation step, if separately authorized, is the model-free native App identity experiment. If it fails, Franz can retain the exact App requirement and pursue GitHub eligibility, explicitly accept a machine account, or choose a different trigger. That choice is not required merely to finish this plan review.
