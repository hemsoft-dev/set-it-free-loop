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
