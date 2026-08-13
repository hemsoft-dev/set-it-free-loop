# HemSoft SFL Pull Request Reviewer

The HemSoft reviewer is a private-repository package that performs three
evidence-based passes over eligible pull requests, publishes native inline
findings, and exposes a current-head approval check suitable for branch
protection.

## Distribution boundary

The deployment scripts fail closed unless the target is owned by `HemSoft` and
has `PRIVATE` visibility. Do not publish the source repository, reviewer
workflows, generated locks, credentials, or deployment pull requests outside
private HemSoft repositories.

## Package

The `review` tier installs these files:

- `.github/workflows/sfl-pr-review.md` - runtime-imported agent instructions
- `.github/workflows/sfl-pr-review.lock.yml` - generated executable workflow
- `.github/workflows/sfl-pr-review-auto.yml` - PR lifecycle dispatcher and
  immutable-head `SFL Reviewer Approval` check publisher
- `.github/workflows/sfl-pr-review-recovery.yml` - one-shot missing-output
  recovery

The lock must be generated from the deployed Markdown with the repository's
checksum-verified `gh-aw` compiler. Never edit the lock directly.

The Kimi model has explicit provider pricing in the Markdown frontmatter. The
compiler must carry that pricing into both the primary reviewer and the
mandatory threat-detection firewall configuration. Generic fallback pricing
is not an acceptable substitute because the two runtimes are generated
independently.

## Required repository configuration

The target repository needs the following Actions configuration:

| Kind | Name | Purpose |
| --- | --- | --- |
| Variable | `SFL_APP_CLIENT_ID` | Reviewer GitHub App client ID |
| Secret | `SFL_APP_PRIVATE_KEY` | App installation private key |
| Secret | `OPENROUTER_API_KEY` | Private Kimi K3 route |

The App installation must be limited to selected intended private repositories.
Its exact repository permission contract is Actions read/write, Checks read,
Contents read, Issues read/write, Metadata read, and Pull requests read/write.
It must not have Checks write, Contents write, Workflows write, an
all-repositories installation, or unrelated repository permissions. Review
evidence and obsolete-thread mutations use the repository-scoped `GITHUB_TOKEN`
instead of expanding the shared App's permission ceiling.

Configure App credentials only through the fail-closed bootstrap:

```powershell
.\deployment\scripts\set-sfl-github-app-credentials.ps1 `
  -Repos HemSoft/private-repository `
  -AppId 123456 `
  -ClientId Iv1.example `
  -PrivateKeyPath C:\secure\sfl-app.private-key.pem
```

Before its first variable or secret write, the bootstrap uses a locally signed
App JWT to verify the App ID, client ID, HemSoft ownership, exact target
installation, selected-repository scope, and permission ceiling for every
requested repository. It also independently verifies each target is a private
`HemSoft/*` repository. If any target fails, none of the targets are mutated.

`gh sfl init`, `gh sfl sync`, and `gh sfl add pr-review` inspect only repository
metadata and credential names; they never read secret values. Before preparing
a reviewer deployment pull request, including a `pr-review` add-on deployment,
they require a default branch, enabled GitHub Actions, the variable above, and
both secrets above. A normal GitHub CLI OAuth token cannot inspect
GitHub App installations owned by the `HemSoft` personal account, so App scope
and permission-ceiling validation belongs to the App-authenticated credential
bootstrap and remains a required rollout step.
Repositories with a selected-actions policy must enable the policy's
GitHub-owned-actions option; exact allowlist patterns are intentionally not a
supported rollout policy because the compiled reviewer uses a changing set of
SHA-pinned `actions/*` and `github/gh-aw-actions/*` actions.
`gh sfl status` reports each missing prerequisite separately from package drift,
gate posture, and reviewer-run health.

## Deployment

Deploy through a reviewable pull request:

```powershell
.\deployment\scripts\deploy-workflow.ps1 `
  -Tier review `
  -Repos "HemSoft/private-repository"
```

The deployer stamps the exact source commit into the reviewer Markdown and the
two standard YAML wrappers, compiles the Markdown in the consumer checkout, and
records all reviewer components in `sfl.json`. A later deployment from the same
source should produce no unexpected workflow diff.

For local dogfood materialization:

```powershell
.\deployment\scripts\deploy-workflow.ps1 -Local -Compile
```

The HemSoft `gh sfl` adapter exposes the equivalent package as the `reviewer`
tier and writes the CLI-compatible `.sfl/sfl.json` manifest. Its Go source is
owned in this repository under `gh-sfl/`; building or installing it does not
read from a Relias checkout. The provenance of the initial source import is
recorded in `gh-sfl/PROVENANCE.md`.

Validate and build without changing the installed extension:

