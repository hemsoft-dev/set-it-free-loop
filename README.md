# Set it Free Loop

## Personal Slack assistant

Franz's November 11, 2026 instruction grants a narrow exception for the
[personal Home Slack assistant](personal-slack/README.md), tracked by
[issue #308](https://github.com/hemsoft-dev/set-it-free-loop/issues/308).
It has separate source, runtime, configuration and task lifecycle. The retired
reviewer, alpha, GitHub App and deployment automation remain frozen. Source
preparation does not establish live delivery or authorize skipping review.

## Current SFL v0

The current operating contract is
[project-manager's SFL v0 documentation](https://github.com/hemsoft-dev/project-manager/blob/main/docs/sfl-v0.md).
HemSoft comments exactly `@sfl review` on an open PR with a same-repository head
to request one advisory review by our T3 agent, published by the SFL App. The
contract covers every public or private hemsoft-dev repository the App
installation can access. Accepted requests wait in a durable FIFO, and there is
no daily setting or counter. Provider pause and hot verdicts still apply.
[V0-REVIEW-FORMAT](docs/V0-REVIEW-FORMAT.md) describes the posted review.

This summary repeats the documented contract. It does not show what any host is
running, and it grants no activation or scope authority. Each further live
activation or scope change needs a new explicit owner go.

## Initial v0 pilot, October 28, 2026

The owner authorized a small pilot on October 28, 2026. The
[initial plan](docs/SIMPLIFY.md) records that authorization and its limits: this
repository only and three attempts per Eastern day, with a fresh comment for
busy requests. Those limits are dated history, not current operation. The
historical alpha and retired organization automation remain frozen.

## Historical freeze

The owner's October 19, 2026 merge-mission instruction authorized an offline
dual-subscription alpha in [`reviewer/`](reviewer/README.md), tracked by
[issue #172](https://github.com/hemsoft-dev/set-it-free-loop/issues/172).
The [approved v0 plan](docs/SIMPLIFY.md#keep-parked-work-parked) parked that
alpha on October 28, 2026, and issue #172 was closed as not planned that day.
The alpha code stays frozen for reference. The only active asset here is the
offline v0 acceptance checker, `reviewer/scripts/check_v0_review.py`.
Credentials, App installation, live inference and deployment retain their
separate gates. The retired implementation and organization automation remain
frozen.

SFL is frozen by owner instruction as of 2026-10-09. It is no longer deployed or available as a PR reviewer in hemsoft-dev. Do not install, activate, or configure it.

The organization GitHub App installation, central reviewer service and public webhook route have been removed. SFL repository integrations, configuration, credentials, open rollout PRs and backlog are being retired across the organization. Existing native reviews and unrelated CI remain independent.

This repository retains the implementation, historical documentation, release artifacts and Git history for reference. Historical runbooks and deployment templates describe retired systems; they are not current setup instructions. Repository Actions workflows and credential-sealing tooling are removed from active configuration.

The freeze does not assert that unresolved historical migration evidence was verified. In particular, issue #138’s fhemmer protection-preservation limitation remains part of the historical record.
