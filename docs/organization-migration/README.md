# Organization migration preflight

This runbook implements the discovery and planning portion of
[issue #135](https://github.com/HemSoft/set-it-free-loop/issues/135).
It does not transfer repositories, change visibility, buy a plan, or install a
reviewer. Franz created `hemsoft-dev`; HemSoft's active owner access and the
selected Team plan are verified.

## Current evidence

[inventory.json](inventory.json) was refreshed on October 6, 2026 at 5:41 PM EDT
with the HemSoft account. Collection continued afterward; individual repository
records include their capture times. The source population was checked again
at the end of collection.

| Source | Repositories | Account type | Access |
| --- | ---: | --- | --- |
| HemSoft | 66 | Personal | Repository administrator |
| fhemmer | 1 | Organization | HemSoft is an active organization owner |
| Total | 67 | | 25 private, 14 archived |

The personal account uses GitHub Pro. The fhemmer organization uses GitHub Free
and has default repository permissions of `write`. The destination is organization
ID 338855369 and uses GitHub Team, with default repository permissions of `read`.
HemSoft is its sole member and active owner; no teams or App installations were
present when inspected. Members may create repositories; required two-factor
authentication is currently disabled. These are observed settings, not changes
made by this preflight.

At Franz's request, the destination avatar now uses the reports site's existing
gold three-ray mark on its dark background. The source SVG was rendered as a
square PNG and uploaded through GitHub's profile flow; the saved avatar was
visually verified from GitHub.

GitHub resolves the requested `hemsoft` login to the existing `HemSoft` personal
account. Franz registered the selected fallback `hemsoft-dev` himself.
Keep HemSoft as the personal login. This plan does not rename or convert it.

The snapshot maps every repository ID to a unique destination name. It records
visibility, archive state, default branch, collaborators, teams, protected
branches and their rules, repository rulesets, Actions/workflow permissions,
workflow names and paths, environments, secret/variable names, deploy key
metadata, webhook delivery hosts, Pages configuration, and App lookup results.
Secret values, variable values, private/public key material, webhook URL paths,
and webhook URL queries are not written to the snapshot.

The expected repository ID list is stored separately from the repository records
inside the snapshot. Offline validation rejects any missing or substituted ID
and any stale summary; retaining both source owners alone is insufficient.
Environment records with custom deployment policies also contain the actual
allowed branch/tag patterns from the paginated deployment-branch-policies API.

## Name collision

| Source | Visibility | Destination |
| --- | --- | --- |
| HemSoft/hs-cli-confluence-search | Public | hemsoft-dev/hs-cli-confluence-search |
| fhemmer/hs-cli-confluence-search | Private | hemsoft-dev/hs-cli-confluence-search-fhemmer |

Retain the public name to avoid changing its consumers. Give the private copy
a source-qualified name. These are separate repositories, so preserve both
IDs, Git history, issues, releases, and their respective visibility. Check the
destination names again immediately before transfer. A new conflict stops the
transfer; it does not authorize merging or deleting either repository.

## Coverage limits and remaining setup

`state: observed` means a successful API response. `state: unverified` means
the endpoint could not be inspected. An unverified 404 does not establish that
an App, setting, or resource is absent. There are 142 unverified repository
endpoints in the initial capture:

- All 67 repository App-installation lookups require different authentication
  and returned 401. The user-installations endpoint also rejects the current
  OAuth credential. Supplemental read-only browser verification of all ten
  personal-account installations is recorded in
  [owner-verification.json](owner-verification.json). Each selected installation
  includes its complete repository list; `all` includes current/future owned
  repositories. Refresh this coverage through the owner's
  [installation settings](https://github.com/settings/installations) before cutover.
- The private fhemmer repository's ruleset lookup returned 403. Its owner settings
  page shows no configured rulesets and warns that private rulesets require Team
  for enforcement. This supplemental browser evidence is recorded separately;
  retain the API failure as an accurate record of that endpoint's access.
- Nine branch-protection detail lookups returned 404 despite successful
  protected-branch listings. All nine effective branch-rule lookups succeeded
  and are recorded in the supplemental verification file, alongside the full
  ruleset details in the main snapshot. Refresh effective rules before transfer.
  The summary still includes the original nested endpoint failures.
- Pages lookups returned 404 for 65 repositories. Two Pages configurations were
  observed: reports uses a workflow build; summarist uses `gh-pages`. Neither
  reported a custom domain. Treat unresolved Pages lookups as unverified.

The fhemmer organization installation list is readable and records Blacksmith,
App ID 807020, with all-repository selection. Check its runner and billing setup
in the new organization during integration migration. The separately owned
[sfl-app](https://github.com/apps/sfl-app) registration is App ID 4448946 and is
owned by the HemSoft personal account. Registration ownership and repository
installation coverage are different facts.

The SFL App installation is ID 150383874, with 64 selected personal repositories.
It excludes `HemSoft/codexbar` and `HemSoft/survival-shelter-opus55`. Fly.io selects
`HemSoft/codexbar`. Vercel selects dashboard, hs-landing-page, now-leadership-group,
and set-it-free-loop-site. Azure Pipelines, ChatGPT Codex Connector, Claude,
cubic-dev-ai, Cursor, Greptile Apps, and Railway App have all-repository access.
Installation access does not prove a live service deployment or reviewer runtime.

External hosting accounts, package registries, runner registrations, environment
credential validity, and webhook routing beyond the recorded delivery hosts need
an owner-verified integration ledger. Repository-level runner registrations and
secret/variable names for all 16 configured environments were additionally
captured in [runtime-metadata.json](runtime-metadata.json), with no failed
lookups. One repository runner is registered under HemSoft/yahtzee. All 16
environment secret/variable name lists were observed empty.
Organization runner groups and external provider runner resources remain
separate checks. Record the provider, repository, service
owner, affected URL/reference, credential source, cutover action, smoke test,
and recovery action. Keep credentials and sensitive callback paths outside Git.
Do not treat an empty webhook list as proof of no external deployment.

The destination's organization, owner membership, and teams are now observed
in the refreshed snapshot. Its Actions policy API returned 403 because the CLI
lacks `admin:org`. The signed-in HemSoft browser verified the policy instead:
all repositories may use all actions/reusable workflows; full-SHA pinning is
not required; standard hosted runners are enabled. First-time contributors need
approval for fork workflows; private fork workflows are disabled. Workflow
tokens default to read contents/packages, and Actions cannot create or approve
pull requests. The supplemental owner-verification file records this evidence
and its source URL. No settings were changed.

## Plan and credential requirements

GitHub Team or Enterprise Cloud is needed to retain protected branches/rulesets
for organization-owned private repositories and to use organization secrets and
variables in private repositories. HemSoft's personal Pro subscription does not
establish the destination organization's plan. The observed destination Team
plan supports these features; still verify each repository's effective policy
and credential access during transfer.

Verify the actual requirements against GitHub's documentation for
[protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches),
[repository transfers](https://docs.github.com/en/repositories/creating-and-managing-repositories/transferring-a-repository),
[organization secrets and variables](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-secrets),
and [organization rulesets](https://docs.github.com/en/organizations/managing-organization-settings/creating-rulesets-for-repositories-in-your-organization).
Franz selected the destination plan himself. The agent made no purchase;
future billing changes remain owner steps.

The private, non-fork `fhemmer/hs-cli-confluence-search` reports effective admin
access for `fhemmerrelias`. The [supplemental owner/API access evidence](fhemmer-access-evidence.json)
shows that this comes from source organization ownership: direct collaborators
and repository teams are empty. Effective access alone does not identify a
retained direct collaborator. Check `collaborators?affiliation=direct` and the
[owner access page](https://github.com/fhemmer/hs-cli-confluence-search/settings/access)
before classifying a transferred account as a billable outside collaborator.

Franz selected removal of that access **after transfer** in
[the access decision](https://github.com/HemSoft/set-it-free-loop/issues/138).
Source organization membership does not grant destination access. No additional
license is needed for the observed direct grants, and no new membership or grant
will be added for `fhemmerrelias`. Verify effective destination access and the
one-seat licensing state after transfer; stop if an unexpected billing change
appears. Do not revoke source membership early or purchase another seat.

The current gh OAuth token has `repo`, `read:org`, `user`, `gist`, and
`admin:public_key`; it lacks `admin:org`. Reading an organization resource can
work without authority to write all organization settings. Grant only the
scopes/permissions needed for the chosen operations through the owner's normal
authentication flow. App tokens need the relevant organization permission in
addition to repository access. Do not assume repository transfer authority
permits all organization secret, variable, runner, or membership changes.

Git remotes use `git@github-personal1:OWNER/REPO.git`; API calls use the HemSoft
account. The existing HemSoft key was verified against GitHub and the missing
`github-personal1` host alias was added on mini. This is a local routing repair,
not a new credential.

## Refresh and validate

Run from the SFL repository root with Python 3 and GitHub CLI:

```bash
gh api user --jq .login
python3 -B deployment/scripts/capture-org-migration.py --destination hemsoft-dev
python3 -B deployment/scripts/capture-org-migration.py --check docs/organization-migration/inventory.json
python3 -B -m unittest discover -s deployment/tests -p test_org_migration.py -v
```

The first command must return `HemSoft`. Collection uses only GET requests and
requires a classic/OAuth credential with full `repo` scope and active HemSoft
ownership of fhemmer. Repository-limited credentials are rejected. The collector
also rejects omission of any ID from an existing output snapshot; reconcile
legitimate source changes explicitly rather than overwriting that baseline.
See GitHub's [OAuth scope definitions](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps).
It requires successful, paginated enumeration of both owners and checks for a
changed source population before writing the snapshot. Offline validation
rejects duplicate IDs, case-insensitive destination collisions, invalid logins,
unexpected owners, and destination names outside the exact collision mapping.
A passing structural check does not mean inaccessible settings are
verified or that the destination exists. The CI workflow performs only offline
validation and does not receive administrative credentials.

The offline `--check` command requires the stored summary and reads both sibling
supplemental JSON files. It reconciles every runtime repository and environment
with the main snapshot, runner and App IDs with their expected manifests, App
repository selections with the personal source population, and effective-rule
coverage with the unresolved classic-protection lookups. Owner identity,
destination ID/plan, members and teams are checked as well. Missing, malformed,
or inconsistent supplemental evidence fails CI. Review updates to the manifests
together with the corresponding owner/API evidence.

Refresh supplemental runner and environment names through paginated GETs to
`repos/OWNER/REPO/actions/runners`,
`repos/OWNER/REPO/environments/ENVIRONMENT/secrets`, and
`repos/OWNER/REPO/environments/ENVIRONMENT/variables`. URL-encode environment
names, and project secret/variable responses to names before writing them.
These supplemental records are separate from the main collector; refresh
the owner-browser App selections, Actions settings and effective branch rules
as well. Preserve the source population and record every failed lookup.

Before the actual transfer, refresh again and compare every source ID, full
name, visibility, archive state, default branch, and destination with the reviewed
map. Review any change before proceeding. Export current refs, issue/PR counts,
releases and integration settings for the transfer receipt. Verify App coverage,
the integration ledger, billing, protections, membership and both collision
names. Stop on unknown or conflicting state.

The fresh reference scan reads the recursive tree and all relevant source files
at every distinct captured branch head. `branch_scans` contains non-default
heads, each with its immutable commit/tree responses and file bytes. Branches
sharing one head need one scan. Secret references and unresolved inherited or
dynamic secret access are reconciled across all branches; the default branch
alone supplies the installed manifest. An unscanned branch cannot clear a
credential waiver.

An exact immutable commit may point to Git's canonical empty tree,
`4b825dc642cb6eb9a060e54bf8d69288fbee4904`. GitHub can return 404 for that tree.
Accept only its exact tree endpoint and `Not Found` response, bound to the
captured commit. Other failed or unavailable trees cannot establish emptiness.

The final source refresh includes complete successful raw `secret_pages` and
`runner_pages` for every repository. Each observation follows the branch scan
and precedes the refresh completion time. Secret names must equal the sealed
inventory; runner IDs must equal the reviewed runtime inventory. New names or
registrations require credential and ledger reconciliation before any transfer.
Never collect secret values for this inventory check.

The source App credential receipt includes immutable contents captures for
`.github/workflows/verify-sfl-app-credential.yml` and
`deployment/scripts/SflGitHubAppBootstrap.psm1` at the successful run SHA. Both
must equal the reviewed canonical bytes. Successful artifact output without
those implementation captures cannot clear the transfer gate.

Pre-sync inspection authenticates both manifest paths at the actual input SHA.
Presence derives from encoded contents and their Git blob identity. Absence
requires the exact contents endpoint's 404 response. Parsed tier, add-ons and
components must equal the captured manifest bytes.

Provider absence, the inventoried external-resource scope and the 42 unused
legacy credential entries include successful raw owner-comment captures. The
shared owner identity and structured-decision checks bind their exact scope to
the existing HemSoft receipts. These captures must precede the final source
refresh. A local summary or comment URL alone cannot establish an owner fact.

## Current execution scope

Franz's [October 6 retention decision](https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028207635)
keeps now-leadership-group and set-it-free-loop-site in personal HemSoft. Preserve
their privacy and the site repository's archive state. The sealed baseline remains
67 IDs; the [scope decisions](scope-decisions.json) and
[rollout matrix](rollout-matrix.json) define 65 transfers and 2 retained sources.
Do not transfer those two repositories or include them in organization rollout.
Verify any effect of App transfer on their existing personal installation.

## Transfer order and recovery

1. Complete organization setup and the destination/permission checks above.
   Prepare the organization-capable SFL tooling in
   [#136](https://github.com/HemSoft/set-it-free-loop/issues/136) and review
   authorization in [#137](https://github.com/HemSoft/set-it-free-loop/issues/137).
   Have a cutover-compatible package available before moving the source.
2. Execute [#138](https://github.com/HemSoft/set-it-free-loop/issues/138) with a
   designated low-risk repository first. Test the transfer and recovery path
   before the rest of the population. Preserve archive state and visibility.
3. Move SFL's source and owned App with their dependent consumers in a bounded
   cutover window. GitHub App registration transfer is separate from installing
   it on the organization. Observe transfer warnings and verify identity,
   installation repository coverage, and token access afterward.
4. Transfer the remaining in-scope repositories in reviewed batches. Keep both
   `hs-cli-confluence-search` IDs and apply the explicit name mapping. Inventory
   `.github` organization defaults before its move, since organization profile
   and community files may affect other repositories.
5. Reconcile integrations and canonical references before the next batch.
   Preserve old repository redirects. Do not recreate repositories at the old
   locations, which would destroy their redirects. Pages sites and package
   links require explicit verification; repository redirects do not establish
   that they migrated.
6. Run [#139](https://github.com/HemSoft/set-it-free-loop/issues/139) only after
   migration and its reviewer prerequisites are verified. Keep archived
   repositories archived and account for them in the rollout report.

For each repository, log the source/destination IDs, before/after full names,
visibility, archive state, refs, releases, issues/PRs, protections, memberships,
App coverage and integration tests. Test a fetch using `github-personal1`, a
normal push through the repository's policy, and representative CI/deployment.
The population check accounts for every original ID exactly once: 65 at their
mapped destinations and the two owner-approved retained IDs at their original
HemSoft locations, with source metadata verified. Retention is not transfer proof.

If a transfer or integration check fails, stop the batch and retain all data.
Reconcile permissions, App access or the failing integration first. A reverse
transfer is an owner operation and must check name availability, plan support,
redirect consequences, and repository access before execution. Restore settings
from the reviewed baseline without weakening protections. Keep old accounts;
this plan does not delete fhemmer or the HemSoft personal account.

## Reviewer baseline

The current [roadmap](../../TODO.md) says the private HemSoft SFL reviewer effort
was discontinued on August 20, 2026. It explicitly rejected the native Codex
observer as an implementation of the SFL PR Reviewer. The current user request
starts a new organization initiative, but this preflight does not restore or
deploy that reviewer. The later reviewer issues must define and prove the
intended SFL-owned runtime before enforcing it; an external `@codex review`
result alone is not SFL reviewer proof. Use connected Codex for review of this
preflight PR under the current repository review policy.

The offline check requires every captured repository settings section and its
result structure. App access mode and selected repository sets are reconciled
against `expected_repository_selections`, separately from installation IDs.
Refresh that manifest from the owner-verified App selections during preflight;
review changes to both records rather than regenerating it from a truncated list.

Top-level organization, owner membership and owned/installed App evidence are
also mandatory. `expected_repository_sources` pins each ID to its source name;
`expected_effective_branch_rule_hashes` preserves every captured branch rule
payload. Unverified credential results cannot contain data.

[evidence-integrity.json](evidence-integrity.json) pins the complete canonical
JSON payload of all three evidence files, including nested collections and
metadata. This catches omitted collaborators, altered permissions, truncated
protections and other evidence changes that preserve record counts. It is an
accidental-change check, not proof that external settings remain current.
After live capture and independent owner/runtime reconciliation, review every
changed evidence record and expected manifest, then deliberately reseal:

```sh
python3 -B deployment/scripts/capture-org-migration.py --write-integrity docs/organization-migration/inventory.json
python3 -B deployment/scripts/capture-org-migration.py --check docs/organization-migration/inventory.json
```

Do not update expected manifests or reseal merely to make a failing check pass.
The reviewer must assess both the changed evidence and manifest in the PR.

## Transfer and rollout work records

Use the [per-repository integration ledger](integration-ledger.csv),
[rollout matrix](rollout-matrix.json), and [operator sequence](ROLLOUT.md) to
complete #138 and #139. Their pending entries are not new verified baseline
evidence. The original snapshot and its integrity manifest remain unchanged.

## Later owner updates

The owner changed source SFL App installation 150383874 to all repositories.
`source-app-selection-owner-update.json` records that report and its issue
receipt. The original selected-repository capture remains historical evidence.
Fresh saved-selection verification is pending. The GET-only App credential
workflow publishes an allowlisted public metadata artifact with installation
identity and repository selection. It does not mint an installation token.
The credential receipt loads the run's immutable artifact GET metadata and
downloaded archive, verifies its GitHub digest and compares the sole metadata
file with the local capture. Run identity, current attempt start and output
chronology must match; a locally authored JSON beside a successful run is
insufficient.

The dashboard Supabase database `cevpnetigzotgstxxjpm` is unused.
`dashboard-database-owner-disposition.json` records the instruction to preserve
its paused database and configuration. This does not authorize resuming, querying
or deleting the database, or waive active dashboard workflow credentials.

Completed reviews require a repository-bound effective branch-policy capture.
It must show the strict SFL gate actually required with GitHub Actions App 15368.
Both effective rules and classic protection include their exact repository and
branch GET endpoint, status, raw data and observation time. Only an explicit
`Branch not protected` 404 establishes absent classic protection. Terminal
response observations must follow the completed review and cutover boundary.
Pilot scenario receipts identify their own repository, release and deployment.
Successful workflow receipts name a workflow deployed by the selected tier or
addons and its immutable run head. Pilot manifests must deploy the review
observer. Newly observed pre-sync manifests bind their installed addons and
custom components, including repositories without a manifest in the original
scan. Final new-repository onboarding follows an independent completion capture
for every baseline repository and uses the captured Codex installation ID.
Baseline-covered archived repositories must preserve destination SFL App access.

Final onboarding records include an independent post-status manifest capture,
canonical release/checksum verification, actual merged init PR, successful sync
outcome and zero-change repeat results. Revision receipts follow the operation
sequence through final status. A repeat or sync that changes no files records
`no_changes`; it must not invent a new PR.
The manifest capture names `.sfl/sfl.json` or `sfl.json` and includes its successful
immutable contents response. Decode the repository bytes and verify their Git
blob identity before deriving tier, addons, source, SHA and version. A local
manifest summary cannot establish the installed configuration.

All transferred SFL App coverage reconciles with the single destination
installation in `owned-app-organization-installation-evidence.json`. Its current
state is pending and does not claim an installation. The source credential
receipt must independently verify installation 150383874 selecting `all`, as
reported by the owner. The wider pilot executes a successful non-Auditor
workflow distinct from its Auditor run. Known provider actions must preserve
the captured dashboard paused-database and modern-web-stack Git-retirement
decisions; nonempty text that contradicts those decisions cannot clear a gate.

Workflow run heads match the source release SHA or a separately captured
consumer/pilot revision containing the recorded manifest. Review permission
captures bind the human requester, repository, PR, head and base to a successful
GET of that actor's exact collaborator-permission endpoint. The recorded result
must equal its raw response data. The required gate covers the inventory default
branch, including `master` and `develop`. Its completed successful Actions
check or run is reconciled with a separate result capture and review artifact;
an issue URL or cancelled gate cannot verify a review. Every active consumer
configuration must deploy the observer, including minimal and standard tiers.

Pilot init/sync operations record merged PR outcomes. Repeat operations record
zero changes, status records a healthy result, and gate uninstall records only
gate removal with zero unrelated changes. Their revisions follow the actual
operation order. Final new-repository checksum evidence binds the canonical
release, asset URL, source repository ID/SHA, target ID, matching SHA256 and
successful checksum/attestation verification. Runtime scope exceptions need a
separate captured HemSoft decision on issue 138 or 139 with matching repository,
disposition, reason and approval time; the matrix cannot authorize its own
exception. Destination protection proofs reconcile with independent captures
of post-transfer identity, revision, observation time, rulesets and classic
protection before comparing the preserved baseline.

Integration rows record `smoke_outcome`, `smoke_phase` and a local
`smoke_evidence_url`. Captures bind the resource and repository to successful
continuity, approved recovery, preservation of an unused resource, or unchanged
baseline observations. Destructive changes and unsuccessful results fail the
gate. Pre-transfer readiness can preserve an unused database or retired Git
connection without executing runtime operations; final completion requires
post-transfer continuity captures. The modern-web-stack preservation capture
records its existing approved disconnection and makes no post-transfer claim.
Its fresh successful Vercel project GET verifies the preserved project ID,
configuration, environment names, aliases and historical deployments.

Reports preparation PR [63](https://github.com/HemSoft/reports/pull/63) is merged.
Its [live read-only credential check](https://github.com/HemSoft/reports/actions/runs/37569272374)
now verifies HemSoft identity, classic repo scope and private organization
access, completing Reports issue [62](https://github.com/HemSoft/reports/issues/62).
The organization Actions API scope block is also cleared; this does not claim
that organization secrets have been distributed or transfer gates completed.

The pre-transfer App credential proof loads the workflow's uploaded public
metadata and a separate successful main-run API capture. App/client/owner,
installation, selection, permission ceiling and reviewed SHA must match those
captures. Consumer, pilot, protected-source and final onboarding downloads all
use the same structured canonical asset/checksum/attestation contract.
Registered reviews include a captured repository comparison showing their base
contains the deployed revision; a coherent older review cannot validate a newer
deployment. Classic protection comparisons normalize only the known source to
destination URL prefixes, preserving checks on every semantic setting. Both PR
and main-push validation run for changes to any source workflow YAML.

Completion now loads each active consumer's successful `gh sfl status` capture
at its deployed revision. The capture reconciles the exact manifest and every
installed workflow's presence and matching content hash. A final inventory must
follow all terminal new-repository operations and registered-review captures,
including the actual status command rather than only its manifest observation.
Runner registration, isolation, service and smoke execution follow both the
App transfer and independently captured destination cutover.

Workflow-fixture inputs include GitHub's contents response requested at the
immutable deployed revision. Decoded bytes, size, Git blob SHA and repository
blob URL must match the tested workflow. Registered-review captures load the
unedited request comment, requester-created registry status and native Codex
artifact. The successful check's full external ID must match their context,
request ID, creation time and artifact ID. Pilot metadata supplies the actual
default branch; its required gate and final inventory must preserve that branch.
All offline regression captures are synthetic and do not claim live transfers.
Request comments and native artifacts derive from their exact successful GETs.
Registry statuses derive from complete raw status pages for the reviewed head.
Recorded fixture success also requires independently executing the reviewed
observer fixture against the captured immutable workflow bytes in temporary
storage. Compare the resulting identity, scenario, outcome and source digests;
an output file that merely claims success cannot clear the pilot gate.
Pending-record checks are offline. Completed release-download records additionally
run the real GitHub signature verifier against file bytes, downloading the exact
canonical asset when its captured local path is unavailable. Captured success
flags and attestation JSON alone cannot satisfy this gate.

Successful observer checks now publish an execution record containing their
Actions run ID and attempt, execution/workflow revisions, exact check ID and
registered external ID. Completion reconciles that record with the actual
successful observer run and independently captured executed/deployed workflow
contents. The executed workflow must preserve the deployed bytes and descend
from the deployed revision. Custom reviewed-head checks can have a different
check suite from an issue-comment execution; suite equality is not required.
This provenance output ships with the next release containing these templates.

Release download proofs load a separate successful `gh release verify-asset`
result and its signed GitHub release statement. The canonical repository ID,
release tag, source commit and signed asset digest must match the download.
The download must name the executable for its recorded platform,
`gh-sfl_<version>_linux_amd64` or `gh-sfl_<version>_windows_amd64.exe`.
Equal local expected/actual hashes and verification flags alone cannot clear
this gate. Final inventory also compares the exact `visibility` value for every
baseline repository, pilot and additional repository, preserving public or
private access rather than accepting internal visibility through a private flag.

Before the final source refresh, save an independent pre-cutover ledger readiness
capture and set `pre_cutover_ledger_evidence_url` in the matrix. It contains the
complete rows, their canonical JSON SHA256, owner identity, organization ID,
observation time and SHA256 pins for every referenced local evidence file. Run
the same integration contracts against these frozen rows. All transfer gates
must already be verified, with pre-transfer provider smokes and verification
and evidence times no later than the snapshot. Source refresh and App transfer
follow this capture. Preserve it when the current ledger gains post-transfer
smokes; current status strings alone cannot establish earlier readiness.

After the complete source refresh, save `pre_cutover_tree_evidence_url` with fresh
canonical metadata and complete branch GETs for both originally unavailable trees.
Bind fhemmer's unchanged main head to its raw empty-tree Git commit; n8n-workflows
must still have no branches. Preserve the original recheck as historical evidence.
Every observation follows source refresh, and the completed tree capture precedes
every repository or App transfer. New commits or branches require a new source
scan and reconciliation rather than reusing an earlier empty classification.
Immediately before App transfer, save `owned_app_transfer.pre_transfer_owner_evidence_url`
from an authenticated `GET /apps/sfl-app` after the completed tree capture. It must
still show the approved personally owned App/client/permission identity. The later
organization-owned registration capture must follow this observation, bracketing
the ownership change after every gate rather than merely observing it afterward.
That later capture also records the exact registration GET request URL and its
successful HTTP status. Preserve the returned owner metadata while checking the
approved organization ID, login and type.

Terminal archive outcomes load actual destination repository metadata. Active
consumer and protected-source outcomes additionally load full protection captures
after terminal gate installation, retaining baseline rules while proving the gate
is still required. These observations contribute to final completion chronology.
The final new-repository onboarding orders all five terminal CLI operations and
loads a repository-specific, unsuspended owned-App installation GET after creation.

Every terminal Actions capture includes the API run's `created_at` and
`updated_at`. Captures follow actual completion, and final rollout chronology
includes that completion time. The raw source credential run also records its
local `captured_at`, before the final source refresh.

Live negative scenario proof requires a downloaded immutable Actions artifact
named `sfl-observer-scenario-<scenario>`. Save its GET metadata, repository-bound
request/download URLs, archive bytes and GitHub SHA256 digest separately. Its
only file, `scenario.json`, must equal the scenario output and identify the exact
run and attempt. The captured artifact ID belongs to that observer run and
deployed revision. A run without a matching scenario artifact does not establish
live proof; the tracked executed workflow fixture remains an explicitly labeled
alternative for the supported scenarios.

Pilot init, repeated init, sync, repeated sync and status follow both revision
and execution order. Gate removal follows the latest validated scenario,
registered review, required policy and wider-workflow evidence. An independent
default-branch policy capture after removal must show the SFL gate absent.
Required-policy evidence before this cleanup still proves the operational test;
the separate final onboarding repository keeps its required working gate.

Final account preservation compares the full observed unlinked Supabase project
record with the sealed project, including reference, name, region, paused state
and resource URL. A no-change flag alone is insufficient. This remains a
read-only configuration check and does not authorize a database operation.

Consumer status hashes must derive from separate immutable canonical-source and
destination contents GETs for every selected workflow. The validator checks
decoded Git blob bytes and the supported CLI rendering before accepting status.
Final inventory additions also require the actual issue138 comment, its HemSoft
author identity and an explicit structured approval matching the added repository.
See the capture fields in [the rollout runbook](ROLLOUT.md).

Before cutover, the [rollout capture contract](ROLLOUT.md) requires a fresh
reference scan and head/branch comparison for every source, followed by ledger
reconciliation and a current-main owned-App credential run. Provider success
records carry service-specific GET responses, repository bindings and observed
production state. Historical preparation checks do not clear these live gates.

Before cutover, every source and destination account enumeration must include
its raw successful GET pages, observation times and response headers. Follow
all `Link` next links on the exact owner endpoint and reconcile their repository
bodies with the recorded list before checking mapped-name collisions. Summary
lists and `all_pages` flags alone cannot establish a complete enumeration.

Active consumers need `default_branch_evidence_url` from a successful GET of
their actual default ref after status, review, policy and wider-workflow proof.
The observed commit must equal the verified deployment revision. If the branch
advances, verify the new revision and repeat the terminal capture before marking
completion. A descendant commit alone does not prove it preserved installed files.

Unlinked paused Supabase preservation also needs a successful resource-specific
project metadata GET after rollout, bound to the sealed name, region, organization
and `INACTIVE` state. Native dashboard metadata requires the matching organization
GET to bind its numeric organization ID to the sealed slug. This reads metadata
only and preserves the paused database and configuration.

Source branch captures include raw successful GET pages and response headers,
follow every `Link` next page with consecutive page numbers and unchanged filters,
and derive the recorded branch list from those bodies. The reference scan and
fresh head refresh both validate this contract. Refresh pages follow the scan;
the separate empty/uninitialized-tree branch recheck follows source refresh.

Codex all-current-and-future coverage derives from the raw paginated
[organization installation GET](https://docs.github.com/en/rest/orgs/orgs#list-app-installations-for-an-organization).
Bind the actual installation/App IDs, account identity, `repository_selection=all`
and explicit unsuspended state. Final onboarding saves a new
`codex_installation_policy_evidence_url` after repository creation, then a terminal
`default_branch_evidence_url` after all onboarding and registered review evidence.
The final ref must equal the verified installed revision. Final inventory follows
that last ref observation.

An approved recovery marker names the exact ledger `recovery_action` and concrete
operation, including command and arguments. Its separate timestamped execution
receipt binds the same repository, provider, resource and approved operation to a
successful terminal result. Execution follows the owner approval, and provider
metadata GETs follow execution. A free-form reason or healthy final state cannot
authorize a different operation.
The effective approval time is the owner comment's `updated_at`, including when
an earlier comment is edited to add the structured decision. The approved
operation must start after that effective approval.

Protected-source governance captures retain successful repository-specific GETs
for Actions policy and workflow permissions, complete raw label pages, and the
immutable `.github/CODEOWNERS` contents at the verified source revision. Their
observations follow App cutover and derive the recorded governance summaries.

Runner smoke jobs derive from complete successful raw pages of the exact run
attempt's jobs API. Record each response body, HTTP status, observation time and
pagination headers. The executed smoke job must identify the preserved runner;
a manually supplied job summary cannot establish machine continuity.

Transfer captures include the owner-authenticated JSON audit export request,
ready response, successful truncation verification and downloaded gzip bytes.
The request uses `action:repo.transfer`; bind all returned routes by SHA256 and
retain only their origin, path and query parameter names, keeping signed routes
outside Git. Require `truncated=false`, matching archive digest and the exact
transfer event decoded from the complete raw export. Its timestamp must follow
the final source/tree/ledger cutoff. The [organization audit export](https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization)
works with Team; the audit REST API requires Enterprise Cloud.

Fresh workflow scans reject an unused-credential waiver when `secrets: inherit`
or computed `secrets[...]` leaves its scope unresolved. Reconcile the reusable
workflow or dynamic access and obtain the required credential evidence before
cutover. Static dot and literal bracket references remain explicitly enumerated.
