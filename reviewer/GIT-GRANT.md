# Git acquisition descriptor grants

These private Linux helpers supply the one-use descriptor handoff for the
unfinished acquisition factory. They start no service, read no credentials and
grant no provider or publisher capability. The fixed executable and bootstrap receiver now exist; aggregate physical
qualification, full helper admission and controller composition remain
unfinished. See [bootstrap and entry points](GIT-BOOTSTRAP.md). See the
[unit owner](GIT-UNIT.md) and [descriptor design research](GIT-WORKER-FD-HANDOFF.md).

## Transport and fixed roles

Control uses connected AF_UNIX SOCK_SEQPACKET sockets. Each atomic record has
at most 8 KiB and eight descriptors. Receipt uses MSG_CMSG_CLOEXEC, checks exact
descriptor counts and rejects truncated data/control, empty records and
unexpected ancillary types. Every received FD is owned before rejection,
including an unexpected kernel SCM_PIDFD. Rejected packets close all delivered
descriptors. Export is a separate bounded pipe; it never becomes a control
packet or a host-side Git parser.

Sending duplicates caller-owned descriptors under RawConn.Control and closes
the temporary duplicates afterward. It avoids os.File.Fd, which can switch a
Go-managed nonblocking pipe to blocking mode. Receipt also preserves that
open-file-description flag. The fixed worker must explicitly establish its own
nonblocking export writer and close-on-exec policy after the bootstrap exec
transitions; transport preservation alone does not prove those transitions.

The canonical grant packet contains version, stage, immutable binding,
recipient PID/start/boot/invocation identity and manifest hash. Bootstrap and
worker are the only stages. Unknown fields, duplicate or differently cased
fields, noncanonical bytes and substituted binding/stage metadata refuse.

Release manifest version2 selects the Git runtime artifact hash, Git/helper
pins, original namespace helper, sealed worker and fixed payload filter. Its
hash must equal the binding's RuntimeHash. Bootstrap grants carry exactly:

| Index | Role | Validation |
| --- | --- | --- |
| 0 | Canonical manifest | Sealed non-executable bytes, 4 KiB maximum, exact hash/schema |
| 1 | Prepared runtime directory | Exact immutable layout; reconstruct pinned artifact size/hash and all library bytes |
| 2 | Namespace helper | Retained original root-owned executable, pinned size/hash |
| 3 | Worker image | Sealed executable, exact unit-owner worker hash and pinned size |
| 4 | Payload filter | Sealed non-executable bytes matching the fixed compiled policy |
| 5 | Broker directory | Owned mode0700 directory containing only the two mode0600 sockets and exact used marker |
| 6 | Export writer | Write end of a pipe |
| 7 | Supervisor pidfd | Original live supervisor identity and native 64-bit pidfs representation |

Version1 release manifests are rejected. The canonical grant packet stays
version1 because its fields and durable consumption hashes are unchanged;
preexisting journal records retain their recovery semantics. The release
manifest hash in the binding distinguishes the new role contract, alongside
the pinned worker image. Old artifact descriptors cannot satisfy the directory
role.

The supervisor prepares only trusted release files outside the candidate
workspace. An executable directory cannot live on the mandatory `noexec`
candidate tmpfs. Its canonical metadata and every file reconstruct the original
artifact framing/hash; fixed empty device mount targets are separately checked.
The directory must be executable, immutable and contain no extra paths. The
namespace helper consumes its FD after binding it read-only. Host directory
FDs never reach Git. This change supplies no writable host fallback for
candidate objects.

The worker grant carries no descriptors. The factory must separately prove
runtime format, mounts, helper/library identity, resources, route absence,
descriptor closure and final policy enforcement before that stage. Checking
asset hashes and FD roles does not establish the complete boundary.

## Durable one-use authority

Only a completed fresh unit start in this owner instance arms grant authority.
Opening old state or recapturing a live unit never arms it. Recapture exists to
stop and recover old work. Grants also require the original live root pidfd,
unchanged manager/boot/invocation and cgroup, and a kernel-authenticated peer
assigned to that unit with the expected executable. The bootstrap recipient
must be the exact captured root. The worker must be a distinct process in the
same invocation. Namespace and resource qualification remains the composing
factory's responsibility.

Before the sole atomic send, the owner fsyncs a hash of the canonical packet.
Worker consumption additionally records its exact recipient and requires prior
bootstrap consumption. Both hashes are recomputed during journal validation.
An uncertain send consumes the stage, records revocation where possible and
latches failure in the live owner. No automatic resend follows. A failed intent
write sends nothing. Stopping removes fresh grant authority before attempting
the durable revocation write. Unresolved ownership still retains its process
lock and image through the existing stop/recovery contract.

New empty fields are omitted, preserving canonical pre-grant version1 journal
records. Reopening those records supplies recovery authority only. Publication
still requires broker drain, physical whole-unit stop and authenticated export
retention; a consumed grant is not a completed source acquisition.

## Supervisor identity inside the private PID namespace

The inner worker uses a fresh pathname connection, with the supervisor pidfd
inherited through the authenticated bootstrap. The supervisor can appear as
numeric PID zero through SO_PEERCRED. The worker therefore compares the
SO_PEERPIDFD's native pidfs device/inode identity with the trusted inherited
descriptor and polls both for liveness. It requires PID_FS_MAGIC, the actual
pseudo-inode representation, close-on-exec and matching UID/GID. It never
reopens a numeric PID or accepts a UID-only fallback. Anonymous inherited
socketpairs are refused by this authenticator because they identify creation,
not a later writer. This authenticates a process, not a source grant.

The actual mini fixture runs the compiled Go receiver under the fixed final
filter in private PID/user/network/mount namespaces. It observes PID zero,
authenticates the correct supervisor and refuses a different live pidfd. After
clearing worker dumpability and dropping capabilities, a Git-equivalent child
cannot open its parent's private descriptor and does not inherit it. The host
still independently rechecks the retained worker network boundary on this
kernel. A target where that recheck fails must withhold admission.

## Validation and limits

Packet tests exercise real Linux SCM_RIGHTS/SCM_PIDFD, truncation, rejected-FD
cleanup, exact record boundaries, cancellation and pipe-mode preservation.
Grant tests use actual inert peer processes and descriptor transfers with a
fake manager/kernel adapter. They cover both stages, wrong roles, canonical
metadata, pre-send storage failure, lost delivery, replay, changed authority and
reopening followed by recapture. The latter releases controller handles in the
fixture; the separate unit-owner crash test exercises an actual SIGKILL. Neither
test claims an aggregate production unit or resource-pressure qualification.

The opt-in inert namespace checks use SFL_GIT_BOOTSTRAP_NAMESPACE=1. They mount
trusted fixture binaries and /usr for compatibility testing, not the production
minimal runtime. The fixture's /dev and temporary mounts likewise do not qualify
the final mount contract. No live GitHub/provider backend, credentials, inference,
paid usage or activation is exercised.
