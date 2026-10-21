# Git launch constraints

Research, October 21, 2026 EDT. Source facts and read-only host observations
below do not qualify a production launcher. No units, mounts, credentials,
inference, policy changes or privileged commands were used for this research.

## Current result

Keep acquisition unavailable. The required bounded `noswap` tmpfs has no
verified unprivileged creation path on mini. A capability-bearing executable
after bubblewrap does not solve the requirement. Preserve the fixed unit
properties and fail closed; changing their ownership or timing requires a
reviewed design revision, even if a substitute later proves equivalent.

The parent task reported two actual inert failures: systemd 259.5 rejected
`TemporaryFileSystem=/tmp:size=268435456,nr_inodes=65536,noswap,nosuid,nodev,noexec,mode=0700`
with `226/NAMESPACE`, "Operation not supported"; mandatory bubblewrap reached
payload exec but `execvp("/proc/self/fd/5")` returned `EACCES` for a sealed
`MFD_EXEC` memfd with mode `0500`. These are distinct failures. Earlier
PrivateNetwork success with host-network retention is documented in
[GIT-BOOTSTRAP-NETWORK.md](GIT-BOOTSTRAP-NETWORK.md).

## The tmpfs privilege barrier

Upstream Linux 7.0 rejects `noswap` during tmpfs option parsing unless the
filesystem context belongs to `init_user_ns` and the caller passes
`capable(CAP_SYS_ADMIN)`. The exact diagnostic is "Turning off swap in
unprivileged tmpfs mounts unsupported". This condition precedes filesystem
creation. Namespace-local capabilities cannot satisfy it. Tmpfs otherwise
supports user-namespace mounts. See [option parsing](https://github.com/torvalds/linux/blob/v7.0/mm/shmem.c#L4693-L4699)
and [filesystem flags](https://github.com/torvalds/linux/blob/v7.0/mm/shmem.c#L5350-L5360).

Read-only host metadata identifies Ubuntu 26.04 and
`Ubuntu 7.0.0-30.30-generic 7.0.12` from `/proc/version_signature`.
The matching Ubuntu patchset remains unqualified: its Launchpad source endpoint
was inaccessible. The upstream check is strong evidence for the barrier,
not proof of this distribution binary's exact implementation.

`size` and `nr_inodes` bound separate resources. `noswap` must be established
when the mount is created; remount cannot turn an ordinary tmpfs into a
`noswap` tmpfs. A memory/swap cgroup limit does not establish the required
filesystem option. See [kernel tmpfs contract](https://docs.kernel.org/filesystems/tmpfs.html)
and [remount checks](https://github.com/torvalds/linux/blob/v7.0/mm/shmem.c#L4829-L4857).

Pinned bubblewrap 0.11.1, commit
`124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe`, builds its tmpfs options from mode
and optional size only. It passes `MS_NOSUID|MS_NODEV`; it provides no
`nr_inodes` or `noswap` argument. Its internal root tmpfs is separate from the
required writable work mount. See [tmpfs mount operation](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L1120-L1142).

## Why a post-bubblewrap mount stage is insufficient

Bubblewrap retains requested effective, permitted and inheritable capability
bits, and attempts to raise matching ambient bits before exec. Ambient raising
ignores `EINVAL` and `EPERM`, so requested arguments are insufficient evidence
of capabilities across exec. See [capability setup](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L703-L803)
and [final exec ordering](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L3607-L3637).

Furthermore, `--disable-userns` creates a second child user namespace after
mount setup. A payload's capability there grants no capability in the ancestor
user namespace owning the existing mount namespace. Thus
`--cap-drop ALL --cap-add CAP_SYS_ADMIN` does not grant mount authority over
that existing namespace. Creating another mount namespace would change the
proposed fixed boundary and still cannot satisfy the initial-userns `noswap`
check. See [nested transition](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L3452-L3502)
and [namespace capability scope](https://man7.org/linux/man-pages/man7/user_namespaces.7.html).

Read-only local policy adds another independent obstacle.
[/etc/apparmor.d/bwrap-userns-restrict](/etc/apparmor.d/bwrap-userns-restrict)
is root-owned mode `0644`. Its `bwrap` profile permits namespace setup, then
stacks executed children with `unpriv_bwrap`, whose rules explicitly deny all
capability use. The optional local override files are absent. The global loaded-profile list was unreadable. A subsequent inert child
probe read `/proc/self/attr/current` as `bwrap//&unpriv_bwrap (enforce)`,
proving attachment for that child, not every service launch. Its real, effective
and filesystem UIDs were 1000, matching the image owner; the sealed image
was mode `0500`, and all six expected seals were present. The capability
sets were zero and no-new-privileges was one. Descriptor-based exec still
returned `EACCES`. No policy changes are authorized.

A generally sound staged design mounts exact bounded storage before dropping
privilege, then clears every capability set, retains no-new-privileges,
installs the final mount/namespace-denying filter, and only then executes the
sealed worker. Here it lacks an authorized, verified creator for `noswap`.
Moving that work into a trusted namespace stage cannot manufacture host
privilege. An already-provisioned eligible mount would require separate
provenance and authority; no host-directory or unbounded-tmpfs fallback is
acceptable.

## The memfd EACCES remains unlocalized

Pinned bubblewrap's `drop_privs()` explicitly sets `PR_SET_DUMPABLE=1` after
capability changes, before payload exec. Upstream ptrace access permits
same-thread-group introspection before credential checks. Therefore ordinary
self-ptrace denial from nondumpability is not an established explanation for
this failure. See [dumpability reset](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L937-L953)
and [self access](https://github.com/torvalds/linux/blob/v7.0/kernel/ptrace.c#L287-L299).

Reopening a proc FD still checks the referenced inode's permissions;
inheriting an FD does not guarantee reopening it succeeds. A proc mount's
`noexec` flag alone does not identify the executable target's mount. Record
actual UID maps, FSUID, inode owner/mode and target mount identity.
See [proc FD contract](https://man7.org/linux/man-pages/man5/proc_pid_fd.5.html).

A measured ELF trampoline using direct `execveat(fd,"",...,AT_EMPTY_PATH)`
removes proc pathname traversal. It does not bypass executable inode/mount
checks or AppArmor's executable-file path and transition checks. Consequently
it was a testable candidate. The later static ELF differential below tested
it and did not obtain a successful sealed launch. The installed policy's
`attach_disconnected,mediate_deleted` flags also prevent inferring denial
merely because the object is anonymous or deleted.
See [execveat](https://man7.org/linux/man-pages/man2/execveat.2.html),
[kernel exec checks](https://github.com/torvalds/linux/blob/v7.0/fs/exec.c#L764-L796)
and [AppArmor executable transition](https://github.com/torvalds/linux/blob/v7.0/security/apparmor/domain.c#L659-L764).

## Reproducing the executable checks

`SFL_GIT_BOOTSTRAP_NAMESPACE=1 go -C reviewer test -count=1 -run '^TestGitSupervisorActualPrivatePIDNamespaceAndDumpableBoundary$'`
uses an ordinary read-only executable and tests only namespace/authentication
and descriptor isolation. It passes locally. It does not qualify sealed exec.

`SFL_GIT_SEALED_WORKER_EXEC=1 go -C reviewer test -count=1 -run '^TestGitSupervisorActualSealedWorkerExec$'`
requires the same actual Go executable to run from its retained sealed inode,
with host hash/seals/inode checks before and after dumpable-zero. It currently
fails at the descriptor-exec prerequisite. Opting in preserves that failure;
it is not skipped, waived or converted into a successful qualification.
The diagnostic trampoline is a fixture only. No production Python or broad
`/usr` runtime is introduced. The ordinary fixture passes the sealed source
FD solely to check its immediate closure, and makes no running-image claim.

## Required bounded inert qualification

1. Match installed kernel/helper/manager/libc binaries to their distribution
   sources and record hashes, active AppArmor labels and exact launch receipt.
2. Separate namespace setup, tmpfs parsing and mount failures with a fixed
   non-parser probe. Preserve errno and namespace ownership. A `noswap` failure
   must stop qualification; no weaker retry may acquire a broker grant.
3. Compare the same sealed ELF through proc exec and direct execveat under
   identical full properties. Record descriptor identity/seals, maps, FSUID,
   capability sets, dumpability, proc traversal and active labels immediately
   before exec. Diagnose the exact failing syscall without changing policy.
4. Only after both prerequisites succeed, independently attest mount flags,
   byte/inode exhaustion, absence of extra writable storage, post-setup mount
   denial, namespaces and worker identity. Keep the worker inert until the
   supervisor authorizes admission. Credentials, Git parsing and model use
   remain outside this qualification.


## Evening static ELF differential

The October 21 evening follow-up reran the existing actual Go namespace and
sealed-worker tests at source `ef691485f9bf72668a1760e1b0cb85658c9ab15a`.
The ordinary executable fixture passed; sealed descriptor exec again returned
`EACCES`. This is still a failed qualification.

A fixed static C ELF then compared both kernel entry methods under one bounded
inert user unit. It used the actual Go-produced payload filter, 704 bytes,
SHA-256 `f56ba6deb092f7e5af3119571a9c90341946032832d693b5d74e7e7c6561ff4e`.
The same sealed inode, mode `0500` and all six seals `63` were retained across
the comparisons. Effective UID matched the image owner, dumpability was one,
no-new-privileges was one, and the memfd's reported filesystem flags did not
include noexec.

| Image | Outside bwrap, proc exec | Outside bwrap, execveat | Inside bwrap, proc exec | Inside bwrap, execveat |
| --- | --- | --- | --- | --- |
| Ordinary read-only file | Target reached | Target reached | Target reached | Target reached |
| Same sealed memfd | Target reached | Target reached | EACCES | EACCES |

The confined payload reported `bwrap//&unpriv_bwrap (enforce)`; the outside
control reported `unconfined`. This narrows the failure to an interaction with
the confinement boundary. It does not isolate AppArmor from namespace or other
boundary effects. The result disproves a Python-only or proc-path-only cause,
and also disproves ordinary image mode/seals alone as the cause. An ELF
`execveat` trampoline is not a demonstrated workaround on mini.

A further read-only `--ro-bind-fd` attempt kept the sealed inode rather than
copying it. Bubblewrap rejected the anonymous memfd source path with `ENOENT`
before executing the payload. The ordinary-file named-bind control reached the
target. This is a separate setup failure, not another measured exec errno.
No copied executable, relaxed seal requirement or host policy change followed.
Every diagnostic unit was collected and independently observed inactive with
MainPID zero.

The reproducible opt-in [diagnostic](scripts/diagnostics/git-sealed-exec/README.md)
records source/tool hashes, inode/seal observations, each failure stage and
unit cleanup. A zero tool exit means the comparison was collected, not that
acquisition is qualified. Its `qualified` and `noswap_qualified` fields remain
false. Both the original tmpfs barrier and the sealed-worker gate remain open.
