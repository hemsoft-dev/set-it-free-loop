# Durable evaluation execution

`RunEvaluation` executes the six arms in a validated frozen `EvaluationPlan`.
`OpenEvaluationJournal` binds a private SQLite journal to the exact corpus and
plan hashes. This execution layer does not label findings, score quality,
publish GitHub reviews or authorize model inference.

The natural session counts are four for adversarial review, two for the
independent pair, one each for Codex and Claude alone, and four each for their
single-provider self-critique controls. `plan.Order` determines arm order for
each case and repeat. Every control step is a fresh session with the same
provider and the complete frozen history. No retries or extra sessions pad a
failed arm.

## Operator composition

Supply all four `EvaluationRunnerOptions` fields explicitly:

- `Journal` is a dedicated evaluation journal opened with the frozen corpus,
  plan and repository enrollment. Keep its private directory separate from
  auth stores and interactive profiles.
- `Check` verifies the exact trusted release, eligibility, activation, quota
  and backoff gates. The runner calls it at entry and before possible work.
  A missing check denies execution. Construction does not supply permission.
- `Source` resolves only the requested immutable snapshot and frozen packet.
  Its request contains no corpus labels. `Bind` can use
  `RestoreSnapshotEvidence` to retain the packet's exact source omissions.
- `Workers` constructs audited review or control workers for the requested arm.
  Construction must not start inference. The runner commits a charged phase
  reservation before invoking a worker. There is no provider fallback.

Workers, source capabilities and their resources remain owned by the caller.
The runner joins each call before returning; the caller must preserve resources
needed for stopped recovery. A cross-process directory lock prevents concurrent
runs or retirement in one journal. `Close` refuses to close an active journal.
Opening an existing journal with a different corpus or plan is fenced.

## Durable results and restart

Each arm retains its immutable job, exact normal or control inputs, ordered
phase reservations, charged session count, times, private source/provider
receipts and validated reports. Reports are checked against the output bound
again after authoritative source coverage is merged. A complete ledger is
accepted only after every required phase is durably complete.

On restart, completed or safely failed arms are reused without constructing
workers or reopening source. If every phase was saved but finalization was
interrupted, the journal rebuilds the ledger without calling a model. A stopped
partial arm becomes `incomplete_expired`; it is never silently resumed with a
changed history. An inflight reservation or unsafe receipt stops progression
with `ErrEvaluationRecovery`. Unknown work remains charged.

A trusted before-work failure can refund a reservation only without conflicting
provider requests or pending safety evidence. A known stopped failure produces
no partial substantive ledger. It remains a failed arm in the denominators.

## Explicit stopped retirement

`EvaluationJournal.RetireStopped` requires an explicit operator check, the
original auth store, exact `AuthBinding`, stopped supervisor, and source/provider
receipts for the reserved capability. It cannot replay work or make a failed
attempt successful. Valid receipts already saved for that attempt cannot be
replaced during recovery.

For a pending returned profile, import the matching return through the normal
stopped-auth path first. Retirement persists an intent before completing that
profile. If the final database write fails after disposal, retry uses the
intent, exact returned auth-state hash and absence of a lease. A missing return
never permits the original seed to substitute for returned credentials.

For native VMs, version 2 `auth-return-intent.json`, `native-request.json` and `source-access.json` records
bind the original stopped unit, request, capability, manifest, returned auth
hash and captured receipts. The private canonical native request includes the
captured packet, paths, policy and non-secret profile hints; it contains no auth
bytes. Recovery validates its full hash separately from the capability hash.
The host validates stopped provider evidence for the same captured session and
policy before saving these records and disposing the profile. Invalid source or
provider evidence, or a failed record write, keeps the returned lease. A malformed source audit cannot
create a null source receipt or a qualifying intent.
`NativeVMSupervisor.StoppedAttemptEvidence` reopens those private records;
`ConfirmReturnedAuth` supplies the typed proof for an already disposed profile.
Missing, altered, unqualified or legacy receipts remain fenced. Preserve the
original containment and auth directories until retirement is durable.

## Independent assessment

`EvaluationJournal.Assess` converts retained execution into an `EvaluationResult`
only after the caller supplies independently evidenced finding labels. It then
uses the existing scorer validation. Reviewer findings never label themselves.
Usage remains unknown when a charged attempt lacks captured usage. No execution
receipt establishes that the corpus is held out or that a provider subscription
is eligible for this repository.

## Model-free qualification

The unit tests exercise a synthetic 20-case, 120-arm, 320-session schedule,
SQLite close/reopen with zero replay, failed result/finalization writes,
process exit after reservation, concurrent ownership, unknown stop/auth fences
and explicit stopped retirement. Synthetic labels only validate the protocol.

`cmd/evaluationfixture` and `scripts/evaluation-native-fake.py` are an explicit
local fake TLS qualification. They create fresh synthetic auth and a temporary
Git repository, capture immutable packets, and run one synthetic case through
all six arms with pinned native clients. The other 19 cases remain explicitly
missing. Restart must launch no VM for the six completed arms. This fixture
produces no quality score, provider-policy approval or live activation grant.

Use a clean committed source tree and a measured native image:

```text
python3 reviewer/scripts/evaluation-native-fake.py --run-fake-only \
  --image /absolute/measured-native-image \
  --output /absolute/new-private-receipt-directory \
  --git /absolute/pinned-git --git-sha256 <measured-sha256> \
  --go /absolute/pinned-go --module-cache /absolute/offline-module-cache
```

Retain the exact source and image hashes, runner receipt, native unit journals,
source/provider records and physical stop/runtime-cleanup evidence. A build or
fake result alone does not qualify host resource containment. Actual comparative
model evaluation, provider/account eligibility and the owner-authorized pilot
remain separate gates under issues #173, #174 and #177.

For a separate, fresh synthetic run, add `--stopped-journal-fault adversarial`,
`codex-self-critique` or `claude-self-critique`. A real SQLite trigger refuses the
selected arm's first stopped result write. Earlier arms retain their natural
order and session counts. The fixture closes and reopens its execution journal,
proves that the inflight reservation blocks replay without reopening source or
workers, and retrieves the original supervisor's durable stopped/auth-return
proof. The already disposed profile must remain disposed. Explicit retirement
keeps the attempt charged, marks it incomplete and produces no ledger. Repeating
retirement is idempotent; a subsequent run reuses the retired arm without a new
native call and stops at the next deliberately denied source request. The result
records each gate and the original proof/retirement hashes. This is an opt-in
qualification mode, not a live recovery command or permission to replay work.
