# Bounded worker transport

For direct official-client stdin/stdout framing, [NativeDriver](NATIVE.md) uses
the same supervisor lifecycle with the native envelope normalizer. The wrapper
protocol below remains available for trusted wrappers; the two wire formats are
explicit and never auto-detected from untrusted output.

`WorkerDriver` implements the `Driver` contract over a versioned JSONL channel
to a trusted worker wrapper. Both adapters include the captured phase limits in
the invocation. A wrapper may request `read` and literal bounded `search` through
that invocation's source capability, then return one `report` or `failure`.
The transport accepts no executable, environment, credentials, identity or job
selector from the worker. Source errors return a generic denial; the trusted
broker retains the actual coverage limitation.

This is a wrapper protocol, not either official CLI's native protocol or MCP.
`WorkerLauncher` is the trusted integration boundary for a future OS supervisor
and official-client wrapper. Its wrapper must normalize official-client output with `NormalizeCLIOutput`,
including client metadata rather than model-reported usage. Its `Inspect` must
verify real pinned subscription,
client and model identity. The driver checks it again after execution; worker
JSON cannot attest identity. A nil launcher remains blocked.

The transport bounds invocation serialization, each frame, total worker output,
source reply bytes, message count and wall time. Captured `ReaderCalls` bounds
source requests separately from the one terminal message. Unknown methods,
unknown or duplicate envelope fields, malformed or trailing messages, oversized
reports, nonzero exit and missing completion fail the phase. A report is accepted
only after stdout EOF, successful process wait, supervisor stop confirmation and
post-execution identity inspection. A worker failure is never a before-work
refund attestation. Only a trusted launcher returning no process on start failure
can attest that no provider work began. There are no implicit retries.

Cancellation closes the transport pipes, cancels source calls and invokes
`Stop` with an independent five-second deadline. The trusted supervisor must
confirm termination and reaping of the whole worker, including descendants.
`Wait` or a valid report alone is insufficient. The launcher must persist its own
job-to-worker recovery identity before launching, return a worker handle whenever
a process may exist, and honor launch/inspect contexts. No production supervisor
or official-client wrapper is supplied in this change.

If stop confirmation fails or times out, `ErrWorkerStopPending` prevents output
acceptance, stopped-worker audit writes, refunds and durable lease release.
`RunStopped` retains the nonce, outstanding attempt and session charge. Expiry
cannot steal that lease. The returned `Outcome.FailedSourceAccess` retains broker
evidence for explicit recovery. A trusted operator must first prove physical
termination through the supervisor's persisted identity. Then repair storage if
needed, save the exact receipt with `RecordStoppedSourceAccess` and release that
same lease with `ReleaseStopped`. Unknown work remains charged. Do not rerun a
provider or synthesize a stop receipt to unblock the queue.

The Unix test wrapper executes real fake-worker processes with a fixed test
binary, arguments and minimal environment. It measures both providers through
four sessions, source read/search receipts, durable reservations and fake GitHub
publication. Host environment canaries are absent. Fault fixtures exercise
quota/auth/unavailable results, malformed/duplicate/unknown/trailing frames,
byte and call exhaustion, nonzero exits, blocked input, terminal hangs, timeout,
identity substitution and missing supervisor confirmation with explicit durable
recovery. The fixture reaps its process group; it is not a sandbox and does not
prove confinement of an adversarial client that can escape that group.

Run from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Separate OS identities, protected provider stores/refresh, official-client
Read/Search mapping, memory and inference/tool limits, filesystem/egress
confinement and all credential-bearing probes remain requirements of
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
Actual supervisor packaging and GitHub transport/live validation remain under
[issue #176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176).
Those requirements do not prevent developing and testing their remaining source
with fake clients and transports. Live activation still requires its own gates
and owner authority.
