# Systemd containment supervisor

`SystemdSupervisor` connects the durable launch gate to mini's Linux user
manager. Its only payload is the pinned `cmd/unitstub` fake program. The
review engine does not instantiate it or select it as a provider fallback.
No native client, VM, provider auth profile or GitHub transport is launched.

## Launch and grant

`PrepareFake` persists the session/launch/containment binding, manifest digest,
boot ID, manager identity and executable digest in the launch's private unit
journal. `StartFake` records start intent before its one `StartTransientUnit`
call. It never retries an old start. Unknown replies retain that intent.

The D-Bus adapter connects to the existing fixed user-bus socket and uses only
EXTERNAL UID authentication. It does not read cookie files or start a bus.
It captures the bus ID and systemd's unique owner, then targets that owner.
JobRemoved subscription precedes start. Only a pending owned unit or its bound
job path can enter the bounded result cache. Unrelated job traffic is ignored.
Early signals are retained until the method reply binds the exact job; consumed
or superseded results are removed. Another job removed on a tracked unit marks
it foreign; this refuses only the settled-job birth correlation, never WaitJob.
Recovery termination does not depend on consuming an abandoned start result.
Malformed, failed or unknown results cannot become successful launch evidence.
Recovery takes one unit reference to prevent GC during stop observation.
Concurrent and repeated polls reuse its independent, bounded acquisition lifetime;
each caller, including its creator, keeps its own cancellation deadline. Manager
close cancels and joins these metadata requests. Unknown acquisition replies stay
cached without retry.

Unit properties use a fixed argv array and `Type=exec`, `Restart=no`,
`ExitType=cgroup`, `RemainAfterExit=no`, `CollectMode=inactive-or-failed`,
`AddRef=true`, control-group killing, SIGKILL escalation and no-new-privileges. Memory is 1 GiB, swap zero,
tasks 64, CPU one core, runtime 120 seconds and stop timeout five seconds.
Protocol operations have a ten-second local context ceiling and honor earlier
caller deadlines. No model chooses these properties or the executable.

The supplied executable path is copied into a verified, sealed executable memfd.
Systemd receives that exact image through ExtraFileDescriptors. A private blob
named by its digest permits recovery after the original pathname is replaced;
it is never executed by pathname. The cache must be retained while its bindings
can still require recovery. The live process's `/proc/PID/exe` must have the
required seals and expected digest before grant. A child self-exec uses
`/proc/self/exe`, preserving the same image.

Systemd resolves ExecStart before packing extra FDs into their final positions.
A fixed root-owned OS Python interpreter performs the subsequent `execve(3)`.
The bootstrap uses isolated/no-site mode and constant code, treats the four
stub arguments only as argv data, unsets loader-injection variables, and gives
the payload only INVOCATION_ID. This root-owned interpreter is part of the OS
trust boundary, along with systemd; its path and ancestors must be root-owned
and not writable by other users. No shell or user Python configuration is used.
The supervisor waits for the sealed image to replace that bootstrap before
capturing the launch. Closing fences new starts and drains senders before the
image descriptor is released. Preparation is registered under the same shutdown fence.
A preparation that finishes after Close begins revokes its new listener before
the drain completes and returns an error.

Before grant, the supervisor verifies the completed start, unit description,
invocation, actual main PID and typed resource properties. It passes a live
pidfd to systemd's GetUnitByPIDFD and checks boot ID and process start ticks.
It independently opens the actual cgroup-v2 directory, captures its inode and
checks memory, swap, tasks and CPU limits in the kernel. The binding is durable
before the grant. These checks and the sealed executable evidence are repeated before
`ContainmentGate.GrantFake` authenticates the connected peer.

## Stop and auth recovery

`AbortAndWait` durably revokes the gate first. It drains any in-flight start
call before inspecting the unit. A call timeout does not prove an asynchronous
start failed. A missing unit alone cannot release an auth lease.

Post-start journal writes may outlive caller cancellation to retain an accepted
launch, but use a separate one-second recovery deadline. A held flock cannot
make those writes wait forever.

Stopping requires the captured manager, invocation and cgroup identity. After
permanent revocation, the supervisor reopens the original cgroup-v2 hierarchy,
checks its captured directory inode, and opens `cgroup.kill` relative to that
original directory descriptor. The kernel recursively terminates that instance.
A replacement between observation and action cannot redirect the descriptor.
No `StopUnit` or other name-targeted stop API remains in the manager adapter.

A successful write is only a termination request. systemd can clear its
ControlGroup property after successful exit. The retained invocation must still
match, and a fresh kernel check must prove the captured original group empty.
Within a finite deadline, the supervisor independently checks that the original cgroup has no populated
subtree, then checks the unit's original identity, no job and no main PID before
recording stop. Revoked recovery may accept a previously captured legacy
`RemainAfterExit=yes`/`CollectMode=inactive` unit after physical shutdown; that
pair can never qualify a fresh grant. This does not deactivate a legacy name.
Changed identity, permissions, hierarchy or uncertain kernel/manager observations keep recovery
required. Historical `StopJob` journal fields remain readable but are never
replayed. It never kills a numeric PID or a reused cgroup path.

