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
Its tests launch fake wrapper executables. Official-client wrappers and a
qualified OS launcher remain separate implementation work; live execution waits
for passing gates and activation authority. Substituting a fake is not isolation
proof. Optional trusted `SourceBridge` configuration now binds each session to
one MCP server through a credential-free Linux Unix-socket proxy. Its required
isolated client profile remains a launcher obligation; see
[SOURCE-CLIENTS.md](SOURCE-CLIENTS.md).

The inspected pins are Codex CLI 0.163.0 with `gpt-6-astra` and Claude Code
2.1.303 with `claude-opus-5-5`. Pins do not establish account model access.
Codex feature switches and Claude safe/restricted/tool/MCP settings are candidate
controls based on the installed help. They have not passed a credential-bearing
tool inventory or hostile-client probe. No CLI permission flag is treated as an
OS boundary. The CLI manifests have no qualified reader RPC/MCP mapping. Fake
drivers receive only the session-bound [source capability](SOURCE.md); a future
qualified driver must map that job's reader and no ambient MCP servers. The generated strict Report schema is mounted read-only for Codex and passed through Claude's schema option. `NormalizeCLIOutput` requires a completed Codex JSONL turn or a successful Claude result envelope, rejects forbidden tool traces and malformed/incomplete output, and replaces model-reported usage with available client metadata. CLI turn counts are not a bound on internal inference calls; that bound still requires external qualification.

`Reader` accepts only listed paths and the three captured revisions. It caps
calls, bytes, search results and evidence logs, verifies quoted source anchors,
and records omissions. Accepted reports merge authoritative reader coverage, even when the model omits it. Citation batches read each object once. R3 focus excludes withdrawn claims and requires a response or origin retraction for each focused own finding. `GitStore` invokes a trusted absolute Git executable
without shell, user environment or checkout. Symlinks, submodules, binaries,
missing objects, cross-job references and exhausted limits become coverage
limitations, never evidence of clean code. Candidate hooks and instructions are
inert data. The reader must run as a separate identity outside the provider
worker, without provider/GitHub credentials.

`AuthStore` on Linux/macOS serializes trusted refresh callbacks with a
cross-process advisory lock, reads the current state after locking, and
atomically writes it with fsync. State and lock files are private. Provider
stores are separate. Callback/precommit errors preserve previous state; a directory fsync failure after rename reports uncertain durability and must never cause restoring an old seed. Tests use fake bytes only.
It does not import the owner's interactive auth, provision worker credentials,
or itself prove distinct OS users/network isolation. Stale seeds are not an
input to refresh; the callback sees only the current leased state.

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
