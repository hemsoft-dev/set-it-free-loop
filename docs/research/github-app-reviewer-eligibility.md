# GitHub App bots as pull-request reviewers

Research date: 2026-08-16

This note answers two narrow questions:

1. What does GitHub publicly document about making a GitHub App bot an
   assignable pull-request reviewer?
2. Does registering the App under a personal account instead of an organization
   change reviewer eligibility?

The short answer is uncomfortable but useful. GitHub's current GraphQL schema
supports bot review requests, but GitHub does not publish the eligibility rules
that decide which App bots the reviewer picker and APIs will accept. App
ownership changes who manages the registration and where a private App can be
installed. No public GitHub document found in this research says that personal
or organization ownership changes reviewer eligibility.

Do not transfer either SFL App or add a new user-authorization flow based on the
current evidence. First run the same controlled test against both registrations,
then take the two result sets to GitHub Support.

## What GitHub documents

### Bot review requests exist

GitHub's current GraphQL schema has two mutations for setting pull-request
review requests:

- `requestReviewsByLogin` accepts `botLogins`. GitHub says the login must include
  the `[bot]` suffix, and gives `copilot-pull-request-reviewer[bot]` as its
  example.
- `requestReviews` accepts `botIds`, described as the node IDs of bots to request.
- Both inputs accept `union: true`, which adds requested reviewers rather than
  replacing the existing set.

The same schema defines `RequestedReviewer` as a union that includes `Bot`,
`User`, `Team`, `EnterpriseTeam`, and `Mannequin`. A successful bot review
request is therefore a first-class GitHub review request, not a simulated
comment.

GitHub added `requestReviewsByLogin` and `botLogins` to the schema on
2026-01-22. This is recent platform behavior, not a long-standing general App
contract.

