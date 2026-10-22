# Native GitHub discovery

The selected trigger adapter is now [comment and automatic polling discovery](GITHUB-TRIGGERS.md), following Franz's October21 decision. Native picker requests are not a trigger or activation prerequisite. The native-specific reader below is internal historical/reconciliation test support; its constructor is no longer exported. Generic bounded HTTP, pagination and base-tip helpers are reused by the selected adapter.

`GitHubDiscoveryHTTP` implements the publisher's native `Current` and `History`
reader. It is an explicitly constructed library with a bound SQLite Store and
an explicit read-only HTTP transport. Tests use local TLS servers with synthetic
credentials. Construction does not find credentials, register an App, poll,
launch providers or activate the retired controller.

The [credential owner](CREDENTIAL-OWNER.md) and [service assembly](GIT-SERVICE.md)
compose explicit selected-installation and role checks. Actual final installation
qualification remains open; enrollment integers do not attest the credential.
The adapter rejects write methods before forwarding to that transport. The
publisher uses a separate transport. Neither transport belongs in a model,
provider environment or source-reader worker.

## Repository and snapshot

Every read first checks scope, the Store's exact enrollment and the canonical
repository's immutable ID/name. Wrong scope makes zero HTTP calls. HTTP uses the
same fixed origin, version header, redirect refusal, strict JSON checks and
bounded requests as publication. No API-provided destination is followed.

A capture reads PR identity, open state, head commit and head repository ID,
including fork heads. It resolves the PR's base branch through the Git ref API.
The PR's cached `base.sha` is not accepted as the current branch tip. This matters
when another PR merges while GitHub's PR metadata still names the previous base.
Ref names must pass a finite unambiguous grammar before URL escaping.

The merge base comes from a comparison of the two captured immutable commit IDs.
Page two with `per_page=1` requests comparison metadata without the first-page
changed-file patches. The returned comparison base must equal the captured base;
all snapshot object IDs must validate. An existing-owner read-only GitHub check
also returned this metadata on page two for zero-commit and one-commit comparisons,
with an empty commit list and no changed-file payload. This is comparison protocol
evidence, not an App credential or native-review eligibility qualification.

PR/head/fork/base-ref/open/pending state is captured again after event and command
collection. Any movement defers the entire batch. No incomplete or unstable batch
advances SQLite state. These reads are not an atomic GitHub snapshot and do not
prove the absence of an intermediate move that returns to the same revisions.

## Request provenance

The adapter exhausts native issue-event pages for `review_requested` and
`review_request_removed`. Only immutable numeric identities select the enrolled
bot and owner. A native owner request requires a positive event ID, canonical
event URL, timestamp and matching actor/review-requester IDs. A pending username
or model text cannot create a request. Team requests and other reviewer identities
cannot authorize this bot. Missing target/requester data defers.

Request/removal events are ordered by platform time. Equal timestamps have no
assumed numeric-ID causal tie-break and defer. Explicit removals retire their
preceding request. A later request supersedes the earlier one, including when a
non-owner requests the same bot. Non-owner requests cannot admit jobs or generate
ACKs, and cannot leave an older owner request eligible.

SQLite retains every relevant request and removal event's ID, kind, bot,
actor/requester IDs and creation time. Later complete reads must reproduce all
of those immutable records, including removals, across restart. A boolean
removed flag cannot stand in for missing remote history. The current scan must
also independently reconstruct each already-removed request.

A previously unseen event at or before the durable history cutoff contradicts
that cutoff and defers both Current and History without advancing local state.
It cannot become admissible merely because a later scan returns it. Known
request creation records from the legacy normalized seam can acquire matching
creation metadata, but this does not authorize importing an unknown old removal.
After event records exist, a batch omitting the native event collection is
rejected. Adoption of untracked live state would require explicit migration or
requalification; this frozen alpha has no live state migration path.

The snapshot on a new native request is its first qualified discovery capture,
not a claim about revisions at the historical event's creation time. After the
controller persists that request, later reads reuse its original snapshot.
Restart, head pushes and base movement cannot rewrite it. Current returns both
the current snapshot and the request's captured snapshot; runtime admission must
fence moved revisions before provider work as well as before publication.
Previously retained native event identities must remain visible in complete
remote reads. Missing history, identity changes, duplicates or inconsistent
pending state defer instead of silently dropping local provenance.

