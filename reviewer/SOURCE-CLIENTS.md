# Client source configuration and private proxy

Set `Adapter.SourceBridge` only in trusted release configuration. It supplies the
installed helper path, protected endpoint directory and expected reader UID.
`Adapter.Invocation` then builds one `sfl-source` MCP server and a distinct socket
path containing the full captured session hash. Per-PR commands and model output
cannot set these fields. Wrong/missing reader snapshots, invalid paths/UIDs and
Unix socket paths over 107 bytes fail before worker launch.

Codex receives command-line MCP overrides, requires this server to initialize,
and exposes only its Read/Search tools. The terminal stdin marker and pinned
model/client/subscription contract remain intact. No ambient credential variable
is forwarded to the helper. Claude receives one explicit stdio server, strict
MCP configuration, no built-in source tools, a source-tool permission allowlist,
noninteractive denial, and the captured maximum agentic-turn count.
Its stdio configuration sets `alwaysLoad: true`, and its trusted environment
sets `ENABLE_TOOL_SEARCH=false`. The pinned client therefore exposes both source
schemas directly without requiring a ToolSearch call before Read/Search.

Claude's wait/strict flags do not require successful source initialization.
The source-enabled path therefore uses stream-json input/output and NativeDriver's
control handshake. Before sending any user prompt, it correlates initialize and
bounded `mcp_status` responses, requires exactly one dynamic `sfl-source` server,
checks its actual command/arguments against the captured configuration, and
requires connected status with exactly raw `read` and `search` tools. Missing,
failed, disabled, authentication-required, redacted or mismatched status stops
the worker without sending review content. Pending status can be rechecked at
most 32 times, one second apart, within the captured phase deadline.

The first-turn init must then match the pinned model/client, connected server
and source tool inventory. Forbidden tool traces and duplicate completion fail.
Before accepting a result, a final correlated status check must still match the
captured server/tools. That final check measures the current state, not continuous
availability or actual reads. Broker receipts remain authoritative for actual
source activity. Status omits environment variables, so the pristine filesystem,
allowlisted environment and OS boundary still need separate qualification.

The adapter accepts source-enabled Claude only through a concrete `NativeDriver`
value or non-nil pointer. An embedding wrapper could inherit a marker method
while overriding execution, so it is rejected before inspection or launch.
The JSONL wrapper driver is
not an implicit fallback. The native control path's fake-process tests prove
zero user frames for failed/missing/partial tools, wrong helper/config/scope,
uncorrelated responses and pending timeout. They also cover successful startup,
pending-to-ready, forbidden inventories, identity changes and post-turn disconnect.
These are control-protocol fixtures; the exact unmodified client still needs its
credential-free fake-backend qualification before any live use.

The helper command is `/usr/bin/env -i LANG=C.UTF-8` followed by an absolute
`sourceproxy` program and typed socket/UID/limit options. It runs no shell. A
trusted test fixture uses a shell wrapper solely to dispatch the Go test binary;
source/model text never selects or executes it. Production uses the installed
binary directly. The proxy does not load provider auth, interactive profiles,
GitHub credentials, client settings or source objects.

Claude safe mode disables MCP, so the source-enabled path replaces that option
with the `isolated-source-profile-1` requirement. It retains restricted mode,
ignores user/project/local settings, supplies `disableAllHooks`, disables slash
commands and uses a disposable client filesystem. Before inference the trusted
launcher must prove that filesystem has no ambient managed hooks, plugins,
settings or candidate instructions and that external tool/egress restrictions
hold. Only leased credential files may come from protected auth storage; the
client config/home and execution state must be disposable for each invocation.
The whole persistent auth directory must not become a source-enabled client home. A manifest is only a requirement. It is not proof those conditions hold,
and this configuration component supplies no launcher. The separate
[native VM assembly](NATIVE-VM.md) implements guest profile, role and transport
wiring. Broader physical capability/resource qualification remains open. The
default no-source invocation retains safe mode and live drivers remain blocked
by default.

