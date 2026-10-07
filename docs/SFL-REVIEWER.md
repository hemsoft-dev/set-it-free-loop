# HemSoft SFL pull request reviewer

HemSoft SFL uses the native Codex GitHub review connected to Franz's ChatGPT
subscription. It does not run a model through OpenRouter and does not require an
OpenAI API key.

## Package

The review tier installs one workflow:

- .github/workflows/sfl-pr-review-auto.yml observes authenticated Codex results
  and publishes both the audit check and required SFL Reviewer Gate Runner
  status on the immutable reviewed head.

A synchronized deployment also removes the retired sfl-pr-review.md,
sfl-pr-review.lock.yml, and sfl-pr-review-recovery.yml files.

## Prerequisite

Install and connect the Codex GitHub App for the target repository using the
same ChatGPT account that owns the Codex subscription. SFL cannot inspect that
subscription connection through GitHub CLI, so a current-head smoke review is
the rollout proof.

No reviewer Actions variable or AI-provider secret is required. The reviewer
does not read OPENROUTER_API_KEY, OPENAI_API_KEY, CODEX_API_KEY,
SFL_APP_CLIENT_ID, or SFL_APP_PRIVATE_KEY.

The observer uses the SHA-pinned GitHub-owned actions/github-script action. A
selected-actions policy must therefore allow GitHub-owned actions.

## Deploy

Deploy through a pull request:

    .\deployment\scripts\deploy-workflow.ps1 `
      -Tier review `
      -Repos "HemSoft/repository"

The gh sfl adapter provides the same package:

    gh sfl init --repo HemSoft/repository
    gh sfl sync --repo HemSoft/repository

Both paths are restricted to HemSoft-owned consumers and default to a
reviewable deployment pull request.

## Request a review

Request one review for the current pull request head:

    gh sfl review --repo HemSoft/repository --pr 42

The reviewer supports pull requests whose head branch is in the target
repository and whose base is the repository's default branch. Fork pull
requests are rejected because GitHub downgrades workflow tokens for fork review
events, preventing reliable publication of the required check. Non-default base
branches are rejected because base-advance invalidation is deliberately scoped
to the trusted default branch.

Only review requests posted by the repository owner and registered by
`gh sfl review` are accepted. This HemSoft reviewer is owner-operated; member,
collaborator, and hand-crafted owner comments cannot authorize a gate result.

The command:

1. verifies the active GitHub identity is HemSoft;
2. verifies the pull request is open and the observer is installed;
3. waits for applicable pull-context and default-branch invalidation workflows,
   then revalidates the pull request head, base, and default branch;
4. posts @codex review with an invisible marker containing the full head SHA,
   base SHA, and latest SFL invalidation context token;
5. records the returned comment ID in the append-only
   `SFL Codex Review Request Registry` commit-status context;
6. reuses the existing request URL instead of posting a duplicate for that
   head.

The registry preserves request identity if the owner later edits the comment.
The observer rejects edited markers, while `--retry` waits for the registered
request's Codex reaction lifecycle before posting an immutable replacement.
Ordinary edited discussion comments are not review requests.
Each registered request also publishes an immediate failing gate before its
Codex result is observed, so an earlier success cannot remain merge-valid while
a newer review is pending.

A new commit creates a new head and therefore permits one new request.
If a base change or retarget would reuse a head that was already requested
against another base SHA, update the pull request branch first. Native Codex
artifacts identify the reviewed head but not the invoking base, so SFL rejects
same-head cross-base reuse rather than guessing which request produced a result.

## Result contract

Codex currently emits two result shapes:

- clean: an issue comment beginning with
  "Codex Review: Didn't find any major issues.";
- findings: a COMMENTED pull request review, usually with inline comments.

Both include a Reviewed commit SHA prefix. The observer resolves that prefix
through GitHub and requires the resulting full SHA to equal the current pull
request head. A stale result is ignored and cannot satisfy or fail the new head.

For clean comments, the observer requires GitHub App ID 1144995, slug
chatgpt-codex-connector, owner openai, and bot user ID 199175422. GitHub's
pull-request-review REST object currently omits performed_via_github_app;
finding reviews therefore require the immutable bot user ID and login, a
COMMENTED state, the full review commit_id, and the resolved reviewed-commit
prefix. If GitHub starts returning App provenance on reviews, the observer
requires the same App identity.

Authenticated malformed current-head output fails closed. Spoofed or stale
output is ignored. Each request can publish only one terminal gate, and event
redelivery is idempotent through a request-specific check-run external ID.

