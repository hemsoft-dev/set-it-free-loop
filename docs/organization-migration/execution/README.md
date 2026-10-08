# Executed organization migration

All 65 repository transfers and the owned SFL App transfer are complete. The two private personal Vercel repositories remain in HemSoft under Franz's retention decision. The original 67 IDs therefore reconcile as 65 destination repositories and two retained sources; 13 archived destinations retain their archive state. These records supplement the historical preparation ledger. They do not claim that the remaining 52-active-repository SFL rollout is complete.

[Transfer completion](https://github.com/hemsoft-dev/set-it-free-loop/issues/138#issuecomment-6051060735) and [App completion](https://github.com/hemsoft-dev/set-it-free-loop/issues/138#issuecomment-6051388689) record the repository-first execution directed by Franz. [Issue #138](https://github.com/hemsoft-dev/set-it-free-loop/issues/138) remains open for the executed transfer report and its documented evidence limitation. [Issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139) tracks the remaining rollout.

The transfer receipts preserve original repository IDs, refs, privacy, archive state and default branches. The private name collision maps to `hs-cli-confluence-search-fhemmer`. GitHub dropped dashboard's administrator bypass during transfer; its exact original value was restored and independently verified. The raw discrepancy and repair receipts remain part of the record.

The SFL App is privately owned by hemsoft-dev with unchanged App/client identity and permission ceiling. Installation `169090497` covers all current and future organization repositories. The existing-key GET-only credential workflow passed at the canonical source revision. No organization credential delivery, key sealing or rotation is claimed.

The current independently verified immutable release is signed `v2.1.0-rc.21` at source `89425320ace3127a829d86e3b642fe2b31fd979e`. [Its download proof](rc21-independent-download-proof.json) verifies the default private installer, signatures, checksums and isolated CLI version/status. Earlier rc16 default-discovery failures and the rc20 unregistered old-base review gap are historical primary failures, superseded by verified fixes rather than relabeled as passes.

[Both rc21 pilots are qualified](rc21-pilots-qualified-before-wider.json): 13 production-workflow fixtures per actual installed observer, authenticated registered live reviews with strict required Actions gates, and repeated signed init/sync/status with no PR or default-branch change. Fresh captures after terminal repeats preceded removal of only the owned temporary pilot rules. Synthetic negative fixtures are distinct from actual live provider executions. The deliberately failed pending-review runs held their gates until authenticated native completions succeeded.

Hs-buddy's full rc21 deployment is being prepared in [PR #770](https://github.com/hemsoft-dev/hs-buddy/pull/770). Its existing Auditor, Dispatcher and sync-policy hashes are preserved. The other agent's merged Rust work expanded the configured CodeQL languages; the older candidate's missing Rust analysis caused an actual merge refusal. Current-head review, all configured analyses and applicable CI must pass before merging. The current candidate also preserves the subsequent Dependabot change from PR #773. Its fresh native review and all configured CodeQL analyses pass; two macOS packaging jobs are waiting for GitHub runners. Full installed runtime verification remains pending.

The protected distribution source now has an [organization-owned strict review gate](rc21-source-only-org-review-gate-created-primary.json), scoped only to repository ID `1169772257`, bound to GitHub Actions App `15368`, with no bypass actors. The unrelated existing organization rule was preserved. This policy receipt does not claim completed in-place source runtime verification.

The fhemmer repository has an **unwaived historical source-protection evidence gap**. Its last owner-browser empty-policy capture predates transfer by several hours; the cutover ruleset API returned HTTP403. The retrospective browser audit reports no policy events for October 7 onward, but does not replace a fresh pre-transfer capture. [The limitation](fhemmer-protection-verification-limitation.json) remains visible. Repository identity and refs are independently verified. Franz's decision to defer SFL work does not waive this separate protection-evidence requirement.

All captures in this directory are projected to exclude credential values. Canonical workflow definitions and nonsecret review-context tokens are retained so provenance can be checked. No fleet-completion or scope-exception claim is made by these intermediate records.

Validate the executed identity and ref evidence independently of the original pre-cutover acceptance gate:

```sh
python3 deployment/scripts/validate-executed-transfers.py docs/organization-migration
python3 -m unittest deployment/tests/test_executed_transfers.py
```

This check derives refs from the actual GitHub matching-ref responses, including the captured empty n8n repository response. It reports 65 verified transfers, two retained sources and 13 transferred archives. Its successful exit does not mean that migration acceptance, source protection evidence or the SFL rollout is complete. The original strict rollout validator remains unchanged.

The executed transfer validator binds each accepted POST response to its repository ID, transfer URL and submission interval, and rejects duplicate retained rows. Pending-review failure-log references resolve to their committed primary logs. The onboarding contract checks the designated checksum-verified install instruction; version bumps update that instruction, and documentation-only changes trigger its Go test. The release-version helper changes affect future version updates; current signed rc21 assets and runtime Go/observer code remain unchanged.
