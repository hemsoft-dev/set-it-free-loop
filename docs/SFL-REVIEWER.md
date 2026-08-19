# HemSoft SFL pull request reviewer

HemSoft SFL uses the native Codex GitHub review connected to Franz's ChatGPT
subscription. It does not run a model through OpenRouter and does not require an
OpenAI API key.

## Package

The review tier installs one workflow:

- .github/workflows/sfl-pr-review-auto.yml observes authenticated Codex results
  and publishes the immutable-head SFL Reviewer Gate Runner check.

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

The command:

1. verifies the active GitHub identity is HemSoft;
2. verifies the pull request is open and the observer is installed;
3. posts @codex review with an invisible marker containing the full head and
   base SHAs;
4. reuses the existing request URL instead of posting a duplicate for that
   head.

A new commit creates a new head and therefore permits one new request.

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
15368 with strict base freshness. The observer writes that exact check on the
reviewed head:

- clean Codex result: success;
- current-head Codex findings or malformed authenticated output: failure;
- stale or spoofed artifact: no check.

## Semantic change from the retired reviewer

The retired Kimi/OpenRouter reviewer ran a prescribed three-pass,
full-spectrum prompt, assigned Critical/High/Medium/Low severities, managed its
own SFL review threads, and retried missing output once.

Native Codex owns review depth, severity, and presentation. SFL now verifies
Codex provenance and head freshness and translates its clean/finding result
into the existing branch gate. It does not promise three passes, all severity
classes, SFL-authored approvals, obsolete-thread cleanup, or recovery retries.
A rerun is an explicit new `gh sfl review` request. `--retry` is rejected while
the latest request is still outstanding, preventing out-of-order results from
competing for the same gate. If the current-head request already exists but its
Codex result event was skipped while SFL was stopped, run
`gh sfl review --repo HemSoft/repository --pr 42 --retry` after restarting SFL;
the existing Codex artifact proves the prior request finished before the retry
is posted.

Pilot deployments are limited to HemSoft/hs-buddy until the source change and
smoke evidence are accepted. Do not deploy this migration to
developer-documentation.
