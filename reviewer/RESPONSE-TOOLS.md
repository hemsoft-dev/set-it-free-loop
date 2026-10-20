# Provider response tool restrictions

A CLI's advertised inventory does not prove that hidden handlers cannot execute.
The pinned Codex baseline dispatched an omitted apply_patch handler; its readonly
sandbox rejected the write and its later transcript omitted the attempted call.
This guard inspects provider responses before any response bytes reach a CLI.

EgressPolicy.ResponsePolicy is trusted launch configuration. The predefined
policies are codex-source-responses-v1 and claude-source-messages-v1. The private
provider receipt's PolicyHash binds the selected policy. An empty field preserves
legacy fake receipts and means unknown enforcement, never a qualified native
fallback. The future native supervisor must require its captured policy.

The bridge already buffers bounded responses. For inference, it now validates
the entire SSE or JSON response before writing status, headers or body. A denied
response latches bridge failure and returns a fixed HTTP 400 without tool bytes.
Its request, forwarding and observed response-byte counters remain charged; it
is not counted as an accepted response and cannot qualify a completed receipt.
Metadata, refresh and local negotiation keep their separately captured routes.

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

## Validation

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
to the auth-bearing client's profile. VM, auth and exclusive bridge integration
remain separate implementation work; no live inference or activation is enabled.
