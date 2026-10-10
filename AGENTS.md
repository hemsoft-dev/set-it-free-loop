## Project freeze

SFL is frozen by owner instruction as of 2026-10-09. Preserve its source and history for reference. Do not reactivate automation, deploy the reviewer, recreate SFL credentials or install the App without a new explicit owner instruction. The current retirement cleanup may be completed under the existing instruction.

## Repository Identity

This checkout is the frozen `hemsoft-dev/set-it-free-loop` repository,
transferred from `HemSoft/set-it-free-loop` with its original repository ID
`1169772257`. Use `hemsoft-dev/set-it-free-loop` for GitHub API and release
repository arguments. GitHub Actions sets `GITHUB_REPOSITORY` to that canonical
organization path; the legacy URL redirects to it.

When using GitHub CLI or GitHub MCP/API tools for this repository, use the
`HemSoft` GitHub account.

When using Git remotes for this repository, use the `github-personal1` SSH
profile. This profile authenticates to GitHub as `HemSoft`. The canonical authorized remote
URL is:

```text
git@github-personal1:hemsoft-dev/set-it-free-loop.git
```

The previous `HemSoft/set-it-free-loop` remote redirects to this canonical path.
Preserve the `github-personal1` authentication profile for either path. Do not rewrite other
agents' remotes or checkouts during a migration.

Do not use `github-work1` for this repository. That profile authenticates as
`fhemmerrelias` and only has read access to the private HemSoft repositories.
