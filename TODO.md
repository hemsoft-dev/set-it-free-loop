# SFL roadmap

## Goal

Keep the HemSoft reviewer reliable and finish one milestone at a time.

Issue #125 replaced the HemSoft reviewer model runtime with native Codex review
from Franz's ChatGPT subscription. SFL can request one current-head review and
authenticate Codex output, but no consumer currently requires the SFL gate.

## Current operating state

### Current state

- SFL PR #134 fixed issue #133 and is merged at `eb83674`.
- `hs-buddy` PR #521 and deployment PR #522 are merged at `58203bd`.
- The broken `hs-buddy` SFL reviewer ruleset is removed. Its separate Copilot
  review ruleset remains active.
- PR #121 closes issue #120 as superseded. The subscription-backed Codex path
  has no concurrency-cancelled recovery wrapper, and contract tests require
  the retired workflow files to remain absent.
- `developer-documentation` is excluded and remains read-only.

### Completed canary: `hs-buddy` PR #427

Status: passed and merged.

- Canary head: `1c6b76dd39a2ee821db16167d556ac53f041ddaf`.
- The PR's recorded base is `e49e4ae41cfed134d52aa8b424b0e3a76e7d38f3`;
  current `hs-buddy` `main` is `87c625cf65cbd56cedd3e2ce49c92553c360a222`.
- `hs-buddy` deployment PR #523 merged at
  `47074a14b3d6d7db6ee3015ba9d792beca86eb66` and pins the observer to SFL
  source `82819b273b9f9878cf8c4c7b3b5e5ddec9dcd873`, including PR #134's
  reviewed-head gate fix. Its branch and isolated worktree are deleted.
- The merged observer completed push run `32325578697` successfully and
  invalidated PR #427 with context
  `sfl-codex-review:base-advance:at:1787193734000:32325578697:427`.
- Two identical current-head `@codex review` requests already exist from the
  prior cycle (comments `5348122372` and `5348122430`). Codex returned one
  connector error and one clean result for reviewed commit `1c6b76dd39`
  (comment `5348143375`).
- The installed `gh sfl` is the retired `6.5.16` dispatcher. Any resumed
  canary must use the HemSoft source CLI after deploying the current source.
- This canary posted exactly one new request, comment `5350591677`. Codex
  returned a clean result for reviewed commit `1c6b76dd39` in comment
  `5350608246`. The observer authenticated it and published terminal success in
  check `96297674647` on the immutable head.
- PR #427 merged as `87c625cf65cbd56cedd3e2ce49c92553c360a222`.
  GitHub deleted its branch and the stale local tracking ref was pruned.
- The source CLI launches `gh.exe` directly, bypassing the PowerShell wrapper
  that selects the HemSoft account. Until the CLI owns account selection, run
  it with `GH_CONFIG_DIR=$HOME/.gh-personal` and verify `gh.exe api user`
  returns `HemSoft` before any write.
- Retry recovery waits up to two minutes for a connector reaction when an old
  request has a stale context marker, even when no reaction exists. Shortening
  that bounded wait is follow-up work; do not change or redeploy it during the
  current consumer sequence.

Next: baseline `hs-buddy` PR #428 and process it as a separate review epoch.
Do not overlap its request with another consumer PR.

### Completed consumer: `hs-buddy` PR #428

Status: passed and merged.

- Head: `e6cfc64b9b1add9d0d19ca9aa461e166d3e17d08`.
- Recorded base: `7b5e9bc870bdda54fb38d9037fca8ba89d4808cb`;
  current `hs-buddy` `main`: `5d6723b84a486fd98b1579f80975ddc78c2bd430`.
- Current-head CI, lockfile validation, and the historical reviewer checks are
  green. There are no review threads.
- The current observer invalidation is
  `sfl-codex-review:base-advance:at:1787194360000:32326201694:428`.
- The sequence posted exactly one request, comment `5350661988`. Codex returned
  clean result comment `5350682707` for reviewed commit `e6cfc64b9b`.
- The observer authenticated that result and published terminal success in
  check `96298915667` on the immutable head.
- PR #428 merged as `5d6723b84a486fd98b1579f80975ddc78c2bd430`.
  GitHub deleted its branch and the local tracking ref was pruned.

Next: baseline `hs-buddy` PR #429 as a new, non-overlapping review epoch.

### Hard guardrails

- Do not create refresh commits, rebases, force-pushes, dummy changes,
  duplicate review requests, or replacement deployment PRs for the completed
  `hs-buddy` cycle.
- Any future required gate must use an immutable review identity, never a
  synthetic merge SHA that GitHub can regenerate during merge evaluation.
- Any replacement must preserve exact reviewed-head and reviewed-base
  validation, strict base freshness, and fail-closed invalidation behavior.
- Never start a second simultaneous consumer review cycle.
- Keep `developer-documentation` and account-wide deployment out of scope.
- Clean branches and worktrees only after their PR is confirmed merged or
  closed; do not touch unrelated worktrees.

### Operating rules

1. Leave the broken `hs-buddy` SFL reviewer rule absent.
2. Do not start another deployment or reviewer cycle from this recovery work.
3. If SFL enforcement is revisited, begin with one bounded issue and prove the
   replacement gate before enabling it in a consumer repository.

## Working baseline

- `HemSoft/hs-buddy` remains the only repository approved for live HemSoft
  validation.
- The subscription-backed Codex reviewer source and `hs-buddy` pilot deployment
  are merged. The unreliable required SFL rule was removed from `hs-buddy`.
- No consumer repository currently requires `SFL Reviewer Gate Runner`.
- The Codex GitHub App is connected for the approved pilot repository. Do not
  change its installation scope as part of this milestone.
- Run `gh sfl init` or `gh sfl sync` only for an explicitly approved repository.
  Never fan either command out across the account. If approval is withdrawn,
  close the pending deployment PR and delete its branch.

## Completed milestone: issue #125

- [x] Remove the OpenRouter/Kimi reviewer, compiled lock, recovery wrapper, and
  reviewer credential requirements from the HemSoft package.
- [x] Make `gh sfl review --repo OWNER/REPO --pr NUMBER` post one head-bound
  `@codex review` request and deduplicate retries.
- [x] Authenticate clean comments and finding reviews, resolve reviewed commit
  prefixes, and reject stale, spoofed, or malformed evidence.
- [x] Preserve the strict `SFL Reviewer Gate Runner` branch-rule contract.
- [x] Land the subscription-backed Codex source in PR #126.
- [x] Deploy the pilot only to `HemSoft/hs-buddy` through PR #522.
- [x] Stop the pilot without reenabling the unreliable required SFL rule.

End-to-end clean and finding proofs through a required Codex-backed gate were
not completed. Any renewed enforcement starts with a new bounded issue.

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
