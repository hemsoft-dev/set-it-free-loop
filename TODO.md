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
- The deployed source is `v2.1.0-rc.9` at
  `77aa2059d98bad34a9bd5026e196820da8fb3725`. HemSoft intentionally uses Kimi
  K3 through its private OpenRouter route.
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

## Current milestone: close issue #73

PR #74 already delivered the transactional repository-scoped gate, including
idempotency, preservation of unrelated policy, and rollback. The only remaining
gap is that the CLI contract still names `SFL Reviewer Approval` while the
working repository rule requires `SFL Reviewer Gate Runner`.

- [ ] Update `gh sfl gate`, `status`, `uninstall`, tests, and documentation to
  use the deployed `SFL Reviewer Gate Runner` contract.
- [ ] Preserve PR #74's transaction, idempotency, unrelated-policy, and rollback
  protections.
- [ ] Prove the change on one controlled `hs-buddy` PR without changing the
  working App installation or reviewer workflow.
- [ ] Close [issue #73](https://github.com/HemSoft/set-it-free-loop/issues/73).

## Next milestone

After issue #73 closes, add one comment-triggered entry point:
`@sfl-app review`. It must dispatch the existing exact-head review path and
reply with the run link. Add a separate full-review command only if normal
usage shows a need for it.

## Not active work

Native reviewer-picker assignment, App transfer, another App or authorization
flow, installation-scope hardening, pause/resume controls, incremental-only
reviews, fix agents, dashboards, code graphs, feedback learning, and a deployment
broker are not current goals.

If reviewer-picker research resumes, use only `hs-buddy` and start with
[the existing evidence note](docs/research/github-app-reviewer-eligibility.md).
