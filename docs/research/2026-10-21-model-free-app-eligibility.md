# Model-free App reviewer experiment

Checked October 21, 2026, Eastern, for [#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173). The new App can publish a COMMENT review. Neither the checked native UI nor either documented Bot-request mutation created a pending review request. Native reviewer assignment is therefore **not qualified** for this App/configuration. Its later appearance as an existing commenter does not establish requestability. The experiment does not identify a GitHub eligibility rule or root cause.

Franz authorized this model-free experiment on October 20 at 20:28 Eastern, relayed by the Chief in the task. The prerequisite label work merged in #216/#217. The Chief created and installed the new App; this experiment used the supplied key for App JWTs and ephemeral installation tokens. It used no provider profile, model inference, paid usage or deployed alpha service. Normal repository review of this report is a separate delivery gate.

[Sanitized machine-readable evidence](evidence/2026-10-21-app-experiment.json) records the API results, exact IDs, screenshot hashes/attachment URLs and closure receipt. The [throwaway PR #221](https://github.com/hemsoft-dev/set-it-free-loop/pull/221) is **closed unmerged**. Its head was `81bf8669e29f7333582b62caa3e8e06cec1b2c0e`, based on `041c89cce041ec22c868a296f729265c5d14e10c`.

## Gate results

| Gate | Result | What the evidence proves |
| --- | --- | --- |
| New identity | Pass | App `hemsoft-sfl-reviewer`, ID **5406096**, owned by `hemsoft-dev`; Bot `hemsoft-sfl-reviewer[bot]`, ID **345646943**. Authenticated `GET /app` and Bot lookup agree. The retired identity was not reused. |
| Selected installation and permissions | Pass | Installation **173628126** has `repository_selection=selected`. Its scoped token enumerated exactly one repository, **1169772257**, `hemsoft-dev/set-it-free-loop`. Contents/metadata read and pull_requests write match the App and installation. Events are empty; the Chief reports no webhook. |
| Initial native picker | Failed in observed browser | Before the App commented, searches for `hemsoft-sfl-reviewer`, `HemSoft SFL` and `sfl` returned no match. Nothing was selected. |
| Native Bot request persistence through API | Failed | Both valid GraphQL mutations returned data without errors but empty `reviewRequests.nodes`. REST also returned empty pending users/teams and no request/removal timeline events. |
| App-authored COMMENT | Pass | [Review **5595158403**](https://github.com/hemsoft-dev/set-it-free-loop/pull/221#pullrequestreview-5595158403), state `COMMENTED`, Bot ID345646943, exact fixture head, submitted **01:01:22 EDT** / `05:01:22Z`. The body explicitly identifies canned, model-free text. |
| Existing-reviewer display after COMMENT | Pass | The App appears as an existing reviewer and an already-ticked picker entry after publishing its comment. This is participant display only. |
| Post-comment native request or re-request | Failed in observed browser | The Chief found no request/re-request control. Selecting the ticked entry, closing and reloading left the sidebar unchanged and persisted no request/event. |
| Owner attribution on a native request | Blocked | The API caller was independently verified as HemSoft/8227352, but there is no persisted native request/event whose actor can be qualified. |
| Polling discovery and requests during downtime | Blocked | No native request exists to discover or replay. Empty polling reads cannot qualify this gate. |
| COMMENT fulfilling a native request | Blocked | The comment had no pending request to fulfill. Authorship alone cannot prove completion semantics. |
| Native removal and re-request lifecycle | Blocked | No pending request or removal event was created. |
| Old completion A versus newer request B | Blocked | Neither A nor B could be established as a native request. No safe reconciliation behavior is claimed from this experiment. |
| Throwaway cleanup | Pass | #221 closed **01:13:30 EDT** / `05:13:30Z`; `mergedAt` and `mergeCommit` are null. Its feature branch was deleted after retaining the fixture privately for evidence. |
| Retired automation | Pass at checked snapshot | Actions remains disabled and registered workflows are zero. No legacy service was restored or replacement trigger enabled. |

## Procedure and exact observations

1. As HemSoft, verify user ID8227352 and the public repository ID1169772257. Authenticate the new App, inspect installation173628126, and mint a token explicitly restricted to that repository and the stated permissions. Read installation repositories and reject any larger scope. Credential values are never evidence; none are committed or retained in these receipts.
2. Open the inert-marker PR #221. The Chief checks the native Reviewers gear in Chromium on mini, signed in as HemSoft. Initial searches return no App. Capture screenshots and inspect REST pending reviewers plus the full paginated timeline.
3. Execute `requestReviewsByLogin` using `botLogins: ["hemsoft-sfl-reviewer[bot]"]`, the actual PR node ID and `union: true`. Separately execute `requestReviews` using the Bot node ID returned by GitHub and `union: true`. Both return no GraphQL errors and empty request nodes. The full variables/results are in the sanitized evidence. After each, REST pending users/teams and request/removal events remain empty. An earlier CLI variable-encoding preflight failed before execution and was corrected; it is not counted as a native-request outcome.
4. Submit one installation-token review with `event: "COMMENT"`, the exact head SHA and fixed text declaring that no model analyzed the PR. Verify returned and subsequently listed author ID, review ID, state, head and submission time. [GitHub documents COMMENT review creation and its pull-request write permission](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request).
5. The Chief repeats the native UI check after the App comment. The Bot is listed as an existing reviewer, and the picker entry is already ticked. Selecting it and closing/reloading creates no pending request. The Chief reports no request/re-request controls. Independently fetch the pending reviewers, complete paginated timeline and reviews: empty users/teams, zero request/removal events, and exactly the one App COMMENT.
6. Upload both screenshot sets into #221's Validation section with GitHub CLI2.101.0 `--attach`; fetch the saved body and verify all eight local references were rewritten to GitHub attachment URLs. Close #221 unmerged and verify its terminal fields. No inference controller or poller was activated.

The App, selected installation and supplied key files remain as provided for Franz's separate decision. The experiment did not uninstall the App or remove those owner-managed files. Ephemeral token values were held only in the individual experiment processes and were not written to these artifacts; revocation is not claimed. This report does not grant live provider use, paid overflow, deployment, a new trigger or broader installation scope.

## UI evidence and limits

The root agent visually inspected all eight supplied PNGs before upload. They show the public PR and relevant picker/sidebar state, with no credential values. The browser was automated Chromium, not an independently repeated manual browser. The Chief reports one earlier App-install form rejection from that browser. Preserve that caveat without inferring its cause or treating it as proof that another browser would succeed. No recording was supplied, so the evidence consists of still screenshots and API state.

The Chief's UI times were approximate: about01:03 Eastern for the first set and01:10–01:12 for the second. Exact screenshot mtimes are00:57:16–00:57:28 and01:09:23–01:10:00 EDT. Retain those timestamps separately from GitHub's exact review/closure times; the approximate reports are not used as timestamp proof. The images themselves and captured API sequence distinguish the pre-comment and post-comment states.

![Initial exact-slug search: Nothing to show](https://github.com/user-attachments/assets/d1bbeaf3-d540-4098-a3ab-488af96d0421)

![Initial display-name search: no match](https://github.com/user-attachments/assets/69d16c19-1820-4ac5-89d0-d5e852fad3bc)

![Initial broad search: no match](https://github.com/user-attachments/assets/978cb2e0-f3de-43b4-94db-241ae5368512)

![Initial final state: no selection](https://github.com/user-attachments/assets/f419b4f8-faed-46d2-bb0e-f816530a4b38)

![After COMMENT: existing-reviewer sidebar](https://github.com/user-attachments/assets/33b7f8e0-fe6b-4bc3-845c-84d8e694b9d2)

![After COMMENT: Bot now appears in picker](https://github.com/user-attachments/assets/336e0c41-d971-4be2-8d22-3fc6ce937b73)

![After COMMENT: already-ticked entry selected](https://github.com/user-attachments/assets/b2ea6109-5319-444e-aafa-a6f9d636e128)

![After selection/reload: no pending request](https://github.com/user-attachments/assets/b7c621d1-ba5e-4ff2-9536-ae7fd49edecb)

## Reproduce state inspection

Use the repository's task-bound HemSoft session and verify the actor before writes. The following are read-only and contain no App credential:

```sh
gh api user --jq '{login,id}'
gh api repos/hemsoft-dev/set-it-free-loop/pulls/221/requested_reviewers
gh api 'repos/hemsoft-dev/set-it-free-loop/issues/221/timeline?per_page=100' --paginate --slurp
gh api 'repos/hemsoft-dev/set-it-free-loop/pulls/221/reviews?per_page=100' --paginate --slurp
gh pr view 221 --repo hemsoft-dev/set-it-free-loop --json state,closedAt,mergedAt,mergeCommit,headRefOid
gh api repos/hemsoft-dev/set-it-free-loop/actions/permissions --jq .enabled
gh api repos/hemsoft-dev/set-it-free-loop/actions/workflows --jq .total_count
```

Expected retained experiment state: CLOSED/unmerged, exact fixture head, one COMMENT by345646943, no pending native reviewer and no request/removal events. Because the PR is now closed, these reads inspect retained state; they do not repeat an open-PR lifecycle qualification. Repeating a live identity experiment requires its own bounded fixture/authority, and source or documentation examples are not authority to import a key.

## Trigger choices for Franz

No replacement was selected or implemented. These are design options, not verified production paths or substitutes silently adopted by this experiment:

1. **Owner PR comment command, recommended:** for example `/sfl review`, polled by the controller. Use the comment ID as the durable request key and require author ID8227352. This offers an explicit per-PR request and a fresh ID for each re-request, without enabling Actions or webhooks. GitHub documents [reading comments on PRs with pull-request read permission](https://docs.github.com/en/rest/issues/comments#list-issue-comments). Adapter, configuration, cancellation and stale-job semantics still need implementation and qualification.
2. **Owner-applied label:** for example `sfl-review`, polled from the PR. GitHub supports [labels on pull requests](https://docs.github.com/en/rest/issues/labels). Owner attribution and a fresh request identity for remove/re-add must use retained events; current label presence alone is insufficient for downtime or re-request handling.
3. **Manual Run workflow button:** choose a PR number with [`workflow_dispatch`](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow). This adds a GitHub button and run identity but requires separately authorizing Actions and a trusted runner; Actions is currently disabled. No workflow was added or enabled.
4. **Keep native Reviewers as a requirement:** ask GitHub whether this exact App can become eligible, with this evidence package. A support question can be prepared, but sending it needs owner direction. Eligibility or eventual success is not promised.

Provider/account/paid-overflow, official-worker isolation, production control/benchmark, promotion and live-activation gates remain independent and open. The [October19 report](2026-10-19-alpha-eligibility.md), later issue evidence and #173 retain those unresolved requirements.
