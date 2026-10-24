# Git acquisition factory

The private source factory composes the existing acquisition unit, pinned
credential generation, measured runtime, authenticated bootstrap, worker and
plain export archive. The polling controller takes it through `PollOptions.Source`
so capture, restore, consumer release and retention share one owner. Work roots
must have different opened directory identities from both journal roots; aliases
refuse before transferring ownership. Raw
Capture/Restore callbacks remain for fakes; they cannot be mixed with Source.
The trusted service must construct and retain the factory;
this change does not install a service, import credentials or activate polling.

Construction takes exclusive lifecycle ownership of a newly opened unit owner,
export archive and prepared runtime. It validates the fixed manifest/helper and
runtime pins and a shared filesystem for the unit/archive journals, then
performs startup recovery before requesting any credential
generation. The composing service must call this before credential Refresh and
retain the factory until controller work has joined. Before local handles are retained, a nil factory with an error leaves inputs
owned by its caller. After ownership transfers, a failed startup returns a
complete nonnil factory with the error. Retain that handle for explicit Recover
or Close; Capture and Restore remain fenced until recovery succeeds. Unknown
units keep their runtime and owner lock. Credentials remain shared
with the publisher; the source factory never exposes a raw token or refresh
capability to a worker or broker.

## Capture and grant order

Credential readiness is checked before allocating any durable slot or work
directory. Each admitted capture spends a random 128-bit attempt nonce. Its binding fixes the
release hash, canonical runtime-manifest hash and immutable PR head/base/merge
base. A fresh private attempt directory contains separate bootstrap and broker
sockets. No failed or uncertain attempt automatically starts again. Unix pathname bounds
are checked against each actual bootstrap/worker/broker socket path, including
the fixed nonce directory. The bootstrap endpoint must fit the unit owner's
100-byte limit; broker endpoints must fit the kernel's 107-byte limit.

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
the exact admitted packet and snapshot, rechecks the retained attempt's original
release/runtime binding across both durable journals and reopens the bounded
archive into sealed blobs. New capture assets do not replace an older attempt's
provenance. The controller qualifies the job's admitting release before Restore,
including after an upgrade; a revoked release exposes no restored source. A
canonical protected stopped receipt remains valid after systemd collects the
unit and cgroup; an unfinished stop still needs independent live proof. There
is no network refetch, host Git parser, recapture or label-based fallback.

## Worker capture refusals

