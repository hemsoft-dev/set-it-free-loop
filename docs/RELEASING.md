# Private HemSoft release contract

HemSoft SFL releases are private, manually initiated, and immutable after tag
creation. Stable publication is not enabled yet.

## Prepare the version through a pull request

Start from an issue branch and synchronize all version-bearing metadata with one
command:

```powershell
.\deployment\scripts\set-release-version.ps1 -Version 2.1.0-rc.1
.\deployment\tests\test-release-metadata.ps1 `
  -ExpectedVersion 2.1.0-rc.1 -RequirePrerelease
```

The command updates `VERSION`, `sfl.json`, and the distribution version, tag,
and prerelease marker in `deployment/release-metadata.json`. Commit those files
with the release implementation and merge them through an ordinary reviewed PR.

## Protect releases and tags

Authenticate GitHub CLI as `HemSoft`, preview the repository setting, and apply it:

```powershell
.\deployment\scripts\set-release-protection.ps1 -Plan
.\deployment\scripts\set-release-protection.ps1
```

The idempotent script only targets the private
`HemSoft/set-it-free-loop` repository. GitHub immutable releases allow a draft
to be repaired, then lock its published tag and assets against movement,
replacement, or deletion and issue release attestations. This is stronger than
a tag-only ruleset because the checksum file and binaries become immutable too.
After verifying the admin-only setting, the script records
`SFL_IMMUTABLE_RELEASES_ENABLED=true` as the workflow's preflight bridge. The
workflow token cannot read GitHub's repository Administration endpoint; the
signed release and per-asset attestations remain the authoritative post-publish
proof.

## Publish a prerelease

The version metadata and tag ruleset must already be on `main`:

```powershell
gh workflow run publish-private-prerelease.yml `
  --repo HemSoft/set-it-free-loop `
  --ref main `
  -f version=2.1.0-rc.1
```

The workflow verifies clean immutable `main`, rejects existing tags/releases,
runs Go generation, formatting, vet, and tests, and builds:

- `gh-sfl_<version>_windows_amd64.exe`
- `gh-sfl_<version>_linux_amd64`
- `install-gh-sfl-hemsoft.ps1`
- `SHA256SUMS`

It creates a draft with every asset, publishes it under GitHub's immutable
release policy, verifies GitHub's signed release and per-asset attestations, and
then downloads the Linux artifact through the installer into a fresh temporary
directory to prove checksum verification and embedded version identity.

## Install and verify

With GitHub CLI authenticated to an account that can read the private release:

```powershell
.\deployment\scripts\install-gh-sfl-hemsoft.ps1 `
  -ReleaseVersion 2.1.0-rc.1
gh sfl version
```

The installer selects the Windows or Linux amd64 asset and refuses missing,
duplicated, or mismatched checksums. It never falls back to an unverified build.
