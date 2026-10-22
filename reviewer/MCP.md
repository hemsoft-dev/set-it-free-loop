# Session-bound MCP source connection

`ServeSourceMCP` maps the invocation's `SourceTools` broker to two MCP tools,
`read` and bounded literal `search`. It consumes an already-established private
stdio or local IPC connection. It creates no listener, loads no configuration,
reads no filesystem and handles no authentication. Its caller binds that
connection to precisely one captured invocation. Neither tool accepts a job,
repository root, endpoint or credential selector.

The implementation pins the stateful `2025-11-25` MCP protocol. It supports
initialize, initialized notification, ping, tools/list, tools/call and client
cancellation. Initialization negotiates that version and advertises only tools.
A client that offers another version must accept the returned version or
close the connection. The newer stateless MCP lifecycle is not implemented or
claimed compatible. No server requests for roots, sampling, elicitation,
resources, prompts or logging are sent. Other requests receive a method error;
notifications cannot invoke source tools. Tool annotations describe read-only,
closed-world operations, and the broker enforces the actual path/revision rules.

Frames, total input/output, nesting, request IDs, metadata messages, tool calls
and duration have finite limits derived from the captured phase limits. Duplicate
keys and reused request IDs, including alternate string escapes, fail the
connection. Only one source operation runs at a time. An overlapping source
request receives a busy error without starting another read. Client cancellation
cancels that operation and suppresses its late response. Controller cancellation
closes blocked connection I/O, cancels source work and joins handlers before
return. Trusted source implementations must honor cancellation.

Argument and unknown-tool errors cause no source access. Traversal, wrong
revisions and other broker denials produce a generic tool error and retain the
broker's authoritative coverage receipt. Backend errors and host paths are not
sent to the client. Read and Search use the same per-session broker budget,
including every object scanned by search. The returned text remains untrusted
source evidence, and the connection provides no execution operation.

Run the offline fixtures from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Tests exercise both provider identities over private in-memory connections and
verify actual broker read/search receipts, lifecycle/capability negotiation,
denials, cross-snapshot and job-selector rejection, notifications, malformed and
oversized frames, canonical ID reuse, client cancellation, blocked connection
deadlines, metadata floods, output caps and private backend-error redaction.
These tests run no official client or inference.

This implements the source wire protocol. [Client configuration](SOURCE-CLIENTS.md)
and the [native VM assembly](NATIVE-VM.md) implement the separate reader identity,
private IPC and pinned-client wiring, exercised against local fake backends.
Complete physical containment and live-account compatibility remain unqualified under
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
Neither a protocol test nor tool annotations establish those boundaries. The
existing freeze and explicit activation gates still apply.

Protocol references, inspected October 19, 2026:

- [MCP stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports) defines newline-separated JSON-RPC on stdin/stdout.
- [MCP lifecycle](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle) defines version and capability negotiation.
- [MCP tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools) defines tool schemas, discovery and invocation results.
- [Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) and [Claude MCP configuration](https://code.claude.com/docs/en/mcp) document client integration. They do not prove this connection has been qualified with either pinned client.
