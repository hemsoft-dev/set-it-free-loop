# VM source implementation assessment

The [existing host acquisition path](GIT-LAUNCH-CONSTRAINTS.md) remains
unavailable on mini. Its exact bounded noswap mount and sealed worker exec
failed under the required host confinement. A successful guest probe does not
change those results or authorize a weaker host launch.

A credential-free inert probe found an alternative environment. The measured
`7.0.0-38-generic` guest created the exact 256 MiB, 65,536-inode noswap mount.
The same sealed image executed through both proc and execveat as UID 1203 with
zero capabilities in new namespaces and wrote to the work mount. Mode and
ownership were observed. The paired host receipt proves that run's unit
stopped and its original cgroup was removed. See the
[reproducible diagnostic](scripts/diagnostics/git-vm-source/README.md) and
[actual producer record](scripts/diagnostics/git-vm-source/testdata/README.md).
This establishes feasibility of those guest prerequisites only. No Git objects,
credentials, provider clients or model inference participated.

## Existing interface to preserve

`PollSource` already gives the controller four operations: capture an immutable
snapshot, restore its admitted packet, release a capture and release a job.
The source implementation owns resources until release. The retention hook is
separate. This is a real seam for host and VM adapters; the polling controller
does not need a transport selection or a new source tool for models.

The current acquisition worker already exports bounded plain materialization:
canonical snapshot evidence, a complete allowed-path/revision table and bounded
blobs with Git object hashes. The host receiver verifies the frame without Git
parsing. After qualified physical stop, that materialization serves the existing
read/search interface without keeping a Git parser alive through model sessions.
A VM implementation can reuse this export and its verification rather than
introducing live guest RPC into every model read.

## Source protocol implemented

The private `gitVMManifest` and `gitVMRequest` define purpose
`isolated-git-source-vm`, separately from provider and host acquisition manifests.
Named pins bind QEMU, kernel, initrd, BIOS, QEMU runtime, original host namespace
helper and bootstrap libraries, Git runtime, sealed worker and fixed payload
filter. The materialization binding's runtime hash is the complete source
manifest hash. Release, attempt and immutable snapshot remain independently
bound. There is no provider, auth lease, credential, URL, command or host path
selector in the request. Bootstrap library names are fixed trusted pin metadata.

`writeGitVMExport` prefixes the existing bounded plain materialization with a
canonical request-bound source header. Its Git parsing belongs inside the protected
source guest. `receiveGitVMExport` validates the header and complete
materialization, closes intake on cancellation including during the header,
and requires exact EOF. It returns only private staging. A valid header does
not authenticate a guest or prove shutdown, broker drain or source eligibility.
Neither the existing host receiver nor provider result decoder accepts this
source envelope. No existing host journal or release changes mechanism.

Tests use the actual installed Git producer against small synthetic SHA-1 and
SHA-256 repositories, then read sealed exported blobs. They reject request,
release, snapshot, purpose and pin mismatches, provider-shaped records, bare host
exports, oversized/truncated frames, unknown fields and trailing bytes. These
are protocol tests on the host, with synthetic asset pins. They establish no
guest execution, measured pin provenance, credential handling or isolation.

## Source read channel implemented

The private [Git read channel](git_vm_read_linux.go) now supplies the host and
guest transport component. Its distinct `source-vm-git-read` binding fixes the
materialization attempt, snapshot, release/runtime hashes, channel nonce and
retained credential-generation number. None of those fields establishes
admission. The host requires a fresh original-VM/channel/generation check
before each upstream exchange and after its response body closes.

Only typed advertisement and upload-pack operations for the existing fixed
repository are representable. The wire carries no token, URL, header, auth
profile or write operation. The host retains the existing reader-generation
borrow through response Read and Close, including cancellation. The guest
buffers one bounded opaque response until its terminal success record. Lost,
stale, oversized or failed replies spend the channel without returning partial
data or replaying a request.

A bounded host intake reader remains active during upstream work. Guest peer
closure cancels the host context and closes its upstream body, while ownership
remains held until Read and Close finish. One prefetched request is allowed;
further pipelining cancels the channel. Guest input intake is serialized with
exchange, so queued uploads do not allocate body buffers before their turn.
Queued cancellation closes that caller's unread body and leaves the active
exchange alone.

The component allows at most 32 exchanges, 16 MiB total decoded request bytes
and 128 MiB total response bytes per channel. Individual requests retain the
existing 4 MiB limit; responses retain the 4 MiB advertisement and 64 MiB
upload-pack limits. Reply chunks contain at most 32 KiB. The host has a
two-minute lifetime and each client exchange has a 90-second deadline covering
input intake, queueing and transport. These bounds are additional transport
limits, not permission to exceed an acquisition's existing bounds.

The [transport tests](git_vm_read_linux_test.go) use local channels and a local
TLS backend with synthetic installation credentials. They check exact bytes,
foreign/extended requests, stale admission, malformed replies, cancellation,
owned input/response drain, close errors, queue isolation and limits. They do
not prove a measured guest, helper ancestry or original host VM admission.
The transport is not yet connected to a source factory or trusted startup.


## Remaining host composition and qualification

Connect the distinct source VM purpose to a production host launcher and
stop-proof type. A source receipt
must never qualify a provider worker or release a provider auth lease. Preserve
the existing host path and journals; do not reinterpret them as VM evidence.

The source guest runs fetch, index-pack, graph/fsck/diff/tree/blob parsing under
the finite acquisition bounds. Guest root prepares exact storage and immutable
runtime, then starts a separate unprivileged source role.
Models retain only the existing read/search capability. There is no new model
shell or candidate-code execution contract.

The guest has no NIC, host filesystem share, provider profile or GitHub token.
Its fixed HTTP helper reaches only a private, job-bound host read capability.
The host credential owner keeps App signing and token generations; the broker
retains exact repository/snapshot/request bounds, byte/call/time limits and
revocation/draining. The guest authenticates the actual helper. Production host composition must
bind that evidence to the pinned guest and original host VM invocation.

Do not accept staged materialization until broker revocation/draining and fresh
whole-VM stop are proved. Persist acquisition/receipt/publication intent before
each irreversible handoff. Recovery reopens original unit, channel, generation
and archive records without replay; unknown work remains owned and unavailable.
Connect the resulting `PollSource` adapter through explicit trusted release and
startup packages. No constructor installs credentials, promotes a build or runs
work. No existing release silently changes its acquisition mechanism.

Extend the actual pinned-Git guest qualification to hostile repositories,
storage/resource exhaustion, cross-job access, cancellation, lost output and
process/restart recovery before claiming this implementation complete. Host
unit confinement and kernel/guest/runtime pins need their own evidence; the
inert diagnostic is not that proof. The host's old failed prerequisites remain
failed even if a new VM implementation later qualifies.

Provider/account/paid-overflow evidence, actual comparative evaluation, final
GitHub observations, trusted promotion and separate owner activation remain the
[existing gates](../docs/research/2026-10-21-alpha-activation-checklist.md).
This assessment and diagnostic grant none of them.

The separate [source guest and image producer](GIT-VM-GUEST.md) now implement
root-mediated Git reads and a sealed UID/GID 1203 worker. A contained,
credential-free actual-Git test qualifies the guest data path. Production host
VM ownership, durable stop/recovery, startup composition and aggregate
qualification remain implementation work. Guest completion does not authorize
host staging publication or release an auth lease.
