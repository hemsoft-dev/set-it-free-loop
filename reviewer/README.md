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
provenance, config hash, pinned provider/model/client/auth identity and phase
limits. Both Codex and Claude adapters exist from the first version. Unknown
JSON fields, trailing values, oversized output and wrong provenance fail the
phase. An auth/client/model precheck happens before provider work and is checked
again afterward. There is no fallback to another provider or API billing.

`Adapter.Invocation` creates a candidate official-client argument/environment
manifest. It never executes that manifest. The trusted `Driver` seam must
normalize the pinned CLI transport into the versioned `Report` contract and
enforce the declared turn/tool/memory/network limits inside a qualified worker.
`DisabledLiveDriver` always returns a blocked error. A future production driver
requires a separate reviewed change after live gates pass; substituting a fake
driver is not isolation proof.

The inspected pins are Codex CLI 0.163.0 with `gpt-6-astra` and Claude Code
2.1.303 with `claude-opus-5-5`. Pins do not establish account model access.
Codex feature switches and Claude safe/restricted/tool/MCP settings are candidate
controls based on the installed help. They have not passed a credential-bearing
tool inventory or hostile-client probe. No CLI permission flag is treated as an
OS boundary. The manifest currently has no source-broker configuration; a
qualified driver must supply only that job's reader and no ambient MCP servers.

`Reader` accepts only listed paths and the three captured revisions. It caps
calls, bytes, search results and evidence logs, verifies quoted source anchors,
and records omissions. `GitStore` invokes a trusted absolute Git executable
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
official-client response normalization, tool/resource enforcement and explicit
owner activation remain required. If a client cannot remove a forbidden
capability, record it blocked; do not weaken the contract or declare v1 complete.
Keep [the tracking issue](https://github.com/hemsoft-dev/set-it-free-loop/issues/172)
open while its acceptance criteria remain unmet. Raw transcripts and credentials
never belong in public artifacts.