A worker [refusal record](GIT-MATERIALIZATION.md#capture-refusal-record) is
staged as incomplete intake and takes the same stop path as source. The
`gitacquire` worker writes one for each
[permanent refusal](SNAPSHOT-EVIDENCE.md#permanent-refusals), including
`history_limit` when its [quarantine verification](GIT-QUARANTINE.md) finds a
definite object, commit or byte bound overrun under the default acquisition
bounds.
`runGitFactoryAcquisition` returns the bare `CaptureRefusal` only after the
stop capability proved whole-unit stop, the run closed and the credential
owner released the generation, and only with no materialization and before
cancellation. `Capture` then runs the failed-capture retirement below, which
discards the `receiving` intake with its terminal receipt. Only if that
succeeds does it return the refusal to the
[poll worker](POLLING.md#capture-refusals). A failed retirement fences the
factory and returns the opaque error. The attempt's unit record is stopped and
revoked like any failed attempt's, and `PruneSource` retires it because no job
references it. The nonce stays spent; the next capture uses a fresh one. A
record for another attempt, an unknown reason, trailing data, an unknown stop
and cancellation all stay `errGitUnit`. Every other worker failure, including a
fetch that does not complete within the unit's wall-clock, memory, task or
CPU limits, also stays `errGitUnit`; the poll loop then
[backs off that request](POLLING.md#transient-capture-failures) without
holding back other PRs.

The private `capture` method takes the acquisition constructor, so tests
replace only the worker side of a real run and keep its real stop, close and
retirement. Production passes the run itself.

## Restart recovery

Startup joins the newly opened cross-process-locked unit owner with the export
archive. It completes durable retirement intents, independently stops every
remaining recorded old acquisition, then requires
exact binding equality between journals. A staged export needs both durable
bootstrap and worker grants from a captured unit. An orphan, different binding
or archive without worker admission refuses startup source recovery.

An incomplete receiving frame can be discarded only after the concrete unit
owner proves physical stop. A canonical terminal `discarded` receipt is fsynced
before removing the bounded owned payload. Recovery can repeat after a crash;
the nonce remains spent, and discarded intake can never be reopened as source.
A later fresh nonce may stage after the incomplete attempt becomes terminal.
The same retirement runs after a failed capture in the current process, once
its physical stop and credential-generation acceptance are known. Unknown or
corrupt outcomes fence Capture/Restore until explicit Recover first drains the
retained local runs; recovery does not restart them.
Staged admitted source is retained by the separate retention rules below.
All existing 128-record bounds remain. The factory retains its private working
directories as recovery artifacts; it does not recursively delete a replacement
or foreign directory.

## Consumer lifetime and retention

Capture pins its evidence until admission or fencing has finished. Restore pins
the returned job evidence until review and publication preparation have joined.
The polling controller releases both on success and failure, using a bounded
uncanceled cleanup context. Closing the factory refuses while either consumer
remains. A caller outside polling must perform the same releases.

During ordinary polling, before work and after consumer release, retention calls
SQLite's existing Prune and reads the remaining immutable jobs. Idle polls also
retire expired source without requiring another review job. Polling skips the
source retention hook while a worker owns capture, so it does not wait for the
factory operation token or turn a healthy long acquisition into tick backoff.
Before/after-work retention and later idle polls still collect eligible source. Pending captures,
active job evidence, every retained job and every unaccepted acquisition keep their source. An
expired lease alone never permits deletion. Unused sealed caches can close
while the plain archive remains available for exact restore. Retained jobs with
an outstanding worker lease keep their cache as well.

Only unreferenced, revoked, physically stopped attempts can retire. The factory
fsyncs an intent containing the exact terminal unit receipt, export receipt and
original directory/file identities and hashes. It atomically moves the complete
unit directory into archive-owned retirement storage, then the export directory,
before deleting known files. Both journal roots must share a filesystem. This
keeps an interrupted deletion out of the unit owner's normal record scan.
The intent remains until both directories are removed and synced. Recovery
resumes it before checking ordinary unit/export correspondence. Unexpected
entries, changed identities, mismatched content or cancellation retain the
intent and fence new work. No recursive deletion or external path is accepted.

Retirement removes terminal journal slots and in-memory run/binding/cache entries,
so 128 is a simultaneous retained-record limit rather than a lifetime quota.
Fresh captures still generate their own random nonces; there is no model-facing
retry or nonce selector. Evaluation consumers must retain their capture or job
references throughout all sessions that need them, including recovery.

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
Retention tests exceed 128 stopped attempts, preserve unreleased durable leases,
active consumers and pending admission, and reopen after six modeled deletion
boundaries. A real owner process is SIGKILLed after the durable unit-directory
rename; fresh locked owners complete its interrupted retirement. Foreign file
and directory replacements remain untouched.

The complete fixed unit has not passed physical qualification on mini. Its
required systemd noswap tmpfs fails before payload execution with
`226/NAMESPACE`, and sealed inner-worker execution independently fails with
`EACCES`. No production workspace fallback, policy relaxation or fabricated
qualification was added. Successful inert namespace checks do not waive these
failures. See [launch constraints](GIT-LAUNCH-CONSTRAINTS.md).

[Service composition](GIT-SERVICE.md), [trusted startup packages](GIT-STARTUP.md),
[release decisions](RELEASE-PROMOTION.md), [native controls](NATIVE-CONTROLS.md)
and the [durable evaluation runner](EVALUATION-RUNNER.md) have source
implementations. Actual trusted promotion, provider public-target/account/auth/
billing evidence, aggregate containment/recovery and comparative evaluation,
rendered GitHub evidence and explicit owner authorization for live inference
and activation remain separate gates in #172, #173, #174, #176 and #177. This is
offline source implementation; it changes no App configuration or live service.
