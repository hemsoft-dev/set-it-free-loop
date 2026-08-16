# HemSoft SFL Reviewer Parity TODO

Core reviewer parity with `relias-engineering/set-it-free-loop` is complete.
HemSoft intentionally uses Kimi K3 through its private OpenRouter route and
enforces the HemSoft owner boundary; those are supported differences, not
parity gaps.

## Verified current state — 2026-08-15

- HemSoft `main` contains the embedded Unslop communication pass from PR #85
  at commit `79ebcdac5579a2ef9e91be73fc44663316078453`.
- `v2.1.0-rc.8` is the latest published HemSoft release. It points to commit
  `02f274fcccb172a868b0bf05237246dd6e8465e7`, which predates PR #85.
- `HemSoft/hs-buddy` is the approved pilot. Its manifest records
  `v2.1.0-rc.8` and source commit
  `02f274fcccb172a868b0bf05237246dd6e8465e7`.
- The active `hs-buddy` default-branch ruleset requires
  `SFL Reviewer Gate Runner` from GitHub Actions App ID `15368`. The runner
  verifies the App-authored `SFL Reviewer Approval` and
  `SFL Review Evidence` checks without replacing unrelated repository rules.
- Pilot PRs
  [#380](https://github.com/HemSoft/hs-buddy/pull/380) and
  [#381](https://github.com/HemSoft/hs-buddy/pull/381) received current-head
  approvals from `sfl-app`, produced successful review evidence and approval
  checks, passed the required gate runner, and merged.
- Repository-scoped gate transactions, idempotency, and rollback support
  shipped in PR #74. Its CLI contract still targets
  `SFL Reviewer Approval`, while the working pilot ruleset requires
  `SFL Reviewer Gate Runner`; issue #73 therefore remains open.
- Public HemSoft consumers are supported. No private-only consumer is required
  to prove the personal-account rollout.

## Remaining required work

- [ ] Align `gh sfl gate`, `status`, `uninstall`, tests, and documentation with
  the working `SFL Reviewer Gate Runner` contract while preserving PR #74's
  transactional and unrelated-policy protections.
- [ ] Record the `hs-buddy` ruleset and PR #380/#381 pilot evidence, reconcile
  issue #73's acceptance criteria with the working gate, and close the issue.
- [ ] Publish the next HemSoft release from current `main` so it includes the
  embedded Unslop pass from PR #85 and the corrected gate contract.
- [ ] Synchronize `HemSoft/hs-buddy` to that release and verify its manifest
  records the new immutable version and source commit.
- [ ] Exercise the deployed recovery and finding lifecycle in the pilot: carry
  one inline finding through resolution, verify obsolete-thread cleanup, and
  prove a failed review uses the bounded recovery path. Retain automated
  contract coverage for every severity mapping and the recovery retry limit.
- [ ] Confirm the deployed reviewer contains the embedded Unslop pass, then
  complete one current-head pilot review with an `sfl-app` approval and green
  `SFL Reviewer Gate Runner`, `SFL Reviewer Approval`, and
  `SFL Review Evidence` checks.

When those six boxes are complete, replace this required checklist with a short
statement that the current rollout is complete. Keep the product experience
backlog until each item is deliberately implemented or declined.

## Product experience parity snapshot — 2026-08-16

SFL already matches the core GitHub review behavior that matters for this
personal rollout: automatic current-head reviews, native inline findings, a
real App-authored approval, immutable review evidence, a required merge gate,
bounded recovery, and obsolete-thread cleanup.

Commercial products are still smoother around installation and interaction.
CodeRabbit supports automatic and incremental reviews, comment commands, PR
conversation, and one-click fixes. Greptile adds persistent repository indexing,
feedback-based learning, comment commands, and agent fix handoffs. Macroscope
posts a real GitHub approval after its correctness and eligibility checks.

GitHub supports requesting installed bots as reviewers through the GraphQL
`requestReviews` mutations. The HemSoft wrapper already listens for the
`review_requested` event, but it still checks for the Relias
`set-it-free-loop[bot]` identity. The installed HemSoft reviewer is
`sfl-app[bot]`. This is an account-specific wiring bug, not a reason to create
another App or ask for new repository authorization.

### Next product experience work

- [ ] Replace the hard-coded Relias reviewer login with the deployed App login,
  retain the unrelated-reviewer no-op, and add contract tests for
  `sfl-app[bot]` review requests.
- [ ] On one `hs-buddy` pilot PR, request `sfl-app[bot]` through GitHub's native
  reviewer request path and prove that the existing wrapper dispatches one
  current-head SFL review.
- [ ] Add an `@sfl-app review` PR comment command that routes to the existing
  `gh sfl review` exact-head dispatch and reports the resulting run link.
- [ ] Decide from pilot usage whether incremental-only re-review is worth its
  extra state. Full current-head re-review remains the safer default.

### Deliberately deferred

Persistent code-graph indexing, a hosted dashboard, feedback learning,
one-click fix handoffs, and an unattended deployment broker are commercial
scale features. Do not build them for the personal rollout without a concrete
need. Continue to limit consumers to repositories explicitly approved through
`gh sfl init` or `gh sfl sync`.

The existing read-only comparison against current Relias `main` may be
scheduled later. If it detects unapproved reviewer safety, recovery, or gate
drift, it should open one deduplicated issue rather than changing workflows.

Reference snapshot:

- [GitHub bot reviewer request inputs](https://docs.github.com/en/enterprise-cloud@latest/graphql/reference/pulls#requestreviewsbylogininput)
- [CodeRabbit automatic reviews](https://docs.coderabbit.ai/configuration/auto-review)
- [Greptile overview](https://www.greptile.com/docs/introduction)
- [Macroscope Approvability](https://macroscope.com/approvability)
