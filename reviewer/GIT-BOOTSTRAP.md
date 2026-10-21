# Fixed Git bootstrap and executable

October 21, 2026 EDT. `cmd/gitacquire` is an offline-buildable, credential-free
payload. Building or testing it does not deploy a reviewer or activate source
access. The production acquisition factory, physical boundary/helper admission,
archive publication, credential-generation lease and controller composition
remain unfinished. Mini's actual fixed tmpfs and inner sealed exec still fail
as documented in [launch constraints](GIT-LAUNCH-CONSTRAINTS.md).

## Process entry points

The systemd root receives the fixed four arguments after argv0: bootstrap socket,
canonical nonsecret binding JSON, binding digest and a diagnostic supervisor PID.
`INVOCATION_ID` is the sole selected environment input. FD3 is the actual sealed
executable, FD4 the original supervisor pidfd inherited at unit birth. The root
checks raw descriptor types before creating owning Go file wrappers, so a
missing inheritance cannot accidentally close the Go runtime's epoll/eventfd.
It then marks its owned descriptors close-on-exec.

The root connects to the named `bootstrap.sock` and authenticates its peer by
comparing SO_PEERPIDFD with that original inherited pidfd. It never reopens a
numeric supervisor PID. A hello binds its own boot/start/invocation identity to
the job. The sole eight-descriptor bootstrap grant must name that exact root
and binding. Shared [grant validation](GIT-GRANT.md) checks the canonical sealed
manifest, verified runtime directory, original helper, worker image, fixed
filter, broker directory, export pipe and supervisor descriptor. The worker
image must be the same sealed inode as the running root; the granted supervisor
must be the same original kernel identity. Invalid or missing grants start no
child. Received and initial descriptors are closed on rejection.

The inner entry point accepts only `--worker`, canonical binding JSON and its
digest. It takes supervisor3/image4/manifest5/export6, validates every raw role,
and invokes the [private worker phase](GIT-WORKER.md). Paths are fixed to
`/usr/bin/git`, `/work`, `/broker/worker.sock` and `/broker/git.sock`; the broker
URL is the fixed synthetic HTTP destination. No candidate command, path,
credential or network URL can be selected through this CLI. Both entry points
have a 110-second process deadline inside the separate 120-second unit limit.

## Trusted runtime and candidate workspace

The supervisor assembles only trusted release files into its owned immutable
runtime directory. Release manifest version2 changes bootstrap role1 from a
sealed artifact to that prepared directory. The artifact format remains version1.
The directory contains canonical runtime metadata, exact pinned Git/helper and
library files, fixed mount directories and empty regular device placeholders.
Verification reconstructs the original artifact header and file stream and
checks its pinned size/hash, every file, modes, ownership and complete path
inventory. A directory on a `noexec` filesystem is refused. The original artifact
hash still determines all executable/library bytes; metadata is not an unsigned
replacement for it. Same-UID control-plane ownership remains an explicit trust
assumption. The owner must retain these files until whole-unit stop.

Candidate Git objects require the actual unit-private `/tmp` mount with exactly
256 MiB, 65536 inodes, `noswap,nosuid,nodev,noexec`, mode0700 and writable state.
The root cross-checks kernel statfs and bounded mountinfo, requires an empty
mount and creates one fresh attempt directory only after those checks pass.
It does not create or weaken the mount itself. Failed qualification supplies no
workspace or host-directory fallback. Physical namespace provenance, resource
pressure and independent supervisor inspection remain required before a worker
grant. These local checks do not replace that admission.

The original retained root-owned bubblewrap inode starts through its parent
process's open FD path; it is not copied or inherited as a payload capability.
All namespace settings and file positions are fixed. Mandatory user/PID/network/
mount isolation, further-userns denial, no capabilities, a new session and a
cleared environment precede the final sealed Unix-only filter. There is no
host-network or optional-userns alternative.

| Child FD | Role | Before worker exec |
| --- | --- | --- |
| 3 | Original supervisor pidfd | Retained for worker authentication, then closed |
| 4 | Sealed image | Exec source, validated and closed by worker |
| 5 | Sealed manifest | Validated and closed by worker |
| 6 | Export write pipe | Private nonblocking CLOEXEC duplicate in worker |
| 7 | Fixed final BPF | Consumed by bubblewrap |
| 8 | Runtime directory | Consumed after read-only root bind |
| 9 | Broker directory | Consumed after read-only bind |
| 10, 11 | Same bounded workspace | Consumed for `/work` and `/tmp` aliases |

Proc is private and read-only. Only the original null and urandom devices are
bound over fixed placeholders; urandom is read-only. No extra dev/shm/devpts or
unbounded tmpfs is requested. The exact mount/device/filter/descriptor outcome
still needs independent kernel inspection. Intended argv is not enforcement.
The root's ordinary helper exit is never whole-unit stop, broker drain or
source publication authority.

## Validation and remaining gaps

Real producer tests prepare the installed ELF runtime, reconstruct its artifact
hash and reject mutation, extra paths, wrong modes, links, missing placeholders,
old artifact descriptors and wrong hashes/sizes. An actual noexec `/dev/shm`
fixture is conditional. On this host it was skipped because `/dev/shm` did
not provide the required noexec fixture, so actual filesystem-level rejection
remains unmeasured; file-mode rejection and source checks pass. A separate sealed Go
root child exercises actual original pidfds, canonical hellos and eight-FD
transfer, including wrong peer/diagnostic PID, absent/substituted grant,
recipient, directory, supervisor, copied image, ordinary executable and legacy
manifest rejection. It closes its owned descriptors without deleting the
supervisor's runtime. These root fixtures launch no Git or namespace helper.

The actual `cmd/gitacquire` binary is built with the test compiler, local
module/toolchain selection and CGO disabled. Its malformed/oversized/noncanonical
bindings, bad invocation/PID/hash, unknown mode and absent root/worker FD roles
all exit1 silently without writing state or disturbing runtime descriptors.
A sealed actual CLI receives a valid bootstrap grant on this incompatible host
and refuses the workspace prerequisite. This is a negative producer result,
not a successful protected launch. Its build deadline is separate from the
live handshake deadline.

The existing separate opt-in sealed bubblewrap probe still fails with EACCES;
the fixed systemd tmpfs probe failed before payload exec. Neither is waived.
A complete launch, native resource-pressure test, physical helper/library/
namespace/route/mount/FD admission, authenticated stopped archive, recovery and
controller run remain to implement or qualify. No credential, real GitHub
transport, model inference, promotion, paid use or activation is exercised here.
