# Sealed VM boot assets

`OpenVMBootImages` prepares an explicit trusted boot specification for a later
VM supervisor. It starts no process or service and reads no auth profile. Pins,
paths and binding must come from the promoted release/controller, never PR or
model content. The production native VM launcher and guest roles remain
unfinished [issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174) work.

The specification binds a session, launch nonce and containment hash to QEMU,
kernel and initramfs file sizes and SHA256 hashes. Each source must be a regular,
single-link file owned by root or the supervisor, without group/other write
permission. Final symlinks are rejected. Paths are trusted release input, not a
source-reader capability. Reads stream through a 32-KiB buffer with cancellation
checks. QEMU/kernel are capped at 64 MiB each; initramfs is capped at 512 MiB.
These are file caps, not proof that every such image fits a chosen guest budget.

Each accepted file is copied into a private sealed memfd. QEMU is executable;
kernel/initramfs cannot acquire execute permission. Seals prohibit writes,
resizing and execute-bit changes. Adding a writable mode bit does not make sealed
bytes writable. Replacing or deleting any original pathname cannot replace the
retained image. Child descriptors are duplicated under the close fence, so an
owner close cannot invalidate already captured child descriptors. Closed bundles
cannot supply new launches.

## Fixed fake boot exercise

`cmd/vmbootfixture` is an opt-in credential-free test executable, not a
WorkerLauncher, AuthStopVerifier or activation command. It requires a private,
owned 0600 configuration from a clean compiled source revision. Configuration
selects only pinned assets and a valid binding, with purpose
`synthetic-role-fixture`. No argv, environment, device, network, host mount or
provider selector is accepted.

Execution requires an explicitly created user unit named
`sfl-vm-fixture-<nonce>.service`, matching invocation context, cgroup v2, one-GiB
memory limit, zero swap, 64 tasks, one CPU quota and no_new_privs. It refuses an
ordinary shell invocation before starting QEMU or opening KVM. The fixture
controller must separately bound the unit lifetime and verify its final stop.
The guard does not attest a production manager/grant or replace that supervisor.

The fixed QEMU command executes the retained binary from fd3, loads the retained
kernel/initramfs through fd4/fd5, and receives the exact API-version-12 KVM device
through fd6 and a QEMU fdset. It selects only KVM and host CPU, never TCG fallback.
The guest has 512 MiB and one CPU, no NIC, display, monitor, disks or host filesystem
sharing. Guest-core dumping and cross-VM memory merging are disabled. QEMU's
sandbox denies obsolete operations, privilege elevation, spawning and resource
control. Firmware is resolved only from the fixed OS QEMU directory. The dynamic
loader, OS libraries/firmware and QEMU remain part of the trusted host boundary;
sealing the executable alone does not qualify that boundary.

The environment contains only fixed locale/PATH. Serial output is capped at
four MiB. Execution and cleanup have bounded contexts; inherited descendant
pipes cannot make exec.Wait unbounded. The live `/proc/PID/exe` must have the
required seals and QEMU digest. The fixture finalizes descriptor and local serial
resources before writing its execution receipt. That receipt reports verified
executable/normal exit, not guest acceptance or whole-unit shutdown. Its outer
driver must validate the nonce-bound synthetic guest result and complete unit
cleanup before recording a passing qualification.

No credentials are sent into this guest. The existing synthetic role image can
exercise its separate reader/worker identities, source canaries and cgroup stop
checks. A successful boot does not qualify native CLI/code-host separation, auth
transfer/refresh/disposal, provider egress, model eligibility or production
isolation. The fixed systemd fake-marker payload remains unchanged. A separate
pinned VM payload/grant adapter is still required before composing native workers.

## Validation

Ordinary tests never open KVM or start a host unit. They exercise sealed-byte and
execute-bit mutation denial, source pathname replacement, child lifetime across
bundle close, malformed pins, symlinks, hard links, unsafe permissions, byte caps,
cancellation and fixture refusal outside an owned unit. A real test executable
runs from the retained fd after its pathname is replaced and reports the exact
kernel/initramfs descriptors, fixed argv and stripped environment.

```sh
go -C reviewer test -race -run '^TestVMBoot' -count=1 .
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Actual host/KVM execution is separate opt-in fake-only qualification. Preserve
source/image/kernel/QEMU hashes, KVM probe, the captured unit properties, raw
nonce-bound synthetic serial output and terminal whole-unit cleanup. Failed
attempts stay failures and are retained; an observation timeout does not restart
or replace a still-running process.

Protocol sources checked October 20, 2026: [QEMU invocation](https://www.qemu.org/docs/master/system/invocation.html),
[pinned QEMU 10.2.1 KVM device handling](https://github.com/qemu/qemu/blob/v10.2.1/accel/kvm/kvm-all.c),
[Linux KVM API](https://docs.kernel.org/virt/kvm/api.html#kvm-get-api-version)
and [memfd seals](https://man7.org/linux/man-pages/man2/memfd_create.2.html).
