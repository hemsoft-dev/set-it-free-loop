# Transfer gates and organization rollout

Use this with the [migration baseline and transfer procedure](README.md),
[mandatory transfer gates in #138](https://github.com/HemSoft/set-it-free-loop/issues/138),
and [rollout acceptance criteria in #139](https://github.com/HemSoft/set-it-free-loop/issues/139).
The [integration ledger](integration-ledger.csv) and
[rollout matrix](rollout-matrix.json) cover all 67 source IDs and their collision
mappings. They are work records: no transfer or rollout is marked verified.

## Owner integration ledger

Complete a row for every repository before transferring any repository or App.
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
organization Git deployments. Plan or hosting treatment for all three private projects is a mandatory gate.
Project IDs, Git repository IDs, domains, variable names and deployment states
are observed; credential validity remains pending. The owner Shared tab and API both show no
shared variables in this workspace.
No variable values were read or recorded. Preserve the current production
projects and aliases while preparing the cutover.

## Runtime prerequisites

Native Codex is OpenAI-owned App 1144995. Organization installation 168678981 is
verified with all-repository selection; a [real private organization review](https://github.com/hemsoft-dev/sfl-migration-pilot-private/pull/1#issuecomment-6027603864)
completed clean at `f225d94d49a5951d6b2b205ad08990337aff040f`. Check the organization's
[installed Apps](https://github.com/organizations/hemsoft-dev/settings/installations)
and authenticate the requester through their connected account.

The owned SFL App is a separate registration, App 4448946. Its ownership transfer
and organization installation must follow #138's gates. Verify App/client
identity, intended permissions, and actual per-repository operations afterward.
Do not count native Codex installation or a clean external Codex comment as proof
that the intended SFL PR Reviewer runtime is restored. The
[reviewer baseline](README.md#reviewer-baseline) records its discontinued state.
Define and validate that SFL-owned runtime before claiming #139 complete.
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
   active repository. Record the existing manifest tier and addons first; keep
   them during sync. A fresh consumer defaults to reviewer, while wider autonomous
   tiers require deliberate configuration. Archived repositories remain archived.
2. Install the reviewed CLI package and choose an immutable release from the
   canonical source. Follow [organization deployment](../ORGANIZATION-DEPLOYMENT.md)
   once #136 is merged. Verify its checksum and version, then run:

   ```text
   gh sfl init --repo hemsoft-dev/<pilot> --source-ref v<version> --pr
   gh sfl sync --repo hemsoft-dev/<pilot> --source-ref v<version> --pr
   gh sfl status --repo hemsoft-dev/<pilot>
   ```

   Merge the deployment through ordinary repository policy. Repeat sync twice;
   expect one consistent deployment and no duplicate PR, preserving unmanaged
   files, installed tier, and addons. Record the source SHA and actual manifest.
3. On a same-repository PR targeting the default branch, request review as an
   authorized write-or-higher human using `gh sfl review --repo ... --pr ...`.
   Record requester, registry comment/status, immutable head and base, Codex
   artifact identity, gate run, and required status target. Installation selection
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

For each repository fill the matrix's installed/selected tier, source/version/SHA,
App access, gate policy, health, and immutable evidence links. Run status and
compare its manifest, source pin, and effective branch rules with the row. A failed
check remains failed or pending; a scope exception requires an explicit owner
receipt, not a default skip. Archived rows need transfer/settings evidence, not
unarchiving or new workflow runs. Reconcile any inventory additions explicitly.

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

Re-enumerate both sources and the destination. Each original ID must occur exactly
once at its mapped destination; none may remain under the sources. Match the matrix
against that fresh inventory, including archive state and explicit additions.
Close #138 only after data/settings/integration checks and new-source release
installation pass. Close #139 only after every active target and new-repository
onboarding passes, or an explicitly accepted scope exception is linked. An empty
rollout evidence field never establishes completion.