The pending-reviewer endpoint reports current users/teams and clears a reviewer
after submission. Empty pending state therefore returns no current request but
does not mark the last native request removed. `History` still returns B when
publishing A cleared B's pending reviewer. Publisher's postcheck discovers B,
pauses admission and marks A superseded. A never fulfills B.

## Commands and durable discovery

Owner comments beginning with `/sfl ` are collected through complete pagination,
with original numeric comment/author identity, creation/update times and exact PR
URL. Non-owner comments are ignored without responses or writes. Edited-before-
admission commands reach the existing command parser and are rejected there.
Accepted commands remain immutable in SQLite and keep their original ACK ordering.

`Current` commits its complete request/command batch through `Store.Discover`
before returning. A newly observed cancellation or replacement therefore reaches
the publisher's transactional dispatch fence. This can create a local ACK intent;
it performs no GitHub write or inference. `History` returns a complete batch for
the caller's existing discovery transaction. Unknown ACKs continue to defer
configuration-dependent admission.

HTTP 403 remains a structured deferred read error, including both throttling
and unclassified permission failures. It cannot mark an accepted review
superseded or skip its pending postcheck. Retry timing preserves bounded
Retry-After seconds, or the primary rate-limit reset relative to the response's
Date when remaining requests are zero. Only a missing repository or PR lookup
with HTTP 404 establishes inaccessible scope. A missing base ref or other
metadata is a failed snapshot read, not proof of lost repository access.

The first repository response's platform `Date` is the batch cutoff. Relevant
events/commands beyond that cutoff defer. Pagination is bounded to 100 pages of
100 records per collection. Relevant native events and owner commands also obey
the Store's finite record limits. Collections process one page at a time under
the shared 48-MiB page cap; other responses retain two MiB. Each request has a
ten-second timeout. The 45-minute discovery deadline covers two full collections
and stable snapshot reads at that request limit. Shorter caller deadlines still
cancel. Publisher delegates Current/History under the same outer budget.

A platform Date and exhausted pagination are transport evidence, not proof of
GitHub event linearizability or arbitrary-App native picker support. The historical native
contract would require its own recovery qualification. Native-picker eligibility
is now superseded; the selected [comment/automatic adapter](GITHUB-TRIGGERS.md)
must qualify its own discovery and A/B recovery behavior before live use. The tests
below demonstrate the offline contract; they do not qualify GitHub's consistency
or account permissions.

## Validation

Run the repository's ordinary vet and race suite. The focused cases are:

```sh
go -C reviewer test -race -run '^TestGitHubDiscovery' -count=1 .
```

Actual TLS tests exercise stale cached base metadata, fork provenance, immutable
request captures across movement/restart, short event/comment pages with the
repository-ID Link alias, removal/re-request idempotence, owner ACK timing,
non-owner replacement, newly discovered cancellation before POST and disappearance
of B during A's accepted write. Hostile cases include incomplete/foreign pages,
duplicate/ambiguous/future events, missing requester attribution, events arriving
behind the durable cutoff, lost or changed removal identities after restart,
rate-limited postcheck recovery, lost previously retained history and revision
changes during collection. Failed batches leave SQLite untouched. Throttling
leaves postcheck_pending intact and subsequent recovery confirms the original
review without a duplicate POST or supersession annotation.

[Polling/backoff](POLLING.md), [snapshot collection](SNAPSHOT-EVIDENCE.md),
[credential qualification](GITHUB-INSTALLATION.md) and
[trusted runtime packaging](GIT-STARTUP.md) have source implementations. Actual
selected-App/installation consistency and aggregate physical/runtime
qualification remain open under
[issue #176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176). No live deployment or product-completion claim follows from these tests.

Protocol sources checked October 20, 2026: GitHub's [issue events](https://docs.github.com/en/rest/issues/events#list-issue-events),
[native event fields](https://docs.github.com/en/rest/using-the-rest-api/issue-event-types#review_requested),
[pending reviewers](https://docs.github.com/en/rest/pulls/review-requests#get-all-requested-reviewers-for-a-pull-request),
[Git refs](https://docs.github.com/en/rest/git/refs#get-a-reference),
[immutable comparison and first-page file behavior](https://docs.github.com/en/rest/commits/commits#compare-two-commits)
and [pagination](https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api).
