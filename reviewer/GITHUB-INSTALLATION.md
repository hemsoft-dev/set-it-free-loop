# Selected installation credentials

`AcquireGitHubInstallation` supplies an explicit credential-acquisition boundary
for [issue #176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176).
It takes a trusted App JWT and a trusted HTTP transport. It does not read the
environment, locate private keys, sign JWTs, install an App or start polling.
This source is tested only with synthetic credentials and local fake TLS.
The owner freeze still prohibits actual credential acquisition and activation.

## Checks before returning a transport

The handshake uses the fixed `https://api.github.com` origin, pinned API version
`2026-03-10`, ten-second request timeouts and a ninety-second overall deadline.
It checks the authenticated App ID, the repository's selected installation,
organization account, installation ID, App ID and suspension state. Installation
permissions must be exactly metadata read, contents read and pull requests write.
There is no organization wildcard or inherited retired identity.

The token request explicitly names repository ID `1169772257` and the role's
permission map. The response must contain that one canonical repository and
organization, selected access, the exact permissions and a bounded expiry.
Using that token, the handshake checks the accessible repository collection,
the App slug's bot account against the enrolled immutable bot ID, and the
canonical repository's ID and organization. A final App-authenticated lookup
rechecks the installation. Any mismatch, extra permission/repository, suspension,
unfinished pagination or malformed document fails without returning a transport.

These are multiple GitHub reads, not an atomic snapshot. Revocation or changes
after a read can still cause subsequent requests to fail. Admission and publication
must retain their existing access, request generation and revision checks.
The receipt records the checked enrollment, role, check time and token expiry.
It does not establish native reviewer-picker support or authorize activation.

## Separate roles

| Transport | Requested permissions | Allowed requests |
| --- | --- | --- |
| `GitHubReader` | metadata read, contents read, pull requests read | GET within this repository |
| `GitHubPublisher` | metadata read, pull requests write | Repository GET, POST review/PR comment, PUT existing review |

Only the publisher receives write credentials. Discovery and future immutable
object acquisition receive the reader. Neither object may enter a provider
worker, source reader or model prompt. The publication adapter still constructs
advisory COMMENT bodies, verifies trusted snapshots and reconciles uncertain
writes. The transport restricts destinations and methods; it does not replace
those document and request-generation checks.

Each transport refuses a different origin, repository, userinfo, fragment,
ambiguous path, overridden host or ambient Authorization header before HTTP.
Reader writes and merge/delete/other publisher write routes are refused.
Write requests with replayable bodies are refused. Acquisition never follows
redirects or retries token POSTs. A failed or uncertain acquisition requires a
new explicit qualification attempt by the trusted controller; it is not retried
inside this library. No token is returned to the caller as text.

Formatting and JSON forms are redacted for pointer and value handles. Copies
share credential ownership. `Close` clears the owned token buffer and prevents
new requests. Already admitted requests retain their own header until their
trusted transport finishes; closing is not cancellation or remote revocation.
The token is never attached to the caller's request or returned Response.Request.
Go's immutable JSON/header strings cannot be reliably erased; the private
transport must not log or retain them. Caller-owned JWT bytes remain the caller's
responsibility. There is no on-disk credential cache here.

New requests fail one minute before token expiry. No background renewal or
ambient credential fallback exists. The future runtime must explicitly acquire
and swap newly qualified role transports, close old handles and treat failed
qualification as a phase gate. A receipt cannot stand in for provider eligibility,
isolation, billing or owner activation evidence.

## Protocol sources and offline validation

The request and response fields follow GitHub's current primary references:
[authenticated App and installation/token endpoints](https://docs.github.com/en/rest/apps/apps),
[installation repository collection](https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation)
and [bot account lookup](https://docs.github.com/en/rest/users/users#get-a-user).
GitHub permits PR comment creation with
[pull requests write permission](https://docs.github.com/en/rest/issues/comments#create-an-issue-comment),
so this alpha does not request issues write access.

Real local TLS tests exercise the seven-request handshake and both roles,
then compose the qualified publisher with the actual acknowledgment adapter.
They reject wrong App/installation/organization/bot/repository identities,
overbroad permissions, extra repositories, stale/long expiries, changed final
installation state, redirects, uncertain token POSTs, duplicate/trailing/oversized
JSON and incomplete pagination. Additional checks cover pre-HTTP destination,
method, expiry and cancellation rejection, redaction and shared-handle disposal.
These are fake GitHub responses, not a captured live installation qualification.

```sh
go -C reviewer test -race -count=1 -run TestGitHubInstallation .
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

JWT signing/key storage, role refresh composition, qualified immutable object
acquisition, packaged runtime startup and actual installation qualification still
need integration. No live GitHub App token was obtained during these tests.
