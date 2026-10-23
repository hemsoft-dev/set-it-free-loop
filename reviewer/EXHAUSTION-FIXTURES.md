# Native resource-exhaustion fixtures

These opt-in fixtures push one resource bound past its limit inside the native
KVM review VM. They use the unmodified pinned clients, Codex CLI 0.163.0 and
Claude Code 2.1.303, with the loopback TLS fake backend, fresh synthetic auth
and the canaries described in [HOSTILE-FIXTURES.md](HOSTILE-FIXTURES.md). No
provider account, public backend, App, paid route or deployment is involved.
The results are bounded fixture evidence for
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174), not
stress qualification of a production runtime.

## Cases

Every case keeps the captured response policy, reader allowlist, guest limits
and production egress bounds, with two exceptions. Claude's turn flood uses the
production inference bound of 24 instead of the three-turn fixture cap. The
fake backend's request cap now equals the guard's 8 MiB per-request bound, so a
fixture refusal cannot hide the guard. The reader allowlist is `big.go` and
`source.go`. The store returns `big.go`, 1.5 MiB of text, whatever limit is
requested, so the reader and object stream must enforce the byte budget
themselves.

| Case | What the fake model does | Bound under test |
| --- | --- | --- |
| `*-response-flood` | Streams 17 MiB of SSE text in one response, flushed in chunks with no declared length | 16 MiB per-response egress bound |
| `*-turn-flood` | Calls a valid source read on every turn and never reports | 24 inference requests; Claude also has `--max-turns 24` |
| `*-stall` | Accepts the first inference and writes nothing | 75 s phase deadline |
| `*-source-oversize` | Reads `big.go` twice, then reports | 2 MiB reader byte budget |
| `codex-source-flood` | One exec makes 140 sequential source reads | 128 source tool calls |
| `codex-code-memory` | Fills 2 × 32 MiB typed arrays in one exec, then 64 × 32 MiB in the next | 256 MiB code-host guest cgroup |
| `codex-code-output-flood` | Prints 9 MiB from one exec | 8 MiB code-host relay bound |
| `codex-code-spin` | Runs `for(;;){}` in one exec, then reports | Client yield and session stop |

Every case requires these checks: unit bound and physically stopped, provider
stopped, runtime root removed, no stop-pending, no canary anywhere scanned, no
denied destination, no telemetry, a fixed tool inventory and no backend
problem. Auth must be either returned or fenced. Returned auth must still hold
the original synthetic tokens, as in the hostile matrix. Fenced means recovery
is required and agrees with the audit, the error and a reopened store. The
provider receipt must stay within the policy's request and byte bounds. Usage
must not be claimed for failed work. A failed report is empty, so `Usage.Known`
is false and no tokens are reported. The case-specific outcome is either a typed
failure or an honest partial report. A typed failure has an empty report, an
incomplete audit and a `phase-*`, deadline or `auth-recovery` error class; an
unproven stop never counts. An honest partial report is complete only if its
authoritative coverage lists the omission.

The matrix runner accepts a result only with kind `exhaustion` and exactly
these common checks plus the case's own checks from `exhaustionChecks`. A
missing or extra check fails the case, as in the hostile matrix.

## Measured results

### Measured at a6e1b5f

