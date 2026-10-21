# Git acquisition factory

The private source factory composes the existing acquisition unit, pinned
credential generation, measured runtime, authenticated bootstrap, worker and
plain export archive. Its `Capture` and `Restore` methods fit the polling
controller callbacks. The trusted service must construct and retain the factory;
this change does not install a service, import credentials or activate polling.

Construction takes exclusive lifecycle ownership of a newly opened unit owner,
export archive and prepared runtime. It validates the fixed manifest/helper and
runtime pins, then performs startup recovery before requesting any credential
generation. The composing service must call this before credential Refresh and
retain the factory until controller work has joined. Credentials remain shared
with the publisher; the source factory never exposes a raw token or refresh
capability to a worker or broker.

## Capture and grant order

Each capture spends a random 128-bit attempt nonce. Its binding fixes the
release hash, canonical runtime-manifest hash and immutable PR head/base/merge
base. A fresh private attempt directory contains separate bootstrap and broker
sockets. No failed or uncertain attempt automatically starts again.

The factory seals the fixed non-executable manifest, duplicates measured runtime
and helper handles, and retains the original sealed worker image and supervisor
pidfd from the unit owner. The bootstrap receives only the existing eight
descriptor roles. The broker has the typed reader for the one pinned credential
generation; it has no publisher, refresh, raw token or arbitrary HTTP interface.

After the broker is listening, the factory starts the fixed unit once and
authenticates the bootstrap's canonical hello against its actual original peer
pidfd. The unit owner verifies the root, manager, invocation, exact cgroup and
fixed resource properties and fsyncs the bootstrap grant before sending it.
The parent then closes its export write endpoint so it cannot manufacture a
missing EOF.

Worker readiness is a canonical packet bound to that same manifest and attempt.
Before the worker grant, the factory checks:

- The original live root and unchanged manager, invocation, unit object path,
  resource policy and independently captured cgroup.
- A retained ancestor chain from the original worker through only the measured
  namespace helper to the original unit root. Every retained ancestor maps to
  the exact unit and invocation; parent edges and executable inodes are rechecked.
- The measured worker/runtime, six private namespaces, capability/map/network
  policy and original peer identity in the existing process boundary.
- The actual fixed mount/device/private-proc layout and workspace limits in
  [mount observation](GIT-MOUNT-BOUNDARY.md).
- Actual inherited objects, current CLOEXEC bits, exact export pipe, original
  supervisor pidfd and the accepted control connection in
  [descriptor observation](GIT-DESCRIPTOR-BOUNDARY.md).

Installed-filter provenance comes from the measured fixed bootstrap and helper,
the sealed fixed filter in the authenticated bootstrap grant, and the fixed
worker launch path. `Seccomp=2` alone is not the filter's identity or a sandbox
qualification. The fixed code supplies no caller-selectable filter or argv.

Each broker request separately checks measured Git/helper ancestry and fresh
unit ownership before any GitHub dispatch. Admission is a private concrete
composition. No exported option can replace these checks with a model claim or
caller-provided boolean.

## Stop and publication

The plain export is staged and fsynced while the acquisition still owns the
credential generation. Complete framing, valid hashes and EOF do not publish it.
The immutable stop capability first revokes work and cancels the acquisition,
then joins the sole broker, all its callbacks and the acquisition method. It
next proves whole-unit physical stop with the independent unit journal. Only
the credential owner can accept that proof and release the generation lease.
The factory closes retained run assets before returning a materialization.

An early broker setup refusal with a joined zero-dispatch result has no live
broker to retain. A callback or serve goroutine that has not returned keeps
ownership. A lost start reply, populated group, failed physical stop or unknown
outcome keeps the generation, runtime and service locks for explicit recovery.
Close can retry that exact retained generation; it never replaces it with a
different credential pair or drops an unknown unit.

Before returning SnapshotEvidence, capture reconstructs patch anchors from
verified plain blobs. It shares the same packet validation used for restoring
an admitted job, without inventing a job or running host Git. Restore matches
the exact admitted packet, snapshot, release and runtime, rechecks the durable
physical-stop journal and reopens the bounded archive into sealed blobs. There
is no network refetch, host Git parser, recapture or label-based fallback.

## Restart recovery

Startup joins the newly opened cross-process-locked unit owner with the export
archive. It independently stops every recorded old acquisition, then requires
exact binding equality between journals. A staged export needs both durable
bootstrap and worker grants from a captured unit. An orphan, different binding
or archive without worker admission refuses startup source recovery.

An incomplete receiving frame can be discarded only after the concrete unit
owner proves physical stop. A canonical terminal `discarded` receipt is fsynced
before removing the bounded owned payload. Recovery can repeat after a crash;
the nonce remains spent, and discarded intake can never be reopened as source.
A later fresh nonce may stage after the incomplete attempt becomes terminal.
Staged admitted source is retained; this path does not discard a complete export.
All existing 128-record bounds remain. The factory retains its private working
directories as recovery artifacts; it does not recursively delete a replacement
or foreign directory.

## Validation and remaining gates

Offline tests use fake managers/cgroups and local synthetic credential transports.
Actual processes, pipes, original pidfds, flock, sealed files, archive fsync and
small trusted Git repositories are distinguished from those fake observations.
They cover stop-before-publication, unknown-start/stop retention, cancellation,
broker drain, unreturned broker work, setup refusal, exact live-unit rebinding,
interrupted intake retirement, orphan/missing-grant recovery refusal, startup
before credentials and retained packet/anchor restoration after deleting the
synthetic quarantine. The separate actual namespace fixtures measure mount and
descriptor subsets using an ordinary inert executable.

The complete fixed unit has not passed physical qualification on mini. Its
required systemd noswap tmpfs fails before payload execution with
`226/NAMESPACE`, and sealed inner-worker execution independently fails with
`EACCES`. No production workspace fallback, policy relaxation or fabricated
qualification was added. Successful inert namespace checks do not waive these
failures. See [launch constraints](GIT-LAUNCH-CONSTRAINTS.md).

Trusted service/release packaging and promotion, controller startup wiring,
provider public-target/account/auth/billing evidence, native-control evaluation,
rendered GitHub evidence and explicit owner authorization for live inference
and activation remain separate gates in #172, #173, #174, #176 and #177. This is
offline source implementation; it changes no App configuration or live service.
