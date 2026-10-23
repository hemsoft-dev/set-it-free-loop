# VM source implementation assessment

The [existing host acquisition path](GIT-LAUNCH-CONSTRAINTS.md) remains
unavailable on mini. Its exact bounded noswap mount and sealed worker exec
failed under the required host confinement. A successful guest probe does not
change those results or authorize a weaker host launch.

A credential-free inert probe found an alternative environment. The measured
`7.0.0-38-generic` guest created the exact 256 MiB, 65,536-inode noswap mount.
The same sealed image executed through both proc and execveat as UID 1203 with
zero capabilities in new namespaces and wrote to the work mount. Mode and
ownership were observed. The paired host receipt proves that run's unit
stopped and its original cgroup was removed. See the
[reproducible diagnostic](scripts/diagnostics/git-vm-source/README.md) and
[actual producer record](scripts/diagnostics/git-vm-source/testdata/README.md).
This establishes feasibility of those guest prerequisites only. No Git objects,
credentials, provider clients or model inference participated.

## Existing interface to preserve

`PollSource` already gives the controller four operations: capture an immutable
snapshot, restore its admitted packet, release a capture and release a job.
The source implementation owns resources until release. The retention hook is
separate. This is a real seam for host and VM adapters; the polling controller
does not need a transport selection or a new source tool for models.

The current acquisition worker already exports bounded plain materialization:
canonical snapshot evidence, a complete allowed-path/revision table and bounded
blobs with Git object hashes. The host receiver verifies the frame without Git
parsing. After qualified physical stop, that materialization serves the existing
read/search interface without keeping a Git parser alive through model sessions.
A VM implementation can reuse this export and its verification rather than
introducing live guest RPC into every model read.

## Source protocol implemented

The private `gitVMManifest` and `gitVMRequest` define purpose
`isolated-git-source-vm`, separately from provider and host acquisition manifests.
Named pins bind QEMU, kernel, initrd, BIOS, QEMU runtime, original host namespace
helper and bootstrap libraries, Git runtime, sealed worker and fixed payload
filter. The materialization binding's runtime hash is the complete source
manifest hash. Release, attempt and immutable snapshot remain independently
bound. There is no provider, auth lease, credential, URL, command or host path
selector in the request. Bootstrap library names are fixed trusted pin metadata.

`writeGitVMExport` prefixes the existing bounded plain materialization with a
canonical request-bound source header. Its Git parsing belongs inside the protected
source guest. `receiveGitVMExport` validates the header and complete
materialization, closes intake on cancellation including during the header,
and requires exact EOF. It returns only private staging. A valid header does
not authenticate a guest or prove shutdown, broker drain or source eligibility.
Neither the existing host receiver nor provider result decoder accepts this
source envelope. No existing host journal or release changes mechanism.

Tests use the actual installed Git producer against small synthetic SHA-1 and
SHA-256 repositories, then read sealed exported blobs. They reject request,
release, snapshot, purpose and pin mismatches, provider-shaped records, bare host
exports, oversized/truncated frames, unknown fields and trailing bytes. These
are protocol tests on the host, with synthetic asset pins. They establish no
guest execution, measured pin provenance, credential handling or isolation.

## Source read channel implemented

The private [Git read channel](git_vm_read_linux.go) now supplies the host and
guest transport component. Its distinct `source-vm-git-read` binding fixes the
materialization attempt, snapshot, release/runtime hashes, channel nonce and
retained credential-generation number. None of those fields establishes
admission. The host requires a fresh original-VM/channel/generation check
before each upstream exchange and after its response body closes.

Only typed advertisement and upload-pack operations for the existing fixed
repository are representable. The wire carries no token, URL, header, auth
profile or write operation. The host retains the existing reader-generation
borrow through response Read and Close, including cancellation. The guest
buffers one bounded opaque response until its terminal success record. Lost,
stale, oversized or failed replies spend the channel without returning partial
data or replaying a request.

A bounded host intake reader remains active during upstream work. Guest peer
closure cancels the host context and closes its upstream body, while ownership
remains held until Read and Close finish. One prefetched request is allowed;
further pipelining cancels the channel. Guest input intake is serialized with
exchange, so queued uploads do not allocate body buffers before their turn.
Queued cancellation closes that caller's unread body and leaves the active
exchange alone.

