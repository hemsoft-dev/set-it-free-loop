# SFL roadmap

## Goal

Keep the HemSoft reviewer reliable and finish one milestone at a time.

Issue #125 replaces the HemSoft reviewer model runtime with native Codex review
from Franz's ChatGPT subscription. SFL requests one current-head review,
authenticates Codex output, and preserves the required immutable-head gate.

## Active recovery plan: stop the review loop

### Current state

- SFL PR #132 and the `hs-buddy` deployment PR #522 are merged.
- `hs-buddy` PR #521 is frozen after one bounded merge attempt proved that the
  merge operation can regenerate GitHub's synthetic merge SHA and orphan a
  successful required status.
- Issue #133 owns the source repair. No consumer refresh commit, retry, or
  deployment is allowed while that repair is under review.
- `developer-documentation` is excluded and remains read-only.

### Hard guardrails

- Do not create more refresh commits, rebases, force-pushes, dummy changes,
  duplicate review requests, or deployment PRs for `hs-buddy` PR #521.
- Keep the required gate on an immutable review identity; do not rely on a
  synthetic merge SHA that GitHub can regenerate during merge evaluation.
- Preserve exact reviewed-head and reviewed-base validation, strict base
  freshness, and fail-closed invalidation behavior.
- Every write must reduce uncertainty or move issue #133 toward one reviewed
  source PR. Never start a second simultaneous consumer review cycle.
- Keep `developer-documentation` and account-wide deployment out of scope.
- Clean branches and worktrees only after their PR is confirmed merged or
  closed; do not touch unrelated worktrees.

### Next bounded sequence

1. Add a regression test that fails while the gate targets `merge_commit_sha`.
2. Publish every required gate transition on the immutable reviewed head and
   retain strict base freshness.
3. Run the focused deployment contract, complete Go suite, workflow lint, and
   repository deployment tests.
4. Open one source PR for issue #133 and process current-head review feedback.
5. Stop before any new consumer deployment. Reassess the proof plan from a
   green source PR instead of churning `hs-buddy`.

## Working baseline

- SFL is working on `HemSoft/hs-buddy`. It is the only repository approved for
  live HemSoft validation.
