# Private provider evidence

`AuditedReviewer` returns a model `Report`, optional independent `SourceAccess`,
and supervisor-owned `ProviderEvidence` through separate channels. Declaring
this interface requires provider evidence for successful execution. A model's
JSON cannot supply the private fields. Plain `Reviewer` and `SourceReviewer`
remain useful for offline contract fakes; their absent provider receipts mean
unknown usage, never measured zero. They are not qualified live-worker fallbacks.

`ProviderEvidence` contains the captured `EgressPolicy` and private
`ProviderAccess` counters. It contains no raw auth headers, payloads or backend
errors. The constructor and recovery share a pure policy validator. Successful
completion additionally requires a stopped, nonfailed, nonexhausted receipt,
at least one inference request, complete responses and consistent finite counts.
The captured provider, policy hash and complete input-session hash must agree.

A stopped failed receipt remains evidence. A request or response may contain one
observed overrun byte before rejection; recovery preserves that count rather than
truncating it into a successful receipt. Empty stopped receipts can document a
trusted before-work failure but cannot establish a completed review.

## Engine and durable attempts

The engine captures the private receipt and its required-evidence marker in each
artifact. It validates fresh and reused receipts and deep-copies policy slices
when checkpointing. Missing, stale or invalid required evidence cannot complete
a phase. If the configured reviewer declares audited execution, stripping both
artifact fields does not make its cached phase eligible for reuse.

`Store.RunStopped` wraps audited inner reviewers without turning ordinary fakes
into claimed audited execution. The shared durable implementation reserves its
attempt and charge before work, then persists stopped provider evidence against
the exact lease nonce, attempt ID, provider and input hash before checkpointing
or releasing the lease. Source-worker reads remain separate from provider HTTP
counts and citation-validation reads.

The durable job records which providers require audited execution. That intent
survives restarts and before-work refunds. Resuming such a provider through an
ordinary reviewer is rejected before invoking it. Missing or unstopped evidence
returns a pending error and retains the charged attempt, lease and recovery
identity. Required evidence must be recorded before `ReleaseStopped` succeeds.

`RecordStoppedProviderEvidence` is a trusted recovery write. It starts no work,
refunds no usage and releases no lease. Identical writes are idempotent; different
receipts, providers, hashes or attempt IDs are fenced. The existing private-state
and retention bounds apply, with at most 128 provider attempt records per job.

After a storage failure, the outcome retains `FailedProviderEvidence`. A trusted
supervisor must first establish physical shutdown, repair storage and record the
stopped receipt using the retained nonce and attempt ID. It can then release the
stopped lease with the appropriate incomplete state. Do not start another worker
or infer free usage from an absent response. A before-work refund is permitted
only under the existing trusted proof contract; recorded provider requests reject
that refund, and the execution path removes a contradictory before-work claim.

## Publication and recovery

`Publisher.Prepare`, the final pre-POST dispatch transaction and completion
recovery require each audited artifact to have valid completed evidence matching
a private persisted provider attempt. Durable audited-provider intent and attempt
history prevent stripped artifact fields from disguising an audited phase as an
old fake. A prepared intent remains prepared when its evidence is unavailable;
no POST occurs. Restoring trusted retained evidence can resume that intent without
re-running provider work.

The public review renderer receives the report/ledger as before. It does not
publish private policies or receipts. HTTP request counts are separate from the
engine's session ledger and are not a token-billing measurement.

## Validation and limits

Tests run four fake-provider phases through actual local HTTP intake and TLS
backends. Eight forwarded requests produce four private provider receipts, four
separate source receipts and one fake GitHub publication. Recovery reuses those
phases without extra requests. Other tests cover failed/overrun receipts,
missing and unstopped audits, stale hashes, SQLite write failure, exact-nonce
recovery, refund denial, checkpoint copies, missing persisted records, stripped
fields, pre-POST evidence loss and model JSON injection.

Run the module's vet and full race suite. Focused validation can use:

```sh
go -C reviewer test -race -count=1 -run 'TestPrivate|TestRequiredProvider|TestAudited|TestPreparedReview|TestKnownAudited|TestStoppedProviderEvidence|TestProviderAttemptRecovery|TestModelReportCannot' .
```

The [native VM host](NATIVE-VM.md) implements the private provider channel,
captured policy/launch binding, auth transfer and stopped-unit checks. The
[service assembly](GIT-SERVICE.md) selects only explicitly supplied releases
and local gates. Their source and fake-backend fixtures do not qualify aggregate
containment or activate a provider. No actual account, credential import,
App installation, inference, paid usage, deployment or production isolation
qualification is established by these fake-provider tests.
