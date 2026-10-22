# Private Git HTTP broker

`ServeGitReadSocket` is a Linux-only local transport for the [scoped Git reader](GIT-READ.md). It accepts an explicitly supplied exchanger and mandatory trusted peer-admission check. It does not launch Git, start a controller, acquire credentials or install a service. The caller owns its lifetime and must hold the credential generation until all exchanges and response bodies stop.

The caller supplies an empty, private, existing service-owned directory. The broker writes an exclusive used-attempt marker and creates a mode-0600 Unix socket. Used directories are refused, including after failure. Shutdown removes only its own unchanged socket inode and retains the marker. A foreign replacement is preserved and reported as cleanup failure. The parent directory and host service identity must be protected separately.

Before reading SOCKS bytes, the broker gets the kernel PID, UID and GID, obtains the connection's peer pidfd and checks liveness. The mandatory `Admit` callback must bind that peer to the measured Git HTTP helper and the exact protected acquisition unit. It runs before SOCKS and again immediately before exchange. There is no UID-only default or automatic trust in the socket path. Missing kernel support or admission evidence refuses access.

An unexported connection capability now keeps the original SO_PEERPIDFD and
kernel PID/start/boot identity until the handler stops. The factory-only
`retainGitBrokerPeer` duplicates that exact descriptor with CLOEXEC while the
connection is live; it never reopens a PID to manufacture a replacement. The
factory supplies and separately qualifies the invocation against systemd.
Forged public PID/UID/GID fields or metadata without the private capability
cannot retain a peer. Revocation and retention share a lock, so a closed handle
stays invalid even after its old descriptor number names a live process again.
A retained duplicate belongs to the factory; it still needs liveness, measured
helper/library, ancestry, namespace and owning-unit checks at admission. This
capability alone is not a source grant.

The private [process boundary](GIT-PROCESS-BOUNDARY.md) now checks actual
worker/helper root, executable/maps, capabilities, six namespaces, cgroup,
network tables and retained ancestry. It still requires factory composition
with exact manager/unit ownership, mounts, descriptors, launch/filter proof
and physical stop before any production broker admission.

The kernel's [pidfd fdinfo implementation](https://github.com/torvalds/linux/blob/v6.17/fs/pidfs.c#L165-L250)
reports `Pid` in the procfs instance's PID namespace, and its poll operation
reports process exit. The supervisor must use a trusted procfs view consistent
with its host-side peer/manager observations; an absent, nonpositive or mismatched
PID refuses. The [pidfd API](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)
also documents poll-based exit observation. The network boundary separately
requires the kernel's [namespace-type ioctl](https://man7.org/linux/man-pages/man2/NS_GET_NSTYPE.2const.html).
The source/test contracts do not replace the factory's host namespace and
manager qualification.

The callback is a trusted, read-only, bounded capability with a five-second check context. It must verify the production helper, libraries, process identity and protected unit membership. This module does not implement that production policy. Same-UID processes, mutable host files, process-identity races and unit escape remain the supervising factory's responsibility. Peer checks in a test using the existing agent cgroup do not qualify an isolated acquisition unit.

SOCKS5h is terminated locally. Only no-auth CONNECT to the exact domain `sfl.invalid`, port 80, is accepted. The broker never resolves or connects to that target. Git uses the fixed URL `http://sfl.invalid/hemsoft-dev/set-it-free-loop.git` through `http.proxy=socks5h://localhost/ABSOLUTE_PRIVATE_SOCKET`. Upstream requests go solely through the supplied typed exchanger, which injects the credential on the fixed HTTPS GitHub origin. Git receives neither token nor upstream authentication headers.

One HTTP/1.1 request is accepted per connection. Only the exact advertisement GET and upload-pack POST paths are permitted. A bounded raw-header check refuses duplicate headers, folded lines, ambient authorization/cookies, unknown headers, absolute targets and combined Content-Length/Transfer-Encoding framing. Go's HTTP parser handles the accepted request and chunked body. POSTs require the Git request media type. Only protocol 0 or 2 is supported. A valid `100-continue` request receives its interim response after admission and header validation.

Identity and gzip request bodies are supported. Both encoded and decoded bodies are limited to 4 MiB. Truncation, gzip checksum errors, expansion past the limit and trailers refuse dispatch. No body is streamed upstream before complete request validation. The returned representation is identity bytes with a fixed Git content type. It uses chunked transfer framing and closes the connection after completion. Errors while streaming leave an incomplete transfer so an HTTP consumer observes failure. Upstream headers and error messages are never forwarded.

The broker has a two-minute lifetime ceiling, 90 seconds per connection, ten seconds for initial input, at most 64 connections and eight active handlers. Headers are limited to 32 KiB. Accepted response payloads share a 64-MiB aggregate budget, including bytes received before a later client-write failure. Request-byte audit counts cover accepted encoded POST bodies. Rejected inputs remain independently bounded by the connection and per-body limits. These are transport bounds, not a proof of aggregate CPU, memory, process, disk, inode or decompressed Git object limits.

Cancellation closes intake, active Unix connections and current upstream bodies, then joins connection handlers and owned cancellation callbacks. The returned audit records counts and stopped/failed state, with no source text, credential, URL, peer identity or remote error. An ordinary caller-driven stop returns the context error alongside the stopped audit. Trusted exchangers, admission checks and body Close operations must honor their bounded contracts. The broker cannot interrupt an arbitrary injected function that ignores them.

## Validation and remaining composition

The actual installed Git 2.53.0 HTTP client fetches over the Unix SOCKS route to the scoped reader and a local TLS `git-http-backend`. Each protocol v0/v2 case fetches the exact synthetic head and base, finds the unique expected merge base, passes full strict fsck and compares all 14 fetched objects byte for byte. The fake upstream checks the synthetic reader Basic identity and removes it before invoking the Git backend.

The Git test launcher installs an inherited syscall filter, positively checks that both Internet socket families return EPERM, and then execs Git with an empty private HOME and explicit configuration. The broker's test admission checks the actual HTTP helper inode/hash, its kernel credentials, seccomp status and ancestry to the launched Git root. These tests prove this local route works for the measured producer. The test filter is not the production sandbox, and the test admission is not a protected acquisition-unit policy.

An actual Unix-connection test retains the original peer pidfd, checks public
metadata forgery and missing-capability refusal, races retention with shutdown,
proves the caller-owned duplicate survives, and deliberately reuses the spent
descriptor number without reviving the revoked handle.

Other cases cover chunked, gzip, 100-continue and binary payloads; malformed/ambiguous framing, wrong routes, caller credentials and peer refusal before dispatch; gzip expansion/truncation; active-handler capacity; used roots and foreign path preservation; and cancellation during SOCKS, headers, input, exchange and output. An aggregate-overflow regression first delivered 24 MiB of the second response with a successful HTTP EOF. Chunked response framing now makes that same overflow an HTTP transfer failure.

No candidate source is checked out or executed. Production acquisition still needs protected peer policy, measured immutable Git/helper/library inputs, enforced aggregate resources, fresh quarantine, exact full-parent-graph/object/merge-base verification, retained immutable store publication and complete supervised shutdown. The [source factory](GIT-FACTORY.md) and [service assembly](GIT-SERVICE.md) provide the source composition. Aggregate physical qualification, App eligibility, provider/account support and owner activation remain separate gates. No real credential/profile/key, live GitHub/provider backend, paid inference or retired automation was used or enabled.

The internal [quarantine verifier](GIT-QUARANTINE.md), in-unit capture/export
and retained-store composition have source implementations in the factory.
Synthetic algorithm and fake-owner tests do not qualify the complete protected
production acquisition path.