- `v2.1.0-rc.13` is the last immutable private release at
  `076325cd78119442dd0922abab2023451cff5189`. It is deployed to `hs-buddy`
  through [PR #454](https://github.com/HemSoft/hs-buddy/pull/454). HemSoft
  uses the retired Kimi/OpenRouter reviewer. It must be superseded before the
  native Codex observer can be deployed.
- `hs-buddy` PR #415 proved the full finding lifecycle on 2026-08-16. SFL found
  real issues, approved the fixes on the current head, recovered automatically
  from one failed evidence attempt, and finished with zero unresolved threads
  and a green required gate.
- The working repository rule requires `SFL Reviewer Gate Runner`. The new
  observer publishes that same Actions-owned check after authenticating Codex
  and proving the reviewed commit is the current head.
- The Codex GitHub App is connected for the approved pilot repository. Do not
  change its installation scope as part of this milestone.
- Run `gh sfl init` or `gh sfl sync` only for an explicitly approved repository.
  Never fan either command out across the account. If approval is withdrawn,
  close the pending deployment PR and delete its branch.

## Active milestone: issue #125

- [x] Remove the OpenRouter/Kimi reviewer, compiled lock, recovery wrapper, and
  reviewer credential requirements from the HemSoft package.
- [x] Make `gh sfl review --repo OWNER/REPO --pr NUMBER` post one head-bound
  `@codex review` request and deduplicate retries.
- [x] Authenticate clean comments and finding reviews, resolve reviewed commit
  prefixes, and reject stale, spoofed, or malformed evidence.
- [x] Preserve the strict `SFL Reviewer Gate Runner` branch-rule contract.
- [ ] Land the source PR after current-head Copilot and SFL review.
- [ ] Publish the next prerelease and pilot only on `HemSoft/hs-buddy`.
- [ ] Prove one clean result and one finding result through the deployed gate.

This milestone explicitly excludes developer-documentation and account-wide
deployment.

## Completed milestone: issue #73

PR #74 delivered the transactional repository-scoped gate. Issue #73 aligns
that transaction with the deployed runner and GitHub's live ruleset API.

- [x] Update `gh sfl gate`, `status`, `uninstall`, tests, and documentation to
  use the deployed `SFL Reviewer Gate Runner` contract.
- [x] Preserve idempotency, unrelated-policy protection, drift detection,
  verification, and bounded rollback. GitHub rejects conditional headers on
  repository ruleset updates and deletes, so the CLI rechecks the entity tag
  and complete state immediately before each mutation and aborts on drift.
- [x] Keep maintenance mode fail closed. It pauses dispatch and recovery while
  the required `SFL Reviewer Gate Runner` continues to execute and block new
  unreviewed heads.
- [x] Prove the change against `hs-buddy` PR #415 without changing the App or
  reviewer workflow. The first gate run changed strict freshness from `false`
  to `true`; the second run made no write; `status` recognized the strict gate;
  and the PR retained green approval, evidence, and gate-runner checks.
- [x] Close [issue #73](https://github.com/HemSoft/set-it-free-loop/issues/73).

## Completed milestone: comment-command rollout

- [x] Implement [issue #98](https://github.com/HemSoft/set-it-free-loop/issues/98):
  add one comment-triggered entry point, `@sfl-app review`.
- [x] Reuse the existing exact-head review path and reply once with its run
  link.
- [x] Preserve automatic and `sfl-review` label triggers, and prove command
  authorization and duplicate-delivery behavior with focused tests.
- [x] Publish rc.10 and open `hs-buddy` deployment PR #438. Review of the
  rollout exposed five source defects before merge: ambiguous run correlation,
  a timeout acknowledgement that incorrectly suppresses retry, a
  comment-triggered review that bypasses the trusted approval gate, and loss of
  the selected review-effort label, plus stale acknowledgement reuse after the
  pull request head changes.
- [x] Merge [issue #103](https://github.com/HemSoft/set-it-free-loop/issues/103)
  via [PR #104](https://github.com/HemSoft/set-it-free-loop/pull/104), using a
  command-specific dispatch ID, an exact final
  acknowledgement marker, the trusted approval gate, and the selected review
  effort while preserving the gate's explicit fail-closed maintenance-mode
  behavior.
- [x] Publish rc.11 and update `hs-buddy` PR #438 from that immutable release.
  The first current-head SFL run approved with zero findings. A duplicate
  metadata-triggered run then exposed a fail-open edge case: when the
  `comment-command` job fails before emitting outputs, the approval job can
  mistake the empty reviewer state for a bootstrap deployment.
- [x] Make the comment-command path fail closed via
  [PR #107](https://github.com/HemSoft/set-it-free-loop/pull/107), and run its
  platform contract test in CI.
- [x] Publish rc.12 and deploy it to `hs-buddy` through PR #438.
- [x] Correct the command validator's jq boolean default via
  [PR #110](https://github.com/HemSoft/set-it-free-loop/pull/110). The rc.12
  proof on `hs-buddy` PR #415 failed closed because `.draft // true` converts a
  valid `draft: false` response to `true`. The fix now accepts only the literal
  boolean `false` and keeps missing, null, and malformed values fail-closed.
- [x] Publish rc.13 and update `hs-buddy` through
  [PR #454](https://github.com/HemSoft/hs-buddy/pull/454).
- [x] Raise the reviewer's consecutive cache-miss allowance from the `gh-aw`
  default of 5 to 10. The OpenRouter key and Kimi route succeeded for five
  requests; the proxy rejected request six with HTTP 403 and Copilot reported
  that proxy limit as an authentication failure. A pre-final PR #104 head
  completed successfully after the source change, but the final head still
  dispatched the reviewer definition from `main` and exhausted the old
  five-miss limit. The rc.11 release activates the new ceiling for consumers.
  Do not rotate the working secret for this failure.
- [x] Validate the command end to end on `HemSoft/hs-buddy` PR #430. The
  [owner command](https://github.com/HemSoft/hs-buddy/pull/430#issuecomment-5322099781)
  produced an
  [SFL App acknowledgement](https://github.com/HemSoft/hs-buddy/pull/430#issuecomment-5322101120)
  linked to exact
  [review run 32086489284](https://github.com/HemSoft/hs-buddy/actions/runs/32086489284).
  The App submitted a current-head
  [zero-finding approval](https://github.com/HemSoft/hs-buddy/pull/430#pullrequestreview-4956123968),
  and the rerun after resolving one fixed historical finding produced green
  [review evidence](https://github.com/HemSoft/hs-buddy/runs/95560069528),
  [reviewer approval](https://github.com/HemSoft/hs-buddy/runs/95561565685),
  and the required
  [command gate](https://github.com/HemSoft/hs-buddy/actions/runs/32086477350).

Do not add a separate full-review command unless normal usage shows a need for
it.

## Not active work

Native reviewer-picker assignment, App transfer, another App or authorization
flow, installation-scope hardening, pause/resume controls, incremental-only
reviews, fix agents, dashboards, code graphs, feedback learning, and a deployment
broker are not current goals.

If reviewer-picker research resumes, use only `hs-buddy` and start with
[the existing evidence note](docs/research/github-app-reviewer-eligibility.md).