Claude's documented EndConversation control can remain when MCP tools are
present. It is not a source operation. Do not claim the tool flags prove an empty
built-in inventory. Exact client inventory, internal inference/retry bounds,
subscription identity and paid-overflow prevention remain qualification gates.
CLI agentic-turn counts are not inference-request counts. Codex's internal
request/turn enforcement is still a launcher qualification requirement.

`SourceEndpoint` tells the supervisor the session hash, socket, expected reader
UID and required profile. Before starting a client it must bind that exact socket
to the same invocation's broker under the separate credential-free reader
identity. It must protect parent directories and endpoint access from other
workers and candidate content. A matching UID alone does not prove job binding
or prevent a same-UID process from replacing an unprotected path.

Build the helper from the repository root without installing or launching it:

```sh
go -C reviewer build -o /tmp/sfl-sourceproxy ./cmd/sourceproxy
```

On Linux, `ProxySourceMCP` connects only over AF_UNIX, rejects symlink/non-socket
endpoints and verifies `SO_PEERCRED` against the captured non-root reader UID
before reading or sending any client data. It forwards opaque bounded MCP bytes;
the session MCP server enforces methods and source rules. It has no listener,
HTTP client, DNS, arbitrary endpoint scheme or credential flow. Input/output caps
include protocol overhead and exceeding either cap terminates the connection.
Deadlines close owned streams and join both copy directions. Errors are static
and contain no host path, source payload or backend message. Other platforms
fail closed.

Validate with:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Linux fixtures require a non-root account, matching the alpha host. Both generated
client commands execute the fake helper with parent credential/home canaries
absent and complete MCP initialization/discovery plus captured Read/Search over
the private socket. Actual broker receipts retain both search scans and remain
separate from citation validation. Tests check wrong
peer UID consumes/sends zero client bytes, input/output caps, exactly-once stream
closure, blocked-I/O timeout,
symlink/missing endpoints, malformed flags, typed per-session configuration and
profile controls. These are fake-client and transport tests, not provider
inference, account entitlement, official-client tool inventory or OS isolation.

The [bounded reader program](READER-SERVICE.md) implements protected socket
binding and a separate launch-bound audit channel. The
[native VM assembly](NATIVE-VM.md) composes separate reader/client roles, wrapper
execution and host supervisor packaging. Actual aggregate physical isolation,
resource/crash and live-account qualification remain unproved under
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The freeze still forbids credential acquisition, App installation, live inference
or deployment without their separate gates and explicit owner authority.

References inspected October 19, 2026:

