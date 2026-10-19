# Offline durable controller

This module supplies SQLite state, configuration and request admission, durable
phase reservations, and a COMMENT review outbox through fake transport seams.
It does not contain an App HTTP transport, polling daemon, production launcher,
credentials, installation flow or activation command. The retired SFL runtime
remains frozen. Live acceptance is tracked in [#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173),
[#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174) and
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176).

`OpenStore` requires a private existing directory, a 0600 database and exact
repository, installation, App, bot and owner IDs. SQLite WAL, FULL synchronous
writes and IMMEDIATE transactions serialize mutations across handles. State has
finite record and byte limits. Reaching a cap stops admission; accepted commands,
unknown writes and unresolved native history are never discarded to hide it.
This deliberately small alpha stores one versioned JSON record transactionally.
A schema migration is required to change enrollment or trusted defaults.

A qualified transport normalizes complete paginated original commands and native
request/removal/re-request history into `Discovery`. Creation timestamps are
platform timestamps. Partial history defers admission. Command discovery sorts
by original creation time and ID. Accepted IDs retain their original body hash;
later edits cannot change them. Late events older than a trusted completed
cutoff defer reconciliation. Configurations form a FIFO acknowledgment chain.
Only a confirmed ACK's platform timestamp makes its version effective. Requests
before it keep the preceding version; equal timestamps defer. Pending ACKs block
admission. Configuring does not invoke a provider. Cancellation advances the
generation immediately and requires a new native request for replacement work.

`Admit` captures the job's immutable snapshot, release and config. Only owner
requests for the exact bot can admit. Duplicate events reuse the captured job;
retained admission tombstones prevent an old native request from rerunning after
result retention. Pushes never create request identities. `RunStopped` wraps
Engine with `DurableReviewer`, reserving each session before work and storing
validated output before returning. A crash or uncertain failure consumes its
reservation. Only a trusted before-work failure refunds a session. Resume uses
the persisted checkpoint and exact input hashes. Daily counts are charged when
sessions begin, including jobs queued on earlier dates.

Only one lease may exist. Expiration does not authorize stealing it. A separately
qualified supervisor must prove the worker has stopped before `ReleaseStopped`.
Drivers supplied to `RunStopped` must return only after worker termination.
The library cannot prove process termination or external OS isolation. Those
remain live qualification gates. Generation/lease checks fence old workers from
saving evidence after cancellation or replacement.

`Publisher.Prepare` accepts a persisted complete outcome and renders its bounded
document. Known controller secrets are redacted across summary and inline bodies
before hashing/persistence. No raw transcript is retained. Remote adapters must
compute normalized document hashes from actual comments, including inline bodies,
and verify immutable App author identity. They must submit advisory COMMENT
reviews against the captured commit, never approvals or change requests.

Intent is persisted before POST. A lost response becomes unknown. Reconciliation
requires complete pagination and exactly one matching App author, unique marker
and whole-document hash. Empty or delayed scans never authorize blind retry.
ACKs use the same recovery boundary. A transport returning before a remote write
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

The failure fixtures cover uncertain ACK and review writes, restart, incomplete
pagination, spoofed receipts, reversed discovery, configuration timestamps,
concurrent SQLite handles, crash session accounting, lease expiration, stale
base, cancel fencing and the adverse A/B outcome. These are simulated transport
results. They do not establish subscription support, official-client confinement,
App picker eligibility, actual GitHub rendering or a successful live alpha.

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
`quota_wait`. Final secret redaction covers raw and rendered encodings.