Only after the stopped journal is durable does `RetireUnit` remove the original
start's local job tracking and drop this authenticated connection's counted unit
references, including the creation-time AddRef. Other clients' references remain
untouched. It creates no manager job and does not stop/reset a name. Repeated calls reuse the same
bounded release result, including unknown replies, and cannot acquire a new
reference for the spent unit.

`ExitType=cgroup` keeps an original escaped child within the active service
lifecycle when its main process exits. With `RemainAfterExit=no`, new units can
be collected after normal or failed whole-group exit. A closed connection has
already released its references. Recovery of a durable stopped record rechecks original kernel evidence, without requiring
metadata that has legitimately been collected. A changed host boot can prove the
old kernel processes dead after permanent grant revocation. The journal also binds
the cgroup-v2 hierarchy root device/inode and mount/cgroup namespace identities.
A missing child counts as removed only after reopening that same verified
hierarchy. An absent mount, ordinary directory, changed hierarchy or namespace
keeps recovery required.

`ConfirmAuthStopped` reopens the exact durable binding and repeats the necessary
stop checks. A controller restart cannot regrant an old launch. It implements
AuthStopVerifier only for this pinned fake payload, which creates no profile
mounts. An unresolved start without a captured or recoverable live identity
remains recovery-required; an absent service is insufficient evidence. The
current implementation deliberately retains that uncertainty.

## Fake payload and evidence

Before grant, unitstub opens no profile and starts no child. After grant it
hashes the synthetic marker, then executes only its own fixed inert child mode.
That child creates a new session, ignores TERM, writes a private readiness
receipt and sleeps. This is a bounded fixture for proving whole-cgroup SIGKILL
escalation, never an arbitrary-command API or a provider worker.

Ordinary tests cover the real adapter against a private dbus-daemon and exported
fake systemd manager, with real D-Bus variants, pidfd transfer and early job
signals. Separate fake-manager tests run an actual child through the gate and
complete a synthetic auth lease after reopening the supervisor. They reject
unknown starts, missing units, undrained starts, changed manager/invocation/
cgroup/resource/executable identities and populated containment.

The explicitly invoked `cmd/systemdfixture` uses the existing host manager,
fresh private gate/auth directories and synthetic bytes. It checks actual
atomic replacement of the original executable path, start/grant through the
retained image, kernel limits, a live escaped child, physical stop, restart recovery
and synthetic auth completion. It also requires the stopped unit's metadata to
be collected while the original manager connection is still open, without a
name-targeted stop/reset. Any fixture-only fallback cleanup keeps the
qualification failed, even after the main exercise tentatively passed. Deferred
close failure also fails the receipt, including the reopened supervisor. The
reopened handle is closed before the tentative pass. Fallback cleanup retries the same bound supervisor proof with a fresh
ten-second deadline after the first stop context ends; it never stops a unit by name. The final journal
must be retained with its exact binding and captured/aborted/stopped evidence.
A missing, malformed or mismatched journal fails qualification. Receipt-write
failure also fails the command. An earlier exercise error is preserved.
Normal tests never start a host unit.

From `reviewer/`, the opt-in model-free test is:

```sh
SFL_TEST_ORIGINAL_CGROUP=1 go test -run '^TestContainmentKernelOriginalCgroupTermination$' -v -count=1 .
```

It starts two fresh inert user services in succession under one nonce-bound name. It exercises
the actual Linux termination implementation, verifies original main/descendant
pidfd death and subtree emptiness, then proves that the retained old kill
descriptor returns `ENODEV` and the old captured binding refuses the replacement.
Both replacement processes remain alive until their own original binding is
terminated. Units have finite runtime, memory, swap, task and CPU limits. Cleanup
uses only retained original descriptors. This is model-free host qualification,
not an alpha activation or full hostile/host-crash qualification.

The [October 22 mini capture](testdata/containment-stop/mini-2026-10-22.json)
records both actual kernel cases and the actual supervisor/synthetic-auth fixture.
The same-name replacement had a different invocation and cgroup inode; its
processes survived use of the old descriptor and old binding. The supervisor
fixture completed escaped-child shutdown and recovery with no fallback cleanup.
The report includes source hashes and raw producer observations. These bounded
cases do not close the broader containment acceptance criteria.


The first host fixture exposed a ControlGroup interface mismatch. The corrected
adapter reads that property from Service; the private fake-bus regression uses
the same interface split. Failed receipts remain evidence, not passing results.

## Remaining integration

The [native VM assembly](NATIVE-VM.md) implements fixed QEMU assets, separate
guest roles, protected auth transfer/refresh/disposal, bounded private provider
transport and `AuditedReviewer` composition. Full resource pressure, broader
hostile capabilities and arbitrary host-crash recovery still need physical
qualification.
These tests do not grant actual subscription permission, capability-allowlist
qualification, App installation, paid usage, deployment or activation.

Protocol references: [systemd D-Bus interface](https://www.freedesktop.org/software/systemd/man/org.freedesktop.systemd1.html),
[godbus v5.1.0](https://github.com/godbus/dbus/tree/v5.1.0), and
[kernel cgroup-v2 controls](https://docs.kernel.org/admin-guide/cgroup-v2.html).

The FD-packing order is checked against the [systemd v259.5 executor](https://github.com/systemd/systemd/blob/v259.5/src/core/exec-invoke.c#L5434-L5507).
