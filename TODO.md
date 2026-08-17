# SFL roadmap

## Goal

Keep the HemSoft reviewer reliable and finish one milestone at a time.

Core reviewer parity is complete. SFL automatically reviews the current head,
creates inline findings, submits App-authored reviews, publishes immutable
evidence, enforces a required gate, retries bounded operational failures,
cleans up obsolete threads, and filters reviewer communication through Unslop.

## Working baseline

- SFL is working on `HemSoft/hs-buddy`. It is the only repository approved for
  live HemSoft validation.
- `v2.1.0-rc.10` is the current immutable private release at
  `3920ff6c0c5718cafc0edbebff623527eff4d453`. The corrected rc.11 release is
  prepared from `00f5ae1d5ec82bdf3e72e12171da73b199ffe6ab`. `hs-buddy` remains on
  rc.9 until its deployment PR #438 is corrected and merged. HemSoft
  intentionally uses Kimi K3 through its private OpenRouter route.
- `hs-buddy` PR #415 proved the full finding lifecycle on 2026-08-16. SFL found
  real issues, approved the fixes on the current head, recovered automatically
  from one failed evidence attempt, and finished with zero unresolved threads
  and a green required gate.
- The working repository rule requires `SFL Reviewer Gate Runner`. That runner
  authenticates the App review plus `SFL Reviewer Approval` and
  `SFL Review Evidence`.
- The personal-account App installation currently selects all repositories and
  works. Do not change its authentication, permissions, ownership, or scope as
  part of the current milestone.
- Run `gh sfl init` or `gh sfl sync` only for an explicitly approved repository.
  Never fan either command out across the account. If approval is withdrawn,
  close the pending deployment PR and delete its branch.

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

## Current milestone: comment-command rollout

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
- [ ] Publish the corrected immutable prerelease and update `hs-buddy` PR #438
  from that release.
- [x] Raise the reviewer's consecutive cache-miss allowance from the `gh-aw`
  default of 5 to 10. The OpenRouter key and Kimi route succeeded for five
  requests; the proxy rejected request six with HTTP 403 and Copilot reported
  that proxy limit as an authentication failure. A pre-final PR #104 head
  completed successfully after the source change, but the final head still
  dispatched the reviewer definition from `main` and exhausted the old
  five-miss limit. The rc.11 release activates the new ceiling for consumers.
  Do not rotate the working secret for this failure.
- [ ] Validate the command end to end only on `HemSoft/hs-buddy`, then record
  the exact command acknowledgement, Actions run, App review, evidence, and
  required-gate links here.

Do not add a separate full-review command unless normal usage shows a need for
it.

## Not active work

Native reviewer-picker assignment, App transfer, another App or authorization
flow, installation-scope hardening, pause/resume controls, incremental-only
reviews, fix agents, dashboards, code graphs, feedback learning, and a deployment
broker are not current goals.

If reviewer-picker research resumes, use only `hs-buddy` and start with
[the existing evidence note](docs/research/github-app-reviewer-eligibility.md).
