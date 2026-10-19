# Offline evaluation and pilot gate

The score harness and self-critique control are offline code for
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177).
No live benchmark or pilot has run. The candidate manifest contains 20 actual
historical PR head/base snapshots, with all human labels unset. It is a selection
record, not a frozen ground-truth corpus, and does not establish ten clean cases.
Tests generate synthetic labels only to check arithmetic and failure behavior.

Before tuning or collecting results, Franz must confirm held-out eligibility,
identify at least ten clean controls and assign evidence-backed defect labels
for the remaining cases. Record his immutable human ID, rationale, source
evidence and label provenance in `EvaluationCorpus`. Model outputs cannot create
or revise these labels. The alpha accepts owner labels only. Additional named
human labelers require an explicit policy change. Preserve disagreements as
label evidence; do not quietly resolve them in favor of evaluated output.

Each case uses repository ID 1169772257, immutable head/base/merge-base and a
fixed evidence packet/source capability. Keep tuning cases separate. Historical
PRs may already have been seen by models or maintainers; record that limitation
when approving a held-out set. Candidate selection predates any live evaluation.

Freeze the corpus first, then freeze `EvaluationPlan` with its corpus hash,
trusted release, pinned config and creation time. One repeat is the default;
preselect at most three. The plan rotates six arms by case index and repeat:

| Arm | Sessions | Protocol |
| --- | --- | --- |
| Adversarial | 4 | Codex R1, Claude R1, Codex R2, Claude R2 |
| Independent pair | 2 | One independent R1 from each provider |
| Codex only | 1 | One Codex R1 |
| Claude only | 1 | One Claude R1 |
| Codex self-critique | 4 | Initial pass plus three self-critiques |
| Claude self-critique | 4 | Initial pass plus three self-critiques |

Production modes retain their natural budgets. `RunSelfCritique` gives a single
provider its frozen earlier outputs for three further critiques. Each active
allegation requires a cited confirmation/rejection, inability to verify, or
withdrawal. New allegations retain IDs and cannot reuse prior IDs. Withdrawals
stay in the ledger. Self-confirmation never becomes peer corroboration. A failed
control exposes no substantive partial ledger. Controls have four bounded
sessions with no automatic retries or extra judge.

Hold snapshot, packet, reader policy, pinned clients/auth/models, timeout, tool
and resource caps fixed. Disclose model/context differences, history size and
actual token/turn counts. Aggregate input reserves space for peer/control
history and may reduce report-output caps. Equal sessions do not imply equal
tokens, cost, latency or reasoning effort. Production adapter and OS confinement
qualification is still required for control workers. The control interface is
not a production credential-bearing launcher.

Retain sanitized results with corpus/plan hashes, immutable snapshot, arm,
repeat, start time, provider schedule, consumed sessions, report digest, outcome,
latency, queue duration, usage availability and ledger counts. A named human
assesses every reported finding after the run, with rationale and explicit
matches to frozen defects. One defect gets recall credit once per run. Mark
duplicate or unsupported findings non-actionable rather than counting repeated
matches as extra precision. A clean control with any allegation fails its clean
criterion even when the human rejects that allegation.

The scorer accepts only complete required schedules for successful runs. Failed
runs cannot contain partial findings. Missing, failed and quota-wait cases stay
in all-case recall and clean-control denominators; report them separately from
completed latency samples. Undefined precision with zero predictions is null.
Unknown usage is counted and never converted to zero-token success. It reports
precision/recall/clean numerators and denominators, Wilson intervals, latency and
queue p50/p95, failures, quota waits, missing cases, concessions and disputes.
Repeated/correlated cases weaken interval interpretation.

After human labels and retained sanitized artifacts exist:

```sh
go -C reviewer run ./cmd/evalscore /absolute/corpus.json /absolute/plan.json /absolute/results.json
```

This command only reads files and writes JSON to stdout. It does not run models,
use credentials, publish reviews or promote a release. Invalid or unlabeled
artifacts fail. Synthetic tests exercise this path but are not quality evidence.

Provisional goals are 90% actionable precision, 70% material-defect recall and
90% clean-control success. Compare dual performance with the independent pair,
both singles and both four-session controls, using the same frozen cases and
showing denominators and uncertainty. A tiny, incomplete or inconclusive result
cannot authorize rollout. The score always retains a stop gate requiring the
human comparative decision and pilot evidence. Poor dual performance stops
rollout; it does not silently change Franz's requested default.

Live evaluation still requires applicable public-target subscription support,
account/billing evidence, qualified official workers/source isolation, durable
GitHub transport qualification and explicit activation. Do not treat a successful
model response as provider permission. Do not invent human labels or use APIs,
paid overflow, a machine-user trigger or a private mirror as an inferred fallback.

After all gates pass, promote a separately pinned trusted release with recorded
human provenance. Candidate reviewer prompts/auth/allowlists/runtime remain inert
source input. Merge does not promote a build. Independently check clean and
defective PRs, disputed/late output, provider/quota failure, cancellation and moved
head/base, with actual rendered GitHub evidence. Run an owner-only two-week pilot
on this repo. Record deferred cases, subscription impact, recovery and maintenance,
plus p50 under ten minutes and p95 under twenty minutes when online and quota is
available. Verify stop/rollback before claiming pilot completion. No org expansion,
automatic push reviews, autonomous fixes, approvals or merges belong to this alpha.

The frozen evaluation cap is at most 32 KiB of report output per session, with
256 KiB session input and serialized packets no larger than 64 KiB. Every arm
must honor the same configured output cap; the harness rejects a control that
would reduce it. Results bind the frozen packet hash and exact output cap.
Corpus validation rejects repeated immutable snapshots and requires at least
one material defect. The scorer checks actual start-time chronology against the
frozen arm rotation, rejects substantive counters on failed runs and rejects
peer-corroboration counters from any arm without peer examination. Descriptive
cross-arm deltas and provisional-goal results accompany the mandatory stop gate.
