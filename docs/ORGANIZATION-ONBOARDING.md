# Organization onboarding

The canonical source is [hemsoft-dev/set-it-free-loop](https://github.com/hemsoft-dev/set-it-free-loop). Authenticate the operator as HemSoft; use the `github-personal1` SSH profile. The private SFL App and native Codex installation cover all current and future organization repositories. Reviewer deployments require neither an App private key nor a model credential.

## New repository

Install the [checksum-verified canonical release](https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v2.1.0-rc.21). With repository write access, run `gh sfl init --repo hemsoft-dev/REPOSITORY --pr`. The default tier is reviewer. Add owner-bound `.github/CODEOWNERS` only when no CODEOWNERS already exists, keeping it separate from SFL managed files. Review and merge the deployment PR, then run `gh sfl gate --repo hemsoft-dev/REPOSITORY`. Run a registered `gh sfl review --repo hemsoft-dev/REPOSITORY --pr NUMBER` on an actual consumer PR. Inspect the current head/base and successful Actions-owned gate before merging.

Repeat init/sync through `--pr` and confirm no additional changes. `gh sfl status --repo hemsoft-dev/REPOSITORY` reports the deployed version, source pin, managed file drift and effective gate. Status alone does not turn an advisory deployment into a required gate.

## Existing repository

Use `gh sfl sync --repo hemsoft-dev/REPOSITORY --pr`. Preserve existing tiers, addons, unmanaged files, archive state and unrelated protections. No archived repository is unarchived for rollout. The personal now-leadership-group and set-it-free-loop-site repositories remain outside this organization rollout.

## Consumer-owned wider workflows

A full or custom consumer can place a versioned `.sfl/sync-policy.json` on its default branch to keep deliberate wider-workflow changes outside sync's replacement and deletion set. For example, hs-buddy keeps its manual Auditor, manual Dispatcher and retired issue processor under consumer control:

```json
{
  "version": 1,
  "unmanagedWorkflows": [
    "sfl-auditor.yml",
    "sfl-dispatcher.yml",
    "issue-processor.md"
  ]
}
```

Only recognized wider workflow filenames are accepted. The native reviewer cannot be exempted. An excluded file stays present or absent exactly as the consumer maintains it; source updates to that workflow require a separate consumer PR. Existing recorded engine choices for excluded workflows are retained and do not establish that a retired component is enabled. The full tier and addons remain unchanged.

Use `gh sfl sync --repo hemsoft-dev/REPOSITORY --pr` with this policy. The CLI binds the policy to the default-branch revision and rejects a moved base or an existing sync PR that changes those consumer-owned files. Direct sync is refused when a policy exists. Repeat sync still updates required reviewer files and reconciles other managed files, including retired review workflows.

## Review recovery

A review is bound to the PR head, base and context. Findings, malformed results, pending requests and changed context block the gate. If the base advances, the base is edited, or the PR is reopened, update the PR branch to a new head before requesting another review. The observer requires one trusted opened or head-change context on that head, bound to this PR and base. Legacy context records and multiple contexts on one head stay blocked, even when only the latest request is registered. This also rejects a late automatic or unregistered review started against an earlier base. After upgrading an existing open PR, advance its branch to a new head to establish the new context record. `--retry` cannot safely reuse the same head across different bases because native Codex artifacts do not identify the originating base/request. A newer registered request also supersedes an older result when the requests overlap. The gate stays blocked because same-head artifacts cannot identify which overlapping request produced them. Update the branch to a new head, then register one review request and wait for its terminal result before retrying.

## Governance and credentials

HemSoft remains the valid individual CODEOWNER. Preserve existing repository CODEOWNERS and unrelated ownership rules; organization `.github` CODEOWNERS is not inherited by other repositories. Reviewer tier deliberately provisions no autonomous-agent labels. Existing full tiers retain their labels and governance. Record the effective organization and repository Actions policy without tightening it across unrelated workflows.

Existing App-backed repositories retain their narrow repository-scoped credentials. Organization secrets are not required by reviewer deployments. Repository/environment overrides take precedence over shared values and must be inventoried before any shared credential provision or rotation. No key sealing, export or organization credential write is part of this rollout.

## Recovery and uninstall

Inspect active organization and repository rules before gate changes or uninstall. CLI operations only mutate supported repository-owned SFL rules; inherited organization gates must be removed or changed through their owning organization. Preserve unrelated rules. On disposable validation repositories, verify rejection leaves inherited policy and managed files unchanged, then remove only the deliberately created SFL validation rule and test uninstall under an unrelated retained rule. Use `--keep-labels` for disposable uninstall tests so pre-existing labels remain. Production consumers are not uninstalled as part of this test.

Rotate an App key only through a separately authorized secure credential operation. Verify App identity, installation owner and selected repository coverage, deploy to existing credential consumers, run their read-only checks, then revoke the old key after successful validation. Keep keys, tokens and MFA codes out of logs and issue bodies.

## Installation coverage evidence

The [main-only verification workflow](https://github.com/hemsoft-dev/set-it-free-loop/actions/workflows/verify-sfl-app-credential.yml) reuses the existing source key inside Actions and verifies repository identity and installation with GET requests. It creates a temporary installation token restricted to metadata read on the enumerated IDs, keeps it in memory, and revokes it before qualification succeeds. No stored credential is created or changed. It retains the source credential metadata artifact and adds a separate repository-installation coverage artifact for the 65 transfer targets and three disposable validation repositories. Both retained personal repositories are excluded. Optional extra targets require an explicit bounded JSON array of owner-verified IDs and canonical organization names.

The App JWT and temporary metadata token stay in memory and are masked; it is never included in artifacts. Coverage receipts bind the actual repository ID/name/owner responses to the requested target, actual HTTP 200 installation response, organization installation, permission ceiling and observation time. These checks establish App coverage and credential validity; reviewer and wider-workflow runtime require their separate PR/run evidence in [#139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139).

Setting `SFL_ENABLED=false` pauses request processing and result observation. It does not waive a required reviewer gate. Context invalidation continues to prevent an earlier green result from approving an unreviewed diff.
