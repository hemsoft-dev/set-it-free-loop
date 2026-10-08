# gh-sfl — GitHub CLI Extension for Set it Free Loop

Deploy and manage the **SFL PR Reviewer** in GitHub repositories from the command line.

## Installation

From the private `hemsoft-dev/set-it-free-loop` checkout, install a published release:

```powershell
.\deployment\scripts\install-gh-sfl-hemsoft.ps1 -ReleaseVersion 2.1.0-rc.16
```

The installer uses the active `HemSoft` GitHub CLI authentication to download
the private Windows or Linux amd64 artifact and fails unless its SHA-256 value
matches the release's `SHA256SUMS`. The installer and release builder default to
`hemsoft-dev/set-it-free-loop`; `-Repository HemSoft/set-it-free-loop` remains
available for older release metadata. Published binaries embed the publishing
owner, while the Go module path stays unchanged for import compatibility.
For development, omit `-ReleaseVersion` to
validate and build the checked-out source. No separate `HemSoft/gh-sfl`
repository or local Relias checkout is used.

## Commands

### `gh sfl init`

Deploy the latest synchronized SFL PR Reviewer release to a repository through
a pull request.

```bash
gh sfl init --repo owner/repo              # Latest reviewer → deployment PR
gh sfl init                                # Latest reviewer → current repo PR
gh sfl init --direct                       # Explicit direct deployment
gh sfl init --tier standard                # Advanced legacy suite deployment
gh sfl init --slack-webhook "https://hooks.slack.com/..."  # Enable Slack notifications
```

**Tiers:**

| Tier | Workflows | Description |
|------|-----------|-------------|
| `reviewer` | Subscription-backed Codex observer and gate | Default reviewer-only package |
| `minimal` | Dispatcher | Legacy label routing only |
| `standard` | Dispatcher, Processor, Review Reactor | Full quality loop |
| `full` | Standard + Repo Audit, Simplisticate Audit, Simplisticate PR | Everything |

### `gh sfl sync`

Update deployed SFL workflows from the motherrepo to the latest version.

```bash
gh sfl sync                                # Open a PR for the current repo
gh sfl sync --repo owner/repo              # Open a PR for a specific repo
gh sfl sync --repo owner/repo --direct     # Explicit direct update
gh sfl sync --dry-run                      # Preview without changes
```

### `gh sfl add`

Add the reviewer package to an existing minimal or standard installation. The
workflow sources remain pinned to the installation manifest's immutable source
commit, and the command opens a pull request by default.

```bash
gh sfl add pr-review                       # Open an add-on PR for the current repo
gh sfl add pr-review --repo owner/repo     # Open an add-on PR for a specific repo
gh sfl add pr-review --direct              # Explicit direct deployment
```

### `gh sfl gate`

Reviewer deployments are advisory-only by default. After merging the deployment
pull request, explicitly require its native approval check:

```bash
gh sfl gate --repo owner/repo
```

The command creates one repository ruleset for the default branch. It requires
`SFL Reviewer Gate Runner` from GitHub Actions App ID `15368` and enables strict
base freshness. The observer authenticates the native Codex result against the
exact reviewed head and base, then writes the required runner commit status on
that immutable head. Strict base freshness prevents a status for an older base
from satisfying the rule. An already-correct
gate causes no write. A stale dedicated SFL gate is updated with its current
entity tag and state rechecked immediately before the write. The command aborts
if either changed, verifies the result, and restores the pre-write snapshot if
GitHub accepts an invalid update that remains unchanged before rollback. GitHub
does not support conditional headers on repository ruleset updates or deletes.
The command refuses to rewrite a shared or inherited rule. It does not use
organization endpoints or require the `admin:org` scope.

When `gh sfl stop` enables maintenance mode, the observer ignores Codex result
events, so new pull request heads remain blocked by the absent required gate.
`gh sfl start` re-enables observation.

### `gh sfl uninstall`

Remove SFL workflows and governance files from a repository.
The file-removal commit is prepared before dedicated reviewer gate rulesets are
removed. If its push fails, the original dedicated rulesets are restored. Shared
or inherited gate rulesets must be separated by an administrator before
uninstall proceeds. When a manifest is missing,
`--allow-manifest-fallback` explicitly selects the union of every known managed
SFL path; `--force` remains solely the destructive-operation confirmation.

```bash
gh sfl uninstall --dry-run                 # Preview what would be removed
gh sfl uninstall --force                   # Uninstall from current repo
gh sfl uninstall --force --repo owner/repo # Uninstall from specific repo
gh sfl uninstall --force --keep-labels     # Keep SFL labels
gh sfl uninstall --dry-run --allow-manifest-fallback # Preview without a manifest
```

### `gh sfl list`

Show recent SFL workflow runs with color-coded status.

```bash
gh sfl list                                # Current repo, last 20 runs
gh sfl list --repo owner/repo              # Specific repo
gh sfl list --limit 10                     # Fewer results
```

### `gh sfl status`

Show SFL health dashboard: installation status, version, labels, and active PRs.

```bash
gh sfl status                              # Current repo
gh sfl status --repo owner/repo            # Specific repo
```

### `gh sfl version`

Show version, build date, and check for updates.

```bash
gh sfl version
```

## How It Works

`gh sfl init` resolves the latest synchronized release of
`HemSoft/set-it-free-loop`, pins every source read to that release's
commit, and opens a deployment pull request. A `.sfl/sfl.json` manifest tracks
the immutable version, source commit, tier, and installed components.
`gh sfl sync` preserves the installed tier while updating it to the latest
synchronized release.

`init`, `sync`, and `add` create deployment pull requests by default. The extension
creates the deployment commit through GitHub,
opens a pull request against the default branch, and reuses an existing open
SFL deployment pull request by updating its branch instead of creating duplicates.

## Building

```powershell
cd gh-sfl
.\build.ps1 -NoInstall
```

This checks formatting, runs `go vet` and `go test`, then builds with version
metadata. Omit `-NoInstall` to install the result into the `gh` extensions
directory.

Release discovery uses the most recently published semantic-version release, including private prereleases. Drafts and unrelated tags are excluded. An explicit `--source-ref` continues to select a specific immutable release; deployment still checks its tag against `VERSION`.
