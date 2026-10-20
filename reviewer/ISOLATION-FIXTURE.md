# Credential-free Linux role fixture

This fixture exercises the real managed reader, immutable Git object reader,
Unix source proxy and native transport under actual distinct Linux identities.
All provider behavior and source content are synthetic. It creates no host
accounts, imports no profiles, calls no provider and installs no service.

The guest PID1 provisions reader UID1201, worker UID1202 and shared IPC GID1301.
The reader receives a private manifest/audit pipe and a synthetic root-owned
object store. The worker receives a fresh chroot with a fixed fake executable,
the source proxy and only the captured readonly job IPC mount. It has no source
objects, host home, shell, Git, procfs, provider/GitHub environment or network
interface. Its cwd is explicitly inside the chroot; both absolute and relative
escape paths are probed. Both roles drop supplementary groups/capabilities and set
no_new_privs. A third UID in the shared IPC group reaches the socket but must
receive no source bytes. The real reader records that denial before accepting
the authorized worker's Read/Search calls.

Each role starts atomically in its own cgroup through CLONE_INTO_CGROUP. The
fixture sets 96 MiB memory.max, zero swap, pids.max32 and one CPU quota. Stop
uses cgroup.kill, observes populated0 and reaps the direct process. The fake
worker leaves a child in a separate process group; success requires the worker
cgroup to kill it and PID1 to reap the orphan. These tests exercise containment
mechanics for trusted fixtures, not a malicious or credential-bearing client.

The host builder creates a sorted, deterministic newc/gzip image from explicitly
selected binaries/libraries, fixed device records and a tiny synthetic object
repository. No host directory is shared with the guest. The manifest records the
source commit, executable/library/kernel/image hashes, toolchain and fresh nonce.
Builds require committed source and use cached modules with network retrieval
disabled. The kernel is supplied separately with its required SHA256; this tool
does not download or install one.

From the repository root on a Linux host with QEMU and delegated user cgroups:

```sh
python3 reviewer/scripts/isolation-fixture.py \
  --kernel /absolute/verified/vmlinuz \
  --kernel-sha256 VERIFIED_KERNEL_SHA256 \
  --output /absolute/new/private-artifact-directory \
  --go /absolute/pinned/go \
  --run
```

Omit --run to build only. Output must be a new directory. Execution uses one
fresh transient user unit with 1 GiB MemoryMax, zero swap, TasksMax64, CPUQuota100%
and a 120-second runtime bound. QEMU has no NIC, host filesystem device, monitor
or display. Only nonce-bound output from this invocation can qualify; missing,
ambiguous or failed output cannot use a previous report. Actual kernel resource
observations and stop confirmation must pass before result.json is written.
Required cleanup failure fails qualification while preserving an earlier error.

The default accelerator is TCG, used only for these trusted, inert fixtures.
Pass `--accelerator kvm` to exercise hardware virtualization explicitly. That
mode requires an accessible `/dev/kvm` reporting API version12 and passes only
`-accel kvm -cpu host` to QEMU. Failure to open KVM or boot the guest fails the
fixture without a TCG fallback. The manifest captures the accelerator and QEMU
version/hash; launch verifies the executable hash again. Prepared and final
receipts record the selected accelerator and device probe.

QEMU does not promise a production security boundary for TCG. A successful KVM
fixture boot proves hardware execution of these fake roles, not qualification
of production client packaging, credential transfer or egress. This command never selects real clients,
credentials, App identity or live inference, and its result explicitly records
production_boundary_qualified=false. Production needs a separately qualified
KVM/host boundary, protected auth/profile packaging, provider request/egress
limits, durable OS recovery and exact-client qualification. Those remain source
or qualification work under the related implementation issue.

References: [QEMU security policy](https://github.com/qemu/qemu/blob/v10.2.1/docs/system/security.rst),
[Linux initramfs](https://www.kernel.org/doc/html/latest/filesystems/ramfs-rootfs-initramfs.html),
[cgroup v2](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html).
