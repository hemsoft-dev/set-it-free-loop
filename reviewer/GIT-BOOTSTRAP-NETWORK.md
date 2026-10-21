# Git bootstrap network compatibility

Source research and inert compatibility probes, October 21, 2026 EDT.
No Git transport, provider inference or credential access occurred. The probes
below establish bootstrap compatibility only. Aggregate acquisition, resource,
helper-admission and recovery qualification remains unfinished.

## Exact upstream baseline

The annotated release tags resolve to these commits, verified through GitHub's
tag objects rather than a moving branch:

- [bubblewrap 0.11.1 tag](https://api.github.com/repos/containers/bubblewrap/git/tags/1e75470e57fb9d2e9028c1451d6bc856e0ac03ca):
  `124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe`.
- [systemd 259.5 tag](https://api.github.com/repos/systemd/systemd/git/tags/4f8299955d8edfa6dab6879cbd7f199274b121fc):
  `b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a`.

All implementation links below use those immutable commits. Installed version
strings alone cannot establish binary equality or exclude distribution patches.

## Verified facts

Bubblewrap's `--unshare-all` enables network isolation; a subsequent
`--share-net` clears that flag. The flag controls both `CLONE_NEWNET` and the
call to `loopback_setup()`. Thus `--unshare-all --share-net` keeps the caller's
network namespace and skips bubblewrap's loopback setup. Option order matters.
Also, `--unshare-all` uses the user/cgroup *try* modes. Add explicit
`--unshare-user` if user namespace creation must fail rather than fall back.
[Parser](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L1876-L1923),
[clone flags](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L3064-L3085),
[loopback call](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L3275-L3278).

Loopback setup calls `if_nametoindex("lo")`, then explicitly opens
`PF_NETLINK/SOCK_RAW/NETLINK_ROUTE`; failure exits. This conflicts with an
inherited AF_UNIX-only filter. The function's `AF_INET` value describes a
netlink address message, not a direct Internet socket creation. Any extra
socket used by `if_nametoindex()` depends on the actual libc and needs tracing;
this report does not assert that AF_INET is mandatory.
[Exact network implementation](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/network.c#L136-L198).

Systemd creates namespaces before applying address-family, architecture and
syscall filters. A user manager can first create a user namespace to gain the
namespace-scoped privileges needed for network and mount setup. Explicit
`PrivateUsers=true` makes a userns setup error fatal; an implicitly requested
userns can fail with logging and continuation. Network setup itself can also
log and continue without isolation on privilege/support failure. Neither a
successful start nor `PrivateNetwork=true` readback proves isolation.
[Userns ordering and error handling](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/exec-invoke.c#L5830-L5877),
[network fallback](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/exec-invoke.c#L4632-L4654),
[late socket filter](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/exec-invoke.c#L6249-L6259),
[late architecture/syscall filters](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/exec-invoke.c#L6316-L6349).

Systemd's network setup creates or joins a retained network namespace and
attempts loopback configuration before saving its FD. It ignores loopback
setup's result, so inspect actual state if loopback availability is required.
[Namespace implementation](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/namespace.c#L3352-L3405).
For non-root user managers, PrivateUsers omits the host-root identity mapping.
It needs working unprivileged user namespaces, and local policy can still
prevent their use.
[PrivateUsers semantics](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/man/systemd.exec.xml#L2191-L2203),
[user-manager prerequisite](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/man/system-or-user-ns.xml#L9-L16).

AF_UNIX permits abstract and filesystem sockets. PrivateNetwork hides host
abstract sockets; filesystem sockets remain reachable through visible paths.
PrivateIPC does not isolate AF_UNIX. RestrictAddressFamilies only filters
`socket()`, leaving inherited descriptors, `socketpair()` and io_uring outside
that restriction. Alternate ABIs can also evade it; supported native ABI and
effective architecture filtering are required.
[Pinned socket semantics](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/man/systemd.exec.xml#L2000-L2018),
[IPC distinction](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/man/systemd.exec.xml#L2081-L2089),
[filter limits](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/man/systemd.exec.xml#L2377-L2393).
Systemd can skip address-family/architecture filtering when seccomp is
unavailable. Inspecting requested properties cannot detect that outcome.
[Skip paths](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/exec-invoke.c#L1736-L1759).

## Chosen composition and rejected alternative

The initial design proposed systemd `PrivateUsers=true`, `PrivateNetwork=true`
and bubblewrap `--unshare-all --share-net --unshare-user`. Source inspection
showed that this could work only after independently qualifying the unit's
network namespace. The actual mini probe rejected it: the unit started, but
systemd logged that network isolation was unavailable and continued in the
host namespace. A nested bubblewrap launch then failed userns creation. Never
use `--share-net` as a host fallback after that failure.

The compatible design keeps systemd's Internet-socket denial from birth and
allows `AF_NETLINK` only to the trusted bootstrap. Bubblewrap runs mandatory
`--unshare-all --unshare-user --disable-userns`; it initializes the new private
network namespace, then installs the sealed fixed payload filter with
`--seccomp` before exec. The payload and every Git child inherit AF_UNIX-only
socket creation. The bootstrap never receives a Git broker grant and parses no
repository objects. This deliberately distinguishes bootstrap setup from the
final parser boundary. No socket permission is widened after payload start.

The transient unit does not request `PrivateNetwork` or `PrivateUsers`; mandatory
bubblewrap creation and independently inspected kernel namespaces supply those
boundaries. A start failure or an unverifiable namespace withholds admission.
The host probe is no substitute for those checks in the unfinished factory.

Use these typed transient values. These are D-Bus values, not unit-file strings:

| Property | Signature | Value |
| --- | --- | --- |
| `RestrictAddressFamilies` | `(bas)` | `true, ["AF_UNIX", "AF_NETLINK"]` |
| `SystemCallArchitectures` | `as` | `["native"]` |
| `SystemCallFilter` | `(bas)` | `false, ["io_uring_setup", "io_uring_enter", "io_uring_register"]` |
| `SystemCallErrorNumber` | `i` | `1`, Linux `EPERM` |
| `TemporaryFileSystem` | `a(ss)` | `[("/tmp", "size=268435456,nr_inodes=65536,noswap,nosuid,nodev,noexec,mode=0700")]` |

The false syscall-filter boolean means denylist. The setter parses syscall
names permissively, so verify the actual enforcement too.
[Property signatures](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/dbus-execute.c#L1343-L1396),
[PrivateUsers setter](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/dbus-execute.c#L2136-L2168),
[syscall setter](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/dbus-execute.c#L2830-L2899),
[tmpfs setter](https://github.com/systemd/systemd/blob/b3d8fc43e9cb531d958c17ef2cd93b374bc14e8a/src/core/dbus-execute.c#L3930-L3953).

Deny all three io_uring syscalls at unit scope and again in the final measured
payload filter. Allow namespace/mount setup until bubblewrap finishes, then
deny namespace changes, mount APIs and process inspection escapes before any
Git parser runs. Bubblewrap applies supplied seccomp programs late, after
setup. Its inherited systemd filter remains in force throughout.
[Bubblewrap final filter](https://github.com/containers/bubblewrap/blob/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe/bubblewrap.c#L3610-L3624).
Reject inherited Internet sockets and io_uring descriptors/mappings. In
particular, an existing ring can use shared submission queues, so denying
only `io_uring_setup` is insufficient.
[io_uring programming model](https://man7.org/linux/man-pages/man7/io_uring.7.html).

Before any broker grant, the trusted supervisor should:

1. Bind the inert sealed root to the captured manager, invocation, pidfd and
   cgroup. Retain opened network-namespace FDs for the supervisor, manager and
   inner worker. Require the worker namespace differs from both host-side identities.
   Check namespace type and compare `st_dev/st_ino`, retaining the FDs through
   stop. Open namespace FDs preserve identity and lifetime.
   [Kernel interface contract](https://man7.org/linux/man-pages/man7/namespaces.7.html).
2. Reject namespace-joining overrides or reuse across units. Check only the
   isolated loopback interfaces/routes are present using bounded inspection of
   the captured process's network view. Distinct namespace identity alone does
   not prove absence of a veth or other route.
   [Network namespace scope](https://man7.org/linux/man-pages/man7/network_namespaces.7.html).
3. Hold the measured inner worker inert. Require its network namespace equals
   the retained private worker namespace, and its required user/mount/PID namespaces and
   UID/GID maps match the launch policy. Verify actual inherited tmpfs
   byte/inode ceilings, `noswap`, mount flags and sole writable-work mapping.
   A self-reported namespace identifier is insufficient.
4. Admit only the pathname broker socket through the minimal filesystem.
   Exclude manager/agent/DNS sockets and inherited host socket or directory
   descriptors. Recheck namespace identity on each helper admission. Any
   missing proc visibility, mismatch, setup failure or ambiguous observation
   revokes the attempt. Keep the same-UID host-process trust assumption explicit.

## Inert mini probes

At 03:11 EDT on October 21, three one-shot user units ran under ten-second
runtime limits, 256 MiB memory limits and 32-task limits. Each was collected and
confirmed inactive with no MainPID. They opened no network connections.

- The old AF_UNIX-only unit plus `bwrap --unshare-all` failed with
  `Failed to create NETLINK_ROUTE socket: Address family not supported by protocol`.
- The `PrivateUsers=yes, PrivateNetwork=yes` unit returned success but retained
  the host network namespace. Its journal explicitly reported that systemd was
  proceeding without network isolation. AF_INET, AF_INET6, AF_NETLINK and
  AF_PACKET creation were denied; all three io_uring calls returned EPERM.
  Socket denial therefore did not establish private-namespace isolation.
- Nested bubblewrap with `--share-net` failed to create a user namespace. No
  weaker retry or broker grant followed.

At 03:14 EDT, a distinct one-shot unit allowed only AF_UNIX and AF_NETLINK
creation during setup, with all io_uring calls denied. Mandatory bubblewrap
created distinct network, user, mount and PID namespaces and installed a sealed
AF_UNIX-only classic BPF filter before an inert Python payload. The payload
created a Unix socket; AF_INET, AF_INET6, AF_NETLINK and AF_PACKET all returned
EAFNOSUPPORT. The unit was collected and verified inactive. These are
self-reported payload namespace observations for compatibility, not trusted
supervisor attestation or a production grant.

Private mission receipts `git-bootstrap-netprobe-r1.json` and
`git-bootstrap-netprobe-r2.json` preserve exact arguments, errors, namespace IDs
and terminal observations. Installed versions were bubblewrap 0.11.1 and systemd
259.5-0ubuntu3.4. They are distribution binaries, not asserted identical to the
upstream commits above. The source test `TestGitPayloadSeccompActualBubblewrap`
uses the Go-produced sealed payload filter with the actual helper.
`TestGitNetworkBoundaryActualPrivateNamespace` independently opens and retains
the actual private namespace from the host, verifies original-peer matching and
host-peer refusal, and rejects the worker after exit. It deliberately shares
the fixture PID namespace to locate its host PID, so it does not qualify the
production PID boundary. Both tests use the opt-in
flag is `SFL_GIT_BOOTSTRAP_NAMESPACE=1`.

## Remaining aggregate qualification

Qualification must use exact shipped binaries, libc, kernel and manager with
the complete production properties, immutable runtime and descriptor handoff.
Existing owner authority covers inert offline tests. Credentials, model use,
provider policy, deployment and activation retain their separate gates.

- Compose the implemented supervisor-opened network namespace FD checks with
  manager/unit, full namespace and route validation;
  prove permitted broker access and denied host abstract-socket access.
- Inspect actual seccomp enforcement, alternate ABI refusal and absence of
  inherited Internet sockets, io_uring descriptors and ring mappings.
- Exhaust work bytes/inodes, confirm no swap and unchanged noexec/nosuid/nodev
  flags after bind; prove every required namespace, helper and library identity.
- Inject unavailable userns/netns/seccomp, wrong namespace identity, denied proc
  inspection and launch cancellation. Withhold every grant and complete the
  owned stop protocol without a host-directory fallback.

The working bootstrap is one prerequisite, not a completed acquisition factory.
