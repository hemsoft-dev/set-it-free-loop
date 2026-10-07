# Transfer gates and organization rollout

Use this with the [migration baseline and transfer procedure](README.md),
[mandatory transfer gates in #138](https://github.com/HemSoft/set-it-free-loop/issues/138),
and [rollout acceptance criteria in #139](https://github.com/HemSoft/set-it-free-loop/issues/139).
The [integration ledger](integration-ledger.csv) and
[rollout matrix](rollout-matrix.json) cover all 67 source IDs and their collision
mappings. They are work records: no transfer or rollout is marked verified.

## Owner integration ledger

Complete the ledger for every transfer target and any retained repository
affected by App transfer before transferring any repository or App.
Add rows when a repository uses multiple provider resources. The candidate column
comes only from App access selections; it does not assert hosting or deployment.
Check GitHub workflows, environments, Pages, packages, webhooks, deployment keys,
repository variables and secrets, and the actual provider dashboard or account.

For each resource record its owner, exact resource URL, billing dependency,
credential source and validity, affected reference, transfer action, smoke test,
and recovery action. Store references to credentials, never values or sensitive
callback URLs. `status=verified` requires `verified_by`, `verified_at`, and a
source evidence URL or approved owner receipt. Where there is no external
integration, record `provider=none` and an evidence-backed `absence_reason`.
Empty fields, missing access, and failed lookups remain pending.

The [fresh runtime evidence](runtime-refresh-evidence.json) contains successful
runner lookups for all 67 sources and secret/variable name inventories for all
16 environments. Only HemSoft/yahtzee has a registered runner, online and idle;
all 32 environment inventories are empty. Destination owner pages show no
self-hosted runners and a Default group excluding public repositories. Refresh
these observations, App selections and effective protections immediately before
cutover. The [fhemmer owner-browser evidence](fhemmer-protection-evidence.json) observes
no classic protections or rulesets; reconfirm its settings immediately before cutover. Keep original failures visible beside supplemental evidence.
The [existing snapshot checker](README.md#refresh-and-validate) establishes
inventory integrity, not provider ownership or live credential validity.
CI also runs `validate-org-rollout.py` against the ledger and matrix. It rejects
missing or changed baseline mappings, invalid statuses, verified rows without
owner/evidence fields, and completed rollouts without immutable deployment/review
receipts. Pending addon lists are `null`; an observed empty list is `[]`.
Completed active rows require the observed pre-sync installed tier. Use
`not_installed` only for a verified absence of an existing deployment; installed
custom tiers also need their observed component list. Installed tier `review` is a legacy alias for selected `reviewer`; `custom` tiers
retain their observed manifest components and need recognized selected workflows.
Selected addons must be in the CLI's authoritative catalog. Completed rollout,
archive and scope-exception rows require every integration row verified. Exceptions
also need transfer and destination-settings evidence; they waive only rollout scope.
A verified active row requires a structured strict gate policy bound to GitHub
Actions and SFL registered Codex artifact identity at the recorded head and base, with
registration, registry-status and artifact receipts. Native Codex connection proof
alone cannot fill those SFL runtime fields. Baseline SFL App coverage is checked
against owner-verification.json; disposable pilot identities must remain recorded.
Disposable pilots retain explicit pending/failed/verified validation status. Verified
pilots need initial and repeated onboarding/sync, registration, SFL registered Codex artifact,
gate, status and safe gate/uninstall receipts. Both public and private pilots must
pass before any active rollout row can claim verification. Verified pilot receipts
include deployment_source, release_version, deployment_sha, manifest_identity
with matching source/sourceSha/version/tier, manifest_evidence_url and
release_download_verification_url. Semantic release versions are validated for
active manifests, pilot deployments and the protected source release. Review-only custom
components need no unrelated wider-workflow run; custom selections containing
wider workflows require those run receipts.
Use `health=verified` for a completed active rollout, `archived_verified` for
archive-preserving transfer/settings evidence, or `scope_exception` with an
explicit owner receipt. A verified provider row requires every integration field
and credential validity `verified` or `not_required`. For `provider=none`, record
the owner, timestamp, evidence and absence reason instead.

The destination Team plan has one occupied seat. Franz selected removal of
`fhemmerrelias` access **after transfer** in
[the recorded access decision](https://github.com/HemSoft/set-it-free-loop/issues/138).
The [owner/API access evidence](fhemmer-access-evidence.json) distinguishes source
organization-owner access from direct grants: there are zero direct collaborators
and zero repository teams. No extra seat is required for those observed grants.
Do not add destination membership or direct access, revoke source membership
before transfer, or purchase a seat. Verify effective access and the
[current organization licensing](https://github.com/organizations/hemsoft-dev/settings/licensing)
after transfer, and stop if an unexpected billing change appears.

The [Vercel owner evidence](vercel-provider-evidence.json) records five linked
projects in the existing Hobby workspace. Three connected source repositories
are private: [now-leadership-group](https://vercel.com/franz-hemmers-projects/now-leadership-group),
[set-it-free-loop-site](https://vercel.com/franz-hemmers-projects/set-it-free-loop-site),
and [modern-web-stack-poc](https://vercel.com/franz-hemmers-projects/modern-web-stack-poc).
This exceeds the four repository selections in the historical App snapshot.
[Vercel's Hobby policy](https://vercel.com/docs/git#using-hobby-teams) blocks private
organization Git deployments. Franz selected retirement of the unused modern-web-stack-poc Git deployment. The
[retirement receipt](modern-web-stack-git-retirement-evidence.json) records the
disconnected Git connection and a fresh settings reload confirming no connected
repository. Keep its GitHub repository in the migration. The Vercel project,
configuration, historical deployments and database data are preserved. No Pro
purchase is required for this disconnected Git deployment. Franz selected retaining the other two Git repositories in personal HemSoft; their Vercel Git connections avoid the organization restriction. The
[now-leadership-group live-hosting check](now-leadership-live-hosting-evidence.json)
confirms the configured Vercel origin through owner DNS: proxied apex A
76.76.21.21 and www CNAME cname.vercel-dns.com, with no zone Workers routes.
Cloudflare provides DNS/proxy and Vercel hosts the website. This later owner
observation resolves the earlier origin uncertainty. Retaining the personal Git
repository avoids the private-organization Hobby restriction.
Project IDs, Git repository IDs, domains, variable names and deployment states
are observed; credential validity remains pending. The owner Shared tab and API both show no
shared variables in this workspace.
No variable values were read or recorded. Preserve the current production
projects and aliases while preparing the cutover.

The [Fly owner-account observation](fly-provider-evidence.json) shows one
accessible organization with no apps or machines. The
[Railway visible-workspace observation](railway-provider-evidence.json) shows zero
projects and an expired trial. These observations cover only the authenticated
accounts shown; they do not establish absence in other accounts or complete the
per-repository ledger. Railway's terms dialog prevented further workspace
inspection; no legal terms, upgrade or resource creation was submitted.

The [Supabase owner observation](supabase-provider-evidence.json) records two
paused projects in HemSoft's Org, Free, with Franz's account as sole Owner. One
has a Vercel dashboard connection. The modern-web-stack-poc public deployment
references a different Supabase hostname that now returns DNS NXDOMAIN. Franz
[confirmed the repository is unused](external-resource-owner-scope-evidence.json).
Its legacy database integration has no active workload to preserve; this does not
authorize deleting a database.
Do not resume or delete databases as a consequence of retiring a Vercel project.
The [Blacksmith owner observation](blacksmith-provider-evidence.json) records
zero October jobs, runner minutes and spend, with no payment method or invoices.
Neither provider observation establishes credential validity or global absence.

The [source-reference scan](source-reference-evidence.json) observed 65 complete
default-branch trees; fhemmer's previous 404 and n8n-workflows' 409 remain
unverified. The [workflow/manifest scan](workflow-reference-evidence.json) read
100 SHA-addressed blobs successfully, including 96 workflow files. Record source
references and secret names, not values. hs-buddy has a canonical full-tier
manifest; buddy-ios has only a legacy root manifest with tier review. Preserve
these observations and resolve their runtime migration before rollout. Text
markers and configuration files do not establish provider resource ownership.

## Owner retention decision

Franz directed that HemSoft/now-leadership-group and HemSoft/set-it-free-loop-site
remain in the personal account. The [scope decision](scope-decisions.json) records
his [receipt](https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028207635)
and fresh source IDs, visibility and archive state. These repositories are outside
organization transfer and SFL rollout. Preserve their existing hosting and settings.
Keeping personal ownership avoids Vercel Hobby's private-organization Git restriction;
no Pro purchase is selected for either project. The site repository remains archived.

Keep the sealed 67-ID baseline and original destination mappings as historical
planning evidence. The execution plan now has 65 transfers and 2 retained sources,
including 52 active and 13 archived transfer targets. Matrix health retained_source
requires a matching owner decision and a separate captured API metadata artifact.
Its App dependency status may remain pending during preparation, but no completed
rollout or final report can pass with an unresolved retained App dependency.
Franz confirmed that neither retained site uses the App and its remaining
credentials are unused in [the owner receipt](https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028536416). It does not masquerade
as a completed transfer. Other rollout exceptions still require actual transfer proof.
Validation pins the two retained IDs and their owner receipt. Changing the
retained set requires a new owner decision and a deliberate validator update.
Provider candidate lists must match captured personal and organization App selections;
a candidate does not prove deployment usage or prevent an evidenced absence result.
Provider ledger completion applies to transfer targets. Before the SFL App transfer,
also verify any effect on retained repositories' existing personal App installation;
retention does not waive this shared-App dependency check.

## Runtime prerequisites

Native Codex is OpenAI-owned App 1144995. Organization installation 168678981 is
verified with all-repository selection; a [real private organization review](https://github.com/hemsoft-dev/sfl-migration-pilot-private/pull/1#issuecomment-6027603864)
completed clean at `f225d94d49a5951d6b2b205ad08990337aff040f`. Check the organization's
[installed Apps](https://github.com/organizations/hemsoft-dev/settings/installations)
and authenticate the requester through their connected account.

The owned SFL App is a separate registration, App 4448946. Its ownership transfer
and organization installation must follow #138's gates. Its
[private installability observation](sfl-app-installability-evidence.json) offers
only the owning HemSoft account, not hemsoft-dev. Keep visibility private; do not
make the App public to bypass the gated ownership transfer. Verify App/client
identity, intended permissions, and actual per-repository operations afterward.
The [owner transfer preparation warning](sfl-app-transfer-warning-evidence.json)
explicitly states that transfer automatically uninstalls the App from the personal
account. Reconcile all 64 selected repositories and the two retained personal
repositories before proceeding. No transfer has been submitted.
The manual [owned App credential check](../../.github/workflows/verify-sfl-app-credential.yml)
uses the existing source repository secret inside GitHub Actions to authenticate
GET-only App identity and installation checks. Dispatch it on reviewed main and
record its exact run/SHA. It checks the approved App/client IDs, owner, source
installation and permission ceiling without reading keys into the agent or issuing
installation tokens. A failure stays visible and requires correcting the credential
or installation through the owner. It does not clear provider/model credentials,
all-repository coverage or actual PR runtime gates.

Do not count native Codex installation or a standalone clean comment as completed SFL reviewer proof. The current [reviewer contract](../SFL-REVIEWER.md) uses Codex App 1144995 and bot user 199175422, plus an SFL registered request tied to the immutable head/base and a required Actions App15368 gate. Collect that complete observer/registration/gate path for #139. The owned private SFL App4448946 is a separate prerequisite for wider App-backed workflows; it does not author the native reviewer artifact. The [historical baseline](README.md#reviewer-baseline) remains a record of discontinued prior private rollout, not a new artifact-provenance requirement.
The August stop instruction applies to the prior private HemSoft rollout; this
October organization request authorizes preparation for hemsoft-dev. It does not
authorize new billing, publishing credentials, or weakening the required gates.

## Disposable pre-transfer validation

The newly created [private validation repository](https://github.com/hemsoft-dev/sfl-migration-pilot-private)
is outside the 67-source inventory and does not transfer any repository. Its
[read-only workflow permission probe](https://github.com/hemsoft-dev/sfl-migration-pilot-private/actions/runs/37548125725)
verified HemSoft admin and an outside account with no access using the workflow
token. This proves permission API availability, not SFL review runtime readiness.
Record disposable repositories separately from transferred source IDs.

## Pilot sequence

1. After the gates and transfer checks pass, choose one public and one private
   active consumer repository. The SFL distribution repository is protected from
   CLI init, sync, gate and uninstall. Verify its checked-in workflows, release
   and governance in place, with separate evidence in its matrix row. Never use
   a consumer command against the source or silently skip its coverage.
   Record the existing canonical or legacy root manifest tier and addons first; keep
   them during sync. A fresh consumer defaults to reviewer, while wider autonomous
   tiers require deliberate configuration. Archived repositories remain archived.
2. Install the reviewed CLI package and choose an immutable release from the
   canonical source. Follow [organization deployment](../ORGANIZATION-DEPLOYMENT.md)
   once #136 is merged. Verify its checksum and version. For a genuinely empty
   repository, first establish the intended default branch through a reviewed
   bootstrap commit and record it; init needs that branch to create its PR.
   Then run:

   ```text
   gh sfl init --repo hemsoft-dev/<pilot> --source-ref v<version> --pr
   ```

   For a fresh consumer, review and merge the initialization PR through ordinary
   repository policy first. Confirm the manifest exists on the default branch.
   For a consumer with an existing manifest, use sync to preserve its tier and
   addons instead of reinitializing it. Then run:

   ```text
   gh sfl sync --repo hemsoft-dev/<pilot> --source-ref v<version> --pr
   ```

   Review and merge any sync PR before checking the deployed default branch:

   ```text
   gh sfl status --repo hemsoft-dev/<pilot>
   ```

   Repeat sync twice;
   expect one consistent deployment and no duplicate PR, preserving unmanaged
   files, installed tier, and addons. Record the source SHA and actual manifest.
3. On a same-repository PR targeting the default branch, request review as an
   authorized write-or-higher human using `gh sfl review --repo ... --pr ...`.
   Record requester, registry comment/status, immutable head and base, Codex
   artifact identity, the SFL registered Codex review artifact and its immutable identity,
   gate run, and required status target. Installation selection
   alone does not prove this account can request or receive review.
4. Exercise clean and findings results, pending requests, malformed output,
   revoked/denied permission, lookup failure, forged/edited registration, duplicate
   delivery, a new head, and a base advance. Denied or stale evidence must not
   authorize success; pending evidence must invalidate an earlier success. Keep
   genuine failures visible. Do not disable or fabricate required checks.
5. Enable `gh sfl gate --repo ...` only after the reviewer is operational. Capture
   effective repository and inherited organization rules before and after. A Team
   plan does not support every Enterprise ruleset feature; use repository rules
   where necessary. The CLI refuses inherited gates it cannot safely remove.
   Test uninstall in a disposable pilot and confirm unrelated protections remain.
6. Run the designated pilot's configured wider SFL workflows and auditor. Record
   output and run URLs independently from the native Codex observer. Historical
   issue #23's closure did not resolve its reported parsing failures; create a
   precise new defect issue if a current runtime reproduces them.

## Roll out and onboard new repositories

After both pilots pass, process active repositories in small batches. Before each
batch refresh the destination inventory, App coverage, and effective policy.
Reconcile organization labels, existing CODEOWNERS, allowed Actions, and credential
precedence. Keep the valid individual owner `@HemSoft`; use a team only after it
exists and has the required access. Preserve unrelated governance and protections.

For each repository fill the matrix's installed/selected tier and addon lists, source/version/SHA,
App access, gate policy, health, and immutable evidence links. Run status and
compare its manifest, source pin, and effective branch rules with the row. A failed
check remains failed or pending; a scope exception requires an explicit owner
receipt, not a default skip. Archived rows need transfer/settings evidence, not
unarchiving or new workflow runs. The protected distribution repository uses
health=source_verified with in_place_evidence for workflow run URLs, canonical
release/version/SHA, release verification, governance, transfer/status receipts
and verified App coverage, plus a strict review gate and registered Codex-backed SFL
review artifact at the recorded head/base. It has no consumer installed/selected tier and must
never be initialized, synced, gated or uninstalled through consumer commands. Reconcile any inventory additions explicitly.

Create a disposable new organization repository and apply the same onboarding
procedure twice. Native Codex's all-repository selection should include it; verify
access through an actual review. SFL App access and selected organization
credentials must explicitly include the new repository. Repeating onboarding must
preserve existing tier and create no duplicate deployment, labels, or gates.

## Credential rotation and recovery

Review-only consumers require no SFL App key or model API key. Wider tiers use the
approved App and model credentials. Organization credentials must be restricted to
explicit repositories. Repository and environment overrides take precedence;
reconcile them before provisioning and whenever selected coverage changes.
Existing shared credentials require a coverage/rotation plan before replacement.

For App-key rotation, validate the replacement key against the same App/client
identity, update only intended secret scopes, verify token operations in a pilot,
and then revoke the old key through the owner's App settings. Record key references
and receipt URLs, never key values. Recover a failed update from the last known
working credential source and recheck coverage before resuming the batch.

On a failed transfer, review, workflow, or integration check, stop that batch and
record the exact failure and recovery action. Restore reviewed settings without
weakening inherited protections. A reverse transfer requires checking names,
plans, redirects, access, and provider consequences first. Retain both legacy
accounts and source redirects.

## Final coverage report

Re-enumerate both sources and the destination. Each of the 65 transfer target IDs
must occur exactly once at its mapped destination and no longer under its source.
The two retained IDs must remain at their recorded personal HemSoft locations with
unchanged privacy and archive state. Account for all 67 original IDs, and match the
matrix against the fresh inventory, including explicit additions.
Close #138 only after data/settings/integration checks and new-source release
installation pass. Close #139 only after every active target and new-repository
onboarding passes, or an explicitly accepted scope exception is linked. An empty
rollout evidence field never establishes completion.


Additional mandatory record checks: every transfer-target ledger row must be verified before any source transfer or App cutover. A selected custom tier requires an existing custom manifest. Completed consumers must record matching manifest source/sourceSha/version/tier and canonical release/download verification receipts. Verified pilots require a strict required Actions-bound SFL gate, the human requester and write-or-higher permission receipt, the same-repository review PR, and an SFL registered Codex artifact bound to that requester, PR, head and base. At least one verified pilot must deploy a wider tier and supply wider workflow and auditor run receipts before organization rollout.

The known yahtzee runner is a separate resource row reconciled with both runner captures. Its current registration and guest service are recorded in `yahtzee-runner-owner-evidence.json`; fresh pre-transfer smoke and isolation checks passed; post-transfer continuity remains pending. Preserve the existing registration first and replace it only if continuity fails. Never infer runner absence from an empty external-provider dashboard.

Franz confirmed [none of the 65 transfer targets uses Azure Pipelines, Fly.io or Railway](https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028753359). `provider-absence-owner-evidence.json` clears those usage candidates without changing historical App selections or clearing other mandatory gates.

Verified App transfer requires verified retained-site App dependency receipts. Destination private-App access cannot be marked verified before ownership transfer. Existing tiers, addons and custom components are preserved; completed source/consumer/pilot reviews all bind a write-or-higher human, same-repository PR and artifact to the same immutable head/base. The native Codex installation/coverage and smoke identity are checked against the independent `codex-organization-installation-evidence.json` capture.

Franz [confirmed the 42 listed repositories’ unreferenced legacy Actions credentials are unused by external clients](https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028911622). [The exact repository IDs and secret names](legacy-unused-credential-owner-evidence.json) are recorded in the ledger’s `unused_repository_credential_names` and `unused_repository_credential_evidence_url` columns. Keep the encrypted entries in place. This confirmation covers repository Actions entries only. It does not clear Vercel environment variables with the same names, other credentials, or the actively referenced credentials in buddy-ios, hs-buddy and the SFL source. Provider and runner row status remains unchanged.

Verified consumer manifests must match selected add-ons and custom components, as well as source, release SHA, version and tier. Disposable pilots retain the designated ID/name/visibility pairs and use an init-supported tier. The fhemmer repository’s `post_transfer_access` receipt stays pending before cutover; after transfer it must prove fhemmerrelias has no effective destination permission and the organization still has one filled, one paid seat. Generic provider absence cannot substitute for that access/license receipt.

The [latest owner resource confirmation](external-resource-owner-scope-evidence.json) limits active external integrations on the 65 transfer targets to the inventoried resources and confirms no Blacksmith usage. Historical App selections remain unchanged. The unused modern-web-stack-poc Git connection is retired with preserved project configuration and database data. Other active credential and cutover gates remain pending.

The validator reconciles all five captured Vercel project IDs, including retained and disconnected projects. The 42 unused Actions credential records must match the exact scanned repository IDs, names and owner receipt. Verified pilots require completed owned-App transfer and repository-bound post-transfer App coverage. A wider pilot must use standard or full tier so it actually deploys the Auditor. Every required negative scenario records its expected outcome, deployment SHA, execution mode and evidence. Use `live` for observed GitHub behavior and `workflow_fixture` for a fixture executing the deployed observer logic; never describe fixture execution as a live provider test. Scope exceptions require the matching repository ID/name, HemSoft approval identity, time, reason and disposition linked to the owner decision.

Completion also reconciles the two enabled GitHub Pages sites and the Supabase project bound to the dashboard Vercel connection. Their separate resource rows remain pending until their owner, credential, cutover, smoke and recovery gates are verified. Consumer and protected-source App coverage requires the exact destination ID/name, transferred App identity, positive installation ID and evidence. A verified consumer must carry repository-bound pre-sync evidence for both manifest locations; `not_installed` cannot erase a manifest already observed in the source scan. Every pilot onboarding/status/uninstall/review operation has matching repository ID/name, deployment SHA, release version and evidence; applicable GitHub URLs must belong to that pilot.

## Completion evidence binding

Before marking all transfer gates verified, populate
`pre_transfer_credential_verification` with the successful main-only owned-App
credential workflow run, its reviewed SHA, App/client/owner/installation identity
and permission-ceiling result. Original source-tree failures remain visible;
[source-tree-recheck-evidence.json](source-tree-recheck-evidence.json) independently
resolves the empty fhemmer tree and uninitialized n8n repository.

For existing installations, the captured manifest tier, add-ons and components
remain authoritative. A present receipt cannot relabel an existing full deployment
as reviewer. A verified wider pilot also needs `wider_operation_receipts` and
`auditor_operation_receipt` bound to its repository ID, canonical name, deployment
SHA, release version and matching run URL.

Each terminal transfer row needs `destination_protections` with the repository
ID/name, observed revision, ruleset contracts, classic protection settings and
owner evidence. Preserve every unrelated baseline rule while adding the required
SFL gate separately. Do not replace the existing ruleset with the new gate.

Final completion requires `final_inventory` pointing to a separate fresh captured
JSON enumeration of HemSoft, fhemmer and hemsoft-dev. Each account must record
all pages and observed repository IDs, canonical names, privacy and archive state.
Match all 65 destinations and both retained personal sources; preserve both
recorded disposable pilots. Explicitly account for any additional repository
through its owner-approved identity/visibility receipt. Planned mappings and
per-row prose cannot replace this actual-location reconciliation.

Provider-absence ledger rows must use the exact approved all-target owner receipt,
with HemSoft as verifier and a timestamp after confirmation. The independent
Azure/Fly/Railway and inventoried-resource/Blacksmith owner artifacts are matched
before those claims can clear transfer gates. A provider-absence row means no
additional active external resources; it never replaces a mandatory captured
resource row. Cloudflare production DNS/proxy and the nlg-contact-form Worker
have separate mandatory ledger identities derived from the authenticated capture,
even though now-leadership-group remains personal. Final completion requires
retained resources' continuity records as well as transferred resources.

Consumer `wider_operation_receipts` must match each wider workflow URL, repository
ID/name, deployment SHA and release version. Final inventory additions require a
separate local `decision_artifact` matching the added ID/name/visibility and owner
issue-comment receipt, with a reason, `include_final_inventory` disposition and
approval timestamp preceding the final capture. The inventory itself cannot
serve as its own approval artifact.

The unlinked Supabase dashboard-recovery project is explicitly accounted for as
an account-owned resource with no established repository link. Its preservation
record remains pending; final completion requires an independent post-transfer
read-only capture of the same resource owner, identity and paused state. Do not
invent a repository association or resume/delete the database to fill the record.

Verified retained App dependencies must match the exact approved no-dependency
receipt and safe disposition. An unverified fhemmer ruleset contract requires the
independent authenticated owner capture before an empty baseline is accepted.
Completed source workflow runs use `workflow_operation_receipts` bound to source
repository, immutable SHA and release. All completed registered reviews use
`review_operation_receipts` bound to repository ID/name, PR, head/base and each
registry, registration, artifact and gate URL.

The yahtzee completion row needs `post_transfer_runner` proving runner21's
registration, online/idle state, active service, isolation and successful
same-destination smoke run. Pre-transfer runner evidence cannot fill this field.

After baseline rollout, create the designated disposable
`hemsoft-dev/sfl-migration-new-repository` and fill `post_rollout_onboarding`.
Record independent creation metadata after `rollout_completed_at`, canonical
release/SHA, dynamic Codex and owned-App/credential coverage, registered review
and strict gate, and repository-bound initial/repeated init, sync and status
receipts. The final inventory separately accounts for this third operational test
repository; other additions still need owner decisions. Final completion cannot
reuse one of the two original pilots for this test.

The pre-transfer credential check must be followed by a complete
`pre_cutover_source_evidence_url` local JSON capture. Record `phase: pre_cutover`,
`observed_at`, and both source-owner `accounts` with `state: observed`,
`all_pages: true`, and every repository's ID, full name, privacy, archive state,
default branch and current `protections` contract. Reconcile all 67 sealed IDs;
new repositories, changed metadata or changed protections require updating and
reviewing the baseline before transfer. Capture `owned_app` registration identity,
client ID, owner and permissions, `source_installation` identity/all-repository
selection, and `source_organization_installations` separately from repository
metadata. The original inaccessible endpoint records remain preserved.

A verified `owned_app_transfer.evidence_url` must load a separate local capture
with `phase: post_transfer`, `observed_at` after the source refresh, and `app`
registration metadata proving ID 4448946, the original client ID and permissions,
and organization owner hemsoft-dev, ID 338855369. An installation record alone
does not establish App ownership.

The fhemmer access decision requires two local post-transfer captures, observed
after the source refresh and before `verified_at`. The permission capture binds
the repository ID/name and fhemmerrelias to the successful permission GET's
`result.permission: none`; the license capture binds hemsoft-dev's organization
ID/name to its Team `plan` with one filled seat and one purchased seat.

Protected-source `governance_evidence_url` must load post-transfer configuration
at the canonical repository ID/name and released revision. Its captured `labels`
must include every authoritative name, color and description from
`deployment/governance/labels.json`; its `codeowners` must match
`deployment/governance/CODEOWNERS`. Capture repository `actions_policy` and
`workflow_permissions` and preserve their sealed settings. This source check
does not deploy a consumer manifest.

The unlinked Supabase account-preservation capture must record
`phase: post_transfer` and an `observed_at` after both App transfer and baseline
rollout completion. The final all-owner inventory must follow baseline
completion, creation of the designated new repository, and its final captured
onboarding status. Earlier readiness captures cannot establish final preservation
or final repository coverage.

Runner continuity also loads separate local registration, isolation, guest-service
and `run_evidence_url` captures. Each binds the destination repository and runner
21 after the source refresh. Require online/idle registration, an active service,
public DNS/HTTPS success, the six existing private-route checks blocked, no
Tailscale, and the successful read-only Actions run at the recorded revision.