```powershell
.\gh-sfl\build.ps1 -NoInstall
```

Install the local build, then open the default reviewer deployment PR:

```powershell
.\deployment\scripts\install-gh-sfl-hemsoft.ps1
gh sfl init --repo HemSoft/private-repository
```

`init` defaults to the `reviewer` tier and both `init` and `sync` default to a
signed deployment pull request. `--direct` is an explicit opt-in for direct
default-branch mutation. Workflow reads are pinned to the SHA behind the
selected private HemSoft release, and `.sfl/sfl.json` records both its version
and source SHA.

Both deployment paths reject non-HemSoft or non-private mutation targets. The
CLI also requires the active GitHub CLI identity to be `HemSoft`; direct Git
writes use the `github-personal1` SSH profile.

During the first deployment, review-submitted events can load the wrapper from
the deployment branch before the compatible reviewer lock exists on the
default branch. The wrapper detects that state, does not mint or dispatch, and
reports a successful bootstrap gate. This exception ends as soon as the lock
with exact `dispatch_id` correlation reaches the default branch. Configure
branch protection only after that deployment is merged.

The supported rollout order is:

1. Install the App for the explicitly approved private repository and run the
   credential bootstrap above. It verifies the selected-repository scope and
   permission ceiling with App authentication before setting credentials.
2. Run `gh sfl init` or `gh sfl sync` and merge its reviewed deployment PR.
3. Run `gh sfl gate` to require the reviewer without replacing unrelated
   repository rules.
4. Open a smoke PR and require `gh sfl status` to show a successful reviewer
   run, current manifest/files, healthy credentials/App, and the required gate.

Maintainers who can change workflow files on the protected default branch are
trusted in this first HemSoft rollout model. The reviewer prevents unreviewed
pull-request heads from satisfying its gate; it does not attempt to defend
against an authorized maintainer changing the trusted default-branch workflow.

## Automatic and explicit review

An internal, non-draft pull request targeting the default branch is reviewed
when it is opened, reopened, marked ready for review, or synchronized with a new
commit. Fork pull requests are skipped because repository secrets are not made
available to them.

Adding `sfl-review` requests an explicit rerun. The dispatcher consumes the
label after validating the pull request and passes immutable PR number, base
SHA, and head SHA inputs to the executable workflow. Duplicate runs for the
same sealed context are suppressed.

The optional `review_effort` input is an audit marker retained in provenance
and forwarded by recovery. The HemSoft Kimi route does not currently map it to
a provider-specific reasoning control.

A maintainer can dispatch manually from the default branch:

```powershell
$context = '{"item_type":"pull_request","item_number":42,"base_sha":"BASE_SHA","head_sha":"HEAD_SHA"}'
gh workflow run sfl-pr-review.lock.yml `
  --ref main `
  -f item_number=42 `
  -f base_sha=BASE_SHA `
  -f head_sha=HEAD_SHA `
  -f retry_count=0 `
  -f dispatch_id=manual `
  -f review_effort=low `
  -f "aw_context=$context"
```

## Evidence and approval

Each run validates its repository, workflow path, default-branch revision, pull
request, base, and head before the reviewer starts and again before safe output.
The workflow initializes an in-progress evidence check for the exact run and
head, then finalizes that same evidence after reconciling immutable artifacts
and SFL-owned review threads.

Every unresolved Critical, High, Medium, or Low SFL finding blocks approval.
Older success cannot satisfy a newer head or a pull request whose base branch
advanced. Obsolete threads are resolved only after verifying their SFL App
ownership and proving they belong to the expected pull request context.
The trusted wrapper publishes `SFL Reviewer Approval` directly on that head
with the repository-scoped `GITHUB_TOKEN`; the wrapper job itself runs in the
safer default-branch context and is not used as the required status context.

## Recovery

If a trusted reviewer run fails or times out without one formal review, the
recovery workflow downloads and validates its provenance and safe-output
artifacts. It retries exactly once only when:

- the pull request is still eligible;
- base and head are unchanged;
- no newer run already owns the same context; and
- the agent did not emit an explicit terminal missing-data, missing-tool, or
  incomplete-report signal.

A stale context suppresses recovery. A second missing review fails closed.
Published reviews are reconciled before retry so a missing artifact cannot
produce a duplicate review.

## Advisory and gated operation

Deployment is advisory until repository rules require the reviewer workflow and
its native `SFL Reviewer Approval` freshness check. After the deployment PR is
merged, enable strict gating with:

```powershell
gh sfl gate --repo HemSoft/private-repository
```

The command preserves unrelated rules, binds the required workflow to the
consumer's default branch and repository ID, and requires a fresh native check
from the SFL App. It intentionally refuses public or non-HemSoft repositories.