The component allows at most 32 exchanges, 16 MiB total decoded request bytes
and 128 MiB total response bytes per channel. Individual requests retain the
existing 4 MiB limit; responses retain the 4 MiB advertisement and 64 MiB
upload-pack limits. Reply chunks contain at most 32 KiB. The host has a
two-minute lifetime and each client exchange has a 90-second deadline covering
input intake, queueing and transport. These bounds are additional transport
limits, not permission to exceed an acquisition's existing bounds.

The [transport tests](git_vm_read_linux_test.go) use local channels and a local
TLS backend with synthetic installation credentials. They check exact bytes,
foreign/extended requests, stale admission, malformed replies, cancellation,
owned input/response drain, close errors, queue isolation and limits. They do
not prove a measured guest, helper ancestry or original host VM admission.
The transport is not yet connected to a source factory or trusted startup.


## Source VM ownership journal implemented

The private [ownership journal](git_vm_owner_journal_linux.go) retains purpose
`source-vm-unit-owner` lifecycle metadata for at most 64 source attempts. Each
record binds the complete guest request and its hash, the derived
`sfl-alpha-<attempt>.service` unit, boot ID, manager owner and host stub hash.
Lifecycle flags must appear in order: start intent, start job, original birth
(job, invocation ID, unit object path and captured cgroup), settled start,
captured kernel identity, grant hash, revocation and stop. An unborn record
stores the zero birth rather than JSON `null`, so the strict decoder accepts the
canonical bytes it wrote. Each record is bounded by the 16 KiB guest request
frame plus 4 KiB of metadata, and the file bound admits all 64 maximum-sized
records through the journal's own private reader. A retained lock excludes
other handles and the operation token serializes writes on one handle, so a
stale snapshot cannot overwrite a newer one or race `Close`. Once the lock
exists, a missing journal is refused rather than recreated empty, because that
would forget owned attempts. Foreign purposes, unknown fields, nulls, duplicate
attempts, oversized records and a replaced root also fail closed without
mutation.

The journal is metadata only. It cannot stop a unit, admit a channel or release
staging, and it holds no provider, auth binding or credential bytes. The source
supervisor must still capture and stop the original kernel instance before it
records a terminal state.

## Source VM unit owner implemented

The private [unit owner](git_vm_owner_linux.go) is the original-instance
lifecycle for source VM units. It is a third distinct payload, with description
prefix `SFL source VM acquisition ` followed by the request hash. Neither the
generic single-image start nor the host Git start accepts that prefix, and the
source VM start accepts neither of theirs. The transient unit inherits the
sealed stub image and the original supervisor pidfd, permits only `AF_UNIX`
and `AF_NETLINK` sockets (the pinned namespace helper configures loopback),
denies io_uring and is limited to 2 GiB with no swap, 96 tasks, one CPU and
180 seconds. The matching kernel capture checks those cgroup limits.

The owner stores lifecycle metadata in the ownership journal before each
manager call and never retries an uncertain start. A fresh start binds the
original invocation and cgroup while its accepted job is still attached, or
job-free only when the job tracker already proves that job removed `done` with
no other job on the unit, as for host Git units. It then waits for the job and
captures the stub's pidfd, process identity and executable.
Only that fresh start becomes grant eligible; reopening never re-arms it.

Stop durably revokes first. It terminates the captured cgroup, requires that
cgroup empty and the retained root pidfd dead, and waits for the same
invocation to settle without stopping any unit by name. A born instance that
exited before capture is proved by its unchanged birth and repeated original
cgroup emptiness. A started instance with no birth stays owned and blocks new
work until a changed boot ID proves it gone, which is recorded as `StopBoot`.
A replacement invocation, cgroup or description under the same unit name is
refused without a receipt. One live attempt at a time, at most 64 journal
records, and only stopped records can be forgotten.

