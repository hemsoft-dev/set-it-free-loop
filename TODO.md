# HemSoft SFL Reviewer Roadmap

SFL is working in the HemSoft account. Treat the successful operation observed
on 2026-08-15 as the runtime baseline and preserve it. HemSoft intentionally
uses Kimi K3 through its private OpenRouter route and enforces the HemSoft owner
boundary; those are supported differences from the Relias implementation.

## Verified current state — 2026-08-16

- `v2.1.0-rc.9` contains the embedded Unslop communication pass and the
  corrected `sfl-app[bot]` reviewer identity.
- `HemSoft/hs-buddy` is the only approved HemSoft validation repository. Its
  manifest records `v2.1.0-rc.9` and source commit
  `77aa2059d98bad34a9bd5026e196820da8fb3725` after PR #417.
- Pilot PRs
  [#380](https://github.com/HemSoft/hs-buddy/pull/380),
  [#381](https://github.com/HemSoft/hs-buddy/pull/381), and
  [#417](https://github.com/HemSoft/hs-buddy/pull/417) received current-head
  App reviews, successful evidence and approval checks, and the required gate.
- The active `hs-buddy` default-branch ruleset requires
  `SFL Reviewer Gate Runner`. The runner verifies the App-authored
  `SFL Reviewer Approval` and `SFL Review Evidence` checks without replacing
  unrelated repository rules.
- Repository-scoped gate transactions, idempotency, and rollback support
  shipped in PR #74. Its CLI contract still refers to
  `SFL Reviewer Approval`, while the working pilot ruleset requires
  `SFL Reviewer Gate Runner`; issue #73 tracks that contract mismatch.
- The GitHub App is registered to the personal `HemSoft` account. The account
  owner confirms that its installation selects all repositories. That working,
  owner-confirmed model is not itself a migration task; independently capture
  the installation record before proposing scope hardening.
- The 41 obsolete `v2.0.0` deployment pull requests left by the August 1
  account-wide rollout are closed, and their `sfl/tier-review` branches are
  removed. They were never part of the approved `hs-buddy` validation path.

## Guardrails

- Preserve the working App installation, authentication, workflow, gate, and
  release until a narrowly scoped change has its own evidence and rollback.
- Run any HemSoft SFL lifecycle, recovery, assignment, or command test only on
  `HemSoft/hs-buddy`, using a controlled PR.
- Run `gh sfl init` and `gh sfl sync` only for an explicitly approved
  repository. Do not fan them out across the HemSoft account. If approval for a
  pending rollout is withdrawn, close its deployment PR and delete its branch.
- Do not add another App, user authorization flow, deployment broker, or App
  transfer to solve native reviewer assignment without new evidence.

## Remaining required work

- [ ] Align `gh sfl gate`, `status`, `uninstall`, tests, and documentation with
  the working `SFL Reviewer Gate Runner` contract while preserving PR #74's
  transactional and unrelated-policy protections; then close issue #73.
- [ ] On `hs-buddy`, carry one inline finding through resolution, verify
  obsolete-thread cleanup, and prove a failed review uses the bounded recovery
  path. Retain automated coverage for severity mapping and the retry limit.
- [ ] Before changing App scope or permissions, document the current working
  installation, the exact intended delta, and a rollback. Treat narrowing the
  all-repository personal installation as optional hardening, not a runtime
  repair.

## Product experience parity

Already working:

- [x] Automatic current-head reviews.
- [x] Native inline findings and App-authored approval.
- [x] Immutable review evidence and a required merge gate.
- [x] Bounded recovery and obsolete-thread cleanup.
- [x] Unslop filtering for user-facing reviewer communication.

Next interaction work:

- [ ] Add `@sfl-app review` and `@sfl-app full review` comment commands that
  dispatch the existing exact-head review path and report the run link.
- [ ] After real use, decide whether pause/resume controls or incremental-only
  re-review are worth their extra state. Full current-head review remains the
  safe default.
- [ ] Add PR conversation or fix handoff only after a concrete need appears.

Native reviewer-picker assignment is a separate platform investigation, not a
rollout or commercial-parity blocker. GitHub documents bot review-request APIs
but only explicitly documents reviewer assignment for Copilot. The three
reviewed commercial vendors document App installation, automatic review, and
comment commands as their standard flow. HemSoft PR #385 proved that three SFL
request mutations created no review request, event, or run; it did not prove
the cause. If this investigation resumes, use only a controlled `hs-buddy` PR
and the evidence matrix in
[the research note](docs/research/github-app-reviewer-eligibility.md) before
contacting GitHub Support.

## Deliberately deferred

Persistent code-graph indexing, a hosted dashboard, feedback learning,
one-click fix handoffs, and an unattended deployment broker are commercial
scale features. Do not build them for this personal rollout without a concrete
need.

Reference snapshot:

- [GitHub bot reviewer request inputs](https://docs.github.com/en/graphql/reference/pulls#requestreviewsbylogininput)
- [GitHub GraphQL 2026 changelog](https://docs.github.com/en/graphql/overview/changelog/2026#schema-changes-for-2026-01-22)
- [GitHub Copilot code review](https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/copilot-code-review)
- [CodeRabbit automatic reviews](https://docs.coderabbit.ai/configuration/auto-review)
- [Greptile quickstart](https://www.greptile.com/docs/quickstart)
- [Macroscope code review](https://docs.macroscope.com/bug-detection-and-fixes)
