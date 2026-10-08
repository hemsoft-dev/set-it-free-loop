## Repository Identity

This checkout is the private `hemsoft-dev/set-it-free-loop` repository,
transferred from `HemSoft/set-it-free-loop` with its original repository ID
`1169772257`. Use `hemsoft-dev/set-it-free-loop` for GitHub API and release
repository arguments. GitHub Actions sets `GITHUB_REPOSITORY` to that canonical
organization path; the legacy URL redirects to it.

When using GitHub CLI or GitHub MCP/API tools for this repository, use the
`HemSoft` GitHub account.

When using Git remotes for this repository, use the `github-personal1` SSH
profile. This profile authenticates to GitHub as `HemSoft`. The existing authorized remote
URL is:

```text
git@github-personal1:HemSoft/set-it-free-loop.git
```

GitHub redirects this existing remote to
`git@github-personal1:hemsoft-dev/set-it-free-loop.git`. Preserve the
`github-personal1` authentication profile for either path. Do not rewrite other
agents' remotes or checkouts during a migration.

Do not use `github-work1` for this repository. That profile authenticates as
`fhemmerrelias` and only has read access to the private HemSoft repositories.
