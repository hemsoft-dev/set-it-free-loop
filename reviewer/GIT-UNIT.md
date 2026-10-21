# Git acquisition unit ownership

The private `gitUnitOwner` supplies durable process ownership for the future
acquisition factory. It uses a separate nonsecret Git binding and journal, never
a fabricated provider/auth binding. Opening it does not launch a unit or read a
provider profile. Only the explicit private start method requests a new unit.
The offline [measured runtime builder](GIT-RUNTIME.md) and namespace startup
check are available; they do not qualify aggregate acquisition. The private [worker phase](GIT-WORKER.md) now authenticates the supervisor,
checks its running sealed image, waits for a matching worker grant and performs
fetch/verify/plain export. Its process fixtures run outside bubblewrap. The [fixed worker executable/root bootstrap](GIT-BOOTSTRAP.md) also exists;
actual full-unit inheritance and launch remain unqualified. The public
production factory, aggregate grant/export
orchestration, full mount/descriptor inspection, complete helper admission and broker/result
orchestration remain unfinished.

The private [process boundary](GIT-PROCESS-BOUNDARY.md) adds actual
worker/helper runtime and executable-map checks, six private namespaces,
capabilities, route inspection and retained Git-parent ancestry. Its inert
namespace fixtures pass before and after dumpability is cleared. Exact unit,
mount/descriptor and launch/filter composition still belongs to the factory.

The private Git manager start method requires two fixed inherited descriptor
roles: sealed executable first and the original supervisor pidfd second. The
owner retains that pidfd from construction until it can close all owned work.
Closed or substituted supervisor handles fail before durable start intent.
The generic single-image manager method refuses Git descriptions. The typed
D-Bus `ExtraFileDescriptors` array is `a(hs)` and preserves the two role names.
Its intended packed slots are image FD3 and supervisor FD4. Pinned upstream
[service FD collection](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/service.c)
appends the extra-FD array after socket/store descriptors, and
[exec setup](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/exec-invoke.c)
packs the selected array and clears CLOEXEC before exec. These fresh units
request no socket activation or FD store. Physical two-FD
bootstrap qualification remains outstanding because the full fixed unit fails
before exec on mini. This source change does not qualify that path or permit
numeric-PID reopening inside the future bootstrap.

One private service directory and cross-process flock own the pinned sealed
worker image and at most 128 retained attempt records. Each fresh attempt is a
spent nonce. A nonterminal attempt blocks preparing another one; missing or
malformed records require recovery. Canonical journal validation rejects unknown,
duplicate or differently cased fields, substituted binding/image/unit identity,
invalid jobs and inconsistent state. Records are atomically replaced and fsynced.
No unit-manager call follows a failed start-intent write. A lost start reply or
job failure never causes an automatic second start.

The distinct systemd description selects fixed Git properties. In addition to
1 GiB RAM, no swap, 64 tasks, one CPU quota, a 120-second runtime, no restart and
whole-control-group kill, the typed transient wire requests:

- `TemporaryFileSystem`, signature `a(ss)`: `/tmp` with
  `size=268435456,nr_inodes=65536,noswap,nosuid,nodev,noexec,mode=0700`.
- `RestrictAddressFamilies`, signature `(bas)`: allow `AF_UNIX` and `AF_NETLINK`
  during trusted bootstrap setup. The mandatory late payload filter permits
  only `AF_UNIX` before any Git parser starts.
- `SystemCallArchitectures`, signature `as`: `native`.
- `SystemCallFilter`, signature `(bas)`: deny all three io_uring calls, with
  `SystemCallErrorNumber`, signature `i`, set to EPERM.

The [bootstrap compatibility report](GIT-BOOTSTRAP-NETWORK.md) records why a
unit-wide AF_UNIX rule prevented bubblewrap setup and why systemd
PrivateNetwork readback was insufficient on mini. The fixed sealed payload
filter also denies namespace changes, mount APIs, cross-process inspection,
keyrings, io_uring and x32/foreign ABIs. It allows ordinary Unix sockets and
process creation. The factory must enforce its installation and descriptor
handoff; these private helpers grant no source capability.

An actual inert unit attempt on mini failed with `226/NAMESPACE` before its
payload, so the fixed tmpfs request is currently incompatible there. Sealed
inner-worker execution independently failed with `EACCES`. The
[launch constraints report](GIT-LAUNCH-CONSTRAINTS.md) preserves both failures,
primary-source privilege constraints and separate reproduction commands.
No Git capability was issued by either probe. No host policy was changed.

