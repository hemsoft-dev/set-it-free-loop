> **Retired on 2026-10-09:** SFL is frozen. These are historical instructions, not an active setup or deployment procedure. Do not install, configure, dispatch or restart SFL. See the repository README for the freeze status.

# Note to SFL: source-first fixes needed for `gh-x` PR #3

Date: 2026-08-02

## Context

[`HemSoft/gh-x` PR #3](https://github.com/HemSoft/gh-x/pull/3) deploys the SFL review tier. While processing review feedback, several workflow fixes were made directly in the deployed `gh-x` copy. That is the wrong layer: it creates drift from this repository, which is intended to be the SFL source of truth.

The deployed workflow identifies its source as:

```text
HemSoft/set-it-free-loop/deployment/workflows/sfl-pr-review.md@78483bbf7edf0a4f8d3bf2f68e58678da36044ae
```

The `gh-x` deployment manifest still records that same source SHA, but the deployed workflow now differs from it. The provenance is therefore stale, and a later SFL sync could overwrite the fixes.

## Fixes currently made only in the deployed copy

The direct `gh-x` changes are in commits `3744e86` and `21e0431` on PR #3. They address:

- Preserving unresolved SFL findings when the workflow reruns, instead of losing earlier actionable findings.
- Pinning review operations and comments to the expected pull-request head commit.
- Performing a final SFL App head verification after threat detection so a moved head fails closed.
- Supporting review comments on deleted lines by using the `LEFT` side where appropriate.
- Using a placeholder-safe review body template.
- Failing closed when review state or commit identity is ambiguous.

There were also earlier reviewer-driven edits made directly to PR #3's deployed artifacts. Do not assume reverting only the two commits above will fully restore source parity.

## Recommended remediation

1. Implement the required behavior in this repository's canonical workflow source, expected to be `deployment/workflows/sfl-pr-review.md`, with relevant tests or deployment-contract checks.
2. Review and merge that source change here.
3. Redeploy SFL into `HemSoft/gh-x` using the official deployment/sync path and the merged source SHA.
4. Replace the hand-edited artifacts in PR #3 with the generated deployment output.
5. Verify that all deployed artifacts agree on the same source revision:
   - `.github/workflows/sfl-pr-review.md`
   - `.github/workflows/sfl-pr-review.lock.yml`
   - the deployment manifest's `sourceSha`
   - the workflow provenance comment
6. Confirm a subsequent SFL sync produces no unexpected diff.

Use the compiler version and checksum required by this repository's deployment contract. The `gh-x` artifacts were successfully regenerated during investigation with checksum-verified `gh-aw v0.84.1`, but the source repository's current contract should remain authoritative.

## Separate `gh-x` issue

PR #3's quality gate also reports six inherited cyclomatic-complexity findings in `gh-x`. Those are unrelated application-code debts and should not be folded into the SFL deployment/source correction.

## Desired end state

`set-it-free-loop` contains the behavior, and `gh-x` contains only reproducible deployment output from one identifiable merged SFL source SHA. No SFL behavior should exist solely as a consumer-repository patch.
