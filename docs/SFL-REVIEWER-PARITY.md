> **Retired on 2026-10-09:** SFL is frozen. These are historical instructions, not an active setup or deployment procedure. Do not install, configure, dispatch or restart SFL. See the repository README for the freeze status.

> The repository observer and its required SFL gate are retired. The contracts below describe the retained legacy template. Use the [central service runbook](../central-reviewer/README.md) for the organization App pilot.

# SFL reviewer migration baseline

HemSoft no longer maintains byte-level parity with the Relias compiled reviewer.
Issue #125 deliberately replaces the Kimi/OpenRouter execution path with the
native Codex GitHub review included in Franz's ChatGPT subscription.

The machine-readable runtime identity is recorded in
[deployment/release-metadata.json](../deployment/release-metadata.json). The
deployed contract is one observer workflow, not a copied agent workflow,
compiled lock, and recovery wrapper.

## Verified identities

| Purpose | Identity |
| --- | --- |
| Codex GitHub App | ID 1144995, slug chatgpt-codex-connector, owner openai |
| Codex bot | User ID 199175422, login chatgpt-codex-connector[bot] |
| Required SFL gate | SFL Reviewer Gate Runner, GitHub Actions App ID 15368 |

## Runtime evidence

The initial hs-buddy experiment produced both supported shapes:

- [owner trigger on PR #427](https://github.com/HemSoft/hs-buddy/pull/427#issuecomment-5336227425);
- [clean Codex result on PR #427](https://github.com/HemSoft/hs-buddy/pull/427#issuecomment-5336243021);
- [finding review example on PR #415](https://github.com/HemSoft/hs-buddy/pull/415#pullrequestreview-4947117457).

GitHub exposes full App provenance on the clean issue comment. Its review REST
object omits performed_via_github_app, so findings use the immutable bot ID,
review login, COMMENTED state, full commit_id, resolved reviewed-commit prefix,
and current pull request head.

## Regression evidence

- gh-sfl/reviewer_contract_test.go executes clean, finding, stale, spoofed, and
  malformed classification fixtures.
- gh-sfl/review_test.go proves one head-bound trigger comment and duplicate
  suppression.
- deployment/tests/test-sfl-pr-review.ps1 validates the retained canonical
  template, the absence of the retired source observer, and the absence of
  OpenRouter engine configuration.
- deployment/tests/test-sfl-review-platform.ps1 proves deployment and gate
  wiring.

This migration is intentionally HemSoft-specific. It does not change the Relias
repository or developer-documentation.
