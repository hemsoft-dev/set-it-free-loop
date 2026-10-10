> **Retired on 2026-10-09:** SFL is frozen. These are historical instructions, not an active setup or deployment procedure. Do not install, configure, dispatch or restart SFL. See the repository README for the freeze status.

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

The [earlier runtime evidence](runtime-refresh-evidence.json) contains successful
runner lookups for all 67 sources and secret/variable name inventories for all
16 environments. That capture observed only HemSoft/yahtzee's registered runner;
the [subsequent Windows resource capture](current-survival-resources.json)
also records survival-shelter-opus55 runner10 and its existing startup task.
All 32 environment inventories in that capture are empty. Destination owner pages show no
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
Record each retained dependency's `verified_at` before the independently captured
App ownership-transfer time. A later no-dependency confirmation cannot qualify an
earlier App transfer.

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
The manual [owned App credential check](../../deployment/tests/fixtures/retired-source-workflows/verify-sfl-app-credential.yml)
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
receipt, not a default skip. An archived completion loads a timestamped destination
repository GET proving the original visibility, default branch and `archived: true`
after transfer/settings preservation. Its observation contributes to the rollout
completion cutoff. Preserve archive state without running workflows.
After installing a strict review gate, save `terminal_protections` at the verified
deployment revision. This independent capture follows the terminal gate-policy
capture, includes full rulesets/classic protections and effective gate rules, and
must retain every unrelated baseline rule. The protected distribution repository uses
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
Record terminal times in init, repeated init, sync, repeated sync and status order,
including no-op operations sharing the same revision. Verify owned-App access with
an unsuspended repository-specific installation GET after the new repository's
creation and App cutover. The older all-repositories installation capture alone
does not establish coverage for a repository created afterward.

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

The known yahtzee runner is a separate resource row reconciled with both runner captures. Its current registration and guest service are recorded in `yahtzee-runner-owner-evidence.json`; fresh pre-transfer smoke and isolation checks passed; post-transfer continuity remains pending. The subsequently observed Windows runner has its own ledger row and [reviewed resource contract](current-survival-resources.json). Preserve both registrations and their existing startup mechanisms. Any repair requires its own authorization. Never infer runner absence from an empty external-provider dashboard.

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

Each decision also needs `owner_comment_evidence_url`, a local capture of the
authenticated issue-comment GET. Record its exact request URL, HTTP status,
observation time and raw comment. The comment must belong to issue138, have
HemSoft's user ID8227352 and match the decision's approval timestamp. Capture
the comment after its latest edit and before the final inventory. Its body must
contain exactly one `<!-- sfl-migration-approval:{JSON} -->` marker. The JSON
must match the decision's `repository_id`, `repository`, `visibility`,
`disposition` and `reason`. An agent-authored comment or a matching URL alone
cannot supply owner approval. Accept the source issue URL before or after its
transfer, preserving the same comment ID.

Every consumer status `file_checks` entry needs `source_contents_evidence_url`
and `deployed_contents_evidence_url`. Each local capture records the repository
ID/name, immutable revision, observation time and raw `contents_response` from
an exact SHA-addressed contents GET. Decode and verify the Git blob identity,
size and URL before comparing bytes. Both captures must precede status.
Canonical compiled `.lock.yml` files come from the released source's
`.github/workflows`; handwritten reviewer, dispatcher and Auditor files come
from `deployment/infrastructure`, and other templates come from
`deployment/workflows`. Apply the CLI's version substitution and reviewer
source/default-branch rendering to the canonical bytes. Derive the expected
SHA256 from those bytes and the actual SHA256 from the destination bytes.
The two hashes and installed bytes must agree. A local compiler output or a
successful workflow run does not replace the released execution-file capture.

Runtime scope exceptions use the same raw owner-comment checks. Save
`owner_comment_evidence_url` in the independent exception capture, with its
`observed_at` after the actual comment GET. The issue138 or issue139 comment's
structured marker must match `repository_id`, `repository`,
`exclude_runtime_rollout` disposition and `reason`. Keep the claimed receipt and
the actual API response separate. Approval creation, latest edit and raw capture
must precede the exception observation.

For live pilot scenarios, both Actions run creation and the current attempt start
must follow App cutover and the deployed observer observation. Capturing an old
run or its artifact afterward cannot satisfy a post-cutover scenario.

Runner service evidence must name the sealed systemd unit from
[the runner baseline](yahtzee-runner-owner-evidence.json). Record the exact
`systemctl show UNIT --property=Id,ActiveState --no-pager` argument list in `argv`
and its parsed `Id` and `ActiveState` output in `systemctl_show`. The returned ID
must equal the sealed unit, and both recorded states must be active. An unrelated
active service cannot establish runner health.

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

