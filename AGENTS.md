## Project freeze

SFL is frozen by owner instruction as of 2026-10-09. Preserve its source and history for reference. Do not reactivate automation, deploy the reviewer, recreate SFL credentials or install the App without a new explicit owner instruction. The current retirement cleanup may be completed under the existing instruction.

## Parked reviewer alpha

The October 19, 2026 owner exception for the offline `reviewer/` alpha has
ended. The [approved v0 plan](docs/SIMPLIFY.md#keep-parked-work-parked) parked
that work on October 28, 2026. Issues #172 through #177 are all closed: #175 is
complete, and the others were closed as not planned. Do not revive them, open
alpha PRs or extend the alpha code without a new explicit owner instruction.
Keep `reviewer/`, the retired `central-reviewer/`, `gh-sfl/` and deployment
automation frozen.

## Current v0 role

SFL v0 runs from project-manager, and runner changes belong there. Its
operating contract is
[project-manager's SFL v0 documentation](https://github.com/hemsoft-dev/project-manager/blob/main/docs/sfl-v0.md).
This repository's only active asset is the offline v0 acceptance checker,
`reviewer/scripts/check_v0_review.py`, with its tests in
`reviewer/scripts/test_check_v0_review.py`. The review presentation it checks
is described in [V0-REVIEW-FORMAT](docs/V0-REVIEW-FORMAT.md). This repository
accepts small fixes to that checker and its tests, plus documentation
corrections. The checker needs no credentials or network access and runs no
repository code. This role grants no activation, credential recreation, App
installation, live inference, provider-policy exception, paid usage or scope
authority. Each live activation or scope change still needs a new explicit
owner go.

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
