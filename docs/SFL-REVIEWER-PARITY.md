# SFL reviewer parity baseline

HemSoft tracks the reviewer contract from the private
`relias-engineering/set-it-free-loop` repository without treating byte-for-byte
identity as the goal. Provider configuration and distribution boundaries differ
intentionally, and HemSoft retains tested hardening when it is at least as
strict as the Relias behavior.

The machine-readable baseline and HemSoft release identity are in
[`deployment/release-metadata.json`](../deployment/release-metadata.json).
Its `distribution.version` is the HemSoft package version; the independent
`reviewerBaseline.releaseVersion` identifies the reviewed Relias contract.
It distinguishes the `v6.5.7` tag commit
`8d5e30714fa6cc61a89189266f8eb463132abde9` from the reviewed default-branch
commit `669d4d84ef37ebab107c5931d6727846626062e5`. Only `TODO.md` changed between
those commits; all four reviewer artifacts are identical at both points.

## Intentional differences

| Artifact | Difference class | HemSoft contract | Regression evidence |
| --- | --- | --- | --- |
| Reviewer source and compiled lock | Provider configuration | Uses Kimi K3 through the private OpenRouter route, carries explicit pricing into both runtimes, restricts network access to `openrouter.ai`, and does not request Copilot billing permission. | `test-sfl-pr-review.ps1` |
| Reviewer source and compiled lock | Private distribution boundary | Uses HemSoft source identity, repository-scoped App configuration, and no hard-coded App installation identity. | `test-sfl-pr-review.ps1`, `test-sfl-review-platform.ps1` |
| Reviewer source and compiled lock | Tested HemSoft hardening | Resolves only already-resolved or GitHub-outdated SFL threads and refuses to resolve a live unresolved thread. | `test-sfl-pr-review.ps1`, `test-sfl-verdict-validator.ps1` |
| Auto trigger and gate | Private distribution boundary | Supports bootstrap deployment before the compatible reviewer lock reaches the default branch and mints a repository-scoped App token with pull-request write permission only where label mutation requires it. | `test-sfl-review-platform.ps1` |
| Auto trigger and gate | Tested HemSoft hardening | Reuses successful exact-head reviews after delayed ordinary events, preserves explicit-label reruns, publishes the native immutable-head `SFL Reviewer Approval` check only after authenticating the exact reviewer run, and honors maintenance mode before dispatch or gate execution. | `test-sfl-dispatch-dedup.ps1`, `test-sfl-review-platform.ps1`; issues #46 and #49 runtime evidence |
| CLI gate | Private distribution boundary | Uses a repository-scoped strict `SFL Reviewer Approval` rule owned by GitHub Actions because `HemSoft` is a personal account. Relias uses an organization required-workflow rule. Conditional updates preserve unrelated repository policy and restore the prior dedicated rule after an ambiguous failed write. | `gate_test.go`, `deployment_test.go`; issue #73 |
| Recovery | Tested HemSoft hardening | Fails closed when agent output is absent or malformed, rechecks immutable PR state plus newer-run ownership immediately before retry dispatch, and suppresses retry while maintenance mode is active. | `test-sfl-review-platform.ps1` |

The generated lock is not independently allowlisted: its differences must be
the compiled representation of the documented reviewer-source differences.
`test-sfl-pr-review.ps1` enforces the compiler/runtime markers and explicit
provider pricing, while canonical/staged identity remains enforced by the
reviewer platform contract.

gh-aw v0.86.2 emits `concurrency.queue: max` in the generated conclusion job.
GitHub has accepted and executed that lock, but actionlint v1.7.12 does not yet
recognize the key. `.github/actionlint.yaml` ignores only that exact diagnostic
and only for the generated reviewer lock; all other actionlint checks remain
active for the lock and wrappers.

## Running the audit

The default mode verifies the metadata, normalized HemSoft hashes, staged
copies, difference classes, and referenced tests. Normalizing CRLF and LF before
hashing keeps checkout line endings from creating false drift.

```powershell
pwsh -NoProfile -File deployment/tests/test-relias-reviewer-parity.ps1
```

When an authorized local Relias checkout is available, supply it to verify the
pinned tag, reviewed commit, post-tag path delta, and upstream hashes directly
from Git objects. The audit is read-only and does not check out or modify the
Relias worktree.

```powershell
pwsh -NoProfile -File deployment/tests/test-relias-reviewer-parity.ps1 `
  -ReliasCheckout D:\github\Relias\set-it-free-loop
```

Any reviewer artifact change fails the audit until the baseline hashes and this
contract-diff note are deliberately reviewed together. A future Relias baseline
update must record both the release tag commit and the exact reviewed commit;
never move the recorded commit to a mutable branch name. HemSoft release bumps
change only `distribution.version`; they do not rewrite the reviewer baseline.

## CLI source ownership

The private HemSoft distribution owns its adapted `gh sfl` Go source in
`gh-sfl/`. The initial import is pinned in `gh-sfl/PROVENANCE.md` to Relias
release `v6.5.7` at commit
`8d5e30714fa6cc61a89189266f8eb463132abde9`. Repository builds, tests, and
installation consume only that version-controlled HemSoft tree; the optional
local Relias checkout above is used solely by the read-only reviewer parity
audit.

The CLI preserves the reviewer rollout capabilities while enforcing
HemSoft-specific boundaries: reviewer-only and pull-request-first defaults,
immutable private HemSoft release reads, `HemSoft` CLI identity, HemSoft-owned
consumer targets, protected mother-repository rejection, and source-path
routing for the four-file reviewer package. `.github/workflows/validate-gh-sfl.yml`
keeps the source formatted, vetted, tested, and cross-buildable for Windows and
Linux.