The [owner tests](git_vm_owner_linux_test.go) use a fake manager and kernel
with an inert real child process. They check the exact transient-unit wire,
descriptor roles and payload separation, durable capture and whole stop, lost
start replies, reopen recovery without grant re-arming, replacement refusal,
early-exit and populated-group cases, reboot proof and journal bounds. They do
not run systemd, QEMU or the stub, and grant no source authority.

## Source VM stub, grant and run implemented

The unit runs only the fixed [source VM stub](git_vm_stub_linux.go), built from
`cmd/gitsourcevmstub`. Its arguments are the stub endpoint, the canonical guest
request JSON, that request's hash and the diagnostic supervisor PID. Before it
dials anything, it checks its inherited sealed image and original supervisor
pidfd, its own source unit cgroup and limits, and `NoNewPrivs`. It
authenticates the supervisor through that pidfd, never the numeric PID, and
sends its own identity.

The [owner grant](git_vm_grant_linux.go) is one use, for the fresh start only.
It verifies every boot input against the request's pinned manifest, then the
stub's hello, kernel peer credentials, unit and invocation for the peer pidfd,
executable image, unchanged unit and original cgroup. It records the grant hash
durably before the only send. Eight boot descriptors (QEMU, kernel, initrd,
KVM, BIOS, QEMU runtime artifact, namespace helper and the host-prepared
read-only runtime root) travel in one packet. The three channel endpoints
follow in a second packet bound to the first grant's hash. A lost send revokes
the attempt and is never replayed. The stub writes nothing to disk, discards
bounded boot diagnostics and never writes the guest control channel. It runs
QEMU through the pinned helper with `gitVMSourceQEMUArgs`, the same argument
and descriptor layout the measured guest fixture uses.

The [run](git_vm_run_linux.go) implements the existing private acquisition seam,
so the credential owner holds `run.stop` as the generation's stop capability.
`acquire` builds the guest request with the retained generation number,
prepares the attempt, prepares the read-only QEMU runtime root, starts the unit,
grants the stub and drives the guest protocol. It serves the read channel with
a check that requires the exact binding, an unrevoked run and the owner's live
original-instance recheck before each upstream exchange and after each body
closes. The export is staged through the source archive while the host measures
the actual wire bytes; the guest's finished record must match that byte count
and hash. `stop` revokes, closes the channels, joins acquisition and the read
server, then requires the owner's original-instance stop. `close` removes the
runtime root and listener only after that stop. Each refusal records a fixed,
non-secret stage name.

`TestGitVMRunProductionHostActualGit` is the opt-in production-host
qualification. It needs a measured source image and a stub built from the
exact head:

```sh
go -C reviewer build -o "$OUT/gitsourcevmstub" ./cmd/gitsourcevmstub
chmod 0500 "$OUT/gitsourcevmstub"
go -C reviewer test -c -o "$OUT/reviewer.test" .
SFL_SOURCE_VM_HOST_IMAGE="$IMAGE" SFL_SOURCE_VM_HOST_STUB="$OUT/gitsourcevmstub" \
SFL_SOURCE_VM_HOST_CASE=normal "$OUT/reviewer.test" \
  -test.run '^TestGitVMRunProductionHostActualGit$' -test.v -test.timeout 220s
```

It starts a real transient unit in the caller's user manager, runs QEMU with
KVM and serves Git from a local fixture through synthetic installation
credentials. Cases are `normal`, `upstream-fail` (pack negotiation refused),
`cancel` (cancelled mid-read) and `crash` (the controller loses its pidfds and
journal lock while the guest is held mid-read; a reopened owner must find the
original instance live, stop it from the durable capture alone, and only then
may the reader generation release). Every case requires the original unit's
durable stop receipt. No real credential, provider, App or network destination
participates.


## Source VM factory and archive retirement implemented

The [source VM factory](git_vm_source_factory_linux.go) is the `PollSource`
for this mechanism, with the same controller contract as the host factory.
Opening it requires an owner with no process-local start or grant authority,
assets that pin the factory's exact manifest, an idle credential owner and a
private run directory that neither contains nor sits inside the owner or
archive root. It then runs
startup recovery before any capture:

- Every unstopped attempt must first be proved gone by the owner, and every
  export, including an interrupted `retiring` one, must match an owner record
  before any retirement runs or resumes.