Sources: [GitHub GraphQL pull-request schema](https://docs.github.com/en/graphql/reference/pulls#requestreviewsbylogininput),
[GitHub GraphQL 2026 changelog](https://docs.github.com/en/graphql/overview/changelog/2026#schema-changes-for-2026-01-22)

### The picker has a separate integration-aware data source

`PullRequest.suggestedReviewerActors` returns actor suggestions based on commit
history, past review comments, and "integrations." Its actor result can represent
a bot. This is the strongest public clue that GitHub has an integration-aware
selection layer behind the reviewer picker.

It does not prove that presence in `suggestedReviewerActors` is a prerequisite
for `requestReviews`, and the documentation does not define how an integration
enters that set.

Source: [GitHub GraphQL `suggestedReviewerActors`](https://docs.github.com/en/graphql/reference/pulls#pullrequest)

### A real request leaves observable state

GitHub models a created request in three useful places:

- `PullRequest.reviewRequests` contains a `ReviewRequest` whose
  `requestedReviewer` can be a `Bot`.
- The GraphQL timeline contains a `ReviewRequestedEvent`.
- REST issue and timeline events expose `review_requested`, including the
  requested reviewer and requester.

These are better acceptance criteria than a mutation returning HTTP 200 or a
GraphQL payload without errors. GitHub's documentation does not promise that a
no-error mutation created a request.

Sources: [GitHub GraphQL review-request objects](https://docs.github.com/en/graphql/reference/pulls#reviewrequest),
[GitHub REST issue event types](https://docs.github.com/en/rest/using-the-rest-api/issue-event-types#review_requested)

### `review_requested` is a notification, not an enrollment mechanism

The `pull_request` webhook supports the `review_requested` activity. A GitHub
App needs at least read-level `Pull requests` repository permission to subscribe
to the `pull_request` event. GitHub Actions also supports
`pull_request: types: [review_requested]` and `pull_request_target` with that
activity type.

Subscribing lets an App or workflow react after GitHub creates the request. The
webhook documentation does not say that subscribing makes the App bot available
in the reviewer picker or eligible for a review request.

Sources: [GitHub webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads#pull_request),
[GitHub Actions pull-request events](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request)

### GitHub documents native assignment for Copilot, not arbitrary Apps

GitHub's generic REST endpoint for requesting reviewers describes user logins
and team slugs, and its response schema contains `users` and `teams`. Separately,
GitHub's Copilot documentation explicitly tells users to request Copilot from
the Reviewers sidebar or call that REST endpoint with
`copilot-pull-request-reviewer[bot]`.

This proves that native UI and API assignment work for at least one bot. It does
not establish that the REST endpoint accepts every installed GitHub App bot.
GitHub publishes no matching enablement guide for third-party Apps.

Sources: [GitHub REST review requests](https://docs.github.com/en/rest/pulls/review-requests),
[GitHub Copilot code review](https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/copilot-code-review#request-a-review-from-copilot)

## Personal-account ownership versus organization ownership

GitHub permits registering an App under a personal account or an organization.
Every GitHub App has a built-in bot account. GitHub documents these ownership
effects:

- The owner or an App manager controls the registration.
- A private App can only be installed on the personal or organization account
  that owns it.
- A public App can be installed on other accounts.
- An installation grants repository access and permissions on its installation
  target. Installation is distinct from user authorization.
- Organization policies can restrict who may install an App or approve its
  repository access.

Sources: [Registering a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app),
[Making a GitHub App public or private](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/making-a-github-app-public-or-private),
[Installing your own GitHub App](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app),
[Differences between GitHub Apps and OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/differences-between-github-apps-and-oauth-apps#machine-vs-bot-accounts)

No cited GitHub source connects the registration owner's account type to:

- inclusion in `suggestedReviewerActors`;
- appearance in the pull-request reviewer picker;
- acceptance by `requestReviews` or `requestReviewsByLogin`; or
- delivery of `review_requested` after a request is created.

That absence does not prove owner type is irrelevant to GitHub's unpublished
eligibility logic. It means the current TODO must not state that owner type is
irrelevant as a confirmed fact. It is one variable to isolate.

For SFL, the two registrations give us a useful A/B comparison. Any future
write-capable test must use only the approved validation repositories:

| App | Registration owner, supplied context | Installation target to verify | Bot login to verify |
| --- | --- | --- | --- |
| HemSoft `sfl-app` | Personal account `HemSoft` | Personal account `HemSoft`, including `HemSoft/hs-buddy` | `sfl-app[bot]` |
| Relias `set-it-free-loop` | Organization `relias-engineering` | Organization `relias-engineering`, including `relias-engineering/relias-assistant` | `set-it-free-loop[bot]` |

The owner types above come from the project context. Record the live App settings
and installation records before treating them as verified evidence.

## What the commercial products document

The first-party setup guides for CodeRabbit, Greptile, and Macroscope describe
GitHub App installation, repository selection, automatic review, and comment
commands. None of the reviewed vendor documents explains how its bot became an
eligible native reviewer or claims that ordinary App registration settings are
sufficient.

| Product | First-party documented trigger | Personal versus organization setup | Native sidebar assignment documented? |
| --- | --- | --- | --- |
| CodeRabbit | Reviews start automatically after installation. A user can comment `@coderabbitai review` or `@coderabbitai full review`. | Its GitHub setup asks the user to select an organization or their account name for personal repositories. | Not in the reviewed setup, auto-review, command, or FAQ pages. |
| Greptile | New PRs trigger automatic review after indexing. A user can comment `@greptileai` for a manual review. | Its quickstart asks the user to choose the GitHub account or organization and grant repository access. | Not in the reviewed quickstart, trigger, or developer pages. |
| Macroscope | Connected repositories receive automatic review. A user can comment `@macroscope-app review`. | Its setup sends the user to GitHub to authorize Macroscope and select repositories. | Not in the reviewed setup or code-review pages. |

Sources:

- CodeRabbit: [GitHub installation](https://docs.coderabbit.ai/platforms/github-com),
  [automatic review controls](https://docs.coderabbit.ai/configuration/auto-review),
  [review commands](https://docs.coderabbit.ai/reference/review-commands)
- Greptile: [quickstart](https://www.greptile.com/docs/quickstart),
  [developer essentials](https://www.greptile.com/docs/code-review/developer-essentials),
  [review triggers](https://www.greptile.com/docs/code-review-bot/trigger-code-review)
- Macroscope: [getting started](https://docs.macroscope.com/setup-instructions),
  [code review](https://docs.macroscope.com/bug-detection-and-fixes)

Macroscope's settings page has an `Auto-assign Reviewer` option described as
assigning a reviewer when a PR opens without one. The page does not say that the
assigned reviewer is Macroscope itself. CodeRabbit's `auto_assign_reviewers`
configuration explicitly operates on reviewer usernames and team slugs. Neither
setting proves that the vendor bot is an assignable reviewer.

Sources: [Macroscope settings](https://docs.macroscope.com/settings),
[CodeRabbit reviewer configuration](https://docs.coderabbit.ai/reference/configuration#auto-assign-reviewers)

The practical lesson is that these products' public onboarding paths do not ask
customers to perform a separate OAuth authorization merely to request the bot as
a reviewer. They install the GitHub App, select repositories, and enable review.
That supports rejecting a new SFL user-auth flow unless GitHub identifies it as a
specific requirement.

### Assignment and review submission are separate

A GitHub App does not need a pending native review request to submit a review.
The vendor docs make this distinction visible. CodeRabbit can submit an approval
through `@coderabbitai approve` when its request-changes workflow is enabled.
Macroscope Approvability can approve an eligible low-risk PR after its checks
pass. Both features operate as part of the vendor's automatic or comment-driven
workflow. Neither vendor says that a human must first select its bot in the
Reviewers sidebar.

Sources: [CodeRabbit `approve` command](https://docs.coderabbit.ai/reference/review-commands#coderabbitai-approve),
[Macroscope Approvability](https://docs.macroscope.com/approvability)

SFL's App-authored approval can therefore be commercially comparable even while
native reviewer assignment remains unresolved. These are two different parity
claims and the TODO should track them separately.

## What is proven for HemSoft, and what is not

The current HemSoft TODO records that GitHub accepted three mutations against
[`HemSoft/hs-buddy` PR #385](https://github.com/HemSoft/hs-buddy/pull/385):

- `botLogins: ["sfl-app"]`
- `botLogins: ["sfl-app[bot]"]`
- the bot node ID through `botIds`

No review request, `review_requested` event, or wrapper run followed. SFL also
did not appear in the queried reviewer suggestions. This is strong proof that
those calls did not create a native request in that test. Absence from
`suggestedReviewerActors` alone would not prove absence from the reviewer picker
because GitHub defines that field as suggestions, not as the complete set of
requestable reviewers.

It does not prove why. Plausible explanations that remain unverified include an
undocumented GitHub eligibility program, App visibility, a registration setting,
an installation state, a permission or subscription mismatch, or a product bug.
"GitHub-side reviewer eligibility" is therefore a working hypothesis, not a
confirmed root cause.

## Live personal-versus-organization comparison

The following checks were run on 2026-08-16 after the documentation review.

### HemSoft personal-owned App

- The signed-in HemSoft Developer settings page lists `SFL App` at
  `/settings/apps/sfl-app`, which confirms that the registration belongs to the
  HemSoft personal account.
- The registration detail page required GitHub sudo-mode reauthentication, so
  this audit did not claim live visibility, event-subscription, or requested
  permission values from that page.
- The PR #385 mutation results recorded above remain the live native-assignment
  evidence. They prove no request was created, not why GitHub rejected it.

### Relias organization-owned App

With the `fhemmerrelias` account and its existing `admin:org` scope, GitHub's
organization installation endpoint returned:

- App slug `set-it-free-loop`, App ID `3650906`, installation ID `130729346`;
- target `relias-engineering`, target type `Organization`;
- repository selection `all` and no suspension;
- no subscribed webhook events in the installation response; and
- a permission grant far broader than the reviewer contract in Relias source.

The Relias `gh sfl status` command independently reports the installation drift
on all three v6.5.7 consumers. It requires selected repositories and the narrow
reviewer permission set, but the live installation uses all repositories and
includes many unexpected permissions. This is hardening evidence, not evidence
that the reviewer runtime is broken: SFL was confirmed working on 2026-08-15.
Future live validation is restricted to `relias-assistant`.

On `relias-engineering/configurator` PR #112, the integration-aware reviewer
query returned:

- `set-it-free-loop`: zero results;
- Copilot: one Bot result, `copilot-pull-request-reviewer`;
- CodeRabbit: zero results; and
- Macroscope: zero results.

The organization-owned SFL App therefore is not currently exposed by this
GitHub reviewer query either. Ownership type did not produce a positive result
in the observed pair. This still does not prove that ownership is absent from
GitHub's unpublished eligibility logic.

No native review-request mutation was sent to the Relias PR during this audit.
If that investigation resumes, it must use a controlled
`relias-engineering/relias-assistant` PR.

## Controlled test matrix

Use a new non-draft PR with no previous review from the tested App. Keep the diff
small and identical in intent across `HemSoft/hs-buddy` and
`relias-engineering/relias-assistant`. Do not use another repository for this
matrix.

| Test | HemSoft `sfl-app` | Relias `set-it-free-loop` | Pass evidence |
| --- | --- | --- | --- |
| Registration identity | Record owner type, App ID, slug, bot node ID, visibility | Same | Settings screenshot or authenticated API output |
| Installation | Record installation target, installation ID, repository selection, suspension state | Same | Installation API output or Configure page |
| Permissions | Record `Pull requests` access and all current permissions | Same | Installation token permissions or Configure page |
| Event subscription | Record whether `pull_request` is subscribed | Same | App settings and a test delivery if a webhook endpoint exists |
| Reviewer picker | Search exact bot login in the GitHub PR sidebar | Same | Screenshot showing present or absent |
| Suggested actors | Query `suggestedReviewerActors` for the exact slug | Same | Node type, login, ID, and total count |
| GraphQL by login | Call `requestReviewsByLogin` with the exact `[bot]` login and `union: true` | Same | `reviewRequests` contains the Bot |
| GraphQL by ID | Call `requestReviews` with the bot node ID and `union: true` | Same | `reviewRequests` contains the Bot |
| Timeline | Query `ReviewRequestedEvent` and REST issue events | Same | New event names the Bot and requester |
| Trigger delivery | Observe App webhook delivery or SFL wrapper workflow | Same | Delivery or workflow run ties to the request event |
| Review completion | Let SFL review the exact head | Same | App-authored review on the expected commit |
| Positive control | Repeat the picker and suggested-actor reads for an installed commercial reviewer | Same where installed | Control bot appears where SFL does not |

Interpret the pair, not one result:

- If both Apps fail with matching installation and permission state, owner type
  is a weak explanation.
- If Relias succeeds and HemSoft fails, compare visibility, installation target,
  permissions, subscriptions, and suggested-actor membership before attributing
  the result to personal ownership.
- If the reviewer picker succeeds but GraphQL fails, send GitHub the mutation,
  bot ID, PR ID, time, and request correlation details.
- If GraphQL creates `ReviewRequest` but no event arrives, the problem is event
  delivery or the SFL trigger, not reviewer eligibility.

## Questions for GitHub Support

Open one ticket containing both App registrations and both test PRs. Ask:

1. What documented or internal condition makes a GitHub App bot eligible for the
   pull-request reviewer picker and the GraphQL `botLogins` or `botIds` inputs?
2. Is eligibility restricted to GitHub-selected integrations, Marketplace Apps,
   public Apps, or an allowlist? If so, how can an App owner request enrollment?
3. Does `PullRequest.suggestedReviewerActors` expose the same integration set
   used to validate bot review requests?
4. Can a private App owned by a personal account be a requested reviewer? Can a
   private App owned by an organization be one? Does the registration owner's
   account type affect this feature?
5. Are `Pull requests` permission and the `pull_request` event subscription
   sufficient for a bot reviewer, or is another permission or subscription
   required?
6. Why can `requestReviewsByLogin` and `requestReviews` return without GraphQL
   errors while creating no `ReviewRequest` or `ReviewRequestedEvent` for these
   installed bots?
7. Is silent no-op behavior expected for an ineligible bot? If yes, where is
   that behavior documented, and how should clients detect it?

Attach:

- App slug, App ID, bot node ID, registration owner type, and visibility;
- installation ID, target type, selected repository, and permissions;
- PR node ID and head SHA;
- exact mutation and response, with secrets removed;
- before and after `reviewRequests`, suggested actors, and timeline output;
- UTC timestamps; and
- the HemSoft and Relias comparison table.

GitHub Support: [support.github.com/contact](https://support.github.com/contact)

## Recommended next decision

Keep the existing App installation authentication and wrapper trigger. The
reviewer runtime was working in both accounts on 2026-08-15. Native assignment
testing is optional; if it resumes, run the symmetric matrix only on
`hs-buddy` and `relias-assistant`. If neither App becomes a requested reviewer,
file the support ticket before changing application architecture.

Do not make native reviewer assignment the blocker for commercial-product
parity. The three reviewed vendors document automatic webhook-driven reviews
and comment commands as their normal interaction model. GitHub only explicitly
documents native bot assignment for Copilot. Native assignment remains a useful
product goal and a valid support question, but it is a separate platform
investigation.

Separately, `@sfl-app review` and its Relias equivalent remain worthwhile for
commercial-product interaction parity because every reviewed vendor documents a
comment trigger. That command is useful even if GitHub never makes SFL available
in the reviewer picker, but it should not be presented as proof that the native
assignment gap is solved.
