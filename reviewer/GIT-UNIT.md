# Git acquisition unit ownership

The private `gitUnitOwner` supplies durable process ownership for the future
acquisition factory. It uses a separate nonsecret Git binding and journal, never
a fabricated provider/auth binding. Opening it does not launch a unit or read a
provider profile. Only the explicit private start method contacts the manager.
The public production factory, measured Git runtime, authenticated grant/export
channel, namespace inspection, concrete helper admission and broker/result
orchestration remain unfinished.

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
- `RestrictAddressFamilies`, signature `(bas)`: allow only `AF_UNIX`.
- `SystemCallArchitectures`, signature `as`: `native`.

These are requested properties, not proof that the future Git worker's mounts
or resource pressure have been qualified. The worker still needs its fixed
namespace, immutable measured runtime, inherited syscall policy and actual
kernel/mount inspection before any Git broker grant. Other payload descriptions
cannot select these properties or be mistaken for this Git journal.

Capture pins the manager owner, boot, invocation, actual root PID/start ticks
and retained root pidfd, sealed executable, and kernel cgroup identity/limits.
It rechecks the manager observation and waits only for the known sealed-image
bootstrap transition under the existing bounded manager deadline. It does not
infer helper, library, namespace or broker admission from root capture.

Stop permanently records revocation, resolves an uncertain start by capturing
its exact live invocation, then stops that unit and waits for its owned job.
Completion requires the captured cgroup to be empty, the original root pidfd
readable as terminated, and an unchanged manager invocation reporting no pending
job, no MainPID, and inactive/failed state. After controller restart, reopening
the root PID and checking boot/start ticks distinguishes an exited or reused PID
from the old root. A missing unknown unit alone does not prove termination. A
changed kernel boot can prove the prior boot's processes dead without contacting
another manager's unit.

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
