# Native single-provider controls

`NativeVMControlReviewer` supplies one fresh native session for one step of
`RunSelfCritique`. The benchmark control has four sequential steps of the same
pinned provider. Each input contains its immutable job and frozen earlier
control reports. There is no peer provider, invented peer report, persistent
conversation, retry or judge. Self-confirmation stays unverified. Rejection
marks a finding disputed, and withdrawal retains a retracted ledger entry.
Step four cannot introduce another allegation. Failure exposes no partial
substantive ledger.

## Typed input and output

`ValidateControlSession` replays the earlier reports' hashes, steps and complete
dispositions. It requires an explicit single-provider job, unchanged limits,
exactly `Step-1` prior reports and the common frozen evaluation byte limits.
`ValidateControlReport` applies the same reducer checks used by the control
loop. Native entry validates before eligibility, assets or auth. The trusted
host verifies prior source quotes against the immutable reader before boot.
Each frozen report uses its own bounded verifier with the same snapshot,
allowlist and per-session limits. Historical verification does not consume
another prior report's allowance or the current output's reader budget.
Within one history preflight, repeated reads of the same immutable revision and
path reuse captured object bytes. Every report still validates every quote and
charges its own reader calls and bytes. The cache holds at most the sum of the
historical reports' reader allowances (three reports maximum), keeps no failed
or oversized reads and is discarded before the current worker starts. Replay
retains the caller's existing phase deadline; it does not extend model time or
detach cancellation. Distinct slow objects can still exhaust that deadline and
must fail before auth or provider work.

`ControlCapabilitySession` derives an R1 capability with job ID
`control_` plus the digest of the full control input. It preserves the original
packet, snapshot, config hash, limits and pinned identity. Source, provider and
auth receipts bind this capability. It is neither a production model task nor
a publication job. Changing the step or history changes that binding.

A private VM version 2 request explicitly selects `evaluation-control-v1` and
carries the typed control input. Recomputing its capability must match the
captured session. The ready and finished request hashes cover both. Version 1
review bytes are unchanged; an old guest cannot accept a version 2 request as
review. Control support requires newly built, measured guest assets and a
separately approved release. Existing release and evaluation-plan receipts do
not qualify this new protocol.

The actual prompt contains canonical control JSON once, with precomputed input
and job hashes for copying. `ControlReportSchema` preserves the control format's
existing capitalized JSON field names. It cannot fall back to `Report`.
Both report types share pinned CLI envelope decoding and its tool, completion,
usage and byte limits. Usage comes from client metadata. Claude retains its
same-client source readiness and final status barriers. Codex retains ephemeral
stdin execution and its fixed schema file. Both keep the fixed source tools and
subscription-only profile path.

## Ownership and evidence

`CritiqueAudited` returns the typed report, source access and provider evidence.
The native runner uses the same concrete VM lifecycle as review. Physical stop,
provider/source drain, matching auth return, cleanup and private audit persistence
precede successful completion. Invalid model output may still return matching
auth after proven stop. Uncertain stop cannot import auth or free recovery state.
Output source anchors and merged omission coverage are validated on the host,
including the final output cap.

The fake producer supports the same exact Read and Search exchange before
synthetic control output. Its four steps introduce, reject, cannot-verify and
withdraw one synthetic finding. Run the opt-in fixture separately for each
provider with a newly built image:

```sh
python3 -B reviewer/scripts/native-vm-fake.py --run-fake-only \
  --image /absolute/path/to/new-control-image \
  --output /absolute/path/to/new-private-control-evidence \
  --provider codex --case self-critique \
  --go /absolute/path/to/go --module-cache /absolute/path/to/module-cache
```

Use `--provider claude` and another new output directory for Claude. The fixture
creates synthetic auth, uses only a local fake TLS backend, checks frozen report
lineage and records each session's stopped/auth-return evidence. It refuses
mixed denial, refresh or adversarial modes. It proves transport only, even when
all assertions pass.

## Remaining work and gates

On October 21, 2026, source and fresh image `da1846c` passed four control
sessions each with pinned Codex 0.163.0 and Claude 2.1.303 against the local
fake TLS backend. Codex's four phases took 22.858 seconds and Claude's took
22.186 seconds. Each phase performed the exact Read and Search exchange before
output, retained the expected frozen history and passed transport-owned usage
validation. All eight units were captured, stopped and inactive with zero main
PID and no remaining control group. Matching auth returns completed, and the
owned QEMU runtime directories were removed. These are synthetic producer and
native lifecycle observations, not model-quality, account or whole-host Git
acquisition qualifications. Private receipts and synthetic auth remain local.

This control component does not run or score the frozen corpus. The separate
[durable evaluation runner](EVALUATION-RUNNER.md) implements six-arm execution,
private attempt reservations, no-replay restart accounting and post-run
independent assessment. Actual comparative model outputs and judgments, a
qualified frozen release/plan and the owner-authorized pilot remain unproved
under [#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177).
The corpus and its labels, packet hashes and freeze receipts are unchanged.

Unit tests use fake workers, fake kernel/unit evidence and Unix protocol
transports. The separate pinned-client observations above cover the positive
four-step producer path. Swapped output types, forged hashes/history, source
failures, forbidden tool events, quota and uncertain-stop rejection are tested
with synthetic transports and kernel evidence. Those negative cases have not
all been reproduced through the actual clients. Existing whole-host containment
failures remain failed.
Provider policy, account entitlement, public-target subscription support,
quota and paid-overflow evidence, full containment/resource/recovery
qualification, final App/trigger evidence and explicit owner activation remain
separate gates. This module grants no inference, auth import, App or deployment
authority.
