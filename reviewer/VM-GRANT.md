# One-use VM descriptor grant

`VMFixtureSupervisor` starts a separately pinned `cmd/vmstub`, captures its actual
systemd invocation/process/cgroup identity and grants one fixed synthetic guest
boot. It shares durable start/stop machinery with the inert marker supervisor.
The fake marker remains data; it never becomes a command selector.

This is a credential-free VM payload integration for #174. It deliberately does
not implement `WorkerLauncher` or `AuthStopVerifier`. It supplies no native
provider process, protected auth transfer, source/code-host role separation or
provider route. Those integrations and their qualification remain required.

The VM manifest contains only a fixed purpose, binding and three asset hashes
and byte counts. It contains no paths, commands, environment, profile or network
destination. The controller retains QEMU/kernel/initramfs with the sealed boot
library. Only after the pinned stub is manager-bound can a grant transfer these
three descriptors and the exact KVM device over SCM_RIGHTS.

The gate uses a separate version-2 hello/grant and persists its protocol and
manifest hash before opening the endpoint. It authenticates the Unix peer's
UID, PID, live pidfd, boot identity, start time and invocation. The durable grant
consumes the launch before sending descriptors. Failed delivery never permits
replay. Abort shares the journal lock with grant and closes accepted peers.
Recovery revokes permanently; it never recreates an endpoint or grants again.

The receiver acquires CLOEXEC atomically with `recvmsg`, accepts bounded
fragmented stream data and exactly four descriptors, rejects duplicate/unknown
JSON, extra/truncated ancillary data and malformed manifests, and closes every
delivered descriptor on rejection. It independently checks boot hashes, sizes,
required seals and executable permissions, plus KVM API version 12. Original
asset paths are never reopened by the stub.

Before grant, the stub checks its expected owned unit/cgroup/no-new-privileges
context and starts no QEMU or output. After grant it uses the existing fixed KVM
argv, one-CPU/512-MiB guest, no NIC/disks/shares/monitor/display, and a bounded
serial channel. It verifies the live QEMU executable and finalizes local
descriptors/serial before writing `vm-execution.json`. Its result proves pinned
execution and exit, not guest acceptance or physical whole-unit stop.

The systemd unit retains the existing one-GiB memory, zero swap, 64 tasks, one
CPU, 120-second runtime bound and control-group killing. These are synthetic
fixture limits, not a native client budget. The durable unit record includes a
payload discriminator. Recovery refuses cross-payload records/descriptions and
requires the original manager, invocation and cgroup hierarchy/inode. Old fake
journals default only the newly added discriminator/protocol to their original
fake values; duplicate/null/unknown fields remain rejected. Existing fake
public methods cannot start or grant the VM payload.

## Validation

Ordinary tests use actual Unix sockets/SCM_RIGHTS and sealed files, with an inert
descriptor in the device slot. They never open KVM or create host units. Tests
cover pre-grant silence, durable consumption, fragmented frames, extra/missing
FDs, truncation, CLOEXEC, rejection without FD leaks, wrong pins, unsealed/swapped
assets, cancellation/deadlines, legacy fake journal recovery, cross-payload
refusal and a fresh supervisor's durable stop. The VM wrapper cannot vouch for
an auth lease. Existing marker and actual private D-Bus transport tests remain
part of the full suite.

Explicit host qualification builds committed source with cached dependencies:

```sh
python3 -B reviewer/scripts/vm-grant-fixture.py \
  --kernel /absolute/verified/vmlinuz \
  --kernel-sha256 VERIFIED_KERNEL_SHA256 \
  --output /absolute/new/private-artifact-directory \
  --go /absolute/pinned/go
```

The driver builds a fresh synthetic guest plus a pinned stub and helper. The
helper requires explicitly linked source revision/tree matching its private
configuration. It captures the stub before grant, replaces its original path,
transfers the four descriptors, verifies actual systemd normal-exit properties,
stops the complete unit and recovers that stop with a fresh supervisor. The
private root and journals remain available for diagnosis/recovery.

The outer driver separately validates the actual Go execution/stop receipt,
nonce-bound guest UID/source/cgroup/escaped-child result and fresh terminal unit
state. A success-shaped local file cannot hide failed cleanup or a nonzero stub
exit. Failed attempts stay failures. All profiles remain synthetic/absent, no
provider inference or App/deployment is involved, and production qualification
remains false.
