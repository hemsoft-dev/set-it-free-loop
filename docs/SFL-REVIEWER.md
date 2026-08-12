# HemSoft SFL Pull Request Reviewer

The HemSoft reviewer is a private-repository package that performs three
evidence-based passes over eligible pull requests, publishes native inline
findings, and exposes a current-head approval job suitable for branch
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
  native `SFL Reviewer Approval` job
- `.github/workflows/sfl-pr-review-recovery.yml` - one-shot missing-output
  recovery

The lock must be generated from the deployed Markdown with the repository's
checksum-verified `gh-aw` compiler. Never edit the lock directly.

## Required repository configuration

The target repository needs the following Actions configuration:

| Kind | Name | Purpose |
| --- | --- | --- |
| Variable | `SFL_APP_CLIENT_ID` | Client ID for the HemSoft reviewer GitHub App |
| Secret | `SFL_APP_PRIVATE_KEY` | Private key for short-lived installation tokens |
| Secret | `OPENROUTER_API_KEY` | Runs Kimi K3 through the HemSoft OpenRouter route |

The App installation must be limited to the intended private repositories. It
needs Actions read/write, Contents read, Issues read/write, and Pull requests
read/write for automatic dispatch, label handling, recovery, and SFL-owned
review-thread management. It does not need repository administration.

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
tier and writes the CLI-compatible `.sfl/sfl.json` manifest:

```powershell
.\deployment\scripts\install-gh-sfl-hemsoft.ps1
gh sfl init --repo HemSoft/private-repository --tier reviewer
```

Both deployment paths reject non-HemSoft or non-private mutation targets. The
CLI also requires the active GitHub CLI identity to be `HemSoft`; direct Git
writes use the `github-personal1` SSH profile.

During the first deployment, review-submitted events can load the wrapper from
the deployment branch before the compatible reviewer lock exists on the
default branch. The wrapper detects that state, does not mint or dispatch, and
reports a successful bootstrap gate. This exception ends as soon as the lock
with exact `dispatch_id` correlation reaches the default branch. Configure
branch protection only after that deployment is merged.

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

Deployment is advisory until branch protection requires the native
`SFL Reviewer Approval` job. After the deployment PR is merged, enable strict
gating with:

```powershell
.\deployment\scripts\set-sfl-review-gate.ps1 `
  -Repo HemSoft/private-repository
```

Use `-DryRun` to inspect the payload. The helper preserves existing required
checks when protection already exists, creates a minimal protection policy when
it does not, pins the SFL context to GitHub Actions, and sets strict mode so the
pull request must be current with the base branch. It intentionally refuses
public or non-HemSoft repositories and does not manage organization rulesets.
