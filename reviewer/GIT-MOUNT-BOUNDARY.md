# Git worker mount observation

The private mount observer adds a kernel check for the private acquisition
factory. It returns no Git source capability, token, worker grant or publication
authority. Full admission also requires exact unit/cgroup ownership,
descriptor and installed-filter checks, authenticated export, broker drain and
physical stop, composed by the private [factory](GIT-FACTORY.md). A caller's serialized mount table cannot qualify any of these.

The observer starts from an already retained process boundary and its original
pidfd. It rechecks that worker before and after the operation. It reads bounded
`/proc/PID/mountinfo` and opens the worker's actual root view. That root includes
the target's mounted view, not the supervisor's pathname interpretation.
See the Linux [root-view](https://man7.org/linux/man-pages/man5/proc_pid_root.5.html)
and [mountinfo](https://man7.org/linux/man-pages/man5/proc_pid_mountinfo.5.html)
contracts.

The fixed layout contains only the runtime root, read-only broker, writable
work/tmp aliases, read-only private proc and the two fixed device bindings.
Root and broker inodes must match the retained original directories. Work and
tmp must be the same owned 0700 directory on the exact 256-MiB, 65,536-inode
tmpfs, with nosuid/nodev/noexec, writable mount flags and the kernel's noswap
superblock option. Scalar capacity checks avoid multiplication overflow.
The device targets must be the actual root-owned null and urandom character
devices; urandom stays read-only. The private proc's PID1 namespace must match
the retained worker PID namespace. The mount table must remain unchanged across
the check.

Parsing and policy are distinct. Known kernel optional fields are retained;
duplicate, unknown or inconsistent fields refuse. Shared propagation is refused.
Slave `master` fields are expected in the actual bubblewrap fixture and do not
describe outward propagation. They can receive mount events from the trusted
host, so this check does not claim a boundary against a hostile host service
identity or privileged administrator. The full factory must preserve the
existing trusted-host assumptions and recheck its retained assets and topology.
See [Linux mount propagation](https://man7.org/linux/man-pages/man7/mount_namespaces.7.html).

## Validation and limits

Static kernel-format fixtures exercise all fixed mount roles, read-only versus
superblock flags, aliases, noexec/nodev/nosuid/noswap, capacities and overflow,
unexpected mounts, propagation, malformed fields and byte bounds. They prove
parser/policy behavior, not actual storage qualification.

An actual inert child and original pidfd test kernel mount reads, exact returned
bytes, descriptor lifetime and refusal after process death or identity change.
The namespace opt-in runs the ordinary measured Go fixture in a minimal runtime
under the fixed final filter with seven actual mounts. It verifies the worker's
root view exposes the same work/tmp aliases as the parent's retained descriptor.
An intentionally unqualified empty host directory must be rejected. It carries
only trusted fixture data and is never a production storage fallback.

```sh
SFL_GIT_BOOTSTRAP_NAMESPACE=1 go -C reviewer test -race -count=1 -run '^TestGitMount' .
```

No successful aggregate mount admission is established on mini. The required
full-unit noswap tmpfs and sealed inner-worker execution still fail independently.
This fixture uses ordinary files and unqualified storage; it does not waive
either prerequisite. The [concrete factory](GIT-FACTORY.md) implements descriptor/filter checks and
durable stopped-source publication/recovery. Complete physical qualification,
actual credential/account evidence and live activation remain open in #176 and
its related issues.