## Gate

After deployment, enable the existing strict branch rule:

    gh sfl gate --repo HemSoft/repository

The rule still requires SFL Reviewer Gate Runner from GitHub Actions App ID
15368 with strict base freshness. The observer binds the review artifact to the
reviewed head and base, keeps its terminal audit check on that head, and writes
the required commit status on the same immutable head. Strict base freshness
prevents that head status from satisfying the rule after the base advances:

- clean Codex result: success on the head audit check and required head status;
- current-head Codex findings or malformed authenticated output: failure on
  the head audit check and required head status;
- stale or spoofed artifact: no check.

## Semantic change from the retired reviewer

The retired Kimi/OpenRouter reviewer ran a prescribed three-pass,
full-spectrum prompt, assigned Critical/High/Medium/Low severities, managed its
own SFL review threads, and retried missing output once.

Native Codex owns review depth, severity, and presentation. SFL now verifies
Codex provenance and head freshness and translates its clean/finding result
into the existing branch gate. It does not promise three passes, all severity
classes, SFL-authored approvals, obsolete-thread cleanup, or recovery retries.
A rerun is an explicit new `gh sfl review --retry` request. A normal retry is
rejected until the latest request has a request-specific terminal SFL gate.
Retries after a successful terminal gate are rejected; push a new commit so the
old success cannot satisfy the required check while another review is pending.
For an edited or overlapping request that can never receive its own gate, the
CLI uses the durable request registry and waits up to two minutes for the exact
Codex `eyes` reaction to materialize and clear. It then waits beyond GitHub's
one-second timestamp boundary before posting the replacement. This prevents a
delayed artifact from an older request from consuming the replacement's gate.

Pilot deployments are limited to HemSoft/hs-buddy until the source change and
smoke evidence are accepted. Do not deploy this migration to
developer-documentation.

## Organization requester authorization

For `hemsoft-dev`, the CLI, request invalidator, and result observer use the
[repository permission API](https://docs.github.com/en/rest/collaborators/collaborators#get-repository-permissions-for-a-user)
to authorize people with write or higher access. The organization's login is
never treated as a human requester. The registry creator must match the actual
comment author; both identities require verified permission. Unknown identities,
read-only users, outsiders, and lookup failures cannot authorize a request.
Personal `HemSoft` repositories retain the owner-operated policy.

Permissions are checked again before publishing a result. The exact registered
comment must remain present and unedited, with the same head/base/context marker.
Existing Codex bot/App identity checks, same-repository default-branch scope,
retry ordering, and durable head-status publication remain required. An edited,
forged, or unregistered request cannot produce success. Pending invalidation
runs are awaited only after their request actor passes the same live permission
check. Only request-shaped comments and matching registry creators are queried,
so unrelated discussion does not consume the authorization budget. No authorization decision relies on comment association labels.

A connected Codex account and repository installation remain runtime
prerequisites. Organization ownership alone does not establish them. Record
real fixed-head review evidence in [#137](https://github.com/HemSoft/set-it-free-loop/issues/137)
and the full rollout in [#139](https://github.com/HemSoft/set-it-free-loop/issues/139).

Registered request statuses now preserve their original base SHA in the immutable status description. Historical requests remain ordering and base-conflict barriers after permission revocation or comment edits. For older registrations without a durable base, an edited/deleted comment loses that evidence and requires a new PR head; it cannot authorize matching a delayed artifact to a new base. Current permission and an unchanged exact request body remain required for gate publication.

The result observer serializes per-PR events with `queue: max`, preserving up to GitHub’s 100 pending jobs instead of replacing the pending artifact. A base advance requires a new PR head before another registered review. Native Codex artifacts identify the head but not the originating base or request, so same-head cross-base retries remain blocked to prevent an old review from satisfying the new context. Same-head, same-base retries still use the existing completion and ordering guards.

GitHub documents the supported `queue: max` setting in its [concurrency reference](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency). Pinned actionlint 1.7.12 and upstream main still reject this newly supported key. For this observer only, use `actionlint -ignore 'unexpected key "queue" for "concurrency" section' .github/workflows/sfl-pr-review-auto.yml`, followed by `pwsh -NoProfile -File deployment/tests/test-sfl-pr-review.ps1`. The contract test requires exactly one queue key, set to max in the serialized observer with cancellation disabled, and checks the identical deployment payload. Other lint failures remain errors. Remove this narrow exception when a released linter supports the setting.
