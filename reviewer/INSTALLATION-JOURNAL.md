# Durable installation acquisition

`GitHubInstallationJournal` records explicit acquisition attempts for the
selected-installation credentials in [GITHUB-INSTALLATION.md](GITHUB-INSTALLATION.md).
It is source work for [issue #176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176).
It uses synthetic keys, fake GitHub credentials and local TLS in qualification.
No actual key, account/profile, App, installation or activation is used.

## Boundary and states

The constructor takes an existing private SQLite `Store` and an explicit clock.
It creates a separate metadata table and private reader/publisher lock directories.
It performs no HTTP or background work. The existing service owner lock and
provider auth stores are separate. Each role holds a private cross-process flock
for its complete acquisition or recovery operation, without holding a SQL
transaction during HTTP or trusted recovery callbacks.

`Acquire` binds the store's exact enrollment and the requested reader/publisher
role. It creates a fresh attempt before HTTP and commits `mint_pending` before
the non-replayable token POST. The underlying handshake retains all identity,
selected-repository, permission, destination, expiry and response checks. The
journal clock is authoritative for both its record and that handshake.

| State | Meaning and next permitted action |
| --- | --- |
| `pre_mint` | Acquisition started. An interrupted attempt stays latched until explicit stopped-owner recovery. |
| `pre_mint_failed` | This invocation proved no token POST was dispatched and recorded the failure. A new explicit acquisition is allowed. |
| `mint_pending` | Durable intent precedes dispatch. A crash or failed later persistence leaves an uncertain attempt. |
| `unknown` | A token request or subsequent qualification failed after dispatch became possible. No automatic replay or fresh acquisition is allowed. |
| `qualified` | The complete handshake and nonsecret receipt were persisted. The caller receives the opaque role transport. |
| `retry_authorized` | A separate trusted stopped-work check and matching recovery evidence authorize a new explicit attempt. This operation sends no HTTP. |

Qualified metadata never recreates a transport or proves readiness after restart.
An explicitly requested later acquisition uses a new attempt ID. This library
never renews credentials from a polling tick, a 401 response or constructor.
The original low-level `AcquireGitHubInstallation` remains an explicit,
unpersisted boundary; a service must use its durable lifecycle composition.

If qualification persistence fails, the new transport is closed and withheld.
The journal attempts to retain uncertainty using a bounded cleanup context even
if the caller cancelled. If that write also fails, the preceding durable latch
remains. A crash before or after a token POST cannot erase the earlier intent.

## Explicit recovery and retention

`AuthorizeRetry` requires the exact attempt, immutable owner ID, a nonsecret
hash of retained evidence, its time, and a trusted callback that verifies stopped
prior work. It acquires the same role lock before checking the evidence and fences
the exact persisted attempt before authorization. It cannot race a still-running
acquisition. The callback must be bounded and must not mint credentials or launch
provider work. A model assertion or missing local transport is not stopped-work
evidence. The callback's error text is never persisted or returned.

The prior token's remote creation can remain unknown even after its local owner
stops. An unknown token cannot be revoked without its lost value. Recovery records
that uncertainty and authorizes a separate qualification; it does not claim remote
revocation, retrieve the lost token or retry the old POST.

Records contain only enrollment, role, random attempt ID, stage, times, optional
qualification metadata and recovery evidence. No JWT, token, key, remote error
body or provider charge is stored. Formatting the journal handle is redacted.
The JSON schema requires canonical field names and rejects duplicates, unknowns,
missing required fields, invalid identities, trailing data and oversized records.
Optional receipt/recovery objects follow their own bounded schemas. Records are
at most 4 KiB; reads enforce the same bound before materializing a stored value.

At most 1,024 records are retained. A new attempt prunes terminal history older
than seven days, keeping each role's latest record and every unresolved attempt.
Retention validates every record against its stored row role and attempt ID
before selecting deletions. A full or malformed journal refuses new HTTP work. These records live in a
separate table, so ordinary legacy `Store` state writes cannot erase them.

## Offline evidence and remaining integration

Tests independently verify a generated RSA App JWT and send it through both
actual local TLS role handshakes, observing committed mint intent at the fake
server before its token response. They verify that acquisition changes no review
state or provider budget and that JWT/token bytes do not enter SQLite records,
database/WAL bytes or handle formatting.

Fault tests cover lost responses, rejected post-mint identity, failed intent and
completion writes, a failed uncertainty write, fresh `Store` reopening, missing
or mismatched recovery proof, blocked recovery while a real owner is live,
process locks, bounded history, hostile JSON and namespace symlinks. Real child
processes are killed before a token POST and after fake-server acceptance while
the response is withheld. Both restart paths make zero HTTP calls until explicit
stopped-owner recovery. Every child is reaped on success or failure.

```sh
go -C reviewer test -race -count=1 -run 'TestGitHubInstallationJournal|TestGitHubAppJWT|TestGitHubInstallation' .
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Atomic reader/publisher generation handover, admitted-request cancellation/drain,
role refresh ownership, qualified object acquisition and packaged runtime
composition still need integration. This journal is not a live qualification,
credential import, remote revocation or activation grant. Both
[installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token)
and [token revocation](https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token)
remain distinct operations.
