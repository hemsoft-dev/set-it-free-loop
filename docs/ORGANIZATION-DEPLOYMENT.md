# Organization deployment and source cutover

The supported repository owners are `HemSoft` and `hemsoft-dev`. Authentication
uses a personal GitHub login with permission on the target repository; the login
is never compared to the organization name. Organization deployment requires write, maintain, or admin access; status
accepts read or higher access. Gate changes and destructive uninstall
require admin access. API failures and identity mismatches stop the operation.
Both owners' `set-it-free-loop` repositories are protected from deployment.

## Before repository transfers

Build the CLI from the reviewed checkout:

```powershell
./gh-sfl/build.ps1 -NoInstall
```

The source remains `HemSoft/set-it-free-loop` until transfer. Select an immutable
published release with `--source-ref v<version>` when running `gh sfl init` or
`gh sfl sync`. Use `--repo hemsoft-dev/<consumer>` and `--pr` to review deployment
changes. Repeating sync should reuse the current deployment PR or make no change.
Existing manifest tiers, addons, and unmanaged files remain preserved.

Review-only deployment uses the existing connected Codex integration and requires
no SFL App private key or model API key. A successful observer installation does
not prove that the intended SFL PR Reviewer App has been restored or rolled out.
That runtime evidence is tracked in [issue #139](https://github.com/HemSoft/set-it-free-loop/issues/139).

## After the transfer gates pass

[Issue #138](https://github.com/HemSoft/set-it-free-loop/issues/138) requires owner verification of provider resources, billing and access,
runner registrations, environment credential inventories, and protection settings
before any repository or App transfer. It also owns the actual new-source release
and installer verification. Local fixture tests cannot satisfy those gates.

Select the new canonical source explicitly for the CLI:

```powershell
$env:SFL_SOURCE_REPOSITORY = 'hemsoft-dev/set-it-free-loop'
gh sfl sync --repo hemsoft-dev/<consumer> --source-ref v<version> --pr
```

The selected source controls downloads, workflow SHA provenance, and the manifest
source. Keep immutable SHA pins; do not replace them with `main`. For the
PowerShell deployment helper, pass
`-SourceRepository hemsoft-dev/set-it-free-loop`. A release built with
`build-release-artifacts.ps1 -SourceRepository hemsoft-dev/set-it-free-loop`
embeds that owner as the CLI default. The publication workflow passes its actual
repository identity into the builder and installer after transfer.

Install only after that private release exists:

```powershell
./deployment/scripts/install-gh-sfl-hemsoft.ps1 `
  -Repository hemsoft-dev/set-it-free-loop -ReleaseVersion <version>
```

The installer downloads the executable and `SHA256SUMS` from the selected private
repository and verifies the executable before installing or running it. Record the
release URL, digest, and `gh sfl version` in [#138](https://github.com/HemSoft/set-it-free-loop/issues/138). The Go module path deliberately
remains `github.com/HemSoft/set-it-free-loop/gh-sfl` to preserve compatibility.
Historical references and the valid individual CODEOWNER `@HemSoft` remain valid;
no organization team is invented as part of source cutover.

## App credentials

Repository-scoped setup remains the default. Organization-scoped setup requires
an authenticated organization owner with `admin:org` authorization and admin
access to every explicitly selected repository. Pass `-ExpectedLogin` for the
human account, `-ExpectedOwner hemsoft-dev`, and `-CredentialScope organization`.
During the pre-transfer window, use `-ExpectedAppOwner HemSoft` when the App is
still personally owned; afterward use `hemsoft-dev`. These identities are checked
independently against authenticated App and installation API responses.

```powershell
./deployment/scripts/set-sfl-github-app-credentials.ps1 `
  -Repos hemsoft-dev/<consumer> -ExpectedLogin HemSoft `
  -ExpectedOwner hemsoft-dev -ExpectedAppOwner hemsoft-dev `
  -CredentialScope organization -AppId <id> -ClientId <client-id> `
  -PrivateKeyPath <secure-local-pem-path> -WhatIf
```

Remove `-WhatIf` only when provisioning is intended. Preflight verifies App ID,
client ID, owner, organization installation, repository coverage, and the existing
permission ceiling. Repository and environment credentials override organization
values, so setup refuses matching overrides. It also refuses existing shared SFL
credentials: reconcile their selected coverage and rotation separately before
replacement. Successful writes verify all three credentials' selected repository
sets. A failed write or coverage check is reported; inspect partial state before
retrying. The script never widens secret visibility to all repositories.

## Checkout routing

Continue authenticating as `HemSoft` through `github-personal1`. The Windows
routing helper includes both `D:/github/HemSoft/` and `D:/github/hemsoft-dev/` in
the personal account configuration. On other systems use an explicit remote:

```text
git@github-personal1:hemsoft-dev/<repository>.git
```

Update the actual source checkout remote only after transfer. Keep the current
`HemSoft` remote until then. Gate and release-protection helpers require admin
permission, preserve unrelated requirements, and fail on inherited rules they
cannot safely edit.
