# Private source VM export archive

`gitVMExportArchive` retains source VM request intent and bounded plain source
exports across process loss. It supplies private staging only. It cannot launch
or stop a VM, drain a credential generation, release provider auth, publish a
source capability or qualify a release. The source supervisor and controller
composition remain required by the [source VM assessment](GIT-VM-SOURCE.md).

## Purpose and directory ownership

The trusted caller supplies an existing private, canonical directory. The
opener inspects its contents and canonical `source-vm-export-archive` purpose
before creating a lock. It refuses host archives, provider journals, unknown
entries and another purpose. Locks and inner storage are opened through retained
directory handles; replacement paths cannot redirect those writes. Every
operation checks the original directory and lock identities again.

A lifetime flock excludes other archive owners and a cancellable operation token
serializes intake, reopen and close. The `refresh.lock` filename and private
atomic-file helpers are shared filesystem plumbing. No provider, auth profile,
AuthBinding, credential or auth journal is stored or interpreted here.

The outer `source-vm.json` journal binds each attempt to the complete source
request hash, materialization binding and staging state. The inner
`materialization` directory reuses only the existing plain frame/blob archive.
Host unit, retirement and provider lifecycle records are not accepted in this
container. Existing host archives and journals keep their original meaning.
There are at most 64 retained attempts, each subject to the existing frame,
manifest, packet and blob limits. Capacity exhaustion refuses new staging;
there is no automatic eviction, nonce reuse or deletion of unknown work.

## Intake and restart

Intake fsyncs a `receiving` request record before reading even its source header.
It validates the canonical source-purpose header against the full trusted
request, then streams the body through the existing bounded materialization
validator and durable archive. Only a complete, validated and persisted frame
receives a `staged` acknowledgement. Cancellation closes intake, including a
stalled header, and joins its cancellation callback. Failures retain ownership.
A receiving attempt prevents new staging and cannot be retried or refetched.

Reopen requires the exact original request. It checks retained hashes/framing
and reconstructs fresh sealed blobs without Git parsing, network access or
acquisition replay. If the inner archive committed but the outer acknowledgement
was lost, reopen can repair that staging acknowledgement after validating the
retained payload. Missing or incomplete bytes remain unavailable.

Neither `staged` nor a successful reopen proves guest admission, broker drain,
physical stop or publication eligibility. The future source owner must retain
its independent original-instance launch, channel/generation and stop records
and establish those facts before exposing any materialization to a controller.
This module has no retirement API or default policy for clearing an unknown
attempt. Those require the qualified lifecycle owner rather than a serialized
boolean or a caller-supplied stop callback.

## Validation boundary

The tests use actual installed Git exports of small synthetic repositories,
with synthetic source-manifest asset pins. They check exact restored contents,
new-process recovery after exit without Close, a lost outer acknowledgement,
truncation/trailing-byte refusal, request mismatch and duplicate-attempt refusal,
header cancellation, close exclusion, flock contention, payload/purpose tampering,
root replacement and refusal that leaves a host archive reopenable.

These are offline filesystem/protocol observations. No source VM, provider
client, model inference, live credential, App operation or activation participates.
Complete source-VM recovery, bounded storage qualification and trusted startup
remain unfinished. The separate [activation gates](../docs/research/2026-10-21-alpha-activation-checklist.md)
remain unchanged.
