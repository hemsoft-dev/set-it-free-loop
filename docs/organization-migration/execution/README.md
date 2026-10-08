# Executed organization migration

All 65 repository transfers and the owned SFL App transfer are complete. The two private personal Vercel repositories remain in HemSoft under Franz's retention decision. The original 67 IDs therefore reconcile as 65 destination repositories and two retained sources; 13 archived destinations retain their archive state. These records supplement the historical preparation ledger. They do not claim that the remaining 52-active-repository SFL rollout is complete.

[Transfer completion](https://github.com/hemsoft-dev/set-it-free-loop/issues/138#issuecomment-6051060735) and [App completion](https://github.com/hemsoft-dev/set-it-free-loop/issues/138#issuecomment-6051388689) record the repository-first execution directed by Franz. [Issue #138](https://github.com/hemsoft-dev/set-it-free-loop/issues/138) remains open for the executed transfer report and its documented evidence limitation. [Issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139) tracks the remaining rollout.

The transfer receipts preserve original repository IDs, refs, privacy, archive state and default branches. The private name collision maps to `hs-cli-confluence-search-fhemmer`. GitHub dropped dashboard's administrator bypass during transfer; its exact original value was restored and independently verified. The raw discrepancy and repair receipts remain part of the record.

The SFL App is privately owned by hemsoft-dev with unchanged App/client identity and permission ceiling. Installation `169090497` covers all current and future organization repositories. The existing-key workflow passed at the canonical source revision. It verifies repository metadata without modifying repositories, creates a temporary scoped installation token with POST `/app/installations/169090497/access_tokens`, then revokes it with DELETE `/installation/token`. No organization credential delivery, key sealing or rotation is claimed.

The independently verified immutable release used for this report's pilot captures is signed `v2.1.0-rc.21` at source `89425320ace3127a829d86e3b642fe2b31fd979e`. [Its download proof](rc21-independent-download-proof.json) verifies the default private installer, signatures, checksums and isolated CLI version/status. Earlier rc16 default-discovery failures and the rc20 unregistered old-base review gap are historical primary failures, superseded by verified fixes rather than relabeled as passes.

[The original rc21 operator record marked both pilots qualified](rc21-pilots-qualified-before-wider.json): 13 production-workflow fixtures per actual installed observer, authenticated registered live reviews with strict required Actions gates, and repeated signed init/sync/status with no PR or default-branch change. Fresh captures after terminal repeats preceded removal of only the owned temporary pilot rules. Synthetic negative fixtures are distinct from actual live provider executions. The deliberately failed pending-review runs held their gates until authenticated native completions succeeded.

Hs-buddy's rollout and qualification belong to [issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139). Historical wider-consumer receipts are retained for diagnosis; this transfer report does not qualify those workflows, their CI or consumer-owned hashes.

The protected distribution source now has an [organization-owned strict review gate](rc21-source-only-org-review-gate-created-primary.json), scoped only to repository ID `1169772257`, bound to GitHub Actions App `15368`, with no bypass actors. The unrelated existing organization rule was preserved. This policy receipt does not claim completed in-place source runtime verification.

The fhemmer repository has an **unwaived historical source-protection evidence gap**. Its last owner-browser empty-policy capture predates transfer by several hours; the cutover ruleset API returned HTTP403. The retrospective browser audit reports no policy events for October 7 onward, but does not replace a fresh pre-transfer capture. [The limitation](fhemmer-protection-verification-limitation.json) remains visible. Repository identity and refs are independently verified. Franz's decision to defer SFL work does not waive this separate protection-evidence requirement.

All captures in this directory are projected to exclude credential values. Canonical workflow definitions and nonsecret review-context tokens are retained so provenance can be checked. No fleet-completion or scope-exception claim is made by these intermediate records.

Validate the executed identity and ref evidence independently of the original pre-cutover acceptance gate:

```sh
python3 deployment/scripts/validate-executed-transfers.py docs/organization-migration
python3 -m unittest deployment/tests/test_executed_transfers.py
```

This check derives refs from the actual GitHub matching-ref responses, including the captured empty n8n repository response. It reports 65 verified transfers, two retained sources and 13 transferred archives. Its successful exit does not mean that migration acceptance, source protection evidence or the SFL rollout is complete. The original strict rollout validator remains unchanged.

The executed transfer validator binds each accepted POST response to its repository ID, transfer URL and submission interval, and rejects duplicate retained rows. Pending-review failure-log references resolve to their committed primary logs. The onboarding contract checks the designated checksum-verified install instruction; version bumps update that instruction, and documentation-only changes trigger its Go test. The release-version helper changes affect future version updates; the report leaves signed rc21 assets immutable and preserves the current default-branch Go/observer runtime. Historical rc21 fixture replay uses its reviewed release snapshot after later canonical repairs.

The executed validator also requires post-transfer, repository-bound GET captures for the two retained private sources, bounds source metadata by the original inventory time, and requires owner direction before each submission. It validates the rc21 pilot qualification graph: all 26 installed-workflow fixtures, native/request/gate identities, successful actual workflow runs and observer jobs, guarded merges, terminal repeats, and unrelated policy preservation. Corrupted qualification summaries and primary receipts fail CI.

Pilot validation replays the production fixture blocks against the canonical observer, verifies the final installed GitHub bytes at the merged revision, and binds the required gate completion to an authenticated post-merge PR GET with the actual head, base and merge revision. The actual pending-run metadata and failed-log invocation remain distinct from the later successful native completion. Gate removal and post-removal policy captures must follow the terminal repeats.

The matching-ref responses retain their original request and terminal pagination headers, and every nonempty repository must contain its declared default branch. Terminal pilot repeats require their exact repository-specific command arguments. Source gate creation is bound to its HTTP201 organization POST and unchanged before/after capture of the unrelated rule. Dashboard repair is compared with its original policy and successful repository-bound PUT response.

Release validation binds the successful independent verifier invocation and its reviewed implementation to the signed statement, checksum list, isolated installer/version/status logs, and authenticated immutable release/tag captures. The release/tag captures are retrospective reads of the same immutable rc21 release; they are not relabeled as original publication-time captures. Signature verification was performed by the recorded `gh release verify` invocation inside the successful independent verifier. CI checks the retained evidence graph without claiming a new cryptographic verification or binary download on each run.

The [executed phase status](executed-phase-status.json) records actual App ownership and installation coverage separately from the frozen pre-cutover matrix. The original strict validator requires all historical pre-transfer gates before marking App transfer verified, so its pending fields are preserved rather than changed into a counterfactual claim. The executed validator checks the current organization-owned App registration, successful reviewed main verification run, independently downloaded exact artifact, and all 68 repository identity/installation/permission bindings. Current metadata reads are labeled retrospective.

The protection limitation is derived from the original owner-browser capture, the transfer receipt's cutover HTTP403 response and the later browser audit. The audit's limited observed events do not establish complete historical policy coverage. Changes to either canonical observer or retained release snapshots trigger the migration evidence checks.

The historical rc21 invocation did not record its script digest at execution time. A later filesystem capture binds the exact invoked path to the retained implementation bytes and records this timing limitation explicitly. It is retrospective implementation evidence, separate from the original successful signature, checksum, installer and status results.

The coverage archive was downloaded independently after the report review and its SHA-256 equals the authenticated GitHub artifact digest. Its extracted JSON exactly matches the original coverage primary. CI validates the retained archive and extracted bytes together. The historical rc21 verifier stays byte-for-byte as retained; current acceptance parses the installed CLI version from its first line and rejects a dev build even if update discovery names rc21.

Offline release validation uses OpenSSL to verify the retained DSSE payload signature against the certificate fingerprint pinned from the original successful `gh release verify` primary. The encoding follows the [DSSE protocol](https://github.com/secure-systems-lab/dsse/blob/master/protocol.md). This verifies retained payload integrity; the original GitHub verifier remains the evidence for certificate-chain, timestamp and release identity verification. OpenSSL must be available; missing tools fail validation. Pilot run and job captures must follow the captured terminal timestamps.

App privacy is inferred from paired current provider responses: the unauthenticated
[public discovery endpoint](pr156-app-anonymous-discovery-primary.json) returned
404, followed by an authenticated owner GET that resolved the same App ID.
GitHub documents that [this endpoint supports unauthenticated public resources](https://docs.github.com/en/rest/apps/apps#get-an-app).
The API omits a public/private field; this is an explicit access-behavior inference,
not a fabricated registration field or a settings-page observation.

The historical rc21 pilot merges retained successful pre-merge ruleset-list GETs
that show the owned gates active. Their strict check parameters survive only in
the contemporaneous merge-guard snapshots and successful post-merge detail GETs.
A separate primary detail GET from before each merge was not retained. This report
therefore labels historical pre-merge strict-policy evidence `guard_snapshot_only`,
without claiming independent primary verification of those parameters at merge
time. Issue #139 remains open; final corrected-release pilots must retain their
actual pre-merge effective-rule and ruleset-detail GETs before qualification.

The final dashboard policy GET must follow its successful repair PUT and precede
transfer verification. Installed historical pilot workflows must equal the full
digest-pinned rc21 snapshot after the deterministic deployment header, source pin
and default-branch quoting substitutions. This checks triggers, permissions and
job wiring as well as testable code blocks. The authenticated coverage artifact
listing must be captured after its workflow completes, before its digest is used.
