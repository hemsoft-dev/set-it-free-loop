# Adversarial alpha eligibility evidence

Checked October 19, 2026 EDT for [issue #173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173). This report records documentary and read-only account checks. No App was registered or installed, credentials generated, billing settings changed or inference prototype run.

The owner's merge-mission invocation authorizes offline implementation and guarded merges. It does not supply missing provider permission, billing evidence or separate live activation authority. Keep the [alpha contract](https://github.com/hemsoft-dev/set-it-free-loop/issues/172) intact.

## Gate results

| Gate | Result | Evidence and remaining need |
| --- | --- | --- |
| Exact target | Pass | Authenticated HemSoft API returns public repository `hemsoft-dev/set-it-free-loop`, ID `1169772257`. Actions is disabled, with zero workflows. |
| Official clients installed | Pass for inspection | `codex --version` reports `0.163.0`; `claude --version` reports `2.1.303`. Version inspection is not model entitlement or isolation proof. |
| Existing interactive subscription auth | Pass for existing logins only | `codex login status` reports ChatGPT login. Sanitized `claude auth status --json` reports logged in, first-party `claude.ai` authentication and Pro subscription. These are the user's existing interactive profiles, not provisioned worker credentials. |
| Codex unattended public-target support | Blocked | The cached-account CI guide excludes public/open-source repositories. General scripted execution and a private host do not document an exception for this exact service. Applicable provider clarification is missing. |
| Claude documented script credential | Pass for documented mechanism | Official documentation describes a one-year subscription token for scripts. Account-specific worker entitlement, renewal and ordinary individual-use boundaries still need evidence. |
| Paid overflow prevention | Blocked | Neither an existing login nor CLI dollar estimates proves that paid credits/extra usage are disabled. Account-specific billing evidence or an enforceable worker block is missing. No settings were changed. |
| Native custom-App reviewer eligibility | Blocked | The GraphQL schema has bot reviewer inputs but does not establish arbitrary-App picker eligibility. Authenticated `GET /apps/sfl-app` returns 404; the organization installation inventory contains no entry for retired App ID `4448946` or slug `sfl-app`. A 404 alone is not proof of deletion. A newly authorized exact App experiment is still needed. |
| Restricted client tool set | Unproven | Installed Codex lists `shell_tool` as a stable feature; its existence does not prove that disabling it removes all execution paths. Claude help documents restricted/safe modes, available-tool selection and strict MCP configuration. No credential-bearing tool-discovery probe was run. |
| Host isolation and worker auth lifecycle | Blocked for live use | Fake-provider/source-reader contracts can be tested offline. No disposable guest, egress allowlist, credential refresh store or hostile live-client isolation has been qualified. |

## Two independent routes checked

For Codex, the first route was the [advanced cached-account CI workflow](https://learn.chatgpt.com/docs/auth/ci-cd-auth). It expressly excludes this public target. The second route was [general non-interactive authentication](https://learn.chatgpt.com/docs/non-interactive-mode) and [workspace access tokens](https://learn.chatgpt.com/docs/enterprise/access-tokens). Scriptability is documented; workspace tokens require Business or Enterprise, and no account-specific entitlement or applicable public-target authorization was established. Neither route proves the requested service is supported. Do not substitute API billing, private visibility or native R1-only review.

For native App eligibility, the first route was [GitHub's Bot request schema](https://docs.github.com/en/graphql/reference/pulls#requestreviewsbylogininput) and [native review request documentation](https://docs.github.com/en/pull-requests/how-tos/create-pull-requests/requesting-a-pull-request-review). Those interfaces do not guarantee enrollment of this custom App. The second route was the read-only named-App and organization-installation API inspection above. It establishes no usable SFL installation for the experiment. Do not repoint the retired controller, request the native Codex identity as a substitute, or install anything implicitly.

For account auth and spending, the first route was sanitized CLI status inspection. It establishes current interactive auth metadata only. The second route was the providers' [subscription authentication](https://code.claude.com/docs/en/authentication), [credential-use guidance](https://code.claude.com/docs/en/legal-and-compliance), [Claude extra-usage controls](https://support.claude.com/en/articles/12429409-manage-usage-credits-for-paid-claude-plans) and [Codex pricing/credits guidance](https://learn.chatgpt.com/docs/pricing). These explain mechanisms but cannot establish the owner's current billing settings. No financial decision or credential change is inferred.

## Reproduce the read-only evidence

Use the existing HemSoft credential binding and verify `gh api user --jq .login` returns `HemSoft`. Do not change the shared active account or print tokens.

```sh
gh api repos/hemsoft-dev/set-it-free-loop --jq '{id,full_name,visibility}'
gh api repos/hemsoft-dev/set-it-free-loop/actions/permissions --jq .enabled
gh api repos/hemsoft-dev/set-it-free-loop/actions/workflows --jq .total_count
gh api apps/sfl-app --jq '{id,slug,name}'
gh api orgs/hemsoft-dev/installations --paginate --jq '.installations[] | select(.app_id == 4448946 or .app_slug == "sfl-app") | {app_id,app_slug,repository_selection}'
codex --version
claude --version
codex features list
codex exec --help
claude --help
```

At this check the target identity matched, Actions returned `false`, workflows returned `0`, named-App lookup returned 404 and the filtered installation query returned no rows. Interpret failed reads as unavailable evidence rather than permission to recreate resources. Inspect authentication status locally, retaining only mode/subscription metadata, never raw auth files or account identifiers in public artifacts.

## Prepared provider question

This question is prepared, not sent:

> May a ChatGPT subscription holder use the unmodified Codex CLI with their own ChatGPT login on their own isolated machine, unattended, solely in response to that owner's native GitHub review requests, to review PRs in their own public repository? Both initial review and peer critique would run through the official CLI. Which authentication, refresh and paid-credit controls are supported for this exact workflow?

If a provider answer arrives, preserve its date, scope and supporting link or sanitized written evidence. A successful inference alone is not an entitlement determination.

## Model-free identity experiment, after explicit activation authority

1. Record the new App owner, slug, numeric App/Bot IDs, exact permissions and installation ID. Select only target repository ID `1169772257`; reject the retired installation identity.
2. Open a throwaway PR in this public repository and disclose that its requests/reviews are public. Capture the custom App in the native picker and the persisted Bot request. Verify the immutable requester is HemSoft, ID `8227352`.
3. Discover the request through the proposed paginated polling route. Preserve request/removal/re-request identities, event time and provenance. Include a request created while the poller is stopped.
4. Submit a canned App-authored COMMENT review without inference. Verify completion and a subsequent native re-request. Exercise removal and the old-A/new-B race. If A clears B's native pending state, retain B unresolved locally and pause for reconciliation. Never count A as fulfilling B.
5. Capture actual picker/request/completion screenshots as uploaded GitHub attachments rendered in the implementation PR, with a short recording where practical. Close the throwaway PR unmerged; record cleanup. Failed or ambiguous results remain blocked.

No screenshots of this experiment exist yet, because there is no activated alpha App. Source-reader/fake-client tests cannot substitute for this evidence.

## Exact remaining needs

Live use needs applicable public-target Codex support evidence, account-specific worker/overflow evidence for both providers, an explicitly authorized new App identity experiment, and qualified isolation/auth refresh. These are independent gates. Offline adapter/protocol/controller/evaluation tooling can proceed while they remain blocked. Preserve incomplete status and leave the source issue open until its full acceptance contract is met or the owner explicitly decides otherwise.