Each pilot scenario now needs a local `capture_evidence_url` and a separate local
`output_evidence_url` in that capture. Bind repository ID/name, source release
SHA/version, `tested_revision_sha` from the deployed manifest capture, scenario,
mode, expected outcome and completed execution. The output must record the
same identity and a passing assertion for the expected scenario result. For
`workflow_fixture`, record the command, zero exit code and successful conclusion;
its `evidence_url` names that execution capture. For `live`, use an actual
same-repository Actions run URL and captured completed deployed-observer run
metadata. An issue fragment or a declared expected result is not execution proof.

Post-transfer integration observations must follow the independently captured
source refresh and, when transferred, App registration observation. For terminal
transferred repositories they also cannot precede the independently captured
destination metadata/protection observation. Pre-transfer readiness observations
remain distinct and cannot establish final resource continuity.

Destination protection captures must be observed after the independently
validated source/App cutover, in addition to matching the actual rulesets and
classic protection. A `post_transfer` phase label alone is insufficient.

Every verified consumer also loads its local `pre_sync_installation.evidence_url`
with `phase: pre_sync`. Bind repository ID/name, revision, both manifest paths,
state, tier, add-ons and components. `manifest_files` records each path's pinned
revision and either a successful contents read with its parsed manifest or a
captured HTTP404 absence. Require both `.sfl/sfl.json` and `sfl.json` checks;
authorization failures do not establish absence. Prefer the primary manifest
when both exist. This capture follows destination verification and precedes the
post-deployment manifest capture.

The distinct final new-repository test exercises the documented default
`reviewer` tier with no add-ons. Wider tiers and credential-dependent add-ons
remain deliberate configurations verified in the separate wider pilot; they
cannot substitute for this default onboarding proof. The final all-owner
inventory must still show this newly onboarded repository as unarchived.

Workflow operation receipts also load `capture_evidence_url` with independently
captured terminal Actions `run` metadata: repository ID/name, URL, workflow path,
head SHA, successful conclusion and creation time. Source and consumer runs must
be created after their independently captured destination/App cutover; pilot runs
must follow App cutover. The observation cannot precede run creation. Source
governance configuration is likewise observed after App cutover.

Destination SFL installation evidence records `phase: post_transfer` and a
zoned `observed_at` after the independent App ownership capture. An installation
observed before transfer cannot establish coverage after transfer.

Registered review gate captures also include raw Actions `run` metadata with
repository ID/name, immutable execution SHA, workflow path, successful terminal
state, run URL, `created_at` and `updated_at`. Run creation follows the relevant
App and destination cutover. The effective default-branch gate policy is
observed after that cutover and the completed review run. The final inventory
preserves each baseline repository's actual default branch as well as its
identity, location, privacy and archive state.

Pilot and final onboarding CLI operation receipts load `capture_evidence_url`
with matching repository, release, command, revisions and terminal result. The
capture records a completed execution with exit code zero. A mutation includes
an exact successful `pull_request_response` GET, closed/merged PR metadata and
the matching merge revision. Order it by the actual merge time. Status, no-op
and gate removal record actual command arguments and start/completion times in
`execution`, and use completion for ordering. Execution starts after cutover;
a later capture cannot qualify an earlier operation. A no-op includes captured
zero changes and unchanged revisions; status includes healthy file checks at
the observed revision. `uninstall-gate` names the gate-only operation; record
the actual policy-removal command rather than invent a CLI subcommand.

Protected-source runtime proof uses checked-in distributed SFL product
workflows from the full deployment catalog. Successful migration validation,
credential verification and other ancillary workflows remain separate checks.

Baseline rollout completion is checked against each repository's latest
validated local evidence timestamp, including protection, manifest, review,
workflow, provider-smoke and owner-decision captures. A completion artifact
cannot backdate the aggregate cutoff to make an earlier repository creation
qualify as post-rollout onboarding. The final designated onboarding repository
must retain the default branch used by its captured effective gate policy.

An observer Actions run can have a default-branch or merge execution SHA.
Capture the resulting Actions-owned `check_run` separately: its reviewed head,
context, successful terminal state, URL, App ID15368 and registered PR/base
`external_id` must match the review receipt. Raw run creation/completion still
prove execution chronology. Do not substitute the run's execution SHA for the
reviewed PR head. See [GitHub event semantics](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows).

