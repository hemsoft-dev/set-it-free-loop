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
and no production launcher is supplied. The default no-source invocation retains
safe mode and live drivers remain blocked by default.

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
include protocol overhead and reaching either cap terminates the connection.
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
absent and preserve private socket request/response bytes. Tests check wrong
peer UID consumes/sends zero client bytes, input/output caps, blocked-I/O timeout,
symlink/missing endpoints, malformed flags, typed per-session configuration and
profile controls. These are fake-client and transport tests, not provider
inference, account entitlement, official-client tool inventory or OS isolation.

Continue private reader service binding, official-client wrapper execution and
OS supervisor packaging under
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The freeze still forbids credential acquisition, App installation, live inference
or deployment without their separate gates and explicit owner authority.

References inspected October 19, 2026:

- [Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) covers stdio commands, tool allowlists and required startup.
- [Codex noninteractive mode](https://learn.chatgpt.com/docs/non-interactive-mode) covers isolated configuration and required MCP initialization.
- [Claude CLI controls](https://code.claude.com/docs/en/cli-reference) cover MCP, safe/restricted mode, settings, tools and agentic-turn limits.
- [Claude hook precedence](https://code.claude.com/docs/en/hooks#disable-or-remove-hooks) explains why per-run settings alone cannot disable managed hooks.
