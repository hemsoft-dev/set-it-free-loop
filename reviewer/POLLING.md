# Private polling lifecycle

The selected adapter is [GitHubTriggerDiscoveryHTTP](GITHUB-TRIGGERS.md), using original `@sfl review` comments plus automatic PR-open/head observations. It never consults native pending reviewers. Native lifecycle examples elsewhere describe retained legacy tests.

`OpenPollingController` composes discovery, SQLite admission, immutable evidence,
the stopped-worker engine and the publication outbox. It creates no GitHub App,
acquires no credentials and installs no service. Construction takes an explicit
store, exact enrollment/release, discovery/publisher transports, source capture and persisted-packet restoration,
runtime factory and qualification callback. Missing dependencies refuse startup.
The frozen SFL controller and workflows remain retired.

The qualification callback is trusted integration code, not a model claim. Live
composition must separately establish provider/client/auth eligibility, selected
installation/token scope, billing controls, worker isolation and owner activation.
The callback is checked before polling and before work. Recovering a job from an
older release also requires qualification of that captured release; the factory
must supply its pinned runtime rather than relabeling it as the new release.
Tests supply fake callbacks, providers and GitHub transports. Passing those tests
does not establish live eligibility.

## Ownership and scheduling

One OS flock in the store's private service directory owns the controller
lifetime. A second owner cannot start against the same directory. The store
requires private existing parents and a 0600 database. Opening the controller
does not read an interactive provider profile.

`Poll` performs one cycle. `Run` waits on the persisted next-poll time, worker
completion or cancellation and repeats. The configured interval is one second
through five minutes, exponential backoff is bounded by the supplied interval
through one hour, and one cycle has an explicit one-second through five-minute
deadline. Recognized GitHub Retry-After/reset delays up to one hour take priority
over a shorter exponential delay. Joined failures use the longest valid server
delay, including nested failures from different scopes or writes. Partial, timed-out or malformed collections
never authorize work. A definitive 404 from the enrolled repository lookup
returns `ErrRepositoryAccessLost`. The controller immediately cancels active
work and atomically pauses every retained request, advancing its generation.
Restored access does not unpause those old requests or authorize their prepared
reviews; a fresh authorized comment or subsequent head observation is required. Rate limits, transient errors and
failed collection pages do not carry that repository-wide access-loss signal.
Larger histories may require further qualified collection
and scheduling work; a timeout is not evidence of a clean or complete history.

The bounded schedule stores only version, next time, failure count and a fixed
reason. It lives in a separate `polling_state` SQLite row and commits alongside
job-state changes. Original writers that rewrite the legacy state JSON cannot
erase this backoff. Existing stores acquire the new table additively; missing
schedules mean no previous polling has occurred. Unknown versions and invalid
failure counts defer. Enrollment/default changes still require explicit migration.

## One cycle

1. Check enrollment and local fences, drain a completed invocation and respect
   persisted backoff. Verify qualification before any remote work.
2. List all open PRs, including those without a pending bot request, so owner
   configuration can be acknowledged before its first request. Union these with
   retained PRs and inspect an active worker's scope first. Reject foreign,
   duplicate or oversized candidate inventories.
3. Retrieve complete trigger provenance/commands and stable snapshots. Commit a
   complete batch transactionally, then cancel a locally fenced worker. Access,
   closed-state or revision changes pause the old request; they do not create
   replacement inference. Empty pending state alone never removes history.
