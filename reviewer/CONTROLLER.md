# Offline durable controller

The explicit [selected-installation credential boundary](GITHUB-INSTALLATION.md)
qualifies separate repository reader and publisher transports using an injected
App JWT and transport. Its tests use fake TLS only. It does not supply key
storage, runtime activation or live installation evidence.

This module supplies SQLite state, configuration and request admission, durable
phase reservations, and a COMMENT review outbox through fake transport seams.
The explicit [GitHub publication HTTP adapter](GITHUB-PUBLICATION.md) supplies
COMMENT/ACK writes and whole-document reconciliation. The selected [trigger
reader](GITHUB-TRIGGERS.md) collects authorized `@sfl review` comments, automatic
PR-open/head observations, owner commands and live base revisions through an
explicit read transport. It commits
complete discovery before returning Current so cancellation reaches the local
dispatch fence. [Snapshot evidence](SNAPSHOT-EVIDENCE.md) collects a bounded
merge-base diff from trusted immutable Git objects, verifies inline anchors
against their blobs and retains authoritative omissions through review/recovery.
The [private polling lifecycle](POLLING.md) composes these libraries while a
stopped-worker invocation runs independently. Actual App consistency remains
unqualified. There is no installed daemon, production launcher, credential
acquisition, installation flow or activation command. The retired SFL runtime
remains frozen. Live acceptance is tracked in [#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173),
[#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174) and
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176).

`OpenStore` requires a private existing directory, a 0600 database and exact
repository, installation, App, bot and owner IDs. SQLite WAL, FULL synchronous
writes and IMMEDIATE transactions serialize mutations across handles. State has
finite record and byte limits. Reaching a cap stops admission; accepted commands,
unknown writes and unresolved native history are never discarded to hide it.
This deliberately small alpha stores one job-state JSON record transactionally,
with a separately versioned polling schedule row so older state writers cannot
erase backoff.
A schema migration is required to change enrollment or trusted defaults.

A qualified transport normalizes complete paginated original commands and
comment/automatic provenance into `Discovery`. The [trigger contract](GITHUB-TRIGGERS.md)
describes the policy epoch, atomic cursor, original comment IDs and observed head
generations. Native-specific lifecycle details below remain legacy test coverage. Creation timestamps are
platform timestamps. Partial history defers admission. Command discovery sorts
by original creation time and ID. Accepted IDs retain their original body hash;
later edits cannot change them. Late events older than a trusted completed
cutoff defer reconciliation. Configurations form a FIFO acknowledgment chain.
Only a confirmed ACK's platform timestamp makes its version effective. Requests
before it keep the preceding version; equal timestamps defer. Pending ACKs block
admission. Configuring does not invoke a provider. Cancellation advances the
generation immediately and requires a fresh authorized comment or subsequent head observation for replacement work.

`Admit` captures the job's immutable snapshot, release and config. Only owner
requests for the exact bot can admit. Duplicate events reuse the captured job;
retained admission tombstones prevent an old native request from rerunning after
result retention. The selected adapter creates automatic request identities for newly observed heads, including after completion. Base-only movement fences unpublished work without starting a new request. `RunStopped` wraps
Engine with `DurableReviewer`, reserving each session before work and storing
validated output before returning. A crash or uncertain failure consumes its
reservation. Only a trusted before-work failure refunds a session. Resume uses
the persisted checkpoint and exact input hashes. Daily counts are charged when
sessions begin, including jobs queued on earlier dates.

Only one lease may exist. Expiration does not authorize stealing it. A separately
qualified supervisor must prove the worker has stopped before `ReleaseStopped`.
Drivers supplied to `RunStopped` must return only after worker termination.

Broker-equipped drivers also return trusted per-attempt source receipts, kept
separately from citation-validation reads. Source attempts, including failures,
are persisted under the reserved attempt ID before output is accepted. Audit
storage failure keeps the lease for explicit stopped-worker recovery; see
[SOURCE.md](SOURCE.md). A fake in-process transport does not qualify the official
worker's separate identity or filesystem/network boundary.
The [worker transport](WORKER.md) now requires explicit supervisor confirmation
after waiting or cancelling a wrapper process. `ErrWorkerStopPending` retains
the lease and exact attempt for recovery, and prevents audit writes/refunds that
require a stopped worker. The library cannot independently prove the supervisor's
OS containment or termination attestation. Source implementation can continue
with fakes before actual official-worker qualification. Generation/lease checks
fence old workers from saving evidence after cancellation or replacement.

`Publisher.Prepare` accepts a persisted complete outcome and renders its bounded
document. Known controller secrets are redacted across summary and inline bodies
before hashing/persistence. No raw transcript is retained. Remote adapters must
compute normalized document hashes from actual comments, including inline bodies,
and verify immutable App author identity. They must submit advisory COMMENT
reviews against the captured commit, never approvals or change requests.

Intent is persisted before POST. A lost response becomes unknown. Reconciliation
requires complete pagination and exactly one matching App author, unique marker
and whole-document hash. Empty or delayed scans never authorize blind retry.
ACKs and statuses use the same recovery boundary. A transport returning before a remote write
is visible does not prove no write happened.

Before dispatch and after completion, publication checks access, open state,
head/base and local request generation. GitHub cannot atomically compare and
publish. Each initial document names its revisions; changed state pauses local
admission and annotates supersession remotely when access permits. If publishing
A clears the native pending state for B, B remains unresolved locally and paused
for reconciliation. A's receipt never fulfills B. Annotation failure remains an
error, with local evidence retained.

Run offline validation from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The failure fixtures cover uncertain ACK, review and status writes, restart, incomplete
pagination, spoofed receipts, reversed discovery, configuration timestamps,
concurrent SQLite handles, crash session accounting, lease expiration, stale
base, cancel fencing and the adverse A/B outcome. These are simulated transport
results. They do not establish subscription support, official-client confinement,
selected App/trigger qualification, actual GitHub rendering or a successful live alpha.

Complete `Discover` batches commit commands, native events and the cutoff in one
SQLite transaction. A newly discovered latest request that is already removed
still fences older work. After publication, the transport must retrieve complete
native history through at least the receipt timestamp, including requests that
appeared and disappeared between polls. A currently empty reviewer list alone
cannot prove there was no B request. A fresh unambiguous owner request can clear
a reconciled pause after pending writes/checks finish.

Accepted review receipts enter `postcheck_pending`; transient reads retain that
state for `Reconcile`. Supersession annotations use `annotation_pending` and an
idempotent update of the known review ID. They never append a new review/comment.
A trusted before-work failure refunds both the session and any day charge first
introduced by that attempt. A resumed job hitting the daily cap remains in
`quota_wait`. A job not yet started whose claim meets the daily cap enters
`quota_wait` without a lease, session or day charge. Final secret redaction
covers raw and rendered encodings.

A crash after the final phase and before outbox preparation can recover through
`RecoverCompleted`. It reconstructs the deterministic outcome using cached
validated reports, supplies no provider workers and repeats immutable source
validation. Publication compares the proposed ledger with those durable reports;
a caller cannot replace allegations while retaining genuine phase receipts.

Authentication failure stays `auth_failed` in durable state. After a qualified
external repair, `ResumeUnavailable` can requeue the same still-current native
request without changing its snapshot, config, release, artifacts or consumed
budget. It does no inference, is never called by configuration comments, and
rejects expired/exhausted, cancelled, superseded or paused jobs. Unknown provider
work remains charged; this recovery cannot create additional session capacity.

A lease claim returns its captured job/checkpoint from the claiming transaction.
There is no fallible post-commit read that can hide the committed lease nonce.
`RunStopped` uses that atomic result directly. Superseded/cancelled generations
retire their jobs while preserving any live lease for supervisor-confirmed stop.
A prepared review caught by a local fence becomes stale without POST, so safe
terminal jobs/intents can be pruned rather than permanently consuming queue slots.

An accessible closed PR also retires a prepared review without POST. Lost access
continues to defer. Terminal invalid-output jobs become eligible for retention
pruning; unknown writes and live leases still prevent deletion.

Before-work refunds retain the outstanding phase and any newly introduced day
charge in SQLite. They match the exact lease nonce and phase, so an expired lease
does not prevent refunding a worker that is known to have stopped before work.
If the refund write fails, `ErrRefundPending` preserves both errors and
`RunStopped` keeps the lease. After storage repair, a trusted controller with
proof of the stopped pre-work failure calls `RefundStoppedBeforeWork` using that
nonce/provider/round, then `ReleaseStopped` with the original failure state.
Never release or start a replacement worker while the refund remains unresolved.
Duplicate refunds and a different phase are rejected. Unknown provider work is
still charged. This controller component supplies no supervisor or automatic repair. The
[native VM assembly](NATIVE-VM.md) and [service](GIT-SERVICE.md) compose explicit
workers; their complete physical qualification remains open.

## Status writes

`Publisher.PrepareStatuses` gives a non-complete job a `status` write: an
advisory COMMENT review on its captured head with no inline comment. The
published states and their leading words are:

| Job state | Status |
| --- | --- |
| `quota_wait` | INCOMPLETE: daily quota exhausted; resumes by itself until the configured quota wait expires |
| `auth_failed` | INCOMPLETE: provider authentication unavailable; stopped until an operator repairs it and resumes the job |
| `provider_unavailable` | INCOMPLETE: a selected provider is unavailable; stopped until an operator resumes the job |
| `invalid_output` | INCOMPLETE: provider output failed validation; ended without a review |
| `incomplete_expired` | INCOMPLETE: expired before every required phase completed |
| `cancelled` | CANCELLED: the request was cancelled before the job completed |

The text is fixed controller wording keyed by the durable state and the
captured configuration. It passes through `safeText` and known-secret
redaction like every document, names the reviewed revisions and the completed
phase count, and never includes a finding. `queued` and `running` are
transient. `blocked` is the operator qualification gate, not a review
outcome, and publishes nothing.

Each job publishes at most one status per state. A newly admitted job that
meets the daily review limit at its first claim enters `quota_wait` there, so
it publishes the same status before any phase runs. A `quota_wait` job is
reclaimed every cycle and returns to `quota_wait` without a second status,
including after quota frees and the job waits again mid-run. A job that later
expires publishes its `incomplete_expired` status once. A
status is prepared only while its job passes the review preparation fences:
no lease or outstanding attempt, not paused, and its own request and
generation still current. The one exception is cancellation: when the owner
cancels, or the latest request is removed, before that request's job
completes, its `cancelled` status is bound to the cancelling generation,
which has no current request. A job whose review was published or is in
flight gets no cancellation status. An older request's job gets none.

Before its POST, a status must pass the review's checks: access (lost access
defers), open PR, the job's exact head and base, and the status's own owner
request for this bot. Only a cancellation status accepts no pending request,
and none accepts a different one. Locally, the PR must not be paused, its
generation must be the status's, and the job must still be unleased in the
reported state. A status failing any check before its POST becomes `stale`
without a POST and changes neither the PR nor the job. After the POST, the
review postcheck runs unchanged, with the cancellation exception for the
local request. A status fenced during its POST is handled like a review: the
PR pauses, a later request B stays unresolved and the status is annotated
superseded. A status never marks, admits or fulfills a request, and never
counts as a review for `selectWork`.

A superseded job is never current, so it gets no fresh status. Instead, a
confirmed `quota_wait`, `auth_failed` or `provider_unavailable` status, which
promises later work, is marked `annotation_pending` once its job becomes
`stale` or `cancelled`, unless a later review or status of that job already
replaced it. `Reconcile` then idempotently annotates the known status review
in place with the fixed supersession marker while access permits. It never
appends a comment. Terminal statuses, like reviews, stay as records of the
revisions they name.
