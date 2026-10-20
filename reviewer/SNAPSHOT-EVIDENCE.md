# Immutable snapshot evidence

`CaptureSnapshotEvidence` prepares a bounded evidence packet from an explicitly
supplied trusted `GitStore` and admitted snapshot. It reads committed objects;
it never checks out candidate code, fetches history, loads a provider profile or
starts a reviewer. Repository and fork provenance still come from qualified
discovery and object acquisition. A local object with a matching hash does not
attest installation permissions or which repository supplied it.

Capture requires all three revisions to be commits and the captured merge base
to be the unique merge base of the base tip and head. Missing or ambiguous
history defers the entire capture. The comparison is merge-base to head, even
when an independently advanced base tip has the same tree as head.

Git produces the NUL-delimited raw inventory with full object IDs and renames
disabled. A rename appears as deletion and addition. Modes, status, object IDs,
paths and cardinality are validated. Unsafe paths or incomplete inventories
defer capture. Both SHA-1 and SHA-256 object formats are supported. Packet data
includes every changed path, mode, old/new object identity and status within the
128-file cap. No truncated inventory is admitted.

Regular text blobs and patches share finite bounds. Symlinks, submodules,
binaries, non-UTF-8 objects, unavailable blobs and source/patch size omissions
retain explicit path-specific coverage. Missing commits are not treated as
ordinary blob omissions. There is a two-MiB aggregate blob read budget, a two-MiB
per-command output cap, a 64-KiB JSON packet cap and a one-minute overall capture
deadline. A complete inventory and its coverage must fit; otherwise capture
fails. Patch omission can leave room for later small files. An omitted patch
never supplies inline anchors.

Fixed Git flags disable external diff drivers, text conversion, rename
detection, color, replacement objects and lazy fetch. Git receives no inherited
user environment or global/system configuration. The explicitly selected Git
binary and object repository configuration remain trusted. Their provisioning
must stay outside candidate source. No shell is used by capture.

Each hunk's context, additions, deletions, counts, coordinates, unchanged gaps
and EOF newline markers are checked against the actual old/new blobs. Metadata
and function labels do not select paths or lines. Only verified right-side
context/addition lines become inline anchors. Source-valid findings elsewhere
remain summary findings under the existing renderer.

## Bind the admitted packet

Use the job returned by `Admit`, including on an idempotent retry:

```go
capture, err := CaptureSnapshotEvidence(ctx, trustedGit, snapshot)
// Handle err before admission.
job, err := store.Admit(ctx, scope, requestID, release, capture.Packet(), now)
// Handle err before binding.
bound, err := capture.Bind(job)
// Handle err before providing a reader or publication anchors.
engine.Reader = bound.Reader
// After stopped-worker execution:
intent, err := publisher.Prepare(ctx, outcome, bound.DiffLines())
```

`Bind` requires the exact admitted snapshot and packet plus a valid job. If a
retry returns a previously stored packet and a fresh capture differs, binding
fails. Recovery must reproduce the original captured packet or retain explicit
uncertainty; it cannot silently replace its omissions or source input. A bound
reader uses the job's configured limits and rejects a different snapshot.
Accessors return copies of paths, coverage and inline maps.

Supply an independently budgeted bound reader to each adapter as well as the
engine's citation-verification factory. Initial controller coverage is retained
when the in-process source broker forks the adapter's reader. It consumes no
model read budget. Citation validation and the engine's existing authoritative
coverage merge preserve these omissions even if the model reports none. The
managed source role still requires its separate identity/containment boundary;
this collector does not qualify that boundary.

## Offline evidence and remaining runtime work

Tests create real commits and exercise the installed Git producer, including
both object formats, quoted/Unicode paths, deletion, no-newline text, renames,
symlinks, submodules and binaries. Working-tree changes, hooks, configured
external/textconv drivers and inherited Git environment canaries cannot alter
the packet or execute during capture. A local inert promisor trap first proves
that missing objects would trigger fetching without the controls, then proves
capture records an omission without invoking that trap. No network is used.

Hostile parser fixtures reject forged source, coordinates, counts, hidden edits,
missing newline markers, duplicate paths and partial inventories. The durable
integration test admits this actual Git packet into SQLite, runs four fake
provider phases, recovers completed work without another provider call and
renders authoritative omissions despite empty model coverage declarations.

This is a library prerequisite for #176. Private polling/backoff, qualified
object acquisition, token attestation, runtime composition and packaging remain
required. It does not install an App, enable Actions, activate the retired
controller or qualify live subscription use.