- [Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) covers stdio commands, tool allowlists and required startup.
- [Codex noninteractive mode](https://learn.chatgpt.com/docs/non-interactive-mode) covers isolated configuration and required MCP initialization.
- [Claude CLI controls](https://code.claude.com/docs/en/cli-reference) cover MCP, safe/restricted mode, settings, tools and agentic-turn limits.
- [Claude hook precedence](https://code.claude.com/docs/en/hooks#disable-or-remove-hooks) explains why per-run settings alone cannot disable managed hooks.
- [SDK connection sequence](https://github.com/anthropics/claude-agent-sdk-python/blob/c01a99416a02805b0a050d058da51bde53f6df64/src/claude_agent_sdk/client.py#L220-L233) initializes before the optional first prompt; [MCP status](https://github.com/anthropics/claude-agent-sdk-python/blob/c01a99416a02805b0a050d058da51bde53f6df64/src/claude_agent_sdk/client.py#L471-L501) is available without a query.
- [MCP connection timing](https://code.claude.com/docs/en/agent-sdk/mcp#connection-timing) explains why wait deadlines and first-turn init metadata alone are insufficient readiness guards.

The exact installed Codex CLI 0.163.0 also accepted the generated MCP overrides
through its read-only `mcp get` parser in a disposable Bubblewrap guest with an
empty auth/home filesystem and no network. That measured parser check supplies
syntax/config metadata only. It starts no server or model session and establishes
no subscription permission, tool inventory or inference-time isolation.

## Native Claude fake-backend fixture

On Linux with Bubblewrap and the pinned Claude 2.1.303 executable available,
run the [offline fixture](scripts/claude-fixture.py):

```sh
python3 -B reviewer/scripts/claude-fixture.py \
  --client /absolute/path/to/claude-2.1.303 \
  --output /absolute/path/to/new-private-artifacts
```

The script verifies executable SHA256
`375988f457756524234695cf2d8f64ace99c5f68682e47ea24862a843c191450`, builds
`cmd/claudefixture` with Go, and creates a private mount/PID/network namespace.
It mounts no host home, profile or credential file. The helper creates literal
synthetic account bytes in the new namespace and accepts only a loopback fake
Messages endpoint. It requires the synthetic bearer token, exact model and
three-tool inventory, then supplies Read, Search and StructuredOutput responses.
Each subsequent request must contain the preceding actual MCP tool result.
The real NativeDriver checks source readiness before the prompt and again after
completion; its report normalizer processes the actual client result.

Claude emits `rate_limit_event` metadata after tool requests. The source driver
accepts only an initialized stream with explicit `status: allowed` and
`isUsingOverage: false`. Missing, malformed, unknown and overage states fail the
phase. This observation does not prove paid overflow disabled; the production
external egress/billing gate remains required. Tests retain a sanitized event
from this exact client, with only random event/session IDs replaced, plus hostile
mutations. Raw client and MCP transcripts remain in the private local artifact
directory and must not be uploaded.

The actual client intake passes through `ProviderEgress` to a real local TLS
fake backend. Its explicit fake-only policy permits only three POST requests to
`/v1/messages?beta=true`, with zero refresh, metadata or local negotiation slots.
Captured native header names are allowlisted explicitly. The fixture maps its
single loopback intake to one fixed synthetic authority; this substitutes for
production peer/TLS wiring and does not qualify exclusive egress. Both the
intake and guard must stop, and the private provider receipt must pass
`ValidateCompleted` before accepting a report. Counts and the policy are retained
without raw credential values. A valid report cannot override a failed receipt.

The fixture checks exact Read path/revision and Search needle/limit/revision,
both actual source operations, three backend requests,
provenance and final report validation. Host receipts record source commit/tree,
dirty state and helper/client hashes. The compiler cache is disposable; artifact
output must be a new directory. Only the fixture artifact subdirectory is exposed writable from the host.
The host receipt and helper executable stay outside that writable mount. Deadlines are 35 seconds
for the phase and 50 seconds for the outer process. This fixture supplies no CPU
or memory qualification. Launcher identity/stop inputs are deliberate protocol
fakes. Separate reader UIDs, production auth, whole-containment stop attestation,
exclusive provider egress, account eligibility and live inference remain
unqualified. Every result explicitly keeps both actual-account and production-
boundary qualification false. Passing this fixture cannot activate a provider.

The qualification command requires a clean committed checkout before creating
artifacts. It builds an immutable `git archive` of that commit with local
Go tooling, `GOPROXY=off`, `GOSUMDB=off`, VCS resolution disabled and automatic
toolchain downloads disabled. Ambient Go flags, workspace configuration and
external cache programs are discarded. Required dependencies must already be
in the local module cache, or supplied with `--module-cache`; an incomplete
cache fails closed. This is not a dependency installation command.

The selected client is copied and hashed through one opened inode into a private
snapshot outside the worker's writable artifact directory. Bubblewrap executes
that snapshot, so replacement of the original path cannot change the measured
client. Source identity is rechecked around the build and execution. A dirty or
changed checkout cannot emit a passing qualification. Read and Search outputs
are checked separately for one matching, non-error tool result; Read must equal
the complete synthetic source, while Search must decode to the exact captured
revision/path/line/quote anchor. Shared text cannot stand in for the other result.

Before compiling, the same offline environment runs `go mod verify` against the
cached module zips and extracted sources. Modified dependency caches are rejected;
`-mod=readonly` alone does not prove dependency-source integrity.
