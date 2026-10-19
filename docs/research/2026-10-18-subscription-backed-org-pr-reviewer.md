# An organization PR reviewer using personal coding subscriptions

Research date: October 18, 2026 EDT.

The [adversarial alpha plan](2026-10-19-adversarial-alpha-plan.md) and published [specification issue](https://github.com/hemsoft-dev/set-it-free-loop/issues/172) define the current dual-provider v1 scope. They supersede this report's initial Actions-first recommendation and the intermediate Claude-first plan. The source findings below remain a research snapshot.

Scope: a personal GitHub App for `hemsoft-dev`, reviewing the owner's private repositories with the owner's Codex or Claude subscription. This is research, not authorization to restore SFL. No App installation, credentials, workflow, runner, or deployment was created or reactivated.

## Finding

The review engine is feasible using documented subscription authentication. The exact interaction of choosing a new custom App in GitHub's **Reviewers** picker remains unproven. Organization ownership and installation do not establish that eligibility.

OpenAI documents account authentication on trusted private CI runners. Anthropic documents subscription tokens for its official GitHub Action. These are supported-client paths rather than a proposal to turn consumer website sessions into an unofficial API. Both consume the account's allowance. Neither implies unlimited reviews or entitlement to run a shared commercial service. [Codex CI authentication](https://learn.chatgpt.com/docs/auth/ci-cd-auth), [Claude GitHub Actions](https://code.claude.com/docs/en/github-actions), [Claude credential rules](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use).

The recommended design is one organization App, one private controller repository, a trusted worker on an isolated self-hosted runner, official provider clients, and a separate deterministic GitHub publisher. Target repositories need no workflow files. This is a proposed composition of the documented interfaces below, not a deployed or tested system.

## Subscription feasibility

### Codex

The official advanced CI guide describes a persistent self-hosted Actions runner, a saved `auth.json`, refresh performed by Codex itself, and persistence of the updated file. A credential copy must have a single machine or serialized job stream. Replacing it from an old secret every run discards refreshed credentials. The guide recommends API keys for ordinary automation and expressly excludes public/open-source repositories from this account-auth CI pattern. A private controller does not establish an exception for public review targets. [Advanced CI guide](https://learn.chatgpt.com/docs/auth/ci-cd-auth).

`codex exec` reuses CLI authentication and supports schema-constrained final output, suitable for producing findings for a separate publisher. This is different from assuming the stock `openai/codex-action` accepts a subscription: that Action's documented integration uses an API key and a Responses proxy. A subscription worker should invoke the official CLI through the documented account-auth path. [Non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode), [Codex GitHub Action](https://learn.chatgpt.com/docs/github-action).

A custom CLI review consumes general Codex usage. It does not become native GitHub Code Review usage merely because our App later posts the result on a PR. Remaining capacity depends on the actual account, model, context, and work performed; this research did not inspect Franz's allowance. [Codex pricing and usage](https://learn.chatgpt.com/docs/pricing#what-counts-as-code-review-usage).

### Claude Code

The official Action accepts `claude_code_oauth_token` backed by Pro, Max, Team, or Enterprise subscriptions. Its organization rollout guidance recommends API authentication for credentials shared across repositories because OAuth is tied to the individual who created the token. For this proposal, the important boundary is an owner-operated reviewer for the owner's work, not a pooled entitlement for other people. [Claude GitHub Actions](https://code.claude.com/docs/en/github-actions).

`claude setup-token` uses Anthropic's authorization flow and generates a one-year subscription token for CI/scripts, supplied as `CLAUDE_CODE_OAUTH_TOKEN`. The official client can use it without an interactive browser per review. API credentials can take precedence, so the worker must verify its selected authentication mode and exclude accidental API/provider credentials. Token rotation is still required; a one-year validity period is not a service guarantee. [Claude authentication](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token).

Anthropic distinguishes an end user signing into the unmodified Claude Code binary, including hosted execution, from developers collecting Claude credentials or routing users' requests through personal subscription credentials. A future multi-user product requires a separate supported authentication/billing design. This personal architecture should not be described as permission to share a single subscription across an organization. [Authentication and credential use](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use).

### Existing hosted alternatives

Native Codex GitHub review already supports connected repositories, automatic review, `@codex review`, repository guidance, and ordinary GitHub review output. It is the smallest operational solution if a custom App identity and custom review pipeline are optional. It does not prove the requested custom-App assignment experience. [Codex GitHub integration](https://learn.chatgpt.com/docs/third-party/github).

Anthropic's hosted **Code Review** is a separate Team/Enterprise product with review and verification agents. Its usage is charged through organization usage credits separately from included plan usage. Do not confuse that service with running Claude Code yourself using a subscription token. [Hosted Code Review](https://code.claude.com/docs/en/code-review), [Code Review costs](https://code.claude.com/docs/en/code-review-costs).

## GitHub integration and the assignment requirement

There are three separate capabilities:

1. Installing an App across an organization's repositories.
2. Posting an App-authored review on a PR.
3. Selecting the App as a requested reviewer in GitHub's native picker.

GitHub explicitly supports installation on all or selected repositories. Its review REST API accepts installation tokens with Pull requests write permission, review text, inline comments, and a reviewed commit SHA. These establish the first two capabilities. [App installation](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app), [Create a PR review](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request).

GitHub's GraphQL schema includes `botLogins` in `RequestReviewsByLoginInput`, `botIds` in `RequestReviewsInput`, and bot-capable reviewer suggestions. These prove that bot review requests exist, but the public schema does not specify the enrollment criteria for arbitrary installed Apps. [GraphQL pull-request schema](https://docs.github.com/en/graphql/reference/pulls#requestreviewsbylogininput).

GitHub explicitly documents requesting Copilot in the Reviewers sidebar. That first-party integration does not establish an equivalent contract for a newly registered custom App. [Copilot review requests](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/use-code-review#using-copilot-code-review).

The existing [SFL reviewer-eligibility research](github-app-reviewer-eligibility.md) records this same unresolved issue. It is historical evidence, not proof of today's App state. The retired registrations must not be assumed to remain installed or available.

If native assignment is required, the first future experiment must demonstrate picker visibility, a persisted Bot `ReviewRequest`, a timeline event, delivery of `review_requested`, and an App-authored review on the captured commit. A successful HTTP/GraphQL response alone is insufficient. If the picker or request fails, the next step is a GitHub Support inquiry with that evidence. Do not fund a larger build on an assumption that moving repositories or the App into an organization unlocks it.

The intended trigger is **request review**, not the separate **Assignees** field. An App can react to a review request only after GitHub permits the request. An owner-only PR command or label is a workable alternative trigger if explicitly accepted later, but would not satisfy the exact native-assignment requirement. [Workflow activity types](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request).

## What commercial reviewers actually do

The inspected product documentation describes these public interfaces. It does not reveal every internal model, prompt, or infrastructure decision.

| Product | Documented operation | Lesson for a personal reviewer |
| --- | --- | --- |
| Cursor Bugbot | Connected repositories; automatic reviews; `cursor review` and `bugbot run` comments; inline findings and a check; configurable effort and incremental review | Separate the App trigger, review engine, and GitHub output. It uses Cursor billing, not a documented bring-your-own Claude/Codex subscription flow. [Bugbot documentation](https://cursor.com/docs/bugbot) |
| CodeRabbit | Organization App installation; automatic and comment-triggered review; cloud sandboxes, full repository context, static analysis, review and verification agents | Borrow verification and repository context rather than just the comment format. [Installation](https://docs.coderabbit.ai/platforms/github-com), [Commands](https://docs.coderabbit.ai/reference/review-commands), [Architecture](https://docs.coderabbit.ai/overview/architecture) |
| Greptile | Organization installation, automatic triggers, manual `@greptileai` comments, repository dependency context, and feedback | Context and feedback are first-class components. Commercial self-hosting still needs a license and configured model providers. [Quickstart](https://www.greptile.com/docs/quickstart), [Developer guide](https://www.greptile.com/docs/code-review/developer-essentials), [Overview](https://www.greptile.com/docs/introduction), [Deployment](https://www.greptile.com/docs/deployment-options) |
| Replit | Documented GitHub contribution flow imports a repository, edits/tests it, and opens a PR | This is adjacent authoring functionality. I did not locate an equivalent organization-wide reviewer contract or reviewer-picker trigger. Replit's own AI credit billing does not document reuse of a separate coding subscription. [GitHub workflow](https://docs.replit.com/use-cases/ship-github-change), [AI billing](https://docs.replit.com/billing/ai-billing) |
| Community PR-Agent | CLI, Actions, and separately hosted GitHub App deployment, with centrally configured review behavior and API-backed providers | Useful open-source reference for plumbing and output. It is now a community-maintained legacy project separate from Qodo's commercial product; it does not document the desired subscription adapter. [Current repository](https://github.com/The-PR-Agent/pr-agent), [GitHub deployment](https://docs.pr-agent.ai/installation/github/) |

None of the inspected Bugbot, CodeRabbit, or Greptile trigger guides promises that selecting an arbitrary new GitHub App in the native reviewer picker will work. Their documented manual contracts are comments. PR-Agent's ability to subscribe to `review_requested` likewise does not establish eligibility for its own bot.

Cursor's engineering account describes independent candidate passes, grouping, validation, duplicate suppression, and later agentic code exploration. Its historical pass counts should not be treated as the current implementation. [Building Bugbot](https://prod.cursor.com/blog/building-bugbot).

Commercial presentation is reproducible through GitHub Markdown: CodeRabbit documents grouped change walkthroughs, collapsible details, diagrams, and links to related work, alongside inline findings. We can adopt that vocabulary while keeping our own output concise. [CodeRabbit walkthroughs](https://docs.coderabbit.ai/pr-reviews/walkthroughs).

Old Bugbot $40-per-seat comparisons are stale. Cursor announced usage-based billing in May, then further cost changes in June. A historical average is not a current per-review quote. This report does not assume a fixed October rate. [May pricing change](https://cursor.com/blog/may-2026-bugbot-changes), [June update](https://cursor.com/blog/bugbot-updates-june-2026).

## Proposed execution design

The following is engineering judgment based on the cited APIs:

1. **Organization App.** Install once on approved `hemsoft-dev` targets. Receive PR/review-request events, and optional owner-only comment commands. Keep the App signing key in the controller/publisher, outside the reviewing agent.
2. **Webhook receiver and queue.** Verify delivery signatures and independently authorize the requester, installation, repository, PR state, and configured provider. Deduplicate delivery IDs and capture both head and base SHAs. Public HTTPS ingress is necessary for a push webhook; an ordinary private fleet address is not by itself reachable from GitHub. A polling controller is an alternative with additional latency.
3. **Private controller repository.** Dispatch a centrally maintained workflow at a trusted revision. Pass identifiers and revisions as validated data, not interpolated shell commands. Do not execute a target PR's workflow definition. Only this repository may schedule the sensitive runner.
4. **Official-client worker.** Fetch the exact revisions into a disposable workspace and collect relevant source, PR intent, and approved repository guidance. Run the chosen Codex or Claude client. Start with one provider and serialize its account state. Credentials and quota exhaustion must stop the job, without silently selecting an API key or another paid provider.
5. **Finding verification.** Check candidate issues against surrounding source and remove unsupported or duplicate findings. Produce structured output containing file/range, severity, impact, evidence, suggested fix where justified, and coverage limits.
6. **Separate publisher.** Validate schema, path/range locations, installation scope, and current PR head/base. Reject stale results. Mint a narrowly scoped App token and submit one review on the reviewed commit, with grouped inline comments and a useful summary. Handle retries idempotently and expose incomplete/error status distinctly from a clean review.

GitHub supports dispatching a workflow with an App installation token and Actions write permission. The built-in `GITHUB_TOKEN` belongs to the controller repository; it does not grant organization-wide access to target repos. Use separate scoped dispatch and target-publication credentials as needed. Granting Actions write to the review App everywhere merely to dispatch the controller would be broader than necessary. [Workflow dispatch API](https://docs.github.com/en/rest/actions/workflows#create-a-workflow-dispatch-event), [GITHUB_TOKEN scope](https://docs.github.com/en/actions/concepts/security/github_token).

GitHub Actions is optional. The queue could launch the same official-client worker directly on a dedicated service. Actions adds logs, cancellation, scheduling, and run visibility; it does not provide subscription entitlement. Neither architecture inherently needs Vercel or Vercel Pro.

## Execution and credential boundaries

Self-hosted does not mean safe. GitHub warns that private-repository PR authors can also compromise persistent runners through untrusted execution, and that compromise can cross repositories. Runner access should therefore be limited to the trusted controller, not all target repositories. Use a dedicated OS identity or VM with no unrelated fleet credentials. [GitHub self-hosted runner security](https://docs.github.com/en/actions/reference/security/secure-use#hardening-for-self-hosted-runners).

Treat PR contents, descriptions, repository instructions, hooks, MCP configuration, and package scripts as input to inspect. They must not change controller policy or automatically run in the credential-bearing environment. A read-only checkout alone does not stop an agent from reading account credentials. The worker needs a demonstrated boundary around its credential storage and tools. If tests are needed, execute them in a separate disposable environment without subscription credentials, the App key, or a review-publishing token. The exact isolation mechanism remains implementation work, not a property proven by this report. [OpenAI automation credential guidance](https://learn.chatgpt.com/docs/non-interactive-mode#authenticate-in-automation).

Webhook authenticity also does not establish requester authorization: check both, and recheck repository installation state before publishing. [GitHub webhook validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries).

## Review quality and presentation

Recommended output is one concise review with an outcome, changes covered, risk/coverage notes, and evidence-backed inline findings. Each finding should explain a concrete failure and its impact at the smallest useful range. Include a suggested patch only when it is supported. A clean result must say what was inspected, while an incomplete run must not claim the PR passed.

The engineering work that matters is collecting enough repository context, verifying candidates, retaining useful repository rules, handling new commits, and measuring noise. Adding a polished Markdown summary alone will not reproduce a commercial reviewer's quality.

A personal first version should avoid automatic Approve/Request changes gates. Publish COMMENT reviews and an informational execution result until measured evidence supports stricter behavior. This is a scope recommendation, not a GitHub API limitation.

## Costs and capacity

- **Inference:** consumes the chosen existing subscription allowance. The number of usable reviews is variable, and reviews compete with interactive coding. No fixed PRs-per-month claim is supported for Franz's accounts.
- **Compute:** current GitHub documentation lists self-hosted runner usage as free; the machine's operation remains the owner's cost. Hosted private-repository jobs consume the organization's included minutes and incur additional charges beyond them. Artifact/cache storage is a separate consideration. [Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions).
- **Hosting:** a dedicated existing machine may host the receiver, queue, and runner. A public ingress arrangement and its availability still need selecting. A paid website-hosting plan is not an inherent requirement of this design.
- **Capacity controls:** start with one queued review at a time, bounded context, timeouts, cancellation on superseding revisions, and an explicit quota-wait state. More runners do not increase a subscription's allowance.
- **Billing rule:** subscription mode must never silently switch to API, purchased credits, or another provider. Extra usage needs an explicit owner choice.

## Proposed proof before a build

These are future acceptance criteria, not completed tests:

1. **Native identity:** prove reviewer-picker selection and actual request delivery. If this is impossible under available GitHub access, report the requirement as unmet before implementing the full service.
2. **Subscription execution:** on one representative private test repository, prove the selected account and provider, unattended completion, credential renewal/reuse, and stop-on-quota behavior. Record quota changes without exposing tokens.
3. **Publication integrity:** prove the App identity, expected SHA, valid inline anchors, duplicate-delivery handling, and rejection of superseded head or base revisions.
4. **Useful reviews:** evaluate a small set of historical PRs with known defects and clean controls. Measure missed known defects, false positives, duplicates, latency, and quota use. Judge presentation separately from correctness.
5. **Isolation:** prove the reviewing agent and optional test process cannot read publisher credentials or unrelated host secrets. Demonstrate that PR-controlled configuration cannot broaden tool access.
6. **Organization coverage:** only then extend repository access, preserving central workflow ownership and per-provider policies. Public targets require a separately supported route rather than a silent extension of Codex's private CI account-auth pattern.

No inference calls, credential inspections, GitHub write experiments, account billing checks, or runner readiness tests were performed. The remaining uncertainties are the custom App's native reviewer eligibility, the actual subscription entitlements and quotas, deployment readiness, and empirical review quality.
