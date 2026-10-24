# Provider response tool restrictions

A CLI's advertised inventory does not prove that hidden handlers cannot execute.
The pinned Codex baseline dispatched an omitted apply_patch handler; its readonly
sandbox rejected the write and its later transcript omitted the attempted call.
This guard inspects provider responses before any response bytes reach a CLI.

EgressPolicy.ResponsePolicy is trusted launch configuration. The predefined
policies are codex-source-responses-v1 and claude-source-messages-v1. The private
provider receipt's PolicyHash binds the selected policy. An empty field preserves
legacy fake receipts and means unknown enforcement, never a qualified native
fallback. The [native VM supervisor](NATIVE-VM.md) requires its captured policy.

The bridge already buffers bounded responses. For inference, it now validates
the entire SSE or JSON response before writing status, headers or body. A denied
response latches bridge failure and returns a fixed HTTP 400 without tool bytes.
Its request, forwarding and observed response-byte counters remain charged; it
is not counted as an accepted response and cannot qualify a completed receipt.
Metadata, refresh and local negotiation keep their separately captured routes.
Codex response.failed and response.incomplete also terminate the validated stream
and reach the client unchanged, matching response.completed framing. Their
nested output objects still pass the same tool restrictions, and later executable
events remain denied. This is transport acceptance, not successful review output;
the native phase decoder must still validate completion and the report.
The [official streaming reference](https://developers.openai.com/api/reference/resources/responses/streaming-events)
defines these terminal response events.

The Codex policy admits custom functions.exec calls and assistant/reasoning data
in its supported response events. Other custom or native function handlers are
rejected, including hidden apply_patch. The Claude policy admits only the exact
source read/search MCP names, StructuredOutput and the permitted EndConversation
control. The names share the native-output policy to prevent drift. Both policies inspect nested
output containers, including terminal response objects, and nonstreamed JSON.
Ordinary text mentioning a tool remains text. Standard HTTP JSON error envelopes
can still reach the native client's error handling. A validated Claude in-stream
error is also a terminal data event, including before message_start. It reaches
the client unchanged without latching bridge failure; later tool events and
malformed or executable error envelopes remain denied.

SSE validation bounds frames, requires the terminal event and rejects unknown
events, duplicate JSON keys, malformed/unfinished frames, inconsistent event
names and ambiguous fields. Bare CR is rejected so an EventSource line break
cannot hide executable data behind a validator comment. Claude block indices,
open/closed state and assembled input JSON are checked before delivery. Provider
byte limits, timeouts and source-reader limits remain separate bounds.

## Codex request tool inventory

`EgressPolicy.RequestPolicy` is the request-side counterpart. Its only value is
`codex-source-requests-v1`, and only for Codex. Native Codex launch, release
validation and stopped-attempt recovery require it together with the Codex
response policy; Claude requires it empty. The private `PolicyHash` binds it,
and an empty value is omitted, so existing policy bytes are unchanged.

The host sees plaintext JSON. The guest proxy forwards the client's HTTP
request over the private stream, both Codex launch paths set
`features.enable_request_compression=false`, and `Content-Encoding` is not an
allowed request header. The bridge reads the complete bounded body. For every
inference request it then checks the advertised tool inventory, before it
builds an upstream request. The policy accepts only this shape, measured from
pinned Codex 0.163.0 in code mode:

- valid UTF-8 holding one JSON object, at most 8 MiB, with no duplicate keys
  and nesting of at most 32;
- only the measured top-level keys (`client_metadata`, `include`, `input`,
  `model`, `parallel_tool_calls`, `prompt_cache_key`, `reasoning`, `store`,
  `stream`, `text`, `tool_choice`). An unknown key, such as a top-level
  `tools` list or a stored `prompt`, could carry or select tools.
  `tool_choice` is absent or `auto`, and `model` is a nonempty string;
- `input[0]` is an `additional_tools` item with exactly `id`, `role`
  (`developer`), `tools` and `type`, holding one `namespace` named `functions`
  with exactly `description`, `name`, `tools` and `type`;
- that namespace holds exactly two tools, `exec` and `wait`, each once.
  `exec` is a `custom` tool with exactly `description`, `format`, `name` and
  `type`, and a `grammar`/`lark` format. `wait` is a non-strict `function`
  tool with exactly the measured parameter schema;
- the `exec` description declares exactly `mcp__sfl_source__read` and then
  `mcp__sfl_source__search`, each under its own `###` heading. Code mode lists
  every nested tool it exposes that way: the widened capture lists
  `apply_patch`, `read_mcp_resource`, `view_image` and others;
- no other `tools` key, `additional_tools` item or `namespace` object appears
  anywhere else in the request, including history items, metadata and the
  output schema.

Everything else is refused: a missing, unparseable, misplaced or duplicate
inventory, unknown tool types, extra or missing keys, and nested namespaces.
A refused request gets a fixed HTTP 400 without detail and latches bridge
failure. It counts as a reserved inference with its request bytes and in
`request_denials`, but it is never forwarded. The native fatal marker then
makes the guest stop its client and return auth, exactly as for a refused
response. `ValidateCompleted` rejects any nonzero count. In a stopped receipt,
each denial must be an inference that was not forwarded, under a failed
receipt that carries the request policy.

Evidence: [codex-0.163.0-code-mode-requests.json](testdata/codex-0.163.0-code-mode-requests.json)
holds two sanitized requests from a fake-only run of the pinned client, a
first turn and a third turn with two `exec` calls and their outputs. The
inventory item and every top-level key are unchanged. Of 130 recorded
`/v1/responses` bodies from this client, none had a top-level `tools` key and
each had one `additional_tools` item at `input[0]`. The 109 with the source
flags match the accepted shape. The 21 recorded before the namespace
exclusions are widened, as kept in
[codex-0.163.0-widened-code-mode-tools.json](testdata/codex-0.163.0-widened-code-mode-tools.json).
The [provenance file](testdata/codex-0.163.0-code-mode-requests.provenance.json)
records the sources and hashes. The `codexInventory` flattener in
`internal/nativefixture` remains test-only; a fixture test checks that it and
this policy agree on both captures.

Limits of this check:

- It checks the advertised inventory, not what the client can dispatch. The
  response policy still validates every inference response.
- The `exec` description may omit deferred nested tools, so the heading check
  does not prove the runtime nested set. The code host's `ALL_TOOLS` and the
  response policy stay the runtime boundaries.
- The shape was measured with bundled and synthetic catalogs against fake
  backends only. A real catalog that changes the shape is refused, so the
  review fails closed. Real catalogs are unchecked.
- Claude has no request policy. Every recorded Claude request and hostile
  case advertised exactly `StructuredOutput` and the two source tools, and its
  source policy has no metadata or settings route. No comparable gap has been
  measured; see [HOSTILE-FIXTURES.md](HOSTILE-FIXTURES.md#boundary-failure-codex-model-metadata-restores-direct-tools).

## Validation

Request-policy tests accept both measured requests, byte for byte and
re-encoded, and refuse the direct-mode and measured widened inventories,
nested exec tools, nested and extra namespaces, later inventory items, tool
lists in history, metadata and schemas, duplicate, missing and misplaced
inventories, unknown tool types, altered tool shapes, unknown top-level keys,
duplicate JSON keys, excess nesting and oversized or malformed bodies. Bridge
tests show a refused request reaches no upstream, is recorded, cannot
complete a receipt and leaves the response policy unchanged. Native tests show
the guest stops its client on the refusal and the session ends as a failed
phase with committed returned auth.

Unit tests include actual bridge HTTP intake, allowed tool streams and JSON,
nested/late forbidden tools, mismatched names/namespaces, duplicate keys and
arguments, unfinished blocks, oversized frames, terminal ordering and framing
ambiguities. A rejected late record exposes no earlier buffered tool bytes;
private counters retain the physical work and policy digest.

The pinned-client fixtures select these policies for successful source reviews.
Codex also retains its original unguarded readonly-denial baseline as historical
comparison and adds guarded-undeclared-patch. Claude adds guarded-bash. Guarded
negative cases require one upstream inference, native failure without a report,
no source operation/canary, and a stopped failed bridge receipt. They are not
allowed to pass by accepting a later report after the forbidden call.

These are offline fixtures with fresh synthetic profiles and local fake TLS
backends. A passing response filter does not qualify subscription entitlement,
capability isolation or production containment. In particular, allowed Codex
JavaScript still needs a physically separate code-host identity with no access
to the auth-bearing client's profile. The [native VM assembly](NATIVE-VM.md) implements those role, auth and private
bridge bindings. Complete hostile/resource/crash qualification remains open;
no live inference or activation is enabled.
