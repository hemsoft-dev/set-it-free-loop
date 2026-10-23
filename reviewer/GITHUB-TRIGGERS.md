# Comment and automatic polling triggers

Franz selected both triggers on October 21, 2026 at 01:19 EDT, relayed by the Chief:
an original **`@sfl review`** PR comment and automatic review when a PR opens or
its head receives a new push (`synchronize`). This replaces native-picker
assignment. It adds no machine user, webhook, Actions workflow, service, model
inference, credential import or activation grant. All tests use fake providers
and transports; the real GitHub PR metadata fixture contains no credentials.

`NewGitHubTriggerDiscoveryHTTP` is the selected exported constructor. Supply exact
enrollment, the store, explicit read transport and a `TriggerPolicy.EnabledAt`
epoch. Construction performs no HTTP. The old native constructor is private;
its historical tests retain useful pagination and reconciliation coverage.
The selected adapter satisfies `PollDiscovery`, `Current` and `History`, so the
existing polling controller and publisher use the same durable request ledger.
It never reads `requested_reviewers` or native request/removal events.

## Original commands

Only immutable owner ID 8227352 can issue `@sfl review`, `/sfl config ...` and
`/sfl cancel`. The review command is the exact entire original comment body;
quotes, mentions in other text, extra flags, alternate case, PR body text,
configuration files and model output are not commands. Unknown authors receive
no reply and cause no write. An edited-before-admission review command is ignored.
Accepted comment ID, author, original body hash/time and first captured snapshot
remain immutable after later edits or deletion. Editing an accepted review into
a configuration command, or the reverse, cannot give its ID a second meaning.
A new original comment ID is an explicit same-head re-request.

Configuration alone does not start work. FIFO acknowledgment intent, remote
reconciliation and platform receipt time still determine which configuration a
request captures. Pending/unknown acknowledgments block dependent admission.
Requests preceding an ACK keep the preceding version; equal timestamps defer.
Automatic requests use the observation cutoff for configuration selection, not
an invented push time. Configuring later never changes admitted/resumable work.
Cancellation advances generation and suppresses the current request. The same
head does not undo cancellation; a fresh comment or subsequent observed head
can create replacement work, subject to the same ordering and quota checks.

## Automatic observations and attribution

An open PR created at or after the preserved epoch creates an `opened` request
when first discovered, using its current stable head. A PR predating the epoch
is baselined without an initial automatic run. Subsequent head or head-repository
changes create a `synchronize` observation, even after a completed review.
Base-only movement fences old work and does not create a request. Observed
A→B→A heads are separate generations; the head hash is not a global dedup key.
Closed-PR history retains pending head changes without creating automatic work.
Reopening then creates the pending request, including an observed closed-state
ABA transition; base-only changes still do not originate work. The first closed
observation also cannot consume an eligible new PR's opening.
Repeated identical observations cannot rerun a completed or admitted request.
A fresh manual comment discovered with the same automatic head coalesces into
one explicit request only when it survives all accepted cancellation commands in
that batch. A canceled comment cannot suppress the automatic observation.

These are polling observations, not fabricated webhook receipts. `RequestOrigin`
records the PR's immutable database ID, verified PR author ID with role
`pr_author`, owner policy ID, preserved epoch, observation sequence/time and kind.
For `opened`, the event time is PR creation. For `synchronize`, push actor and
push timestamp are unavailable and event time stays zero. The PR author is never
presented as the pusher. Legacy `RequesterID` carries the authorizing owner-policy
principal; `request_origin` in the hashed job and rendered summary distinguishes
it from an actual owner comment. Manual origin records role `commenter`, its
real comment ID and body hash. Request numeric IDs are local, scoped identities,
not GitHub comment/event IDs.

