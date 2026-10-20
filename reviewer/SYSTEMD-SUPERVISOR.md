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
or superseded results are removed. An abandoned start result cannot prevent a
recovery stop. Malformed, failed or unknown results cannot become successful
launch evidence. Recovery takes a unit reference to prevent GC during stop
observation.

Unit properties use a fixed argv array and `Type=exec`, `Restart=no`,
`RemainAfterExit=yes`, `CollectMode=inactive`, `AddRef=true`, control-group
killing, SIGKILL escalation and no-new-privileges. Memory is 1 GiB, swap zero,
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

Stopping requires the captured manager, invocation and cgroup identity. The
supervisor waits for the exact stop job, independently checks that the captured
cgroup inode has no populated subtree, and checks the unit's final identity,
job, main PID and inactive/failed state before recording stop. It never kills
a numeric PID or a reused cgroup path. A changed host boot can prove the old
kernel processes dead after permanent grant revocation. The journal also binds
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
and synthetic auth completion. Any fixture-only fallback cleanup keeps the
qualification failed, even after the main exercise tentatively passed. Deferred
close failure also fails the receipt, including the reopened supervisor. The
reopened handle is closed before the tentative pass. Fallback stop gets a fresh
ten-second deadline after the supervisor stop context ends. The final journal
must be retained with its exact binding and captured/aborted/stopped evidence.
A missing, malformed or mismatched journal fails qualification. Receipt-write
failure also fails the command. An earlier exercise error is preserved.
Normal tests never start a host unit.

The first host fixture exposed a ControlGroup interface mismatch. The corrected
adapter reads that property from Service; the private fake-bus regression uses
the same interface split. Failed receipts remain evidence, not passing results.

## Remaining integration

The fixed QEMU payload, guest role identity/resource controls, native clients,
protected auth transfer and refresh/disposal, exclusive provider bridge and
AuditedReviewer composition still require implementation and qualification.
These tests do not grant actual subscription permission, capability-allowlist
qualification, App installation, paid usage, deployment or activation.

Protocol references: [systemd D-Bus interface](https://www.freedesktop.org/software/systemd/man/org.freedesktop.systemd1.html),
[godbus v5.1.0](https://github.com/godbus/dbus/tree/v5.1.0), and
[kernel cgroup-v2 controls](https://docs.kernel.org/admin-guide/cgroup-v2.html).

The FD-packing order is checked against the [systemd v259.5 executor](https://github.com/systemd/systemd/blob/v259.5/src/core/exec-invoke.c#L5434-L5507).
