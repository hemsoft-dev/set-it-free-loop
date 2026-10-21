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

Later reads use those retained descriptors and recheck seals, mode, length,
SHA-256 and Git blob identity. They use no Git subprocess, zlib, pack/index,
commit or tree parser. The bounded Go packet/hunk validator reproduces included
patches, preserves original omissions and anchors, and binds the admitted job.
Each provider reader still gets its own original call/byte budget. Closing the
materialization fences later reads and releases its descriptors.

`SnapshotEvidence` now holds `ObjectStore` privately. Trusted offline fixture
helpers keep their Git-backed behavior; the eventual acquisition factory must
choose the materialized path for downloaded source. The private receiver and
restore functions are plumbing, not production stop enforcement. Sealed memfds
are process-local: durable storage/reopening of the exact admitted export and
recovery ownership remain factory work. Losing this staging handle must not
cause an automatic re-fetch or recapture of an admitted job.

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
journal and retained root pidfd. The composing acquisition factory still needs
to authenticate the export channel, join the broker and bind this unit's stop
proof before exposing materialized evidence.
