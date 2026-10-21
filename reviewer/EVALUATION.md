# Offline evaluation and pilot gate

The score harness and self-critique control are offline code for
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177).
No live benchmark or pilot has run. Candidate selection predates evaluation and
contains 20 historical PR head/base snapshots. The
[retained source audit](testdata/evaluation/README.md) now supports 14 clean and 6 defect labels, with none unlabeled. The corpus
remains a draft because candidate #156 exceeds the reader inventory cap.
Selection alone was not ground truth or proof that ten cases were clean.
Synthetic tests check arithmetic and failure behavior only.

Agents may create and revise corpus labels and post-run actionability labels.
Every label records its actual agent, provider, model, session, timestamp,
rationale and independent primary evidence. An agent can use the same model as
an evaluated reviewer, but must inspect independent source evidence. The
reviewers' own outputs on these PRs, including Codex or Claude findings,
comments and paraphrases of those findings, are never label evidence. A later
fix is admissible only through independently inspected source changes or a
linked bug/regression issue, even if a review originally prompted the fix.

A defect cites a concrete **later** fix/revert commit or PR, or a linked
bug/regression issue, with its source event time and affected candidate lines.
The lines identify the candidate head, or its base for removed lines. Explain
how the subsequent change corrects the candidate behavior. A later edit in the
same file or intentional retirement alone does not establish a defect.

A clean label states its checked postmerge window and duration in days, the
immutable history revision, the exact searches and coverage limits. Search
later fixes, reverts and regressions touching the changed lines/behavior, plus
bug-report metadata as available. State when runtime exposure ended. A clean
label means no matching defect was found within that search; it is not proof
of defect absence. Do not shorten a window or relabel a demonstrated defect to
reach ten controls. Leave unsupported cases unlabeled and report why.

`EvaluationCorpus` version 2 requires these records. `EvaluationCase.Label`
contains the current label and `LabelHistory` retains all earlier revisions,
oldest first. Each revision's `Supersedes` is `Digest(previousLabel)`, with a
later timestamp. Each defect records the containing label's actor and time
and its own later evidence. Franz can override any label using kind `owner`,
his immutable human ID, and `OwnerOverride: true` on a revision. Agent revisions
use kind `agent`, zero human ID, and never claim owner override. Owner labels
use provider `human`, model `none` and an identified manual session. Retain
superseded labels and disagreements. Finding assessments use the same actor,
time, evidence and revision rules, and must occur after their run completes.

Validation checks record structure, chronology, primary-source URL forms and
revision chains. It rejects review/comment URLs and evidence kinds. It does
not query GitHub or prove that a description is true, that a fix is causal or
that a line is in the candidate diff. Before freezing, audit those claims
against retained primary source and record the commands/results. Never present
schema validation as that audit.

Each case uses repository ID 1169772257, immutable head/base/merge-base and a
fixed evidence packet/source capability. Keep tuning cases separate. Historical
PRs may already have been seen by models or maintainers; record that limitation
when approving a held-out set. Candidate selection predates any live evaluation.

Freeze the complete labeled corpus first, then freeze `EvaluationPlan` with its corpus hash,
trusted release, pinned config and creation time. Any label revision changes the corpus hash and requires a newly frozen plan.
The existing minimum of 20 cases, ten clean controls and one material defect
still applies. If supported labels fall short, retain a draft and report the
real counts rather than forcing labels or relaxing the gate.

One repeat is the default;
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
latency, queue duration, usage availability and ledger counts. An identified agent or Franz
assesses every reported finding after the run against independent source, with rationale and explicit
matches to frozen defects. One defect gets recall credit once per run. Mark
duplicate or unsupported findings non-actionable rather than counting repeated
matches as extra precision. A clean control with any allegation fails its clean
criterion even when its assessor rejects that allegation.

The scorer accepts only complete required schedules for successful runs. Failed
runs cannot contain partial findings. Missing, failed and quota-wait cases stay
in all-case recall and clean-control denominators; report them separately from
completed latency samples. Undefined precision with zero predictions is null.
Unknown usage is counted and never converted to zero-token success. It reports
precision/recall/clean numerators and denominators, Wilson intervals, latency and
queue p50/p95, failures, quota waits, missing cases, concessions and disputes.
Repeated/correlated cases weaken interval interpretation.

After evidence-backed labels and retained sanitized artifacts exist:

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
model response as provider permission. Do not invent labels or use APIs,
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

The final self-critique session cannot introduce new allegations, because no
session remains to examine them. Earlier control additions must receive an
explicit disposition or withdrawal in a later session. Record this control
restriction alongside production adversarial mode's permitted late findings.

Retain a status for every ledger finding ID and derive corroboration, dispute
and concession counters from those statuses. Withdrawn IDs stay auditable and
receive no active precision/recall credit. Clean-control success remains strict:
any retained allegation, including a withdrawal, fails that case's clean metric.
Record actual provider turns and their availability separately from tokens;
unknown turns never become a zero-turn success. Aggregate turns and unknown-run
counts per arm alongside sessions and token usage.
