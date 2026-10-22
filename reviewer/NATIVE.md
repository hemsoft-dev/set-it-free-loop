# Native CLI transport

Source-enabled Claude uses the [preprompt control guard](SOURCE-CLIENTS.md)
instead of the plain-prompt framing below. It proves the current source helper
and tools are connected before sending review content and rechecks status before
accepting the result. Drivers without that guard cannot run this configuration.

`NativeDriver` connects a captured adapter invocation to an official client's
stdin/stdout protocol through `WorkerLauncher`. It sends the fixed task and
canonical Session JSON, closes stdin to finish the prompt, and bounds collected
stdout by a separate captured transport allowance. The allowance is
`12*ReaderBytes + 6*OutputBytes + ReaderCalls*8192 + 128 KiB`, accounting for
escaped source-tool results, escaped Report JSON and framing. It does not enlarge
source access or the normalized Report's strict `OutputBytes` cap.
The driver requires EOF and successful process wait,
then calls `NormalizeCLIOutput` to validate a completed Codex JSONL turn or Claude
JSON result. Only the normalized Report returns to the adapter. Model-reported
usage is replaced by available native transport metadata.

The native path and the JSONL wrapper path share the same termination code.
Cancellation closes both pipes and requests supervisor-confirmed termination
with an independent deadline. A valid report followed by a hang or nonzero exit
does not complete a phase. Uncertain physical termination returns
`ErrWorkerStopPending`; durable state retains the lease, outstanding attempt and
charge for stopped-worker recovery. Both paths recheck trusted identity after
termination. There are no implicit retries or provider substitutions.

The native path does not interpret stdout as wrapper messages or source requests.
Actual source access belongs on the captured private MCP connection. Error text
and malformed client output are never returned as controller errors or findings.
Codex 0.163.0 exposes message-only failure events, which become
`provider_unavailable` with the underlying cause unknown. Claude 2.1.303's failed
result carries optional `api_error_status`; 401 becomes `auth_failed`, 429 becomes
`quota_wait`, and other execution failures become `provider_unavailable`.
`subtype: success` with `is_error: true` is still failure. Explicit turn/budget
exhaustion uses the existing budget-exhaustion state `incomplete_expired`, and exhausted structured-output attempts become
`invalid_output`. Unknown/malformed envelopes fail closed. No classification
matches arbitrary message text or authorizes a retry/refund. Trusted launcher
prechecks can still report their typed before-work failure state.
Failure classification waits for valid EOF. Duplicate JSON keys, contradictory
terminal events or data after a Codex failure remain invalid output. A later
duplicate Claude discriminator cannot turn an error into a successful report.

`NativeDriver` selects no host executable, credentials, endpoint or launcher. A nil launcher blocks execution. The separate [native VM assembly](NATIVE-VM.md)
implements the host/guest client, role and private-channel wiring. Its source and
fake-backend runs do not qualify the complete production boundary. A qualified
launcher must establish exact
pinned clients and subscription identity, a pristine disposable filesystem,
separate protected auth and reader identities, the session-bound source endpoint,
resource and internal request budgets, restricted egress, and termination of the
entire worker containment. A CLI permission switch is not an OS boundary. The
default adapters still cannot run live clients. Freeze and activation gates
remain in force under [issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).

Unix fixtures execute fake CLI subprocesses with a minimal environment. Both
provider envelope shapes run through four durable adversarial sessions and fake
GitHub COMMENT publication. They verify native usage replaces forged model usage
and that empty actual-read receipts remain empty. They also exercise malformed,
oversized, wrong-session, wrapper-protocol and incomplete output, failed exits,
trailing bytes, blocked prompt I/O, completion hangs, timeout, invalid invocation
rejection, typed native failures on nonzero process exit and lease retention
after uncertain stop. The fixture is not an
official client or sandbox and performs no provider inference.

Run from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The pinned Codex failure shape is defined in
[exec_events.rs](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/exec/src/exec_events.rs#L50-L87).
Claude result fields were inspected in the installed 2.1.303 executable, SHA256
`375988f457756524234695cf2d8f64ace99c5f68682e47ea24862a843c191450`,
schema byte offset 236208000 and result producer 251915200. These pin-specific
observations establish parsing candidates, not actual account or live-client
qualification. Requalify the contract before changing a client pin.
