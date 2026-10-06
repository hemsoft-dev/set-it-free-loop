# Organization migration preflight

This runbook implements the discovery and planning portion of
[issue #135](https://github.com/HemSoft/set-it-free-loop/issues/135).
It does not transfer repositories, change visibility, buy a plan, or install a
reviewer. The destination organization has not yet been created or verified.

## Current evidence

[inventory.json](inventory.json) was captured on October 6, 2026 at 5:24 PM EDT
with the HemSoft account. Collection continued afterward; individual repository
records include their capture times. The source population was checked again
at the end of collection.

| Source | Repositories | Account type | Access |
| --- | ---: | --- | --- |
| HemSoft | 66 | Personal | Repository administrator |
| fhemmer | 1 | Organization | HemSoft is an active organization owner |
| Total | 67 | | 25 private, 14 archived |

The personal account uses GitHub Pro. The fhemmer organization uses GitHub Free
and has default repository permissions of `write`. The new organization must
have an explicitly recorded permission policy rather than inheriting that
choice without inspection.

GitHub resolves the requested `hemsoft` login to the existing `HemSoft` personal
account. The selected fallback for this migration is `hemsoft-dev`. Its lookup
returned 404, which does not reserve the name or prove it can be registered.
Keep HemSoft as the personal login. This plan does not rename or convert it.

The snapshot maps every repository ID to a unique destination name. It records
visibility, archive state, default branch, collaborators, teams, protected
branches and their rules, repository rulesets, Actions/workflow permissions,
workflow names and paths, environments, secret/variable names, deploy key
metadata, webhook delivery hosts, Pages configuration, and App lookup results.
Secret values, variable values, private/public key material, webhook URL paths,
and webhook URL queries are not written to the snapshot.

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
an App, setting, or resource is absent. There are 133 unverified repository
endpoints in the initial capture:

- All 67 repository App-installation lookups require different authentication
  and returned 401. The user-installations endpoint also rejects the current
  OAuth credential. Verify personal-account App installation coverage through
  the owner's [installation settings](https://github.com/settings/installations)
  or an appropriate App credential before cutover.
- The private fhemmer repository's ruleset lookup returned 403. Verify its
  effective protection policy before transfer.
- Pages lookups returned 404 for 65 repositories. Two Pages configurations were
  observed: reports uses a workflow build; summarist uses `gh-pages`. Neither
  reported a custom domain. Treat unresolved Pages lookups as unverified.

The fhemmer organization installation list is readable and records Blacksmith,
App ID 807020, with all-repository selection. Check its runner and billing setup
in the new organization during integration migration. The separately owned
[sfl-app](https://github.com/apps/sfl-app) registration is App ID 4448946 and is
owned by the HemSoft personal account. Registration ownership and repository
installation coverage are different facts.

External hosting accounts, package registries, runner registrations, environment
credential names, and webhook routing beyond the recorded delivery hosts need
an owner-verified integration ledger. Record the provider, repository, service
owner, affected URL/reference, credential source, cutover action, smoke test,
and recovery action. Keep credentials and sensitive callback paths outside Git.
Do not treat an empty webhook list as proof of no external deployment.

Organization creation requires a signed-in owner browser. The collaborative
browser currently reaches GitHub's sign-in page; CLI authentication does not
create a browser session. Use the
[new organization flow](https://github.com/organizations/new), create
`hemsoft-dev` if available, and retain the personal account. A signup CAPTCHA,
MFA step, legal confirmation, or billing step must be completed by the owner.

After creation, record the canonical login, organization ID, active HemSoft
owner membership, members/teams, default repository permissions, Actions
policy, and actual plan. Refresh the snapshot to replace the destination's
currently unverified organization, membership, teams, and policy entries.
No actual destination plan has been selected or purchased in this preflight.

## Plan and credential requirements

GitHub Team or Enterprise Cloud is needed to retain protected branches/rulesets
for organization-owned private repositories and to use organization secrets and
variables in private repositories. HemSoft's personal Pro subscription does not
establish the destination organization's plan. Free can be used to establish an
empty organization, but private transfers requiring these features must remain
blocked until the destination supports them.

Verify the actual requirements against GitHub's documentation for
[protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches),
[repository transfers](https://docs.github.com/en/repositories/creating-and-managing-repositories/transferring-a-repository),
[organization secrets and variables](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-secrets),
and [organization rulesets](https://docs.github.com/en/organizations/managing-organization-settings/creating-rulesets-for-repositories-in-your-organization).
Any paid plan selection remains an owner billing step; this issue does not
authorize a purchase.

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
requires successful, paginated enumeration of both owners. It checks for a
changed source population before writing the snapshot. Offline validation
rejects duplicate IDs, case-insensitive destination collisions, and unexpected
owners. A passing structural check does not mean inaccessible settings are
verified or that the destination exists. The CI workflow performs only offline
validation and does not receive administrative credentials.

Before the actual transfer, refresh again and compare every source ID, full
name, visibility, archive state, default branch, and destination with the reviewed
map. Review any change before proceeding. Export current refs, issue/PR counts,
releases and integration settings for the transfer receipt. Verify App coverage,
the integration ledger, billing, protections, membership and both collision
names. Stop on unknown or conflicting state.

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
4. Transfer the remaining repositories in reviewed batches. Keep both
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
The population check passes only when every original ID appears exactly once
at its mapped destination and none remains at either source owner.

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
