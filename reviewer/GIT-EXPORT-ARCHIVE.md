# Retained Git exports

The private `gitExportArchive` retains the exact bounded plain export emitted by
`writeGitMaterialization`. It fixes the process-local lifetime of sealed blob
files without restoring host Git parsing or automatically repeating acquisition.
It is staging infrastructure, not source qualification or an activation command.

The real acquisition factory must still authenticate the measured producer and
its grant/export channel, revoke and join the Git broker, prove termination of
the whole acquisition unit, and bind those independent stop/eligibility records
to the admitted job before restoring or publishing retained source. An archive
record, payload hash or successful reopen supplies none of that authority.
There is no public constructor that accepts metadata or a caller's stopped flag.

## State and bounds

An existing canonical private service directory is anchored with `os.Root` and
owned for the archive's lifetime by a process flock. A second process cannot
open it concurrently. Root/lock identity, ownership and private permissions are
checked; unknown root entries or malformed record directories require recovery.
At most 128 spent attempt directories are retained. The factory must implement
qualified retention/cleanup; this archive does not discard evidence to free space.

Each `export-<32-hex-attempt>` directory contains only fixed `archive.json` and
`payload.bin` names. Attempt names never come from a worker path. The canonical
metadata binds snapshot, attempt, release/runtime hashes, packet hash, total
wire length and SHA256. Unknown, duplicate or noncanonical metadata refuses.
The wire cap is 32 MiB blob data plus the 256 KiB manifest, 64 KiB packet and
fixed framing; there is no compressed Git pack, tar extraction or arbitrary path.
The 128-attempt cap bounds retained payload to about 4 GiB plus framing/metadata.

`stage` spends and syncs the attempt directory, persists `receiving` intent,
then tees the framed stream into a new 0600 file while the existing bounded
receiver validates lengths, metadata, packet, every raw blob hash and exact EOF.
It produces sealed no-exec/CLOEXEC blob files as before. After payload 0400 mode,
file/directory sync, original file/directory identity checks and canonical record
commit succeed, the record becomes `staged`. That word describes retention only.
Returned sealed materialization belongs to the caller and must be closed.

A failed, canceled or killed intake leaves `receiving` evidence and partial bytes
for inspection. It cannot reopen or be retried; any incomplete attempt blocks
new staging, including a different nonce. Missing/uncertain source never causes
an automatic re-fetch or recapture. The whole-unit journal and qualified recovery
must decide how to resolve an incomplete attempt; no force-delete/retry API is
provided here. The archive can reopen a different complete staged record, but
only the real factory can decide whether global recovery permits its use.

## Reopen and lifetime

`reopen` checks the exact trusted binding, canonical staged record and owned
regular 0400 payload with one link. It makes one bounded Go framing/hash pass
through an opened descriptor into fresh sealed blob files. The whole-wire count
and hash plus packet hash must match the retained receipt. File/root/attempt
identity is checked again; substituted symlinks, hardlinks, writable files,
foreign directories and trailing/truncated content refuse. No host Git, zlib,
pack/index, tree or commit parser runs on the retained path.

A retained descriptor permits post-read identity checks while a separate
CLOEXEC descriptor supplies context-cooperative intake. Cancellation closes and
joins the intake callback; failure closes every newly staged blob. Later source
reads retain their existing per-job/path/revision/call/byte budgets and sealed
blob checks. `restoreGitMaterialization` compares the exact admitted packet and
snapshot; source omits and diff anchors are preserved rather than recaptured.

Archive operations serialize. Close waits for an active operation under its
caller deadline and retains ownership on timeout; an explicit close retry is
possible after intake finishes. Closing the archive does not remove files or
foreign replacements, and does not close materializations already returned to
their caller. No worker, token, credential, App or provider is started/imported.

## Tests

Small trusted actual-Git SHA1/SHA256 fixtures produce the wire, then the entire
quarantine is deleted before a new child process stages it. Parent and another
fresh child reopen it and reproduce packet, omissions, anchors and all raw
objects without a Git fallback. The helper has only explicit synthetic test
environment/path/binding inputs and runs no provider or public transport.

Fault coverage includes payload/metadata/hash/binding/state corruption,
truncation/trailing bytes, duplicate JSON, private modes, symlink/hardlink and
foreign-file/root/lock/attempt replacement, sealed-descriptor cleanup, cancellation,
Close timeout and held cross-process ownership. A real helper process is killed
with SIGKILL while the intake is blocked; its partial record is retained and
cannot qualify source or authorize a new capture.

```sh
go -C reviewer test -race -count=1 -run '^TestGitExportArchive|^TestGitMaterialization' ./
```

These are storage, process-lifetime and bounded-parser proofs. Concrete factory
integration, authenticated producer/peer policy, actual unit resource/egress
qualification, physical-stop/recovery authority, generation leases, controller
composition and live activation remain independent work. See
[GIT-MATERIALIZATION.md](GIT-MATERIALIZATION.md), [GIT-UNIT.md](GIT-UNIT.md) and
[GIT-RUNTIME.md](GIT-RUNTIME.md).
