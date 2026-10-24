# Git worker export and sealed source reads

The private worker export and controller receiver are implementation for #176.
They do not provide the protected acquisition factory or qualify a running unit.
The factory must authenticate the worker channel and measured runtime, bind the
attempt/snapshot/release, revoke the Git broker, and prove the whole unit and
broker stopped before restoring or publishing evidence. Matching export fields
alone cannot prove any of those facts. No public constructor accepts a caller's
metadata or a stopped boolean as qualification.

All Git parsing stays on the worker side: quarantine verification, complete
history, raw diff inventory, packet capture, tree lookup and blob decompression.
`writeGitMaterialization` must run inside the measured acquisition unit's CPU,
memory, process, storage and network restrictions. Direct tests use small trusted
synthetic repositories. They do not qualify aggregate resource limits.

The wire contains a fixed version marker, length-prefixed canonical JSON
manifest and packet, followed by plain blobs in the manifest's object-ID order
and exact EOF. It contains no archive paths, symlinks, Git packs or compressed
objects. Fixed ceilings are:

- 256 KiB manifest and the existing 64 KiB packet.
- 384 revision/path entries and 384 deduplicated blobs.
- 2 MiB per plain blob, 32 MiB aggregate blob data.
- The existing 128 changed paths and original packet/source/reader limits.

The manifest binds the exact snapshot, attempt, release/runtime hashes, worker
inventory, packet hash, and every admitted path at head, base and merge base.
Duplicate revisions are represented once. Missing paths, non-files and extra
reader-context omissions have explicit states. Head/merge-base file identities
must match the packet's raw diff; a directory can occupy a raw diff's absent
file path during a file/directory transition. It remains unreadable source.
Included patch blobs are reserved first, so optional reader context cannot crowd
out the data required to reproduce an admitted inline patch.

The receiver checks lengths before allocating, canonical fields, counts,
identity consistency, SHA-256 and Git's canonical `blob length\0` object hash.
It consumes exact EOF, closes intake on cancellation, joins that callback and
closes every staged descriptor on failure. Its input must be a private,
context-cooperative pipe; the factory must set a wall deadline. The writer's
private pipe must also have a bounded lifetime. The receiver never opens a
worker-provided filename. It writes each accepted blob into a CLOEXEC sealed
memfd with mode 0400 and a non-executable seal. The maximum retained raw data is
32 MiB and descriptor count is 384; these host costs also need factory admission
and concurrency accounting.

## Capture refusal record

A worker whose [quarantine verification](GIT-QUARANTINE.md) or capture reaches
a [permanent refusal](SNAPSHOT-EVIDENCE.md#permanent-refusals) exports a
refusal record instead of a materialization, before any materialization byte.
The record is the fixed `SFL-GIT-CAPTURE-REFUSAL-1\n` marker, which has the
materialization marker's length, then a big-endian length and the canonical
JSON `{"Version":1,"Binding":…,"Reason":…}` of at most 4 KiB, then EOF. The
worker writes it only for a bare, uncancelled refusal with a known reason; any
other failure exports nothing.

The receiver reads the same fixed prefix and accepts the record only if:

- It decodes strictly and re-encodes byte for byte, so no unknown, duplicate,
  missing, escaped or reordered field and no second value is accepted.
- Its version is 1 and its binding equals the receiver's exact attempt,
  release, runtime and snapshot binding.
- Its reason is `missing_history`, `ambiguous_history` or `inventory_limit`.
- Exact EOF follows before cancellation.

It then returns the bare `CaptureRefusal` and no materialization. The record
has no manifest, packet or blob field, so a reason never arrives with source
data. Trailing bytes, a manifest after the refusal marker, a refusal after the
materialization marker and every other shape are opaque materialization
errors. Tests cover each refusal through an actual fetch, verification and
export, and each malformed shape.

Later reads use those retained descriptors and recheck seals, mode, length,
SHA-256 and Git blob identity. They use no Git subprocess, zlib, pack/index,
commit or tree parser. The bounded Go packet/hunk validator reproduces included
patches, preserves original omissions and anchors, and binds the admitted job.
Each provider reader still gets its own original call/byte budget. Closing the
materialization fences later reads and releases its descriptors.

`SnapshotEvidence` now holds `ObjectStore` privately. Trusted offline fixture
helpers keep their Git-backed behavior; the [concrete factory](GIT-FACTORY.md)
selects the materialized path for downloaded source. The receiver and restore
functions alone provide no stop authority. Sealed memfds are process-local; the
[export archive](GIT-EXPORT-ARCHIVE.md) retains and reopens the exact plain export
across controller processes. The factory implements producer/stop binding and
recovery ownership, and the [service assembly](GIT-SERVICE.md) selects that source.
Complete protected-launch and aggregate physical/recovery qualification remain
unproved. Losing source must not cause an automatic
re-fetch or recapture of an admitted job.

Tests use actual Git-produced SHA-1 and SHA-256 packs, capture the packet, delete
the entire quarantine, then restore and read every retained object. They compare
bytes, coverage and anchors, and check non-executable immutable seals, per-read
caps, job fencing, concurrent close, malformed framing, binding/hash/content
mutations, partial record/EOF cancellation and descriptor disposal after late
corruption. File/directory transitions have a failing-before regression: a
literal Git pathspec also selected descendants. Capture excludes separately
inventoried descendant paths to keep each patch bound to its exact blob pair.
No credentials, live provider inference or App activation are part of this work.

[Git unit ownership](GIT-UNIT.md) now provides a separate durable start/stop
journal and retained root pidfd. The [concrete factory](GIT-FACTORY.md)
authenticates the export channel, joins the broker and binds this unit's stop
record before exposing materialized evidence. Its source and fake-owner tests
do not qualify aggregate physical shutdown or arbitrary host-crash recovery.