4. Prepare status intents for non-complete jobs and mark replaced statuses
   for annotation ([status writes](CONTROLLER.md#status-writes)). Reconcile
   uncertain/recovering writes and dispatch prepared writes through
   the publisher's existing checks. Never POST an unknown intent again. FIFO
   command acknowledgments are limited to one new ACK per cycle; remaining ACKs
   defer admission. Prune only records already eligible under retention policy.
5. If no physical invocation or unresolved durable lease exists, select the
   oldest runnable job or authorized comment/automatic request. For a new request, capture source
   and reobserve before admission. One controller exclusion covers each
   observation, publication refresh and the worker's reobservation through
   admission, never capture or work. A concurrent poll therefore cannot make the
   worker's trigger hash stale and discard its capture; real remote changes still
   fence it. Restore the exact SQLite-returned packet
   against its immutable Git inventory before invoking the qualified runtime
   factory. Existing queued/completed jobs use restoration without recapture.
   Originally omitted patches retain their coverage and cannot gain inline
   anchors when a missing blob later becomes available. Included patches must
   still reproduce their retained blobs; unavailable evidence defers recovery. Providers run sequentially under the captured configuration.
   A permanent [capture refusal](#capture-refusals) is admitted instead as a
   terminal job, and the request is never captured again.
6. A background invocation stores completion and prepares its typed publication
   intent. The polling thread remains free to discover cancellation/replacement.
   Publication runs only through a later outbox cycle, with fresh remote/local
   fences. A crash before preparation recovers cached completion without calling
   providers again.

## Capture refusals

One invocation runs at a time and selection takes the oldest runnable
request. A capture that failed every cycle was therefore selected again
every cycle, with only a generic `deferred` backoff, and held back every
later PR.

A bare `CaptureRefusal` from the trusted capture, returned without evidence
and before cancellation, is permanent for that snapshot
([reasons](SNAPSHOT-EVIDENCE.md#permanent-refusals)). The worker then repeats
admission's exclusion and fences: it reobserves the scope, requires access,
an open PR, the same snapshot and the same request, and calls
`Store.RefuseCapture`. That passes every `Admit` fence, requires the request's
own snapshot, marks the request admitted and stores a terminal
`capture_refused` job. Its packet is the canonical
`{"Version":"sfl-capture-refusal-1","Snapshot":…,"Reason":…}` record instead of
evidence, so it can never restore, bind a reader or claim a provider session.
The invocation ends without error, so the poll schedule is not deferred. The
next cycle publishes exactly one INCOMPLETE status naming the reason
([status writes](CONTROLLER.md#status-writes)), and selection moves on to the
next request. A refusal never counts as a review.

A refusal is bound to its request and generation, not to the PR. A new head
pauses the old request like any revision change, and a fresh request is a new
job that is captured again. The refused job then becomes `stale` and its
terminal status is neither reposted nor annotated. If the remote state changed
during the capture, the refusal is fenced and nothing is recorded.

A transient capture failure keeps the deferred backoff and the same request is
captured again once it ends. So do a wrapped or joined refusal and a refusal
with an unknown reason.

Both production source factories carry the refusal out of the acquisition
boundary. The host acquisition unit's worker and the source VM guest classify
Git's definite answers about the history they fetched
([quarantine verification](GIT-QUARANTINE.md), [capture](SNAPSHOT-EVIDENCE.md#permanent-refusals))
and export a [refusal record](GIT-MATERIALIZATION.md#capture-refusal-record)
bound to the attempt instead of source. The host accepts only the exact
canonical record for its own attempt with a known reason and no data, and the
VM run also requires the guest's finished record to measure it. The factory
returns the bare `CaptureRefusal` only after the attempt's proved stop,
generation release and the same retirement as a failed capture
([host](GIT-FACTORY.md#worker-capture-refusals),
[source VM](GIT-VM-SOURCE.md#source-vm-factory-and-archive-retirement-implemented)).
A malformed or foreign record, an unknown stop, cancellation and every other
failure remain the factory's opaque error and keep the backoff.

The reason comes from the confined worker, which parses the PR's untrusted
objects, so a compromised worker could report a refusal it never observed.
That is bounded. The record names one of three fixed reasons and binds one
attempt of one snapshot. At worst it ends that request with an INCOMPLETE
status that names the reason on the PR, visible to the owner. It cannot admit
source, mark a review clean, or touch another PR, a new head or a fresh
request, which is captured again by a new attempt. A worker that fails its
capture already denies review of that snapshot. The PR author can also cause
each reason honestly with unrelated or criss-cross history or more than 128
changed files. The record therefore grants no power beyond what the worker
and the author already had.

Candidate enumeration uses the [documented open default for GitHub's PR list](https://docs.github.com/en/rest/pulls/pulls#list-pull-requests),
with strict same-collection pagination and explicit repository-ID validation.
Listing author names or pending users cannot establish requester provenance.
`PollScope` returns the complete trigger batch for the controller's transaction;
the publisher's `Current` still commits discovery before returning. The local
HTTP tests exercise real TLS transport, pagination and hostile responses. Actual
App history consistency remains a separate qualification gate.

## Shutdown and recovery

`Close` cancels in-progress polling and signals the active invocation's
shutdown drain. Only when a provider session already holds exported auth does
the first `Close` wait, at most the ten-second provider shutdown grace or its
own deadline, for that invocation to end
([GIT-STARTUP.md](GIT-STARTUP.md#shutdown-grace)). It then cancels the
invocation, waits for it and releases controller ownership. A timeout retains
the owner lock for a later close attempt. An uncertain physical worker retains
its original SQLite lease and reports `ErrWorkerStopPending`. Expiration and
process restart never permit lease stealing; a separately qualified supervisor
must prove stopped recovery.

Known-stopped work interrupted only by service shutdown may return to `queued`
with its exact artifacts and consumed budget. A phase that completes during the
grace keeps its artifact, and the engine starts no further phase. Owner
cancellation, generation changes and uncertain stop cannot use that path.
Before restoring evidence for a retained job, the controller ends one whose
remaining session budget can no longer finish its schedule as
`incomplete_expired`, with no runtime, claim or session
([CONTROLLER.md](CONTROLLER.md)). Factory errors become durable
unavailable/blocked states. A failed qualification check cancels active work;
all admitted paths, including discovery/restoration and pre-work qualification,
share one finalization step. An otherwise current queued or cancelled job
becomes `blocked` only when no outstanding worker/lease remains.
Repairing the qualification alone does not restart it. The trusted caller must
explicitly use `ResumeUnavailable`, preserving its packet, artifacts and consumed
budget. Owner cancellation or snapshot fencing remains terminal.
A runtime-factory deadline before `RunStopped` becomes `provider_unavailable`,
with no provider session charged. It can resume only through the same explicit
recovery action; claimed execution retains its existing expiration semantics.
Authentication/provider failures are never resumed
by configuration commands or automatically repaired by this loop.

Close the controller before closing its caller-owned store. The source capture
and runtime factory must honor cancellation, and native drivers must provide the
engine's stopped-worker guarantee. This library cannot independently prove a
process or VM has stopped. The #174 VM/auth/role/egress integration supplies that
missing boundary; the synthetic VM fixture alone cannot qualify it.

## Offline proof and remaining packaging

Tests drive actual immutable Git packets through SQLite, four fake adversarial
phases, typed outbox preparation, fake publication and restart. Fault cases cover
cancel during a blocked worker, pending ACK ordering/rate limits, a late B request
cleared by A's publication, lost POST responses, persisted/cold-store backoff,
legacy state rewrites, startup/enrollment refusal, expired lease retention,
snapshot movement, a poll racing the worker's post-capture observation, shutdown timeouts/owner retention and cached-completion
recovery without inference, recoverable qualification interruption in both
factory, provider and evidence-restoration work, early admitted-job qualification
failure, owner fencing during restoration, repository lookup access loss with active and
prepared work, all-scope repository fences, recoverable pre-work factory deadlines,
joined server delays, packet restoration after
an omitted real Git blob becomes available, one status per non-complete
outcome without a repost on quota resume, a lost status response reconciled
by marker, an owner-cancelled status and annotation of a replaced job's
status, each permanent capture refusal from real Git history recorded once
with one status and no retry, transient and wrapped capture failures that
still back off, a refused head retried on a new head, a refusal fenced by a
head change during capture, and a refused older PR that no longer blocks a
later PR's admission. The loop itself is exercised with bounded cancellation.

This supplies the polling lifecycle source for #176. The
[service assembly](GIT-SERVICE.md), [trusted startup packages](GIT-STARTUP.md),
[credential owner](CREDENTIAL-OWNER.md), [source factory](GIT-FACTORY.md) and
[native runtime](NATIVE-VM.md) implement their integration. Actual installation
and account qualification, aggregate physical acquisition/recovery, trusted
promotion and explicit activation remain open. No process or daemon is
automatically enabled by these libraries.
