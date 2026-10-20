# Path-free auth handoff

`AuthProfile.ExportAuth` transfers the one allowlisted credential file to a
trusted VM broker without handing it a host staging path. `ImportAuthReturn`
stages that broker's final file after a fresh whole-containment stop proof.
`Complete` then commits the returned bytes through the existing auth journal.
This is offline source for issue #174. Tests use synthetic strings and real
local Unix sockets. No interactive profile, actual account or provider is read.

The export gate is a required `AuthExportVerifier`. It must reopen the exact
durable launch binding and establish that the restricted private credential
channel is ready before export reads the staged seed. A model, client report or
unverified socket cannot supply this authorization. There is no production gate
implementation or automatic launcher here. The staging store stays private to
the supervisor and must never be mounted into a worker.

## Record and ownership

`AuthTransfer` is an opaque single-use object. Copies share the same consumption
state. Its fmt and JSON forms are redacted. Only `WriteTo` emits the secret wire
record, and only `Consume` exposes its payload to trusted profile code. Owned
buffers are cleared after use or failure; closing an unused object clears it.
An in-flight consumer owns its bytes until its callback returns and must use
the private channel deadline for cancellation. External copies made by the
trusted caller remain that caller's responsibility. Callbacks must not log or
retain bytes. Their errors are normalized rather than copied into status.

A record contains a fixed magic prefix, a four-byte header length, one canonical
JSON header of at most 2 KiB and at most 64 KiB of opaque auth bytes. The header
contains version, seed/return kind, provider, full `AuthBinding`, original seed
hash, payload hash and byte count. It has no path, environment, command or
provider-selection override. `ReadAuthTransfer` rejects malformed, partial,
oversized, noncanonical, hash-mismatched and trailing data and requires EOF from
one finite private record. The caller supplies authenticated private transport,
a deadline and record termination. Hashes establish integrity and binding;
they do not authenticate a peer or qualify a VM boundary.

The guest writes the seed only to the selected provider's fixed disposable auth
file. After its provider role stops, the trusted guest broker uses
`NewAuthReturn` with the seed's captured metadata and that single final file.
The controller rejects another provider, session, launch, containment or seed
before invoking its stop verifier. The source reader and publisher receive
neither the object nor the bytes. These payloads remain opaque; format, model,
subscription and API-key rejection still belong to qualified pinned-client
identity inspection, not this codec.

## Durable lifecycle

Export first records `transfer_stage=exported` in the existing private lease
journal, then exposes a single-use seed object. Repeated export is refused,
including after restart or a partial/failed wire write. `Complete` cannot use the
unchanged staging seed as a substitute for an unknown guest return.

A matching return requires `AuthStopVerifier` to recheck the original whole-VM
binding. The importer records the return hash and `receiving` stage, durably
stages those bytes in `pending.json`, writes the allowlisted staging file and
records `returned`. Only `Complete` changes canonical state. It checks the
staged file against that exact returned hash and uses the existing
committing/committed protocol before disposal.

After interruption, `RecoverProfile` acquires the same provider's lock with the
same binding. `ResumeAuthReturn` can finish only the exact journal-bound pending
return, with another fresh stop proof. A missing return, changed canonical seed
or unrelated staged file remains blocked. `Retire` may invalidate the known
canonical state after stopped recovery; it never restores the old seed. Known
pending returns survive the receiving/staged/returned and canonical commit
boundaries. Losing bytes before a durable pending return is recoverable only by
obtaining the same trusted final return or explicitly retiring the credentials.

The new journal fields are omitted for legacy leases. Existing leases retain
their original local-profile workflow, while an older binary rejects new
transfer fields rather than silently ignoring their constraints. The existing
exclusive refresh lock, private-file checks, anchored paths and clear-journal-last
disposal remain in force.

## Offline evidence and remaining integration

Tests run actual exported records through Unix sockets, then verify stopped
return, canonical commit and staging disposal for both providers. Mutations of
producer bytes reject bad framing, unknown/duplicate metadata, partial data,
hash changes and extra records. Other tests cover cross-job/provider rejection
before stop, redaction, shared-object one-use, failed callback/write wiping,
repeat export after restart, live-stop refusal, stale seeds, changed staging
and every receiving/returned/commit crash boundary. Missing returned state is
never labeled a successful refresh.

Run from the repository root:

```sh
go -C reviewer test -race -count=1 -run 'TestAuthTransfer|TestAuthProfile' ./...
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The native VM launcher, authenticated/deadline-enforced channels, guest identity
separation and actual-client refresh/return still require integration and
qualification. The host code-host experiment and synthetic stop callbacks cannot
supply that proof. No App, daemon, real auth import, live inference or paid usage
is activated by this library.