Workflow-fixture proof uses the tracked `deployment/tests/run-org-observer-fixtures.cjs`
runner and exact structured `argv`. Its workflow input is a local captured JSON
with repository ID/name, deployed revision, workflow path and source content.
The runner executes extracted observer functions against synthetic API responses
and writes a scenario result. Bind its output file digest, runner digest and
workflow-content digest to the execution capture. An arbitrary successful
command or separately authored passing result is insufficient. Fixture execution
is distinguished from live provider events.

Pre-sync captures include the current default-branch ref GET result and a
separate `deployment_input_evidence_url`: repository ID/name, input revision,
result revision and captured compare metadata showing that the result contains
the inspected input. Capture that input after pre-sync inspection and before
final deployed status. A historical manifest query cannot describe the actual
configuration consumed by deployment.

`baseline_preserved` and `preserved_unused` apply only to the owner-approved
retired modern-web-stack-poc Vercel project and paused dashboard Supabase project.
Bind the exact owner receipt, unchanged sealed baseline, no runtime actions and
successful resource metadata GETs. The Supabase GET must show its exact project
reference, organization slug, name, region and `INACTIVE` state. The retired
Vercel project must remain disconnected, with preserved production state and
aliases. Active resources must demonstrate actual continuity. `approved_recovery`
requires a captured HemSoft-authored decision for the exact resource and action,
plus the same provider-specific successful GETs as ordinary continuity.

The modern-web-stack-poc Git retirement remains complete. Its older preservation
receipt lacks the resource response now required by the validator, so the ledger
records `partial_provider_verified` until fresh metadata proves preservation.

Completed release-download validation reruns `gh release verify-asset` against
the actual file and compares its independently verified signed statement. If the
captured local file is absent, download the exact named asset from the canonical
repository and tag into temporary storage, verify its SHA256 and signature, then
remove it. This is a read-only network check and never executes the binary.
Pending records remain offline. CI supplies its read-only GitHub token solely to
this final validator step.

Post-transfer permission evidence needs the exact destination collaborator
permission GET, HTTP 200 and raw response identifying `fhemmerrelias` with
permission `none`. Seat evidence needs the exact `hemsoft-dev` organization GET,
HTTP 200, organization ID and raw Team plan showing one purchased and filled seat.
Final inventory stores every raw successful account enumeration GET and response
headers, follows each `Link` next page, and derives its repository list from those
pages. Use the authenticated owner-affiliation endpoint for HemSoft to include
private repositories. Missing pages and independently edited lists fail.

