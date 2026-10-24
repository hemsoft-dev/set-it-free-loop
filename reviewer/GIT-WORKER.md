# Private Git acquisition worker

October 21, 2026 EDT. This worker-side component belongs to the
[concrete source factory](GIT-FACTORY.md) and [service assembly](GIT-SERVICE.md).
Its source does not qualify a production launch or live source capability.
The [fixed executable and bootstrap layout](GIT-BOOTSTRAP.md) and independent
inspection code exist; aggregate physical qualification remains incomplete.
On mini, the requested
systemd tmpfs and sealed inner bubblewrap exec currently fail. See
[GIT-LAUNCH-CONSTRAINTS.md](GIT-LAUNCH-CONSTRAINTS.md). No credential, App access,
model inference or host policy change is involved in these tests.

`runGitAcquisitionWorker` owns its supplied descriptors even on rejection. It
requires a canonical sealed manifest whose hash equals the binding's runtime
hash, a sealed executable matching the manifest and the actual running inode,
an original supervisor pidfd and a write-only export pipe. It closes the image
and manifest before any Git child. Its nonblocking, close-on-exec export
duplicate supports Go poller cancellation even when the receiver stops reading.
The source files and descriptor numbers are private bootstrap inputs. The fixed process entry point owns explicit inherited roles; there is no
model-selectable command/path interface or automatic service launcher.

The worker opens a fresh named Unix seqpacket connection and authenticates
[the supervisor's original kernel identity](GIT-GRANT.md), including across
private PID namespaces. It clears dumpability, requires no-new-privileges,
seccomp and zero inheritable/permitted/effective/ambient capabilities, then
announces readiness. These self-checks do not prove the fixed filter, namespace,
cgroup, mount or runtime policy. The trusted supervisor must independently
qualify those before issuing the worker grant. It must also enforce zero
bounding capabilities and the complete descriptor/helper policy.

No repository is created or fetched before receipt of the exact binding's
canonical worker grant. The authenticated grant's invocation, boot and start
ticks must match this worker. Its host PID may be invisible in the worker's PID
namespace; the supervisor owns that original-pidfd and manager correlation.
The supervisor constructs the packet's full host identity from the connection's
original SO_PEERPIDFD and durably hashes that exact recipient before sending.
The worker authenticates that supervisor through its inherited original pidfd.
Comparing the packet's host PID with the worker's namespace-local PID would
reject valid workers. An actual private-PIDns fixture checks the host-observed
kernel `NSpid` mapping and the child's own PID, reads the canonical worker grant,
and exercises the same local identity check. Host and local PIDs differ while
boot/start/invocation match; changed boot, start or invocation refuses. This
inert fixture does not qualify the separately failed sealed inner-worker exec.
Missing, malformed, wrong-binding or wrong-recipient grants fail without source
work. There is no grant retry or substantive partial success.

`acquireGitQuarantine` requires a fresh private workspace and writes a spent
marker before invoking Git. It constructs exact bare metadata itself, without
init templates, candidate configuration, working trees or local object copies.
One fixed HTTP URL passes through the job's Unix SOCKS broker. Fetch requests
both immutable tips with complete history. It disables other protocols,
credential helpers, redirects, hooks, fsmonitor, commit graphs, maintenance,
tags and submodules. Its explicit environment omits inherited proxy/credential
settings and disables terminal prompts. Fetch, index-pack, full strict graph
verification, snapshot capture, diff and blob materialization must all execute
inside the same bounded acquisition unit. Post-fetch inventory checks do not
bound parsing work that occurred during fetch.

Any failed or completed attempt leaves that workspace spent. The worker does
not erase or reuse partial packs. The supervisor's external durable grant
journal, original pidfds and unit-stop recovery retain ownership across crashes;
the workspace marker is an additional local rejection, not a crash-proof
replacement for that journal.

Only bounded plain [materialization data](GIT-UNIT.md), or instead a
[capture refusal record](GIT-MATERIALIZATION.md#capture-refusal-record),
crosses the export pipe. A refusal is a complete export, after which the
worker exits normally.
The worker does not return a host Git directory or publish an ObjectStore. The
supervisor must authenticate the producer and binding, revoke and join the
broker, prove whole-unit termination, and retain the stopped archive authority
before publishing source or releasing the credential generation. A well-formed
export or normal worker exit alone is insufficient.

## Offline validation and its limits

The focused acquisition tests use real Git and the fake TLS installation
transport behind the actual Unix broker. SHA-1 and SHA-256 cases fetch exact
tips, run full verification and materialize source. Deleting both trusted
synthetic repositories before restoring and reading the plain export detects
host Git fallback. Missing tips, a wrong merge base, exceeded object bounds,
invalid destinations/workspaces and reused attempts reject without a usable
export.

The process tests execute the actual compiled Go test image from its retained
sealed inode, outside bubblewrap. They install the fixed final BPF on every
thread and use actual pidfds, sockets, grants and export pipes. The fixture
clears inherited capabilities before exec using `/usr/bin/setpriv`; the host
session has `CAP_WAKE_ALARM` inheritable, which the first worker run correctly
rejected. This changes only the fixture child. No host policy is altered.

These sealed child cases cover both object formats, wrong supervisor, absent
or substituted grants, wrong recipient and an ordinary rather than sealed
executable. Readiness occurs with an untouched workspace. A large synthetic
export fills an unread pipe; cancellation interrupts the write, the child exits,
and the truncated bytes cannot be restored. The runtime metadata is explicitly
synthetic and broker admission is a test callback. These results qualify neither
the inner sealed-exec prerequisite nor production unit/runtime/helper/resource
containment. The separate opt-in sealed bubblewrap test retains its actual
failure and cannot be counted as passing.