- Interrupted archive retirements are completed.
- A staged export survives only with a matching stopped owner record that also
  holds a grant hash; its packet hash comes from the retained archive record.
- Every other export, including incomplete `receiving` intake and staged bytes
  without a grant, is retired. An export without a matching owner record
  refuses recovery and leaves the archive unchanged.
- Owner records that were not retained are forgotten, and stopped attempts'
  run directories are removed, including a crashed run's read-only runtime.

`Capture` uses a fresh random attempt and channel nonce, runs the acquisition
through the generation seam, and on failure retires the intake and records
only after the run's proved stop; an unknown stop keeps everything owned and
fences the factory. `Restore` revalidates the owner's durable stop receipt
(same boot: original cgroup empty and root dead; later boot: proved by the
boot change) before reopening retained bytes, with no fetch or fallback.
If a completed acquisition's evidence cannot be built, its records are
retired at once under an uncancelled bound, or the factory is fenced for
recovery. `PruneSource` retires attempts no longer referenced by pending
captures, active jobs or controller jobs: archive bytes first, then the owner
record. Archive retirement is idempotent once complete, so an interrupted
prune finishes on retry; any failed step fences the factory until recovery.

[Archive retirement](git_vm_archive_retire_linux.go) marks a record
`retiring` durably, renames the attempt directory out of the raw archive into a
sibling `retired` directory in one step, removes it through the anchored root
and drops the record. A crash at any point resumes; a retiring attempt can
never be reopened; and only residue of a retiring record is accepted under
`retired`. Like host retirement this ends the attempt's records, and the
factory never reissues an attempt.

`TestGitVMSourceFactoryProductionHost` is the opt-in factory qualification,
with the same image and stub inputs as the run qualification. It captures and
restores through real source VM units, closes and reopens every component to
restore from retained bytes without refetching, discards a failed capture
without residue, and prunes back to empty journals.


## Remaining host composition and qualification

A source receipt must never qualify a provider worker or release a provider
auth lease. Preserve the existing host path and journals; do not reinterpret
them as VM evidence.

The source guest runs fetch, index-pack, graph/fsck/diff/tree/blob parsing under
the finite acquisition bounds. Guest root prepares exact storage and immutable
runtime, then starts a separate unprivileged source role. Models retain only
the existing read/search capability. There is no new model shell or
candidate-code execution contract.

The guest has no NIC, host filesystem share, provider profile or GitHub token.
Its fixed HTTP helper reaches only a private, job-bound host read capability.
The host credential owner keeps App signing and token generations; the read
channel retains exact repository/snapshot/request bounds, byte/call/time limits
and revocation/draining. The run binds each read to the pinned guest request
and the live original host VM invocation.

Do not accept staged materialization until the read channel is joined and fresh
whole-VM stop is proved; the run's generation stop capability enforces that
order, and the factory's recovery and restore recheck the durable receipt.
Connect the factory through explicit trusted release and startup packages. No
constructor installs credentials, promotes a build or runs work. No existing
release silently changes its acquisition mechanism.

The run qualification's standard fixture contains a symlink, a binary file and
a submodule gitlink. Through the VM their plain semantics match the host store
exactly, and the factory qualification's job reader refuses all three. The
`exhaust` case commits one incompressible 40 MiB blob, beyond the 32 MiB object
bound; the guest refuses it, the export never stages and the original unit is
proved stopped. Lost output is covered by the guest's export-close and
read-close cases and the run's `upstream-fail` case. Cross-job access has no
channel: each attempt gets fresh socket pairs, a fresh unit and VM, and a read
binding fixed to its own attempt, channel nonce and generation. The host's old
failed prerequisites remain failed even though the VM path now qualifies its
own unit.

The separate [source guest and image producer](GIT-VM-GUEST.md) implement
root-mediated Git reads and a sealed UID/GID 1203 worker. Guest completion does
not authorize host staging publication or release an auth lease.

Provider/account/paid-overflow evidence, actual comparative evaluation, final
GitHub observations, trusted promotion and separate owner activation remain the
[existing gates](../docs/research/2026-10-21-alpha-activation-checklist.md).
This implementation grants none of them.
