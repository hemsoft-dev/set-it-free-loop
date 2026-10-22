# Bounded provider HTTP bridge

`ProviderEgress` implements the HTTP guard for the isolated launcher planned in
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
It opens no listener or network connection on construction. It has no default
transport, provider endpoint roster, credentials or activation authority. Tests
use local TLS backends, synthetic auth and explicit test transports.

Construction rejects caller HTTP tracing contexts before any upstream work.
Otherwise inherited `WroteHeaderField` callbacks could observe provider bearer
values when the guard builds its credentialed HTTPS request. Cancellation and
deadlines remain supported; callbacks that observe headers are not inherited.

The trusted supervisor captures an `EgressPolicy` for one validated `Session`.
The policy contains exact lowercase authorities, GET/POST paths, optional literal
canonical query strings, header names and request/byte limits. Request kinds are
inference, refresh, metadata and local negotiation. The bridge copies that policy and binds its
private audit to both the session hash and policy hash. Candidate text and model
output cannot select an authority, route, query value or header policy.

Default maximums are separate from the engine's session ledger:

| Bound | Maximum per phase |
| --- | --- |
| Inference requests | The captured MaxTurns, at most24 |
| Refresh requests | 2 |
| Metadata requests | Codex5, Claude4 |
| Local negotiation requests | Codex1, Claude0 |
| Total requests | Codex MaxTurns+8, Claude MaxTurns+6 |
| One request payload | 8 MiB |
| One response payload | 16 MiB |
| Total observed request/response payload | 32 MiB |
| Concurrent upstream requests | 1 |
| Intake handlers including the active request | 4 |
| Deadline | The captured phase deadline, at most5minutes |

The supervisor may lower these bounds. Every attempt reserves its request kind
before reading the body. Up to three additional handlers can wait for the
active request without reading their bodies. Intake overflow cancels the phase;
waiting observes both the phase and client deadlines. Request budgets are
reserved when a handler acquires the single forwarding slot. Every upstream `RoundTrip` is counted before invocation,
including a response loss that may have incurred usage. A provider429 response
can reach the client, but client retries consume the same finite counters.
Refresh does not consume an inference slot, and neither count is a session count.
The bridge does not infer billed tokens from these counters.

The bridge denies CONNECT, actual WebSocket upgrades, absolute proxy URLs, uncaptured
authorities/routes/queries, path aliases/traversal, request trailers and forbidden
headers. Cookie, API-key and proxy-auth headers cannot enter the allowlist.
An explicitly captured Codex-only GET route may use `local_negotiation` to
answer one empty, well-formed WebSocket attempt with HTTP426. It validates the
upgrade headers, version13 and16-byte key, rejects payloads and never forwards
that request or its credentials. The client can then make a separate HTTP POST
to its captured inference route. A second negotiation fails the phase. The
bridge performs no upgrade, dial or alternate-provider selection. This behavior
is an offline compatibility path, not permission to enable live traffic.

Bearer payloads remain opaque; actual subscription identity validation is still
the launcher's responsibility. Content-Length must agree with parsed framing,
then the bridge rebuilds framing from the bounded captured body. It forwards only
allowlisted headers to the captured HTTPS endpoint and requests identity encoding.

Responses are buffered through a strict bound before any provider payload reaches
the client. Redirects, upgrades, compression, trailers, incomplete responses and
non-JSON/non-SSE payloads fail. Only response Content-Type and the bounded body
are forwarded. This deliberately does not provide a general tunnel. The pinned
clients must be qualified for this HTTP transport and buffering behavior; a
client requiring WebSockets or uncaptured dynamic routes remains unqualified.
Do not relax the contract to make a live probe pass.

`NewPublicEgressTransport` supplies the concrete outbound transport. It ignores
ambient proxies, disables HTTP2 and connection reuse, supplies no replay body,
and makes no redirect request. Disabling reuse prevents net/http from hiding an
automatic retry of an idempotent request on a stale pooled connection. It uses
TLS1.3 or later and verifies the original hostname. A resolver/dialer wrapper
accepts only captured authorities on port443, rejects mixed public/private DNS
answers and dials the checked numeric address without another lookup. Loopback,
private, link-local, Tailscale/CGNAT, documentation, benchmark and IPv6 transition
addresses are denied. There is no fallback to another authority, proxy or IP.

The optional `NewProviderEgressServer` supplies HTTP intake deadlines and a16KiB
header limit. It does not listen, authenticate peers or bound accepted connections.
The [native VM assembly](NATIVE-VM.md) supplies the private provider stream,
bounded guest proxy, separate guest roles and captured policy/launch checks.
Its source includes client-side TLS/proxy configuration and a QEMU launch with
no NIC or host share. This HTTP component alone supplies none of that OS
packaging. The complete deployed assembly must still pass physical qualification
that the worker has no alternate route or access to host home, systemd, SSH/Docker
sockets, private networks, other jobs or publisher credentials. Choosing a custom transport transfers its network and retry
guarantees to trusted supervisor code; the production transport above is tested
separately from the fake transport used by most bridge tests.

`Close` closes admission, cancels in-flight local IO and waits for every admitted
handler, including queued response writers and unread-body closure. Admission
and shutdown share one lifetime barrier; an active request finishing cannot hide
a queued handler. Bodies close exactly once, and waiting handlers never read
them. An uncooperative
transport, queued response writer or body close returns `ErrProviderEgressPending` and an unstopped receipt; this cannot
release a possibly live worker lease. A stopped receipt can still record failed
or exhausted work. Before accepting a model report, the supervisor must call
`ValidateCompleted` with the original session and policy. It requires at least
one inference request, complete forwarded responses, matching local negotiation
request/response counts, consistent total counters and no
failure or exhaustion. Missing/stale receipts cannot become measured zero usage.
Forwarded and upstream-response counts exclude the local426; total requests
include it. Codex metadata has a separate ceiling of five based on the pinned
client capture after telemetry suppression; Claude retains its ceiling of four.
Neither default supplies a route policy. The stopped receipt is immutable and Close is idempotent. A stopped local bridge
does not establish that a remote provider stopped computing after a lost response.

Audit fields contain counts, hashes and status only. Do not store or publish raw
headers, auth, request bodies, responses or backend errors. Preserve the private
receipt even on failure and never refund possible provider work based on a
network error. [Private provider evidence](PROVIDER-AUDIT.md) now connects the audit to engine
artifacts, exact durable attempt recovery and publication checks. The native
assembly implements the channel, durable launch binding and protected auth
transfer. Fake-backend tests exercise those paths; complete exclusive-routing,
resource and arbitrary-crash qualification remain unproved. Live eligibility
and activation remain separate gates.

Validate with the module's vet/full race commands. Focused tests can use:

```sh
go -C reviewer test -race -count=1 -run 'TestProviderEgress|TestPublicEgress' ./...
```

Tests exercise real local HTTP/TLS requests for both providers, finite retries,
auth versus inference budgets, exact/overrun byte caps, hostile routes/headers,
redirect/compression/trailer denial, incomplete IO, uncertain stop, serialized concurrent startup, intake overflow
and cancellation and canary-free audits. A real TLS connection-fault fixture
confirms that the concrete transport makes no hidden pooled-connection retry.
DNS tests replace resolver/dialer dependencies and never contact public endpoints.