Polling coalesces intermediate obsolete heads while offline, and cannot detect
an A→B→A transition entirely between observations. There is no promise to execute
a stale review for every missed push. The freshest observed head is eligible;
unseen event counts/actors/times remain unknown. A fresh explicit comment gives
a durable re-request when needed. GitHub's [activity Events API](https://docs.github.com/en/rest/activity/events)
is a bounded, delayed activity feed, not a complete push ledger. We use stable
[PR metadata](https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request) and
paginated [PR issue comments](https://docs.github.com/en/rest/issues/comments#list-issue-comments)
instead. No webhook permission/event subscription was added.

## Durable and publication fences

All comment pages must pass bounded same-origin pagination before admission.
PR identity, author, creation/update times, head repository/head, open state and
independently resolved base tip must remain stable across the read. Future
metadata, changed identities, backward cutoffs, partial pages and ambiguous late
provenance defer without mutation. A previously unknown original review comment
behind a completed cutoff is uncertainty, not permission to backdate work.

A `TriggerCursor` persists epoch, PR identity, last snapshot and observation
sequence in the existing SQLite transaction. A previous-state hash rejects stale
concurrent reads and prevents duplicate request allocation. It does not claim
GitHub linearizability. The polling controller serializes its own observations,
publication refreshes and post-capture admission, so a concurrent poll's newer
cursor cannot fail the worker's hash fence and discard captured source. Existing native requests/events are refused by the new
adapter: switching a populated legacy native store requires an explicit migration,
not silent reinterpretation. Record caps stop admission without dropping unknown
writes or request tombstones. No automatic replay of ambiguous inference is added.

Head/base/generation fences cancel and retire unpublished stale work. The publisher
refreshes this same ledger before and after COMMENT publication and retains its
existing durable outbox/unknown-POST reconciliation. A's COMMENT never fulfills B.
If B arrives during A's write, A is superseded and annotated when possible. B may
run after that known write is reconciled; it does not need a third request. Serialized automatic observation order also
releases a known same-second B; independent same-time manual events remain
ambiguous. An
unknown A write still blocks release. UI pending-review state is irrelevant.
Only the publisher holds a scoped write transport; origin metadata gives models
no credentials or destination control.

## Validation and live gates

Focused tests cover original/edited/unauthorized commands, namespaces after edits,
open/duplicate/completed-push/manual runs, observed ABA, same-second serialized
head observations, base-only moves, cancellation, config ACK uncertainty/order,
restart, scope/identity/epoch changes, partial/foreign pages, actual retained PR
metadata, and stale A publication during B discovery. Real synthetic Git objects
feed the controller's evidence path; provider outputs and GitHub writes are fake.
The copied PR221 metadata is whitelisted from an actual owner-authenticated GET;
its source, capture time and SHA256 are in the adjacent provenance fixture.

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The [dated activation checklist](../docs/research/2026-10-21-alpha-activation-checklist.md)
records current source versus qualification evidence and prepared provider
questions for these selected triggers. It preserves failed host prerequisites
and the owner's separate activation boundary.

Before live activation, the following remain separate requirements:

- Applicable public-target/unattended subscription permission for both pinned
  official clients, including account/model entitlement and refresh/expiry.
- Account-specific evidence that API-key/paid overflow cannot be used; no
  assumption that a private polling host exempts a public repository.
- Finished credential-owner/acquisition/runtime composition with measured worker,
  source-reader and publisher isolation, hostile/resource tests and whole-worker
  stop plus restart/unknown-attempt recovery; current offline component tests
  do not qualify that aggregate.
- Real control-worker and six-arm benchmark execution/quality evidence, trusted
  release promotion/rollback and bounded pilot preparation under #174/#177.
- Final selected App/installation/permissions qualification at activation,
  model-free verification of the new triggers and live GitHub rendering evidence
  for the required review/status cases under its applicable authority.
- Explicit owner authorization for provider credential use, public-repository
  model inference and live activation. The trigger decision and merged source
  alone supply none of those grants. Paid usage, broader scope and retired
  automation remain frozen.

These are unsatisfied gates, not successful measurements. The native picker is
no longer a blocker. #173/#174/#176/#177 and the final activation checklist must
retain current receipts and distinguish source completion from live qualification.
