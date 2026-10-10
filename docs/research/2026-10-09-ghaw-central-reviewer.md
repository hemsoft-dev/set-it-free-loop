# GHAW feasibility for the organization SFL PR reviewer

Researched October 9, 2026 EDT. Read-only research; no service, App, workflow, authentication, repository or
PR configuration changed. Claims below distinguish documentation, inspected implementation, and proposed
design. Provider subscription evidence follows the GHAW implementation findings.

## Evidence baseline

The latest published release returned by GitHub's release API was **v0.89.21**, published **September 23, 2026
at 4:29 PM EDT**, commit **c35393777e5604a63721d09512263b1383301d4f**. Current `main` was pinned separately at
**0ad7d36767de4d80c6632d6006783ac94ae5f8b4**, commit date **October 9, 2026 at 9:02 PM EDT**. Both revisions
were cloned locally for source inspection. The published documentation includes newer capabilities absent from
the latest release; especially Agy. A production decision must specify the compiler revision rather than
assume the website describes the installed release.
[Release](https://github.com/github/gh-aw/releases/tag/v0.89.21), [release
commit](https://github.com/github/gh-aw/commit/c35393777e5604a63721d09512263b1383301d4f), [inspected
main](https://github.com/github/gh-aw/commit/0ad7d36767de4d80c6632d6006783ac94ae5f8b4).

## Main finding

**GHAW fits the central execution and security infrastructure, but its native integrations do not provide a
universal "use my existing subscriptions" switch.** Provider independence is achievable by retaining one
central SFL review contract and supplying multiple engine adapters. Native Codex, Claude and Agy currently
choose API credentials or supported Copilot inference; they do not reuse the owner's normal subscription
login. Cursor has an imported sample rather than a supported native engine. Grok CLI has no built-in
integration in the inspected revision. Subscription-backed adapters would be additional maintained work,
subject to each provider's permitted automation and billing terms. [Engine
overview](https://github.github.com/gh-aw/reference/engines/), [Codex
setup](https://github.github.com/gh-aw/engines/codex/), [Claude
setup](https://github.github.com/gh-aw/engines/claude/), [custom engine
guide](https://github.github.com/gh-aw/reference/third-party-agent/).

## Engine and credential support

| Desired provider/runtime | Current GHAW route | Existing subscription reuse by that route |
|---|---|---|
| Codex | Built-in `codex`; OpenAI API key, or `copilot/` inference through GitHub | Native integration explicitly does not configure ChatGPT subscription login |
| Claude Code | Built-in `claude`; Anthropic API key/WIF, or `copilot/` Anthropic model | Claude subscription OAuth explicitly unsupported |
| Antigravity | New experimental built-in `agy` on inspected main; verified Linux x64 v1.3.1 | Gemini API key only; fresh per-run profile, no inherited user login |
| Cursor | Imported `.github/workflows/shared/cursor.md` sample using Cursor Agent CLI | Uses `CURSOR_API_KEY`; account entitlement is a Cursor question, and the sample has no GHAW support commitment |
| Grok | No native Grok CLI engine/sample found; custom engine possible | No implemented Grok subscription route; API-compatible models are a separate option |
| GitHub Copilot | Built-in default, also inference backend for Claude/Codex/Pi | Supported Copilot entitlement/quota or organization credits; does not consume a Claude or ChatGPT subscription |
| Pi | Built-in multi-provider agent | Supported paths still configure provider keys or Copilot; Pi's own authentication breadth does not imply GHAW forwards every login |

The supported stable built-ins in the release are Copilot, Claude, Codex, Gemini and Pi. Main adds
experimental Agy and soft-deprecates Gemini while retaining it for WIF and unsupported Agy capabilities.
[Pinned current engine
matrix](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/docs/src/content/docs/reference/engines.md),
[release
matrix](https://github.com/github/gh-aw/blob/c35393777e5604a63721d09512263b1383301d4f/docs/src/content/docs/reference/engines.md).

**Actual Codex implementation:** generated execution sets `CODEX_API_KEY` and `OPENAI_API_KEY` from the
configured secret expression; the harness aborts before launching Codex when both are absent. The sandbox
config selects an AWF Responses proxy with `env_key = CODEX_API_KEY` and `requires_openai_auth = false`.
Copying `auth.json` alone therefore does not enable native subscription execution. This is an implemented
constraint, not merely missing documentation. [Execution
environment](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/codex_engine.go#L462),
[harness credential
preflight](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/actions/setup/js/codex_harness.cjs#L854),
[managed proxy
config](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/codex_config.go#L103).

**Actual Claude implementation:** non-Copilot execution supplies `ANTHROPIC_API_KEY`. CLI secret bootstrap
explicitly rejects a detected `CLAUDE_CODE_OAUTH_TOKEN` when its supported API credential is missing. The
authentication documentation says a repository OAuth secret is ignored. This does not establish that
Anthropic's own GitHub Action cannot use OAuth; it describes GHAW's integration. [Compiler
environment](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/claude_engine.go#L536),
[explicit bootstrap
rejection](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/cli/engine_secrets.go#L342),
[GHAW authentication](https://github.github.com/gh-aw/reference/auth/#anthropic_api_key).

**Actual Agy implementation:** private temporary home, settings `modelProvider: gemini`, removal of ambient
Google credential variables, required Gemini API key or AWF endpoint, and no direct fallback when endpoint
discovery fails. Native authentication has passed upstream gates; full production conformance remains pending.
It is not yet equivalent to Gemini, supports one verified CLI release, and rejects shell restrictions such as
`bash: false` instead of claiming to enforce them. [Agy
reference](https://github.github.com/gh-aw/engines/agy/), [fresh credential
profile](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/actions/setup/js/agy_harness.cjs#L74).

Cursor's sample invokes headless CLI with model/MCP support and a pinned, checksummed installer. It requires
`CURSOR_API_KEY`, which is a Cursor credential rather than proof of direct model API billing. The sample is
explicitly unsupported by GHAW. [Pinned
sample](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/.github/workflows/shared/cursor.md).
Its Cursor key is available inside the agent execution environment: generic behavior-defined engines
deliberately remove declared auth-binding secrets from the AWF exclusion list. Network allowlisting remains,
but this is a weaker credential boundary than built-in proxy-held provider keys. A production Cursor adapter
must address that explicitly. [Generic engine credential
handling](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/behavior_defined_engine.go#L707).

Pi can route additional compatible providers through an explicit inference family and endpoint. Its native
unsandboxed provider map includes `xai` → `XAI_API_KEY`. That enables API access prospects, not Grok
subscription login. Disabling the sandbox for otherwise incompatible native providers removes credential
isolation and is not the recommended path. [Additional Pi
providers](https://github.github.com/gh-aw/engines/pi/#additional-providers), [provider
implementation](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/pi_provider.go).

## Choosing engines and models yourself

Engine identity is resolved at **compilation**, determining installation, credential, network and execution
steps. A runtime input cannot simply replace `engine.id` with an arbitrary provider inside one previously
compiled workflow. Model values can use GitHub Actions expressions and runtime variables, but
provider/authentication selection remains explicit; changing a model prefix at runtime does not automatically
change credentials. [Compiler engine
resolution](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/compiler_yaml_ai_execution.go#L439),
[runtime model variables](https://github.github.com/gh-aw/reference/environment-variables/#model-overrides),
[Codex provider expression
caveat](https://github.github.com/gh-aw/engines/codex/#selecting-codex--github-as-the-ai-engine).

**Proposed implementation:** one request with trusted `provider`, optional `model`, repository, PR number and
immutable revision selects one of several precompiled central workflows. All variants import the same review
instructions and output schema. The caller chooses the provider explicitly; no silent switch to another paid
provider. A custom engine can also implement a controlled selector, but then SFL owns its credentials, tool
policy, timeout/retry semantics and session parsing.

Custom definitions register through imports without modifying the GHAW binary. Built-in command/harness
overrides are additional extension points, but an override alone does not change supported credential
validation or AWF billing. Subscription adapters require explicit validation of the complete execution path.
[Custom
definitions](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/docs/src/content/docs/reference/third-party-agent.md),
[custom harness caveats](https://github.github.com/gh-aw/reference/engines/#custom-harness-script-harness).

## Central organization App, no workflows in target repositories

GHAW documents central-repository workers, external-repository checkout and cross-repository safe outputs.
`workflow_dispatch` and `repository_dispatch` can start a central workflow through an authenticated API call.
Therefore **target repositories do not need SFL workflow files or provider secrets** when the organization App
supplies the trigger bridge. A central repository's ordinary PR/comment workflow triggers do not receive
events originating in hs-buddy; the App must authenticate the request and relay it. App-triggered repository
dispatch has explicit bot allowlisting considerations.
[CentralRepoOps](https://github.github.com/gh-aw/patterns/central-repo-ops/), [dispatch
triggers](https://github.github.com/gh-aw/reference/triggers/#repository-dispatch-trigger-repository_dispatch),
[cross-repository access](https://github.github.com/gh-aw/reference/cross-repository/).

Proposed flow: authorized request on hs-buddy → SFL App webhook/queue on mini → selected central GHAW worker →
review of fixed head/base → validated findings → separate SFL App publisher. This is compatible with the
user's intended independence from native Codex reviews. Requesting an App in GitHub's reviewer dropdown is a
separate identity/UX question; GHAW does not create that identity or implement the bridge automatically.

GHAW can mint scoped GitHub App installation tokens for reads, checkout and safe outputs. Cross-repository
`submit-pull-request-review` and inline review comments support configured `target-repo`/`allowed-repos`;
unrestricted wildcard targets are excluded for these review types. `commit-id` can pin submitted reviews to
the revision actually inspected. [App
authentication](https://github.github.com/gh-aw/reference/auth/#using-a-github-app-for-authentication),
[review
handler](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/actions/setup/js/submit_pr_review.cjs#L110),
[review commit
pinning](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/actions/setup/js/pr_review_buffer.cjs#L317).

**Important SFL integrity gap:** built-in `create-check-run` uses `context.repo.owner/repo`, not a
cross-repository target. With `target` configured it fetches the PR's latest head at publication, rather than
enforcing the reviewed SHA. Its config lacks `target-repo`. A central SFL check therefore needs a
deterministic custom safe-output publisher, or the existing central App publisher adapted for engine results.
That publisher must validate head/base/open status again and reject stale results. Review commit pinning does
not by itself enforce base preservation or cancellation. [Actual check
handler](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/actions/setup/js/create_check_run.cjs#L121),
[check
configuration](https://github.com/github/gh-aw/blob/0ad7d36767de4d80c6632d6006783ac94ae5f8b4/pkg/workflow/create_check_run.go),
[custom safe outputs](https://github.github.com/gh-aw/reference/custom-safe-outputs/).

## Mini, security and cost implications

Self-hosted execution is supported on **Linux with Docker**, not macOS/Windows. A local read-only check found
mini is x86_64 and has all five client commands on PATH. Docker was not on PATH, and the `gh aw` command
reported that the official extension is available to install. Runner readiness and CLI login state were not
tested. Configure agent, framework and safe-output runners explicitly; `require_self_hosted_runners` checks
every generated job. Using mini can avoid hosted compute consumption, but does not change inference billing.
Subscription credentials must not be exposed to untrusted PR code. [Self-hosted
requirements](https://github.github.com/gh-aw/reference/self-hosted-runners/).

GHAW's strengths are read-only GitHub access for agent jobs, network/sandbox controls, tool scoping, buffered
outputs, artifacts and separate scoped writers. Safe-output threat detection is automatically enabled but
defaults to `continue-on-error: true`; strict blocking must be deliberately configured. Custom/Pi detection
normally uses a separate supported engine, often Copilot, creating another entitlement dependency. [Security
architecture](https://github.github.com/gh-aw/introduction/architecture/), [detection
defaults](https://github.github.com/gh-aw/reference/threat-detection/), [Pi detector
authentication](https://github.github.com/gh-aw/engines/pi/#authenticating-threat-detection).

GHAW is free software; Actions compute and provider inference are separate costs. Personal subscription reuse
must be proven per adapter, including refresh persistence, rate limits, concurrent sessions and allowed
repository visibility. Nothing in this research demonstrates a live subscription-backed GHAW run. The existing
mini SFL health endpoint still reported `enabled: false`, `pending: 0`, and `dead_letters: 0`.
[Billing](https://github.github.com/gh-aw/reference/billing/).

## Subscription access through the providers' own clients

Research date: October 9, 2026 EDT. These findings describe documented client
capabilities, not successful authentication or billing verification of Franz's
accounts. No inference requests ran during this research.

### Codex

OpenAI documents ChatGPT subscription sign-in separately from Platform API-key
billing. `codex exec` can reuse saved CLI authentication. Its advanced CI guide
describes trusted private runners, letting Codex refresh credentials itself,
persisting the refreshed file, and serializing access to a credential copy.
However, that guide explicitly says, "Do not use this workflow for public or
open-source repositories." GitHub's read-only repository API confirmed that
both `hemsoft-dev/hs-buddy` and `hemsoft-dev/set-it-free-loop` are public on the
research date. Moving execution to mini or a private control repository does
not establish an exception for reviewing public targets. Therefore, do not
recommend cached ChatGPT authentication for this Actions pilot on the evidence
available. This is a documented usage limitation, not a claim that subscription
sign-in is technically impossible or that all local public-code work is banned.
[OpenAI authentication](https://learn.chatgpt.com/docs/auth),
[non-interactive
authentication](https://learn.chatgpt.com/docs/non-interactive-mode#authenticate-in-automation),
[advanced CI authentication](https://learn.chatgpt.com/docs/auth/ci-cd-auth).

### Claude Code

Anthropic's own GitHub Action supports `CLAUDE_CODE_OAUTH_TOKEN`, generated by
the owner through `claude setup-token`, on eligible subscription plans. Its
documentation explicitly distinguishes subscription usage from API billing.
For credentials shared across an organization, it recommends API authentication
because OAuth belongs to the individual subscriber. This does not establish
GHAW support for the same token.
[Claude Code GitHub Actions](https://code.claude.com/docs/en/github-actions).

Anthropic permits an end user to sign into the unmodified Claude Code binary
with their own subscription, including hosted execution. It restricts third-party
applications that intermediate Claude.ai credentials or route subscription
requests for their users. The plausible subscription design is Franz using his
own official client for his own work. A future multi-user SFL product offering
Claude subscription login needs a separate supported authentication design.
[Claude Code authentication and credential
use](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use).

### Cursor

Cursor documents a headless CLI for scripts, CI, and automated review. The CLI
works with the user's Cursor subscription. CI authenticates using a Cursor user
API key, `CURSOR_API_KEY`; this is an account credential for Cursor's client,
not a requirement to purchase an OpenAI or Anthropic Platform API key.
[CLI subscription announcement](https://cursor.com/en-US/blog/cli),
[CLI authentication](https://cursor.com/docs/cli/reference/authentication),
[headless CLI](https://cursor.com/docs/cli/headless).

Included usage still has limits and model-dependent consumption. Enabling
on-demand usage can create charges after the included allowance is consumed.
For a subscription-only pilot, verify the owner's account limits and keep
on-demand billing disabled. This research did not inspect or change those
settings.
[Cursor usage and limits](https://cursor.com/help/models-and-usage/usage-limits).

### Antigravity

Google now documents native `agy -p` headless execution, JSON output, cached
account credentials, and remote sign-in. An unauthenticated CI run fails instead
of opening an interactive sign-in prompt. The native CLI uses subscription
quota and can consume AI credits after the baseline quota. Google documents
controls to prevent that fallback. Gemini API-key mode is a separate explicit
provider selection. The native subscription path exists, but GHAW's experimental
Agy profile uses that separate API-key mode.
[Headless CLI](https://www.antigravity.google/docs/cli/headless/),
[installation and authentication](https://antigravity.google/docs/cli/install/),
[CLI credits](https://www.antigravity.google/docs/cli/credits/),
[plans and overages](https://www.antigravity.google/docs/plans).

### Grok

Grok Build now has an official coding CLI, `grok -p`, with headless output,
browser/device-code authentication, and refreshable sessions. Its enterprise
documentation recommends `XAI_API_KEY` for CI, while also documenting device
login for headless hosts. SuperGrok's included weekly pool covers Build.
Therefore, describing Grok as having no official headless client would be
incorrect. A subscription-backed CLI adapter is a candidate, but its unattended
GHAW integration, account eligibility, refresh behavior, and billing need proof.
There is no verified built-in GHAW Grok engine in this research.
[Grok Build](https://docs.x.ai/build/overview),
[headless execution](https://docs.x.ai/build/cli/headless-scripting),
[authentication](https://docs.x.ai/build/enterprise#authentication),
[SuperGrok usage pools](https://docs.x.ai/grok/faq#how-do-supergroks-weekly-usage-limits-work).

Grok's ordinary developer API billing is separate from Grok subscription
billing. Selecting a Grok model through a generic API adapter does not prove
the subscription pays for it.
[Developer account billing](https://docs.x.ai/developers/faq/accounts).

### Cost rule for SFL

Subscription access means consuming an existing allowance, not unlimited free
inference. Provider selection must also select the intended account and billing
mode. A quota/authentication error should stop and report that provider's failure.
Changing providers, consuming purchased credits, or falling back to API billing
must require an explicit owner choice. Model identifiers alone do not determine
whose subscription pays for a run.

## Recommendation and pilot acceptance

Adopt GHAW as a candidate central execution framework, retaining provider selection and publication as
SFL-owned interfaces. Do not deploy native API integrations under an assumption that existing personal
subscriptions cover them. The best first candidate is an SFL-maintained Cursor adapter because the official
client documents subscription use and CI authentication, and GHAW already supplies an integration sample.
Harden its credential boundary and prove account billing before treating it as production-ready. Test only
hs-buddy, share the same review contract across later adapters, and treat Codex subscription CI for public
targets as unresolved under current OpenAI guidance. This recommendation does not activate a pilot.

Pilot acceptance should prove: no target workflow/secret added; selected provider/model actually used;
authenticated subscription entitlement and refresh behavior; fixed head/base review; genuine seeded finding
and clean result; SFL App-authored publication; stale-head/base rejection; duplicate/cancellation recovery;
denied outside-pilot requests; and fail-closed behavior when subscription capacity is exhausted. These are
proposed requirements, not completed tests.