On October 23, 2026, `matrix-r9-a6e1b5f` ran all 12 cases with runner and
native image (`image-r8`) both built from `a6e1b5f`, the merged head of
[#249](https://github.com/hemsoft-dev/set-it-free-loop/pull/249). Every case
passed with no failed check, and every unit was stopped independently. The
hostile matrix at the same head, `hostile-r7-a6e1b5f`, had 19 passes and the
one contained `codex-metadata` client failure
([validation comment](https://github.com/hemsoft-dev/set-it-free-loop/pull/249#issuecomment-6370501871)).
Columns are from the run's `matrix.json`. Seconds, Peak and Result mean the
same as in the history table below.

| Case | Verdict | State | Error class | Seconds | Peak | Result |
| --- | --- | --- | --- | --- | --- | --- |
| `claude-response-flood` | pass | failed | `phase-invalid_output` | 6.5 | 1.1G | `17e4b671ae91` |
| `claude-turn-flood` | pass | failed | `phase-invalid_output` | 7.2 | 1.1G | `93163db7f916` |
| `claude-stall` | pass | failed | `auth-recovery` | 75.4 | 1.1G | `eb4c9ed40823` |
| `claude-source-oversize` | pass | complete | none | 5.7 | 1.1G | `2ab98803f47a` |
| `codex-response-flood` | pass | failed | `phase-invalid_output` | 4.9 | 1.1G | `e31938914b78` |
| `codex-turn-flood` | pass | failed | `phase-invalid_output` | 5.2 | 1.2G | `9e213c724218` |
| `codex-stall` | pass | failed | `auth-recovery` | 75.3 | 1.1G | `d43983d6375a` |
| `codex-source-flood` | pass | complete | none | 5.0 | 1.1G | `99d5db207e57` |
| `codex-source-oversize` | pass | complete | none | 5.3 | 1.1G | `70c73413adf2` |
| `codex-code-memory` | pass | failed | `auth-recovery` | 75.3 | 1.1G | `af4c51352a89` |
| `codex-code-output-flood` | pass | failed | `phase-invalid_output` | 5.1 | 1.1G | `42c2ad2494a8` |
| `codex-code-spin` | pass | complete | none | 36.4 | 1.1G | `ac8ad7b3b069` |

This run predates the byte-limit reason below. Its oversize cases passed the
earlier `byte-bound` check, which did not assert the reason. The new reason
changes the guest reader, so it needs a new image and is re-measured
separately.

### History: first run at 5c9f757

The table and details below are from the first runs and are kept as history.

On October 22, 2026, all 12 cases ran serially on mini. The runner and image
were built from `5c9f757`, as `exhaustion-174/image-r1`; the only guest change
from `native-guest-stop-image-r1` is the fix below. The run was split into
`matrix-r1` (8 cases) and `matrix-r1b` (4 cases) after the
`/tmp` user quota stopped one runner build. `codex-code-spin` was rerun as
`matrix-r1c` with runner `a98f7fe` after its expectation was corrected. Every
case passed. Each unit was stopped under the user manager, its cgroup was gone
and its runtime root removed. No canary hit occurred.

Seconds are runner wall time, including VM boot. Peak is the user manager's
`memory peak` accounting line for the whole unit: QEMU, guest RAM and stub. It
does not show guest-internal cgroups. The hash is each private result receipt's
SHA-256 prefix; receipts stay local.

| Case | Provider | Outcome | Auth | Seconds | Peak | Result |
| --- | --- | --- | --- | --- | --- | --- |
| `claude-response-flood` | claude | `phase-invalid_output`; guard failed and exhausted at 16 MiB + 1 byte | returned | 6.2 | 1.1G | `be2fc04622b8` |
| `claude-turn-flood` | claude | `phase-invalid_output` (`client-error-silent-exit`) after exactly 24 inferences and 24 reads | returned | 6.8 | 1.1G | `faad7bd822d8` |
| `claude-stall` | claude | deadline; `provider_unavailable` and `auth-recovery` | fenced | 75.2 | 1.1G | `cb089b83ce9d` |
| `claude-source-oversize` | claude | complete report; second read denied, `big.go` omission recorded | returned | 6.3 | 1.1G | `49f282c7198c` |
| `codex-response-flood` | codex | `phase-invalid_output`; guard failed and exhausted at 16 MiB + 1 byte | returned | 5.2 | 1.2G | `edabb2ec5888` |
| `codex-turn-flood` | codex | `phase-invalid_output`; the guard refused inference 25 | returned | 5.5 | 1.1G | `7741ee68f011` |
| `codex-stall` | codex | deadline; `provider_unavailable` and `auth-recovery` | fenced | 75.3 | 1.1G | `169cf364df6f` |
| `codex-source-flood` | codex | complete report; 128 served, 12 denied, exhaustion recorded | returned | 5.2 | 1.1G | `ed1416d10311` |
| `codex-source-oversize` | codex | complete report; second read denied, `big.go` omission recorded | returned | 5.4 | 1.1G | `50768cbc81b0` |
| `codex-code-memory` | codex | deadline; client never returned the exec | fenced | 75.3 | 1.1G | `cf706728670e` |
| `codex-code-output-flood` | codex | `phase-invalid_output`; relay cut at 8,384,882 bytes, code host failed | returned | 5.2 | 1.1G | `cf1061892847` |
| `codex-code-spin` | codex | complete report; cell yielded after 31 s, code host stopped | returned | 36.5 | 1.1G | `6f020c476b09` |

Details:

- **Response floods.** The fake model wrote 17.0 MiB (Claude) and 16.0 MiB
  (Codex) before the guard closed the stream. The guard read 16 MiB + 1 byte,
  latched `failed` and `exhausted`, and recorded one forwarded request with no
  accepted response. The client received no byte of it. A forwarded request
  without an accepted response is how the receipt shows upstream work that may
  have been consumed but was never delivered.
- **Turn floods.** Claude stopped itself at `--max-turns 24`. The guard saw 24
  inferences and was not exhausted. Codex has no turn limit. The guard refused
  inference 25 with `exhausted`, after 24 code-mode reads. Both are typed
  failures, but Claude's is classified `invalid_output`, not
  `incomplete_expired`; see the limitations below.
- **Stalls.** The guard's upstream request was cancelled at the 75 s phase
  deadline, and the result came back 0.2 to 0.3 s later. The host and guest
  share that deadline, so the host stopped the unit before the guest's finished
  record arrived. The host then had no auth return and no source audit, so auth
  stays fenced for recovery, as documented in [NATIVE-VM.md](NATIVE-VM.md).
  The fake upstream records when it observed the cancellation. The case passes
  only if that is within 2 s after the 75 s deadline and the result returns
  within 20 s of the deadline. The runner's 100 s outer watchdog lies outside
  both intervals, so its cancellation cannot pass the case.
- **Oversized object.** The first read was served and counted at 1,572,864
  bytes. The second read was refused by the host object stream's own budget
  before the reader's check, and was listed as an omission of `big.go` with
  the reason `missing or unreadable object` (see the byte-limit reason
  below). Claude Code itself rejected the 1.5 MiB tool result as exceeding
  its token limit, so the model never saw it. Codex code mode received all of
  it. Both reports completed with the omission in their authoritative
  coverage.
- **Code-host memory.** The host unit's peak stayed at 1.1G, so the pressure
  stayed inside the guest. The Codex client never returned the exec result and
  never sent a second inference, so the phase deadline ended the session and
  auth was fenced. No guest-side OOM record is exported. That the code host was
  killed by its cgroup, rather than stalled, is inferred, not observed.

  That run would not pass the current case. It accepted a typed failure with no
  code-host receipt, which an immediate client or code-host failure before the
  allocation also produces. The host cannot see guest cgroups, so the case now
  runs a control. The first exec touches 64 MiB, under the bound. The second
  is the same program with 2 GiB. The fake model records each result, reduced
  to whether the allocation survived. The case requires both of these:

  - `allocation-dispatched`: the control returned `survived`, the fake model
    then issued the over-bound cell, and the guard relayed every forwarded
    response.
  - `memory-bound-ended-cell`: the over-bound cell never returned `survived`,
    and one of two outcomes holds. In the first, the code host's receipt is
    stopped and failed in its `wait` stage, with output well below the relay
    bound, and the client returned the failure on a third inference. In the
    second, the receipt is lost, the client never returned the cell, there
    were exactly two inferences, and the result came back within 20 s after
    the 75 s phase deadline, before the outer watchdog.

  The only difference between the two cells is crossing 256 MiB. A cell that
  is merely slow yields after about 30 s (see `codex-code-spin`). So a client
  that ran the control but hung on the over-bound cell until the deadline is
  attributed to the memory bound. That is still an inference: no OOM record
  exists. The earlier runs, through `matrix-r6` at `7c5dae4`, predate the
  control. They match the second outcome only in their timing and need a
  fresh run.
- **Code-host output.** The broker stopped the relay before 8 MiB, marked the
  exchange failed and the client reported `failed to read code-mode host
  message: early eof`. The fake model still emitted a report; the guest
  downgraded it to `invalid_output` because the code-host exchange was
  incomplete.
- **Code-host spin.** This case was expected to fail. The pinned client yielded
  the running cell after 31 s (`Script running with cell ID 1`) and the model
  reported. Teardown stopped the code host with a complete relay receipt, and
  the host validated the report. CPU use was bounded by the guest `cpu.max`,
  the client yield and session stop, not by a failed exchange.

## Boundary fix: source tool calls past the bound

Before the fix, the session MCP server closed its connection on tool call 129.
In `codex-source-flood`, run on `native-guest-stop-image-r1` with runner
`b1574e7`, Codex saw 12 `Transport closed` errors. The reader audit then
failed, the guest omitted it, and the host could not persist source evidence
(`source audit could not be persisted`). It fenced the returned synthetic auth
for operator recovery. Result receipt `3bbfb4b3d8a3`. Any model that read
more than 128 times could force that recovery.

`ServeSourceMCP` now answers calls past the bound with `Source request denied`
and keeps the connection. The broker records the omission once and claims
`exhausted` only after it served the full bound. The total request budget of
`ReaderCalls + 32` still closes a larger flood. On the fixed image the same case
completed with 128 served reads and 12 denials. The receipt was exhausted and
recorded `source tool-call limit exhausted` in report coverage, and auth was
returned. `TestSourceMCPToolCallBoundDeniesWithoutDroppingTheAudit` failed before
the fix (`missing MCP response`) and passes after it.

## Byte-limit reason

Through `a6e1b5f`, a byte-budget refusal was recorded as `missing or
unreadable object` whenever the store or the object stream refused it, so
coverage could not tell an exhausted budget from a missing file. The object
stream and the stores returned untyped errors for oversized objects.

The reader now records `source byte limit exhausted` for every byte-budget
refusal: its own checks, and a store that returns `ErrSourceByteLimit`.
`GitStore` and the sealed Git materialization return it for a blob past the
requested limit. The private object reply carries a `ByteLimit` flag, valid
only on a denial, so the host reports a budget refusal without forwarding a
store error or any object byte. The oversize cases' `byte-bound` and
`honest-outcome` checks now require exactly that omission of `big.go`.
`TestVMObjectsCarryByteBudgetRefusalToTheGuestReader` and
`TestReaderRecordsByteBudgetApartFromMissingObjects` failed before the change
and pass after it. `SourceAccess.Exhausted` still describes only the tool-call
bound, so it stays false for a byte-budget refusal.

While testing this, `GitStore`'s output cap turned out not to hold: its buffer
embedded `bytes.Buffer`, whose `ReadFrom` let the pipe copy bypass the cap, so
an oversized blob was read whole and refused only by the reader's size check.
The cap now holds for every capped Git command.

## Remaining limitations

- **Deadline fences auth.** A stall or a hung client always ends with auth fenced
  and source evidence lost, because host and guest share one phase deadline and
  the host does not wait for the guest's finished record. This fails closed,
  but every wall-time exhaustion needs operator recovery. A shorter guest
  deadline with a host grace window would need a guest change and a new image.
  It is not implemented.
- **Claude max-turns classification.** The pinned client emits
  `error_max_turns` with `is_error: true`, which the normalizer maps to
  `incomplete_expired`. The native run reported `invalid_output` with
  `client-error-silent-exit`, so the exchange failed after or around the result
  frame. One likely cause is the post-result `mcp_status` control request
  meeting an exited client, but that is not confirmed. The guest records no
  frame-level observation on the Claude path. It is still a typed failure, not
  success.
- **Byte-limit reason not yet measured natively.** The reason above is
  covered by unit and fixture-check tests. No native run has used an image
  with it yet.
- **Request floods.** More than `ReaderCalls + 32` MCP requests still close the
  source connection, with the pre-fix effect: lost audit and fenced auth.
- **Unreachable pressure.** Guest task pressure cannot be induced through
  either client. Code mode has no process, worker or module API (see the
  `codex-code-probe` results), and Claude has no execution tool. Claude client
  memory pressure is not reachable deterministically: responses are capped at
  16 MiB before the client, and tool results are capped by the reader. Inducing
  either would need a guest change.
- **Observation limits.** Peaks are whole-unit host accounting. Guest-internal
  memory, OOM kills and task counts are not exported. The memory case is
  attributed by its control cell, not by an observed OOM kill.
- **Image provenance.** The current run used an image built from `a6e1b5f`.
  The history table's image was built from `5c9f757`.
- These results do not establish production resource limits, live-account
  behavior or provider eligibility.

## Reproduce

From a clean committed checkout, with a native image built from the same tree:

```sh
python3 -B reviewer/scripts/native-vm-hostile.py --run-fake-only --matrix exhaustion \
  --image /absolute/path/to/native-image \
  --output /absolute/path/to/new-matrix-directory \
  --go /absolute/path/to/go --module-cache /absolute/path/to/module-cache
```

Use `--only NAME` for one case. The three deadline cases take about 75 s each.
The runner's `/tmp` state roots and build caches count against the `/tmp` user
quota. Set `TMPDIR` to a disk directory to move the build cache.

Unit tests run without KVM:

```sh
go -C reviewer test -race -count=1 -run 'SourceMCP' .
go -C reviewer test -race -count=1 ./internal/nativefixture ./cmd/nativevmfixture
python3 -B -m unittest discover -s reviewer/scripts -p 'test_native_vm_hostile.py'
```