These are requested properties, not proof that the future Git worker's mounts
or resource pressure have been qualified. The worker still needs its fixed
namespace, immutable measured runtime, inherited syscall policy and actual
kernel/mount inspection before any Git broker grant. Other payload descriptions
cannot select these properties or be mistaken for this Git journal.

The separate private `gitNetworkBoundary` retains typed network-namespace FDs
and duplicates of the original kernel pidfds for an inert inner worker and
manager. It verifies live pidfd-to-PID correspondence, boot/start ticks, NS_GET_NSTYPE
and namespace device/inode identity before and after capture. The worker must
be outside both supervisor and manager network namespaces. Each peer check
revalidates the retained identities and requires that peer's current network
namespace to match the worker's. Caller descriptor closure cannot substitute
another process. Exited workers, wrong namespace types, host peers and closed
boundaries refuse. This helper does not authenticate the supplied invocation
against systemd, prove ancestry, inspect routes or grant source access. The
future factory must compose those checks and verify the final filter before
using this observation. Actual inert bubblewrap tests independently open the
private worker's namespace from the host; self-reported IDs are not accepted.

Capture pins the manager owner, boot, invocation, actual root PID/start ticks
and retained root pidfd, sealed executable, and kernel cgroup identity/limits.
It rechecks the manager observation and waits only for the known sealed-image
bootstrap transition under the existing bounded manager deadline. It does not
infer helper, library, namespace or broker admission from root capture.

The private [descriptor grant protocol](GIT-GRANT.md) now binds both bootstrap
and worker handoffs to the captured unit. Its journal consumes each stage
before sending, and records the worker recipient. Only a completed fresh start
in the current owner instance can grant; recovery capture never restores that
authority. Its canonical hashes bind the full immutable packet. This is
private protocol plumbing, not the finished worker/factory or a source grant.

Stop permanently records revocation, resolves an uncertain start by capturing
its exact live invocation, then stops that unit and waits for its owned job.
Completion requires the captured cgroup to be empty, the original root pidfd
readable as terminated, and an unchanged manager invocation reporting no pending
job, no MainPID, and inactive/failed state. After controller restart, reopening
the root PID and checking boot/start ticks distinguishes an exited or reused PID
from the old root. A missing unknown unit alone does not prove termination. A
changed kernel boot can prove the prior boot's processes dead without contacting
another manager's unit.

An accepted start can fail before a live root is captured. The fresh start
persists a `Birth` record after the accepted start-job reply and before waiting
for that job. It binds the accepted job to the observed invocation, unit object
path and actual cgroup/hierarchy, with stable manager, boot, fixed properties
and repeated observations. A pending job must be that same accepted job. A
missing or failed birth write never arms a grant or repeats the start.

If that bound unit later fails, a separate early-stop path requires the original
manager and boot, all fixed unit properties, no pending job, no MainPID, and a
terminal state. It uses the original recorded cgroup, requires it empty, then
repeats manager and emptiness observations before persisting a distinct
`EarlyStop` receipt. Recovery never creates a missing birth record by adopting
whichever terminal unit exists then. Invocation, object path and original
cgroup identity must match the durable birth record. The terminal receipt
does not fabricate a root identity or original pidfd. It is valid only for a
revoked, stopped attempt that never captured a root or consumed either grant.

A reopened early-stop receipt rechecks its original cgroup hierarchy and exact
terminal invocation rather than recapturing a replacement group. The original
kernel hierarchy may prove that its previously captured group was removed.
Without that birth record, including a lost accepted-start reply, terminal
recovery remains uncertain even when a matching terminal unit is visible. Changed
properties, repopulation, a new invocation, manager/boot change during proof or
cancellation cannot release ownership. A real kernel reboot retains its existing
separate old-boot recovery rule. This path makes an early failure recoverable
when the required independent evidence survives; it does not qualify an export
or restore fresh grant authority.

Operation admission honors the caller's cancellation while another start/stop
is in progress. Close refuses to discard the process lock or image while an
operation is running or any record lacks a stopped receipt. Recovery calls stop
explicitly. This module proves unit termination only: the factory must revoke
and join the Git broker before releasing the credential generation or publishing
materialized source. A journal field is not a substitute for that ordering.

Tests use a fake manager/kernel adapter and actual inert child processes and
pidfds. They cover fixed D-Bus wire signatures, separate payload identity,
bootstrap transition, lost replies, changed owner/boot/invocation/image/cgroup
and limits, malformed journals, spent nonces, incomplete stop retention,
concurrent admission deadlines and cross-process ownership. One test kills an
actual controller child after a durable uncertain start; reopening refuses a
restart and recovers the separately retained inert root. No production systemd
Git unit, namespace/resource pressure qualification, actual Git worker binary,
credential, App or provider inference is exercised by these tests.
