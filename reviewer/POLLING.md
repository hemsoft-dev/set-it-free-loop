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
4. Reconcile uncertain/recovering writes and dispatch prepared writes through
   the publisher's existing checks. Never POST an unknown intent again. FIFO
   command acknowledgments are limited to one new ACK per cycle; remaining ACKs
   defer admission. Prune only records already eligible under retention policy.
5. If no physical invocation or unresolved durable lease exists, select the
   oldest runnable job or authorized comment/automatic request. For a new request, capture source
   and reobserve before admission. Restore the exact SQLite-returned packet
   against its immutable Git inventory before invoking the qualified runtime
   factory. Existing queued/completed jobs use restoration without recapture.
   Originally omitted patches retain their coverage and cannot gain inline
   anchors when a missing blob later becomes available. Included patches must
   still reproduce their retained blobs; unavailable evidence defers recovery. Providers run sequentially under the captured configuration.
6. A background invocation stores completion and prepares its typed publication
   intent. The polling thread remains free to discover cancellation/replacement.
   Publication runs only through a later outbox cycle, with fresh remote/local
   fences. A crash before preparation recovers cached completion without calling
   providers again.

Candidate enumeration uses the [documented open default for GitHub's PR list](https://docs.github.com/en/rest/pulls/pulls#list-pull-requests),
with strict same-collection pagination and explicit repository-ID validation.
Listing author names or pending users cannot establish requester provenance.
`PollScope` returns the complete trigger batch for the controller's transaction;
the publisher's `Current` still commits discovery before returning. The local
HTTP tests exercise real TLS transport, pagination and hostile responses. Actual
App history consistency remains a separate qualification gate.

## Shutdown and recovery

`Close` cancels in-progress polling and the active invocation, waits for them,
then releases controller ownership. A timeout retains the owner lock for a later
close attempt. An uncertain physical worker retains its original SQLite lease
and reports `ErrWorkerStopPending`. Expiration and process restart never permit
lease stealing; a separately qualified supervisor must prove stopped recovery.

Known-stopped work interrupted only by service shutdown may return to `queued`
with its exact artifacts and consumed budget. Owner cancellation, generation
changes and uncertain stop cannot use that path. Factory errors become durable
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
snapshot movement, shutdown timeouts/owner retention and cached-completion
recovery without inference, recoverable qualification interruption in both
factory, provider and evidence-restoration work, early admitted-job qualification
failure, owner fencing during restoration, repository lookup access loss with active and
prepared work, all-scope repository fences, recoverable pre-work factory deadlines,
joined server delays, and packet restoration after
an omitted real Git blob becomes available. The loop itself is exercised with bounded cancellation.

This supplies the polling lifecycle source for #176. The
[service assembly](GIT-SERVICE.md), [trusted startup packages](GIT-STARTUP.md),
[credential owner](CREDENTIAL-OWNER.md), [source factory](GIT-FACTORY.md) and
[native runtime](NATIVE-VM.md) implement their integration. Actual installation
and account qualification, aggregate physical acquisition/recovery, trusted
promotion and explicit activation remain open. No process or daemon is
automatically enabled by these libraries.
