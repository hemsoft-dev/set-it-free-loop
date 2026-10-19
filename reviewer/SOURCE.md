# Session-bound source access

The trusted adapter gives each worker invocation a `SourceTools` capability with
only `Read` and bounded literal `Search`. It belongs to one captured session and
cannot choose a different job or object store. Each adapter forks a fresh reader
budget over the same snapshot and path allowlist. Citation validation uses a
separate reader, so its I/O cannot consume or masquerade as worker source access.

The source broker enforces the captured object revisions, path allowlist, object
modes, text/size bounds and reader budget. A tool-call cap also counts denied
requests and prevents switching operations to reset the budget. Search records
every object scanned, including nonmatches, rather than just returned matches.
Traversal and other denied reads retain sanitized coverage reasons. The broker
honors controller cancellation even if the worker supplies a background context.
It revokes the capability when execution returns, before validating model output.

`SourceAccess` is a controller-owned receipt for broker activity. It contains a
version, the exact session input hash, tool/object-call and byte counters, object
hash/path/revision records, omissions and tool-budget exhaustion. `Bytes` counts
object bytes fetched within the reader budget, including rejected binary text;
it is not wire bytes or token usage. The model's Report schema has no field that
can supply this evidence. A nil receipt means access was not recorded, rather
than a measured zero-read session. A broker receipt does not prove the official
client lacks other tools or source/host access.

Adapters return receipts through the trusted `SourceReviewer` channel. Engine
artifacts retain them separately from `ValidationReads`; checkpoint copies do
not alias them and resume checks the session hash and resource bounds. Actual
broker omissions also enter report coverage and the rendered summary, even when
the model omits them. Neither a receipt nor a source hash proves a finding true.

The durable wrapper records every broker-equipped attempt before accepting its
output, including provider errors. A random reserved attempt ID distinguishes
retries of the same phase. Successful phase artifacts retain the same receipt.
Recovery from a completed checkpoint repeats no provider work and preserves its
original source history. Failed phase evidence is also returned separately as
`Outcome.FailedSourceAccess`, without a substantive partial finding ledger.

If storage rejects the audit, `ErrSourceAuditPending` keeps the lease and attempt
metadata. A trusted controller must stop the worker, repair storage, then call
`RecordStoppedSourceAccess` with the retained job/lease/attempt identity and the
returned receipt. The write is idempotent for that exact receipt and rejects a
replacement or different attempt. Only then may it release the stopped lease.
If the original driver attested a failure before provider work, also call
`RefundStoppedBeforeWork` before release. Other uncertain work remains charged.
No provider is automatically rerun to reconstruct missing source history.
Source-attempt history is capped at 128 records per job, and the store's existing
byte/record limits also apply. Unknown state is not pruned to make capacity.

This is an in-process capability for the fake transport qualification. No official
CLI RPC/MCP mapping, separate reader OS identity, HTTP listener, real worker
launcher, credentials or activation is supplied. The production driver must map
these tools without exposing its private controller/reader objects, and prove
client/tool/filesystem/egress confinement under [issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The retired runtime remains frozen.

A test-only JSONL bridge runs real fake-worker executables for both provider
identities. Those processes request source through the broker and reject inherited
GitHub/provider/SSH/home environment canaries. That bridge is not shipped as an
official-client transport and proves no filesystem or network sandbox.

Run the offline qualification from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The fixtures measure both providers' fake worker source calls, independently
verify citation reads, reject cross-session receipts, preserve all search reads,
surface omissions, enforce cancellation/revocation and exercise durable restart
and real SQLite audit-write failure/recovery without provider replay. They do not
establish official-client eligibility or live model quality.
