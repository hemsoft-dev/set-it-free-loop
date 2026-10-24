# Pinned Codex source and denied-write observations

The source-enabled adapter gives Codex 0.163.0 explicit controls for its pinned
`gpt-6-astra` model. That model selects code-mode-only regardless of the ordinary
feature boolean. MCP `omit_tools_from=["deferred"]` exposes Read/Search in that
runtime without requiring ToolSearch. Namespace exclusions remove builtin
nested handles, and explicit controls disable questions and native V2 agent
tools. The ordinary `multi_agent=false` setting alone does not control the
model-selected V2 path. Analytics and feedback are disabled explicitly, and
OpenTelemetry metrics use the `none` exporter. Account/model policy metadata
remains distinct from nonessential telemetry.

These are necessary configuration controls. They are not a complete capability
allowlist or proof of the required external isolation profile. The adapter
still requires that profile and defaults to a disabled live driver. No model,
metadata, provider or authentication fallback was added.

Run the [offline fixture](scripts/codex-fixture.py) on Linux with Bubblewrap:

```sh
python3 -B reviewer/scripts/codex-fixture.py \
  --client /absolute/path/to/codex-0.163.0 \
  --code-host /absolute/path/to/codex-code-mode-host \
  --case source \
  --output /absolute/path/to/new-private-source-artifacts

python3 -B reviewer/scripts/codex-fixture.py \
  --client /absolute/path/to/codex-0.163.0 \
  --code-host /absolute/path/to/codex-code-mode-host \
  --case undeclared-patch \
  --output /absolute/path/to/new-private-negative-artifacts
```

Both executable hashes are pinned before launch. The command requires clean
committed source, builds an immutable archive with offline-only module resolution
and executes verified private executable snapshots. Missing cached dependencies
fail closed; no automatic dependency or toolchain download is allowed. The fixture mounts only those
executables, its own helper and minimal `/usr/bin/env` runtime dependencies in a
new PID/mount/network namespace. It creates synthetic stored ChatGPT account
bytes in a fresh profile. No real profile, host home, SSH socket or credential
is mounted. Only a local fake backend is reachable. A local HTTP426 response
negotiates the same builtin provider's WebSocket attempt down to HTTP; the fixture
never opens a WebSocket or substitutes a model/provider. The actual HTTP intake now passes through `ProviderEgress`, using a fixed
fixture authority and an explicit fake-only transport to a real local TLS
backend. The guard handles local426 without forwarding any upgrade. Metadata
has a ceiling of five and the guard serializes upstream calls with at most four
intake handlers. Captured Cache-Control and Responses-Lite header names are
included explicitly; telemetry routes remain absent from the guard policy.
A stopped private provider receipt must pass `ValidateCompleted` before this
fixture accepts a report. Counts/policy are retained without auth values.
Guarded cases also set the Codex request policy, so a request whose tool
inventory differs from the measured code-mode pair is refused before the fake
backend receives it.

This qualifies only the captured protocol path through this guard. The local
HTTP listener and fixed Host mapping stand in for production peer/TLS wiring;
it is not a production egress, separate-identity or exclusive-network proof.

The fixture requires four or five startup metadata requests and zero telemetry
requests. The client can omit a second model-list request on a short run. Five
remains the strict ceiling. Per-route observations require one or two model-list
requests with the pinned client-version query, two account checks and one user
settings request. Repeated requests cannot substitute for a missing route.
Analytics traffic is counted separately and rejected, even if the
client ignores that failure and still returns a valid report.

The source case checks the exact model and advertised `functions.exec`/`wait`
wrappers, then asks the runtime for its actual `ALL_TOOLS` names. They must be
exactly `mcp__sfl_source__read` and `mcp__sfl_source__search`. It executes Read and
Search against the real immutable Reader over a synthetic ObjectStore. Distinct exact non-error Read and Search
results must appear in the subsequent native requests before the backend sends
its final report. Production CLI output normalization and report validation
process the actual native envelopes. The source log must record the exact Search needle `immutable` and limit `5`,
so a different matching query cannot pass on this one-line fixture.
Source operations use the fixture helper,
not a separately qualified reader OS identity.

The negative case emits an undeclared direct `functions.apply_patch` call for a
synthetic `/work/undeclared-canary.txt`. It requires an explicit native rejection
by the read-only sandbox and absence of that file. This observation is narrow:
the hidden handler is still dispatchable even though absent from `ALL_TOOLS`.
The CLI can subsequently produce a normally accepted report without an attempted
patch event in its JSONL output. Therefore a clean report and a small advertised
inventory cannot establish that only declared capabilities were attempted.
The external supervisor and provider protocol policy remain required, and any
change to the owner's capability boundary needs explicit owner direction.

Every receipt leaves `capability_allowlist_qualified`,
`actual_account_qualified` and `production_boundary_qualified` false. The fixture
has 35-second phase and 50-second outer deadlines, with no CPU/memory qualification
or production whole-containment stop attestation. It records helper/client hashes,
source commit/tree and dirty state. Raw native/MCP/backend transcripts stay in
its private local artifact directory and must not be uploaded. No actual model
inference, account entitlement, billing control, App installation, promotion or
activation is proved by either case.

Pinned source references:

- [Model tool-mode precedence](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/mod.rs#L63-L86).
- [MCP exposure and runtime namespace exclusions](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/spec_plan.rs#L197-L289), [nested construction](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/spec_plan.rs#L950-L1009).
- [Question controls](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/spec_plan.rs#L1331-L1369) and [native V2 control](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/spec_plan.rs#L724-L740).
- [Direct call routing](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/router.rs#L258-L393), [registry lookup](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tools/registry.rs#L576-L584) and [native MCP event mapping](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/exec/src/event_processor_with_jsonl_output.rs#L207-L236).

Telemetry controls are pinned in the [analytics sender](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/analytics/src/client.rs#L314-L324).
The [Git attribution policy](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/ext/git-attribution/src/policy.rs#L54-L101)
still requires account settings; the retired Git attribution feature flag cannot
be used as an effective suppression switch.
