# Offline adversarial reviewer alpha

This separate Go module implements the new alpha contracts from
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The retired modules are reference material. No workflow, daemon, App installation
or credential-bearing launcher is included.

## Validate locally

Use Go 1.25 or later from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Git-backed source readers, snapshot capture and corpus replay require Git
2.46+ with `--no-lazy-fetch` support. Older builds fail closed; no environment-only
fallback permits lazy fetching. See the [evaluation prerequisites](testdata/evaluation/README.md#reproduce-validation-and-the-freeze-attempt).

Tests use fake drivers and disposable Git/auth fixtures. They run no provider
inference and require no GitHub or provider credentials. Actions CI remains
disabled; record results at the exact PR head and rerun locally during review.

## Boundaries

`Session` captures immutable target/head/base/merge-base IDs, head-repository
provenance, config hash, job/packet/policy hashes, pinned provider/model/client/auth identity and phase
limits. Both Codex and Claude adapters exist from the first version. Unknown
JSON fields, trailing values, oversized output and wrong provenance fail the
phase. `NewSession` precomputes immutable hash headers for the model to copy, validates both frozen R1 reports for R2 and R1/R2 lineage plus focus IDs for R3, and rejects empty packets. Peer dispositions, own retractions and coverage references are validated, and finding/judgment quotes must pass the same-snapshot reader before success. An auth/client/model precheck happens before provider work and is checked
again afterward. There is no fallback to another provider or API billing.

`Adapter.Invocation` creates a candidate official-client argument/environment
manifest. It never executes that manifest. The trusted `Driver` seam must
use the pure `NormalizeCLIOutput` decoder for the pinned CLI transport and
enforce the declared turn/tool/memory/network limits inside a qualified worker.
`DisabledLiveDriver` always returns a blocked error. The reusable
[worker transport](WORKER.md) supplies bounded JSONL framing, phase cancellation,
trusted pre/post identity inspection and fail-closed supervisor-stop handling.
Its tests launch fake wrapper executables. `NativeDriver` also handles native
client framing and Claude's source-enabled preprompt initialization guard.
The [native VM host and guest](NATIVE-VM.md) connect the pinned clients to a
separate reader, private provider transport and one-use auth transfer. Both
clients have completed opt-in KVM reviews against fake TLS. Full runtime
qualification and live execution still require passing gates and activation
authority. Substituting a fake is not production isolation proof. Optional
trusted `SourceBridge` configuration binds each session to
one MCP server through a credential-free Linux Unix-socket proxy. Its required
isolated client profile remains a launcher obligation; see
[SOURCE-CLIENTS.md](SOURCE-CLIENTS.md).

The inspected pins are Codex CLI 0.163.0 with `gpt-6-astra` and Claude Code
2.1.303 with `claude-opus-5-5`. Pins do not establish account model access.
Codex feature switches and Claude safe/restricted/tool/MCP settings are candidate
controls based on the installed help. They have not passed a credential-bearing
tool inventory or hostile-client probe. No CLI permission flag is treated as an
OS boundary. The native VM fixture exercises the job-local source mapping with
both exact clients. Broader hostile capability and resource qualification is
still required. Fake drivers receive only the session-bound
[source capability](SOURCE.md). The generated strict Report schema is mounted read-only for Codex and passed through Claude's schema option. `NormalizeCLIOutput` requires a completed Codex JSONL turn or a successful Claude result envelope, rejects forbidden tool traces and malformed/incomplete output, and replaces model-reported usage with available client metadata. CLI turn counts are not a bound on internal inference calls; that bound still requires external qualification.

`Reader` accepts only listed paths and the three captured revisions. It caps
calls, bytes, search results and evidence logs, verifies quoted source anchors,
and records omissions. Accepted reports merge authoritative reader coverage, even when the model omits it. Citation batches read each object once. R3 focus excludes withdrawn claims and requires a response or origin retraction for each focused own finding. `GitStore` invokes a trusted absolute Git executable
without shell, user environment or checkout. Symlinks, submodules, binaries,
missing objects, cross-job references and exhausted limits become coverage
limitations, never evidence of clean code. Candidate hooks and instructions are
inert data. The reader must run as a separate identity outside the provider
worker, without provider/GitHub credentials.

[Snapshot evidence](SNAPSHOT-EVIDENCE.md) builds the trusted changed-path packet
and diff-line map without checkout or fetching. It binds both to the exact
SQLite-admitted packet and carries controller-observed omissions into reader
coverage, fake review, recovery and rendering.

`AuthStore` on Linux/macOS serializes trusted local state updates and per-phase
profile leases with a cross-process lock. A durable profile journal survives
controller loss, blocks stale seed reuse and commits returned credentials only
after a trusted containment shutdown check. Provider stores stay separate.
[Protected auth profiles](AUTH-PROFILES.md) describes exact allowlisted files,
crash recovery, retirement and synthetic validation. `Refresh` callbacks must
remain local; provider-side refresh runs in the leased client profile. Tests
use fake bytes only. This code does not import interactive auth, provision
credentials or qualify production credential transfer and OS containment.

[Provider HTTP egress](PROVIDER-EGRESS.md) supplies exact route guards, separate
inference/refresh request budgets, bounded payloads and private completion audits.
Its concrete transport rejects private destinations and hidden connection retries.
No listener, provider policy or network route is activated. The isolated launcher
must still enforce exclusive egress and qualify the pinned clients. The
[private provider evidence path](PROVIDER-AUDIT.md) carries required receipts
through execution, charged attempt storage, recovery and the pre-POST gate.
It selects no production worker or credentials.

## Remaining live gates

Applicable public-target subscription support, account-specific entitlements and
overflow controls, new native-App eligibility, externally enforced isolation,
qualified official-client transport, tool/resource enforcement and explicit
owner activation remain required. If a client cannot remove a forbidden
capability, record it blocked; do not weaken the contract or declare v1 complete.
Keep [the tracking issue](https://github.com/hemsoft-dev/set-it-free-loop/issues/172)
open while its acceptance criteria remain unmet. Raw transcripts and credentials
never belong in public artifacts.

Transport references: [Codex JSONL and schema output](https://learn.chatgpt.com/docs/non-interactive-mode), [Claude structured results](https://code.claude.com/docs/en/headless). These documents support the offline decoders; live compatibility remains unproven.

The Linux [containment launch gate](CONTAINMENT-GATE.md) persists one-use grant
authority before an inert fake-only stub receives work. Its abort/recovery
protocol does not yet attest whole-unit shutdown or release auth leases.

The [systemd supervisor](SYSTEMD-SUPERVISOR.md) binds the fake-only gate to the
user manager, physical cgroup limits and durable stopped-auth recovery. Its
explicit host fixture uses an inert escaped child and synthetic auth bytes.

[Sealed VM boot assets](VM-BOOT.md) retain pinned QEMU/kernel/initramfs bytes in
sealed descriptors. The explicit fake-only KVM boot fixture checks the live
executable and independent guest/host receipts. Native VM payload/grant, client,
code-host, auth and egress composition remain source work; this is not a worker
activation path or production isolation qualification.