Repository `transfer_evidence_url` must name an independent local capture of the
destination organization's accepted `repo.transfer` audit event. Preserve its
`_document_id`, `repo_id`, canonical `repo`, `org`, `org_id`, `actor` and numeric
`@timestamp` in milliseconds. If exported, `created_at` must agree and `repo_was`
must match the original source. The capture also records source/destination,
repository ID, `phase: post_transfer`, `observed_at` and `audit_log_url` pointing
to [the organization audit log](https://github.com/organizations/hemsoft-dev/settings/audit-log).
Use its filtered JSON export when the audit-log REST API is unavailable on Team,
following [GitHub's export instructions](https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization#exporting-the-audit-log).
Retain only these public event fields, never token, request or user-agent details.
The actual accepted event must follow the immutable ledger snapshot and fresh
source recheck, and precede destination protection observations. An issue comment
or later destination metadata capture cannot replace the transfer event.

For active consumers, the deployment-input capture also records `started_at`
from the actual init/sync invocation. It must follow pre-sync inspection and
precede the input/result observation and deployed manifest. Both designated
pilots' latest validated terminal evidence, including wider-workflow tests and
final gate cleanup, must precede this first deployment operation. The protected
distribution source must finish its in-place release, workflow, governance,
review and final default-head verification before either pilot starts. Its latest
validated terminal capture must strictly precede `started_at` from each pilot's
first successful init execution capture. This timestamp must follow App cutover
and precede the init result observation. Source verification can complete while
pilots remain pending. Pilots then finish before active consumer deployment.
The source does not use consumer init/sync.

The yahtzee runner proof additionally records `smoke_job_id` and a local
`jobs_evidence_url`. Capture all pages of the completed current attempt's
`GET /repos/{owner}/{repo}/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100`
response, with its `request_url`, `observed_at`, `all_pages`, `total_count` and
raw `jobs`. The selected successful job must bind the run/attempt/head, execute
on runner 21 named `mini-github-runner-01`, request self-hosted labels drawn from
the preserved registration, and finish within the captured run. Compare the
registration name and full label set with the sealed runner baseline. A passing
GitHub-hosted job cannot establish this runner's continuity.

Every registered review, including consumer, pilot, source and final onboarding,
loads `review_pr_metadata_evidence_url`. This local capture contains the exact
PR REST `request_url`, `observed_at` and raw `pull_request`. Match the PR number,
URL, same-repository head/base IDs and SHAs, and `base.ref` to the actual branch
whose required policy was captured. Query it after cutover and before the final
gate observation. Policy on main cannot qualify a review targeting develop.

Protected-source `in_place_evidence.default_branch_evidence_url` independently
captures `GET /repos/{owner}/{repo}/git/ref/heads/{default_branch}` after all
source workflow, review, governance and release checks. Record repository ID/name,
branch, `phase: post_transfer`, `http_status: 200`, `request_url`, `observed_at`
and raw `data`. Its exact ref and commit SHA must match the verified `source_sha`.
Product workflow runs must name that default branch and revision. A retained
branch/tag run at an older release cannot verify a newer main revision. Capture
and verify again if main advances during qualification.

The mandatory pre-cutover source refresh compares exact `visibility` as well as
the `private` boolean, archive state, default branch and protection contract.
It also includes `destination_account` with `owner`, `state: observed`,
`all_pages: true`, all repository IDs/names and the paginated request URL
`https://api.github.com/orgs/hemsoft-dev/repos?type=all&per_page=100`. Reject any
destination name that now occupies an approved transfer mapping, ignoring case.
Unrelated destination repositories can coexist, but cannot claim a source ID.
For fhemmer's access and one-seat verification, both independent captures must
follow that repository's actual accepted transfer event, not merely the earlier
source refresh. Recheck permission and licensing after the move.

Immediately before cutover, `pre_cutover_source_evidence_url` must reference a
fresh `reference_scan_evidence_url` covering all 67 source IDs. Each scanned
repository records successful immutable commit and complete recursive tree GETs,
the default Git reference, a complete branches GET and SHA-addressed contents
GETs for every workflow, SFL manifest and supported provider configuration file.
The file bytes must match the tree's blob IDs. Derive secret references and
installation manifests from those bytes. A newly referenced credential covered
by the legacy unused waiver requires reconciliation. The current source refresh
must repeat each repository's default head, tree and complete branch list and
match this scan. Keep the original sealed scan as historical evidence.

Reconcile every pre-transfer ledger gate after that scan. Run the owned-App
credential workflow on the current source main revision after the scan and
complete source refresh within 15 minutes of its successful completion. A prior
successful run cannot establish current credential validity.

A provider `success` smoke requires `provider_responses`, with exact resource
GET URLs, HTTP 200, raw response data and individual observation times. Vercel
checks preserve project identity, the expected GitHub repository ID/name and
production branch, READY production and existing production aliases. The retired
modern-web-stack-poc Git connection remains disconnected. Existing preview
failures do not substitute for production state. Pages checks preserve the build,
domain and HTTPS configuration and verify the destination site's actual URL.
Cloudflare checks preserve zone ownership, plan, website DNS/proxy values and
Worker routes; the Worker checks also observe its script, enabled workers.dev
subdomain and account subdomain. Runner checks observe its actual online idle
registration and labels. These primary observations must follow the real
transfer and destination metadata capture. Preserve the unused paused Supabase
resources through baseline preservation without runtime database operations.

Both shared and repository-specific SFL installation GETs must contain the
approved granted permissions. Final healthy status must preserve the revision
from repeated sync. New-repository creation metadata must come from its exact
successful repository GET observed after creation.


Before cutover, capture the complete `/git/matching-refs/tags/` array for every
source and immutable `/git/tags/{sha}` responses for each annotated tag chain.
Include every distinct tagged commit in the workflow/manifest scan. Repeat the
tag capture during source refresh and after each transfer; object identities
and peeled targets must match the scan. The sealed empty n8n-workflows repository
may return its exact empty-repository409 response, confirmed by an owner GET.
Other failed tag collections remain blockers.

Capture each source's current protection contract from complete raw policy
responses, including repository/default-ref metadata, every ruleset detail and
classic branch protection. Compare it with the reviewed preservation baseline
before proceeding. This rejects a newly strengthened source rule until its
preservation is reconciled. Keep the original inventory seal and unavailable
endpoint records unchanged.

For live scenario proof, capture the exact Actions run and artifact metadata
GETs and the immutable downloaded archive. Both metadata responses must match
those recorded objects and follow their terminal observations. After pilot
uninstall-gate, capture successful default-branch effective rules plus successful
classic protection or the exact unprotected-branch404 response. Both observations
must follow the actual removal and precede the final policy capture. Also save
`pre_cleanup_gate_policy_evidence_url` from after the latest completed pilot
validation and before removal starts. Compare both authenticated policies after
removing only the Actions-owned SFL requirement. Preserve unrelated required
checks, strictness, reviews and all other effective/classic policy.

Capture current environment enumeration and each environment's secret names
alongside repository secrets during the final source refresh. Preserve complete
GET pages, response totals and pagination links in `environment_pages` and
`environment_secret_pages`. Reconcile any changed environment or credential
scope against the reviewed runtime inventory and credential ledger.

Immediately before submitting each repository transfer, capture its complete
source policy and compare it with the reviewed preservation contract. Save the
`pre_transfer` capture as `source_protection_evidence_url` in that transfer's
receipt. All policy GETs and its completed capture must be within 60 seconds
before transfer acceptance and after the final cutoff. Refresh the capture if
submission waits longer. Stop and reconcile any late protection change first.

For repeat init/sync, save the actual command, argument array, exit code,
start/completion times and stdout digest in the terminal capture's `execution`.
Both operations must run with `--pr` against their designated repository and
report the canonical no-PR-needed output at the unchanged revision. A copied
zero-change summary alone does not satisfy either idempotence gate.

When adding a scope-exception approval marker by editing an owner comment, record
its effective update time as `approved_at`; preserve the original creation time
separately. For every registered review, capture the exact successful compare
GET from the deployed revision to its actual reviewed base, including raw data,
as `compare_response`. The ancestry observation follows that GET.

Credential and runner run captures include `run_response` with the exact
successful `/actions/runs/{id}` GET and raw data equal to the recorded run. The
GET follows actual completion and precedes capture. The credential run must be
the reviewed main workflow-dispatch run. For yahtzee, require a workflow-dispatch
run of `.github/workflows/self-hosted-smoke.yml` and `workflow_evidence_url` with
immutable contents at its actual head, equal to
[the reviewed read-only workflow](yahtzee-smoke-workflow.yml). Preserve the
existing runner's isolation, service, labels and executed-job requirements.

## Current resources and retired history

The original inventory and unavailable endpoint seal stay unchanged. The
[Windows preparation capture](current-survival-resources.json) adds the observed
main ruleset24630478, runner10 `DESKTOP-7ES73Q4`, `UE_RUNNER_ENABLED=true`, and
immutable `.github/workflows/ci.yml`. Fresh source observations must preserve
these resources alongside the original protections. Preparation does not clear
any transfer gate.

For Windows destination continuity, record `startup_model=windows_logon_task`
and `startup_active=true`, plus `registration_evidence_url`,
`startup_evidence_url`, `run_evidence_url`, `workflow_evidence_url` and
`jobs_evidence_url`. The registration capture includes its exact successful
destination `/actions/runners/10` GET. The startup capture contains fresh
`host_capture` and `startup_capture` from laptop after destination/App/policy
cutover. Preserve the existing limited interactive logon task, `run.cmd`, work
directory and foreground listener. Do not stop human processes, change tasks,
register a replacement, or cancel an existing run to collect this proof.

Dispatch the existing main CI only when the runner is available. Its capture
uses `verification_mode=existing_build_and_simulation_tests` and proves the
successful `Build and simulation tests` job ran on runner10 in the exact current
attempt. Both bare `ci.yml` and exact `ci.yml@main` run paths are supported;
other refs reject. This existing build is separate from yahtzee's zero-permission
read-only smoke. All Linux isolation, route, service and workflow checks remain
mandatory.

The [retired workflow catalog](historical-workflow-reconciliation.json) pins
the exact 14 gh-x blobs and 11 tag-only revisions found in the complete scan.
Their two unused legacy credential names retain the existing owner receipt.
Set gh-x's `inactive_historical_workflows_evidence_url` to an independent fresh
capture after the complete final scan: current repository/ref GETs, complete
current workflow pages, deleted automatic/lock workflow GETs, and the exact
recovery workflow404. The reviewed exception accepts only those bytes on tags
that are absent from current branch heads. Restored branches, changed blobs,
new references, or dynamic secret access require new reconciliation. Preserve
historical manifests while requiring agreement between both manifest locations
on every current branch.

For the inaccessible fhemmer source only, a fresh `owner_browser_evidence_url`
can accompany the exact retained Free-plan ruleset403. Keep complete successful
repository/ref and empty protected-branch GETs. Record authenticated HemSoft
classic/ruleset Settings DOM with the exact source URL and repository header;
the classic page also binds its immutable ID. Browser and policy phases must
match `pre_cutover` or `pre_transfer`. The immediate pre-transfer observations
must fall within the same 60-second acceptance window. This exception never
applies to a destination refusal, quota failure, or newly configured protection.
