# Aggregate end-to-end qualification

`TestE2EQualification` in [e2e_qualification_linux_test.go](e2e_qualification_linux_test.go)
is an opt-in host qualification for
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174). It
composes the components that were previously qualified separately: the version
2 startup with the measured [source VM](GIT-VM-SOURCE.md), polling discovery,
the [native VM](NATIVE-VM.md) providers and COMMENT publication through the
outbox. Everything is fake-only: synthetic App JWT and installation tokens,
synthetic provider auth, a loopback TLS fake GitHub, a local fake Git upstream
and loopback TLS fake model backends. It never contacts a real provider or
GitHub, installs no App, imports no account and activates nothing. Passing it
does not qualify live eligibility, provider policy, paid usage or deployment.

The test skips unless all of `SFL_E2E_SOURCE_IMAGE`, `SFL_E2E_SOURCE_STUB`,
`SFL_E2E_NATIVE_IMAGE` and `SFL_E2E_CASE` are set.

## What is composed

The test lives in the external package `reviewer_test`, so it can import the
fake-only model producers in `internal/nativefixture`, which themselves import
this package. A small in-package bridge,
[e2e_export_linux_test.go](e2e_export_linux_test.go), exposes read-only test
seams: deriving the source VM release and its manifest's runtime hash from a
measured image, the native manifest hash production derives from a provider
release for one launch, nonsecret projections of the source VM owner journal
and archive, the production provider receipt validators, production's
projection of a remote review into its marker and canonical document hash,
production's rendering of a completed review document from given reports and
ledger, and a restore probe.

1. **Startup.** The public `OpenGitReviewerService` is called with a version 2
   `GitReviewerRelease`: the measured source image and exact-head
   `gitsourcevmstub`, plus both native providers pinned to a measured native
   image. The source image's embedded guest release must name its recorded
   commit as `SourceHead`, so a guest built from another commit cannot run
   under this commit's release revision. The stub's embedded Go build info must
   record `vcs=git`, `vcs.revision` equal to that commit and
   `vcs.modified=false`; a stub without that stamp is refused. The running
   `reviewer.test` binary is attested the same way through
   `debug.ReadBuildInfo`: its program path must be this package's test
   binary, with one `vcs=git`, `vcs.revision` equal to the release commit and
   `vcs.modified=false`. The stamp is recorded as `TestBinary` in the JSON
   result, and an unstamped or mismatched binary fails the run. The public
   constructor uses the real user-systemd source VM owner
   (`openGitVMUnitOwner`); no private opener or internal composition is used.
   The credentials are an installation journal over the controller SQLite
   store, a synthetic App JWT callback and two loopback transports. `Refresh`
   and `Run` are called explicitly. Every reopen uses fresh store, journal and
   provider auth handles, as a restarted process would.
2. **Discovery.** A local TLS fake GitHub serves the App and installation
   handshake, role-scoped token minting, the repository, open PR list, PR,
   base ref, compare, issue comments and the review collection for repository
   ID 1169772257. It produces an automatic PR-open request (PR created after the
   trigger epoch) or an owner `@sfl review` comment request (PR created before
   the epoch). It records every request by role and flags any credential that
   crosses the app, reader or publisher boundary, any write with a reader
   token, any Git read without the reader token and any unexpected destination.
   Each role may use only the routes production's credential split gives it:
   the App JWT qualifies the installation and mints tokens; each installation
   token requalifies the installation, the bot user and the repository; then
   only the reader polls, discovers and reads triggers and Git objects, and
   only the publisher posts, scans, reads receipts and annotates. Any other
   route is a violation and is refused, even on the role's own transport. At
   the end, each case must have served exactly its own per-role route set:
   every case the qualification and reader routes, a publishing case also the
   review POST and its receipt read, and `close-publish` also the review scan.
3. **Source acquisition.** The same server answers smart-HTTP
   `info/refs` and `git-upload-pack` with the installed Git producer over a
   synthetic bare upstream, through the reader installation credential that
   the service's source VM read channel attaches. Capture runs in a real
   transient `sfl-alpha-<attempt>.service` unit with KVM and the measured
   source image.
4. **Review.** Each provider host `Check`, which the native reviewer calls once
   before launch, binds a fresh canned fake backend to that session's captured
   revision (`nativefixture.NewForSnapshot`). The default adversarial
   configuration runs four native sessions (Codex R1, Claude R1, Codex R2,
   Claude R2) in real native VM units with the pinned clients. Every phase does
   real source `read` and `search` calls against the source-VM-acquired
   evidence and returns a source-anchored synthetic finding or peer judgment.
5. **Publication.** The engine outcome is prepared as a COMMENT review intent,
   dispatched through the publisher role and confirmed by whole-document
   reconciliation against the fake review collection.

## Cases

| Case | Scenario | Source / native units | Source records | Auth at end |
| --- | --- | --- | --- | --- |
| `review` | PR 1 automatic request through capture, four sessions and one confirmed COMMENT. | 1 / 4 | 1 | both usable |
| `isolation` | PR 1 (automatic) and PR 2 (owner comment) pending together. | 2 / 8 | 2 | both usable |
| `close-capture` | Close the whole service after capture and admission, inside the first session's host check, before any VM launch. Reopen and finish. | 1 / 4 | 1 | both usable |
| `close-provider` | Close the whole service while the first Codex session waits for a withheld inference response. | 1 / 1 | lifecycle order only | Codex fenced, Claude usable |
| `close-provider-reopen` | A second process attached to the root retained by `close-provider` (`SFL_E2E_ATTACH`), as a restarted service. | 0 / 0 | 1 (retained) | Codex fenced, Claude usable |
| `close-provider-recover` | Instead of `close-provider-reopen`: a restarted process recovers the retained attempt through the operator API, then reviews and publishes the next request. | 1 / 4 | 2 (1 retained) | both usable (Codex re-provisioned) |
| `close-publish` | The fake GitHub commits the review POST but withholds its response; close the whole service; reopen and reconcile. | 1 / 4 | 1 | both usable |

If `close-provider`'s `Close` proves completion instead of reporting
`ErrWorkerStopPending`, the case reopens and expects one source unit, one
native unit for the interrupted launch plus one per artifact of the reopened
job, and both auth stores usable. The job must record the interrupted
launch's complete stopped audit evidence. After the reopen it runs the same
completed-review checks as the other successful cases (see Evidence bindings),
with the interrupted launch, stopped, audited and incomplete, as the only
other launch. No host run has taken this branch.

Every case also observes `systemctl --user list-units 'sfl-alpha-*'` every 200
ms, excluding units that existed before the run. At the end it first stops
and joins the periodic sampler, so no listing still in flight can record a
unit afterwards, and then requires a successful listing of its own that shows
none of the units it saw. The last listing is therefore the last observation.
A failed listing never counts as an empty one. It keeps the last successful residual, or every unit
seen before any successful final listing. A listing line that is not one
`sfl-alpha-*` unit with its three states, or that repeats a unit, fails that
listing rather than dropping a unit. If the 30-second grace ends without
a later successful listing, the run fails and records the error as
`UnitListingError`. Each observed unit must correlate with a launch the
run recorded: a source unit with a source VM owner journal record's unit, a
native unit with `sfl-alpha-<launch>.service` for a native launch directory
created by this process. Every launch recorded by this process must have been
observed, and the observed source and native counts must equal the case's
expectation in the table. An attached process may hold the earlier process's
source records, which it need not observe. Before `close-provider-reopen`
accepts an attached root, the root must hold exactly one durable job: PR 1's,
still leased, bound to PR 1's scope and the deterministic fake upstream's
snapshot, and admitted under the current release revision. A root retained
by another revision's `close-provider` is refused, so its interrupted job and
launch are never recorded as this revision's restart. Before
`close-provider-reopen` treats a native launch as the earlier process's, its
root must hold exactly one launch: started, settled, captured, aborted and
stopped, with a bound stopped audit that records the auth as exported and not
returned and the session as incomplete.
Its provider and session must be the retained job's outstanding attempt, which
must be the session the job derives for that provider and round and its only
charged session beyond its artifacts. The job must hold exactly one provider
attempt, that one, and its recorded stopped provider evidence must equal the
audit's complete provider evidence by digest; another valid stopped receipt of
the same session is refused. The job may hold no source attempt but that
attempt's own. The audit must record that attempt's request, the zero report
and exactly the source receipt the job recorded for it (none at `daac99c`),
end at the wait for its result record and fail with `auth-return`. The
first `close-provider` process applies the same check before it retains the
root. A missing launch, an unreadable launch directory or any other entry in
the native root fails setup, and a retained launch must still be present at
the end. Every check of source stop receipts
first requires exactly one owner journal record per expected job, each with
exactly one archive export of the same attempt. As production recovery
requires, the export's request hash must be the digest of the record's source
request, and its binding (attempt, release hash, runtime hash and snapshot)
must equal the record's complete materialization binding. Record and export
agreeing is not enough: the record must name `sfl-alpha-<attempt>.service`,
carry the current release package hash and the runtime hash of the source VM
manifest that release derives, record the release's source stub hash, and
bind the snapshot of exactly one expected job, each job's once. As restore
requires, the staged export's packet hash must be the digest of that job's
packet. This holds for the attached restart too, whose record must bind the
retained job. Each final record must hold the start intent, the settled start,
the capture, the grant, the revocation and the durable stop, recorded in the
current boot and without a later boot's stop proof (`StopBoot` empty). That
conjunction says nothing about
order, so the process that acquires the source also proves it (see Source
lifecycle order below). Once
`review`, `close-capture` and `close-publish` confirm the review, PR 1 must
retain exactly one request, the admitted automatic `opened` request its job
came from, and no other PR may retain any request. In `isolation`, PR 1 must
retain exactly its job's admitted `opened` request, PR 2 exactly its job's
admitted owner `comment` request, and no other PR any request. Each request's
origin must carry the fake upstream's pull request ID, and a comment request
the ID of the fake upstream's owner comment: both appear in the published
trigger line. A further
request could otherwise still be admitted after the reviews are confirmed and
produce a duplicate review. After the provider close, PR 1 must retain
exactly one request: the admitted automatic `opened` request of its job. At
the end of `close-provider-reopen`, PR 1 must still retain only that request,
PR 2 exactly one unadmitted owner `comment` request, and no other PR any
request. A duplicate held back by the lease would otherwise become a second
job once the lease is released. Every case records the requests it judged as
`Requests` in the JSON result. The fence states (`close-capture` after close,
both `close-provider` processes) must hold PR 1's job as the only job and no
write intent. The case enumerates every entry of each provider's native root
except the supervisor's `executables` directory, reads each launch's
`unit.json` and `native-audit.json`, and checks provider auth disposition by
reopening the protected stores. The store itself accepts any nonempty state,
so a store is usable only when it is unfenced and holds the provider's
original synthetic credential, judged by the hostile fixture's rule: the
original synthetic access and refresh tokens, and for Codex the `chatgpt` auth
mode. Corrupted, cross-provider or substituted auth is invalid, which is
neither usable nor fenced. A store is fenced only when it refuses the refresh
with `ErrAuthRecoveryRequired` before yielding its state, as it does while an
exported profile awaits recovery. Any other refusal (a malformed or unsafe
state file, an invalid lock file or a filesystem error) leaves it
unavailable, which is neither usable nor fenced. A directory without a readable `unit.json`,
any other entry and an unreadable root each count as a failed launch, which
no check accepts. The unit journal is read only through production's own
reader (`readUnit`, exported to the test as `E2EReadNativeUnitJournal`): a
private, singly linked file, strictly decoded, of the `native-vm` payload
under the release's stub, with a possible lifecycle (a capture needs a
settled start and a bound process and cgroup, a stop needs the abort). Its
boot ID and its captured process's must be the current boot's, with a systemd
invocation ID and process start time, and every native unit must be started,
settled, captured, aborted and stopped. It then counts only
as version 1 for its own directory's launch. It must name
`sfl-alpha-<launch>.service`, and its binding must derive the containment ID
from its session and launch. It must also be the pinned native payload:
payload kind `native-vm`, the release's stub hash, and the manifest hash that
production's `VMBootImages.NativeManifest` derives from the release's QEMU,
kernel, initrd, firmware, runtime artifact, namespace helper and bootstrap
library pins for that launch and session. A test checks that the harness
derives exactly production's manifest. A journal of another payload, stub or
native release therefore proves nothing. The audit is read as production's
exact encoding of its receipt (`E2EReadNativeAudit`): a private file whose
fields are exactly `persistAudit`'s, in its order, and whose re-encoding is
its bytes, so an unknown, missing, duplicated or reordered field is refused.
It counts only as a version 1 receipt with the same binding and manifest hash,
and its auth lifecycle must be possible: auth is returned only after it was
exported, and a completed session has returned it. A completed audit ends at
`result-validation` with no failure kind; an interrupted one records one.
Once auth was exported or the session completed, the
audit must carry provider evidence: the stopped receipt of that session under
the release's egress policy for that provider. The receipt must also pass the
production provider receipt validators under the session limits. Every
receipt must meet the stopped-receipt invariants that recovery requires before
recording a stopped attempt: consistent, nonnegative and in-budget request,
response and byte counters, and no exhaustion or denial without failure. The
receipt of a completed session must also pass the completed-receipt validation
an artifact requires: not failed or exhausted, at least one inference, every
forwarded request answered and no denial. The receipt's counters are recorded
as `Access`, and the digest of the complete provider evidence as
`EvidenceHash`, for each native unit. A copied, foreign, regenerated, failed
or impossible audit therefore proves nothing about the launch, and every
native unit at the end must have a bound audit. In `isolation`, every session
from the first onward must carry one of the two job IDs, and its PR and head
must be that job's PR and the fake upstream's head of it, as must each source
attempt's head. Each restore probe must report exactly its
five reads in order. Both own reads must return the fake upstream's exact
bytes from both the materialized store and the model-facing reader, each
judged by the SHA-256 of what it returned (`StoreHash`, `ReaderHash`), not
merely by its success. Every foreign read must be refused by both and return
nothing.

### Evidence bindings

Every completed case (`review`, `isolation`, `close-capture`, `close-publish`,
and `close-provider` if it reopens) judges its completed PRs together:

- **Jobs and intents.** The durable state holds exactly one job and exactly
  one review intent for each reviewed PR, and no other job or write of any
  kind. A second job for a PR is refused, not resolved by picking the newest.
- **Job.** The job is exactly the one admission derives from the harness's
  own inputs, restated rather than read back from the job: the ID of its
  retained request, the owner as requester, the retained request's complete
  origin, the configuration the harness opened the store with at its initial
  version 1, the PR's scope, the fake upstream's head, base and merge base,
  and the current release. Only its generation, which the intent must repeat,
  and its packet, whose digest the staged export must carry, are the job's
  own. Every later expectation (sessions, reports, launches, source records
  and document) is derived from this expected job, so a job admitted with
  another valid origin or configuration cannot shape its own expectation.
  `close-provider`, `close-capture`'s recovery and the attached restart hold
  their job to the same expected job. Its checkpoint is of the job and
  consumed exactly four sessions. It holds no lease, deadline, reservation or
  pending charge, its charge days are distinct dates, Codex then Claude were
  audited, and its last attempt is the Claude R2 artifact's attempt and
  session, requiring provider evidence.
- **Artifacts.** The checkpoint holds exactly Codex R1, Claude R1, Codex R2
  and Claude R2, once each. Each artifact's job hash, report job ID, snapshot
  hash and config hash are the expected job's, and no two artifacts share a session.
  Each artifact's session is the one the job derives for its phase, as the
  engine does: `NewSession` under the job's protocol limits, an R2 session
  freezing the stored R1 reports.
- **Reports.** Each artifact holds exactly the canned backend's report for its
  phase, restated in the harness rather than read back from the run: in R1
  the provider's one synthetic finding at `source.go:1` (title, body, severity
  2, quote and the head revision), in R2 a confirmation of the peer's R1
  finding with that anchor as evidence, no retraction and no coverage, under
  the job's policy, packet, snapshot and config headers. Usage is provider
  accounting, is never published and is not compared. No artifact carries
  engine-added coverage, and each holds exactly the engine's one validation
  read: `source.go` at the head with the fake upstream's bytes.
- **Attempts.** The job records each artifact's provider evidence in exactly
  one provider attempt, of the artifact's session and provider, and the
  artifact's source access in exactly one source attempt of that same attempt
  ID. Each artifact requires provider evidence and passes the validation
  production runs on stored artifacts before recovery or publication. The job
  holds no other provider or source attempt, except the allowed interrupted
  launch's attempt with at most its one source attempt, and no attempt ID
  repeats.
- **Launches.** Each artifact session ran in exactly one completed native
  launch of the artifact's provider whose audit evidence equals the
  artifact's by digest, with auth exported and returned and a whole-unit stop.
  Its audit's request hash is the digest of the request production builds for
  that phase (the derived session, the job's sorted diff paths, the release
  policy and the provider's profile hints), its report and source hashes are
  the artifact's report and source receipt, and its code host receipt, native
  result, guest stage and boot diagnostics pass production's predicates. No
  other launch exists except an allowed interrupted one, bound to the one job
  attempt that records its evidence, with the zero report and exactly that
  attempt's recorded source receipt, or none.
- **Backend checks.** Each artifact session was checked exactly once outside a
  fault hold by its own provider's fake backend, for its job, round, PR and
  head. Any other check of the job must be one a fault hook held.
- **Exchanges.** Each artifact session met the exchange contract the native
  fixtures require of a canned phase: its backend answered exactly three
  inferences with no failure and no telemetry, and for Codex captured every
  mandatory metadata route with no negotiation reaching it. The phase made the
  canned read and search (two tool calls). Before any of this, the artifact's
  source receipt must pass production's `SourceAccess.validate` for the
  derived session and record no exhaustion or omission. The artifact's
  stopped receipt counts exactly the inference and metadata requests the
  backend observed.
  Every object the source service served the phase is the fake upstream's
  exact bytes of that path at the job head, and `source.go` is among them.
  The reader call count is not fixed: search reads every allowlisted path of
  the job. Backend counts are read from each session's own backend whenever
  the sessions are collected, so they include every request it served.
- **Intent.** The sole intent is `confirmed`. Its scope, request, generation,
  snapshot, job hash and checkpoint hash are the job's; its marker names its
  own ID; its body hash is its document's digest; and its document carries
  exactly one inline finding, at `source.go:1`.
- **Document.** The intent's document is exactly the one production's
  renderer builds for the expected job (its trigger line from the retained
  request's origin, its configuration and version from the harness) from the
  canned reports, the harness's own expected ledger
  (each finding corroborated by its peer's R2 confirmation and linked to the
  other provider's exact duplicate) and the PR's trusted diff, with the
  intent's marker, in canonical comment order. Its inline comment and summary
  state the canned finding literally: `P2`, its title and body, and the source
  link to `source.go#L1` at the head. Intent and remote review agreeing with
  each other is therefore not enough; both must be the known output.
- **Remote review.** The PR received exactly one POST and holds one COMMENT on
  the fake upstream's head, whose ID is the intent's receipt ID, whose sole marker is the
  intent's, and whose canonical document hash, computed as production computes
  a remote receipt (body plus inline comments in comment order), equals the
  intent's body hash. Its inline anchors are exactly `source.go:1`.
- **Upstream.** Each acquisition made exactly one successful advertisement and
  one successful upload-pack wanting exactly its head and the base, and
  nothing else reached the fake upstream. Every want line counts: a want-ref
  or malformed object ID is kept as an unrecognized want, which fails the set.
- **Source lifecycle order.** While serving each Git exchange, before it
  answers, the fake upstream samples the source owner journal through the
  open service. Production rechecks the live, granted, unrevoked instance
  before every upstream read. The sample taken at an attempt's one upload-pack
  must hold exactly one record of that attempt: start recorded and settled,
  unit captured and granted under the final record's unit, invocation and
  grant hash, and neither revoked nor stopped. The final record must be
  revoked and stopped. Start, settle, capture and grant therefore precede the
  upstream read, and revocation and the durable stop follow it. The order
  among start intent, settle and capture is not sampled. It is the journal's
  own invariant, which production enforces on every write: a capture requires
  a settled start, a settled start requires the start intent, a stop requires
  the revocation, and a grant is written only for a captured, unrevoked
  record. `TestGitVMOwnerJournalRejectsUnboundLifecycleStates`,
  `TestGitVMOwnerDurableStartCaptureAndOriginalStop` and
  `TestGitVMGrantRefusesBeforeDurableAuthority` cover it. `close-provider`
  judges this order before it retains its root. The attached
  `close-provider-reopen` process did not acquire the source, so it judges the
  final records only.

In `close-publish`, the confirmed intent must be the one the closed POST left
`unknown` or `dispatched`, with the same ID, marker, body hash, job,
checkpoint, request and snapshot. The JSON result records each write's
`Marker` and `BodyHash`, each remote review's `document_hash`, each job's
`ArtifactSessions` and `ArtifactSourceCalls`, each native unit's
`ManifestHash`, each restore probe's `ReaderHash`, each Git exchange's
`Journal` sample, and the run's `SourceRuntimeHash` and `SourceStubHash`.

The root is owned as soon as it is created (or, for an attached run, once its
layout is checked), before the unit observer starts. A setup failure after
that point still stops the observer, closes the fake servers after any open
service and removes a fresh root unless `SFL_E2E_KEEP_FAILED` is set. A
failed SQLite close or checkpoint of any store handle is a failed check.
Every close, `close-provider`'s included, waits for `Run` to return before it
cancels `Run`'s own context, so a pass proves that `Close` itself stopped it.
After closing the fake servers, whose `Close` waits for every handler to
return, a
finished run re-reads them: any violation, review POST, remote review, Git
exchange, model session or backend request that the final judgment did not
see is a failed check. Token mints and the API request tally are not
compared after settling, since a wedged handle keeps its credential owner
until the process exits; the final judgment holds the tally's per-role route
set to the case's. Each
run first removes an earlier `<case>.json` from `SFL_E2E_EVIDENCE`, and does
not start if it cannot. It writes the nonsecret JSON result only after cleanup
has closed every handle and removed (or deliberately retained) the root, so a
cleanup failure is recorded as `Passed: false`. The result goes to a synced
temporary in the same directory, which is renamed over `<case>.json`; a failed
write leaves no result rather than an earlier one.

### Record inventory

Every durable record the harness reads, the production validator it applies
before using a field, what it compares, what it leaves open and why, and the
rule that refuses an extra entry.

| Record | Validator applied | Fields compared | Unconstrained (reason) | Exact set |
| --- | --- | --- | --- | --- |
| Native `unit.json` | production `readUnit`: private read, strict decode, payload, stub, possible lifecycle | version, binding (launch, derived containment), unit, payload, stub, manifest hash, boot ID, process boot ID, all five lifecycle flags | manager owner, start and stop jobs, PID, cgroup (host-assigned; `readUnit` checks their shape and the unit cgroup); invocation ID and start ticks beyond shape (systemd- and kernel-assigned) | one per launch directory; every root entry projected |
| `native-audit.json` | exact `persistAudit` encoding; production request validity, code host and native-result predicates, guest stages, failure kinds | version, binding, request, manifest, report and source hashes, provider evidence (digest), completion, stage, failure, auth exported and returned | code host hashes and sizes, native result hash, guest stage and boot diagnostics beyond production's predicates (guest-internal, not derivable on the host) | one per launch |
| Provider evidence (audit, artifact, attempt) | production stopped and completed receipt checks; stored-artifact evidence check | policy (release), session, inference and metadata counts (backend), digest across audit, artifact and attempt | byte, forwarded, response, negotiation and refresh counters within production's bounds (transport timing) | one per attempt |
| Source receipts (artifact, attempt) | production `SourceAccess.validate` for the derived session | version, session, tool calls, every read (fixture bytes at head), no exhaustion or omission, digest across artifact, attempt and audit | reader calls and bytes within production's bounds (search reads every allowlisted path) | one per artifact attempt; at most one for an interrupted attempt |
| Durable job | none in production's loader | the whole job against the expected job (ID, requester, complete origin of the retained request, configuration and version 1 from the harness, fake upstream snapshot, current release), scope, request, state, checkpoint hash and count, lease, deadline, reservation, pending charge, charge days (distinct dates), audited providers, last attempt (ID, provider, round, session, evidence requirement) | generation (production's per-PR counter; the intent repeats it), packet (capture output; the staged export binds its digest), creation time and charge-day values (wall clock) | one per expected PR, no other job |
| Artifacts | engine session derivation; stored-artifact evidence check | job hash, session and report (derived from the expected job; report canned), coverage (none), validation reads (one), source receipt, provider evidence, evidence requirement | report usage (provider accounting, never published) | exactly Codex R1, Claude R1, Codex R2, Claude R2 |
| Provider and source attempts | production receipt checks | ID (an artifact's or the interrupted launch's), evidence and receipt by digest | none | the artifacts' attempts plus the allowed interrupted one |
| Write intents | none | ID, kind, scope, request, generation, job and checkpoint hashes, snapshot, document (canned render for the expected job), body hash, marker, state, receipt ID | creation and receipt times (wall clock) | one review intent per PR, no other write |
| Native requests | none | PR, ID, origin actor, kind, role, pull ID and comment ID (fake upstream), admitted, removed; the complete origin equals the admitted job's | requester, bot, snapshot, creation time and the origin's policy, sequence, body hash and event times (trigger provenance production checks at admission) | the expected request per PR, none elsewhere |
| Source owner records | production strict canonical journal reader | attempt, unit, invocation, request and source request hashes, stub, release, runtime, snapshot, lifecycle flags, grant, empty `StopBoot`, boot ID | channel ID and generation (per-attempt nonces, distinct per job in `isolation`); manager owner, start job, birth, process and cgroup (host-assigned; production checks their consistency) | one per expected job |
| Archive exports | production archive journal reader | attempt, state (`staged`), request hash, release, runtime, snapshot, packet hash (job packet) | wire hash and size (restore verifies them against the payload) | one per source record |
| Released private copy | production store reread | the whole job: only state, lease, deadline, reservation and pending charge differ | none | one job, no write |
| Fake GitHub, model and Git observations | harness-owned | POSTs, reviews (marker, canonical document hash, inline), Git exchanges (operation, wants, status, journal sample), session checks and backend counts, every API route against its role's production capability | token mints, API request counts, and API tallies after settling (a wedged handle keeps its owner) | per-role route set equals the case's; settled observations equal the judged ones |

## Admission rule

The controller admits one invocation at a time. `selectWork` returns nothing
while any durable job holds a lease, and `Poll` starts at most one background
worker, so a second pending request is captured and reviewed only after the
first job's invocation ends. Publication of the first job runs in a later
outbox cycle and can overlap the second job's work. The `isolation` case
therefore proves isolation between sequential jobs, not concurrent ones. It
observes that the two jobs' session checks (session starts) do not interleave.
That whole sessions do not overlap follows from this rule; the case does not
observe it.

## Running

Build exact-head images from a clean commit (see
`scripts/git-source-vm-image.py` and `scripts/native-vm-image.py`), then build
the stub and the test binary from one fresh clone of the same head. Go does not
stamp VCS build info in a linked worktree, and `go test -c` stamps it only
with `-buildvcs=true`. Check both stamps, then run each case under the shared
VM lock, with a bounded timeout:

```sh
SRC=$(mktemp -d)
git clone --quiet --no-hardlinks --no-checkout . "$SRC"
git -C "$SRC" checkout --quiet --detach "$(git rev-parse HEAD)"
go -C "$SRC/reviewer" build -buildvcs=true -o "$OUT/gitsourcevmstub" ./cmd/gitsourcevmstub
go -C "$SRC/reviewer" test -c -buildvcs=true -o "$OUT/reviewer.test" .
rm -rf "$SRC"
for binary in gitsourcevmstub reviewer.test; do
  go version -m "$OUT/$binary" | grep -E '^\s+(path|build\s+vcs(\.revision|\.modified)?=)'
done
chmod 0500 "$OUT/gitsourcevmstub" "$OUT/reviewer.test"
flock -o ~/.cache/sfl-vm.lock env GOMAXPROCS=2 \
  SFL_E2E_SOURCE_IMAGE="$SOURCE_IMAGE" SFL_E2E_SOURCE_STUB="$OUT/gitsourcevmstub" \
  SFL_E2E_NATIVE_IMAGE="$NATIVE_IMAGE" SFL_E2E_CASE=review SFL_E2E_EVIDENCE="$EVIDENCE" \
  timeout 600 nice -n 10 "$OUT/reviewer.test" -test.run '^TestE2EQualification$' -test.v -test.timeout 590s
```

For `close-provider`, run `close-provider-reopen` next in a new process with
`SFL_E2E_ATTACH` set to the `Root` recorded in `close-provider.json`, inside
the same lock hold. State lives under a fresh `/tmp/sfl-e2e-<nonce>` root,
which a passing run removes. `SFL_E2E_KEEP_FAILED=1` retains a failed root.

## Measured at `13251e3`

On October 23, 2026, each case ran on mini under the shared VM lock, one VM at
a time and only with at least 2.5 GB of memory available, from source and
native images, a source stub and a `reviewer.test` all built from `13251e3`,
which is this branch on `main` with #249, #252, #253 and #254. Both binaries
carried `vcs.revision=13251e3…` and `vcs.modified=false`, and the release hash
was `13e1372d6849`. Every case passed with no failures under the checks above,
including every durable record judged through production's readers and the
record inventory, each job held to the job expected from harness inputs, and
each GitHub credential role held to its endpoint allowlist and exact route
set. No unit or E2E root remained afterwards. Only this document changed after
`13251e3`.

| Case | Passed | Seconds | Checks | Source / native units |
| --- | --- | --- | --- | --- |
| `review` | yes | 26.6 | 52 | 1 / 4 |
| `close-publish` | yes | 24.4 | 59 | 1 / 4 |
| `close-provider` | yes | 12.5 | 33 | 1 / 1 |
| `close-provider-reopen` | yes | 38.6 | 29 | 0 / 0 |
| `close-capture` | yes | 25.4 | 64 | 1 / 4 |
| `isolation` | yes | 43.2 | 91 | 2 / 8 |

Earlier runs at `c29e563`, `3bbf701`, `e144bc5`, `0cd2152`, `b876290`,
`daac99c` and `89382c7`, before the later checks, also passed every case.
`close-provider` took the branch where `Close` reports `ErrWorkerStopPending`;
the branch where it proves completion has not run on a host. The sections below
are pre-rebase history only: the commits they name are not ancestors of this
branch, and their checks predate the attestation and binding rules above.

## Pre-rebase history: measured results at `3140f0a`

This section records a run before the branch was rebased. Commit `3140f0a` is
not an ancestor of the current branch.

Measured on mini on 2026-10-22, one case at a time under the shared VM lock,
with `GOMAXPROCS=2` and `nice -n 10`. Code, test binary, stub and both images
come from commit `3140f0a`; that commit is also the release revision, and the
package digest is `12d1adf7…88d3`. At that time only this document changed
afterwards. Every case passed with no failed check and no fake GitHub
violation. The observer saw no remaining unit from any run.

| Case | Wall time | Source VM units | Native units | Upstream Git exchanges | Review POSTs | Final state |
| --- | --- | --- | --- | --- | --- | --- |
| `review` | 30.6 s | 1 (1.4 s) | 4 (5.3–6.8 s) | 2 | 1 | `dual_complete`, write `confirmed` |
| `isolation` | 51.9 s | 2 (1.2 s each) | 8 (4.6–6.2 s) | 4 | 1 per PR | both `dual_complete`, both `confirmed` |
| `close-capture` | 25.4 s | 1 | 4 | 2 (none after reopen) | 1 | `dual_complete`, `confirmed` |
| `close-provider` | 13.1 s | 1 | 1 (stopped, incomplete) | 2 | 0 | `running`, lease retained |
| `close-provider-reopen` | 38.6 s | 0 | 0 | 0 | 0 | `running`, lease retained |
| `close-publish` | 29.6 s | 1 | 4 | 2 | 1 | `dual_complete`, `confirmed` |

Unit times are first-to-last observation by the 200 ms observer, not exact
lifetimes. Each `review` and `isolation` session was a real native VM with a
`Captured`, `Stopped`, `Complete` and `AuthReturned` receipt. Each source
attempt had a durable granted stop receipt and a staged export.

- **review.** An automatic `opened` request reached a confirmed COMMENT on the
  captured head, with one inline comment at `source.go:1`, 30.4 s after
  startup. One reader token and one publisher token were minted.
- **isolation.** The owner-comment request on PR 2 ran first because its
  comment predates PR 1's observation. All eight sessions ran as two
  uninterrupted blocks with one job switch. The two source attempts had
  distinct attempt IDs, units, invocations and read channels, and each
  upload-pack wanted only its own head and the base. Each job was restored
  from retained bytes with no upstream exchange. It read its own `source.go`
  and `prN-only.go`. Its materialized store and reader refused the other PR's
  path at its own head, and both files at the other head.
- **close-capture.** When the hold began, the job was `running` with one
  reserved session and a lease. `Close` returned in 13 ms. The cancelled check
  counted as a trusted before-work failure, so the job returned to `queued`
  with no consumed session and no lease. The reopened service restored the
  retained source without refetching, ran four sessions and published once.
- **close-provider.** The withheld Codex R1 request was cancelled by `Close`.
  The unit was `Captured`, `Aborted` and `Stopped`. Its audit recorded the
  auth as exported but not returned (`auth-return`). The job stayed `running`
  with one consumed session, its lease and an outstanding reservation. All
  three `Close` attempts returned `ErrWorkerStopPending`. Codex auth refused
  further use; Claude auth stayed usable.
- **close-provider-reopen.** The restarted process opened the service in
  217 ms, proved the source attempt stopped and discovered the new PR 2
  `@sfl review` request. Over 30 s it captured nothing, started no session,
  published nothing and kept the same lease. `Close` again reported
  `ErrWorkerStopPending`.
- **close-publish.** The intent was `dispatched` when the POST was held, and
  `unknown` after `Close` (15 ms). After reopen, the full-collection scan found
  the marked review and confirmed receipt 5001 within 0.5 s. GitHub received
  exactly one POST.

### Pre-rebase history: rerun with tightened checks at `7b6b668`

This rerun also predates the rebase. Commit `7b6b668` is not an ancestor of
the current branch.

Review of this qualification tightened its checks. The native image commit
must equal the source image and release commit. Every native release asset
must match the image's recorded pins. The `close-provider` stopped audit must
record an exported, unreturned auth, and the private-copy `ReleaseStopped`
probe must succeed. The remote review must carry the inline comment at
`source.go:1`. Each isolation acquisition must want exactly its own head and
the base. A successful job must consume exactly four sessions, each in
exactly one completed native launch, with no other launch. `Run` may return
only the cancellation caused by `Close`, plus `ErrWorkerStopPending` when the
lease is expected to remain.

All cases were rerun on mini on 2026-10-22 under the same constraints, with
code, test binary, stub and images from commit `7b6b668` (release digest
`a15cb324…9ede`). Every case passed with no failed check, no fake GitHub
violation and no remaining unit:

| Case | Wall time | Source VM units | Native units | Upstream Git exchanges | Review POSTs | Final state |
| --- | --- | --- | --- | --- | --- | --- |
| `review` | 23.5 s | 1 | 4 (4.1–4.9 s) | 2 | 1 | `dual_complete`, `confirmed` |
| `isolation` | 54.8 s | 2 | 8 (4.9–6.2 s) | 4 | 1 per PR | both `dual_complete`, both `confirmed` |
| `close-capture` | 27.5 s | 1 | 4 | 2 | 1 | `dual_complete`, `confirmed` |
| `close-provider` | 14.0 s | 1 | 1 (stopped, incomplete) | 2 | 0 | `running`, lease retained |
| `close-provider-reopen` | 38.6 s | 0 | 0 | 0 | 0 | `running`, lease retained |
| `close-publish` | 26.5 s | 1 | 4 | 2 | 1 | `dual_complete`, `confirmed` |

The first `review` attempt at this commit failed and is retained as
`run-7b6b668-review-fix1`. It ran while two native image builds were running
on the same host outside the VM lock, with memory under pressure. The first
Codex unit was SIGKILLed 6 s after start with a 1.1 GiB memory peak. Its
journal then recorded `Aborted` but not `Stopped`. The native reviewer
returned `ErrWorkerStopPending`, the job kept its lease with no recorded
provider evidence, and `Close` kept reporting `ErrWorkerStopPending`. This is
the documented fail-closed path when a stop cannot be proved, not a
qualification of behavior under load. The cause of the early unit failure
and of the missing stop proof was not isolated. The rerun without concurrent
builds passed. Host resource pressure remains outside this qualification.

The nonsecret JSON results, logs, build logs, summary and run scripts are
kept outside the repository in the merge-mission evidence directory
`e2e-174/`.

## Findings

**A shutdown during a provider session fences the whole controller.**
Cancelling a native session while its client waits for a response stops and
proves the whole unit, but the guest's result record (with its source audit
and auth return) never arrives. The native reviewer then reports
`ErrAuthRecoveryRequired` and `ErrSourceAuditPending`. `RunStopped` keeps the
durable lease and outstanding reservation for any source-audit-pending error,
and the exported Codex auth stays fenced. This is the documented fail-closed
design, and the case confirms it end to end:

- `GitReviewerService.Close` keeps returning `ErrWorkerStopPending`. It
  retains the source factory, credential owner and startup lock, so the same
  process cannot reopen the service. Retrying does not help, because nothing
  in the service clears a lease.
- After a process restart, startup and source VM recovery succeed and polling
  discovers new requests. But no admission happens for any PR, including a new
  owner-comment request on a different PR. No capture, provider session or
  publication occurs.
- `ReleaseStopped` succeeds on a private copy of that state. The attempt's
  durably recorded stopped provider evidence satisfies the store, so an
  explicit trusted supervisor could release the lease. A nil error is not
  taken as the release: the copy is reread through a fresh handle and must
  hold the job as `auth_failed`, unleased and not outstanding, with every
  other field unchanged (job, charged session, attempt, source and provider
  attempt records), and no other job or write. The service composition
  never calls it, and the Codex auth fence would still need an explicit
  stopped-return or retirement path.

Without an operator recovery path, any service stop or crash between a
session's reservation and its stopped receipt would leave the reviewer
offline. Before activation, that path needs to be specified and qualified as a
separate gate: native stopped-attempt verification, a lease release that
records a charged session, auth return or retirement, and job disposition. The
alternative is a bounded grace on shutdown that lets the guest stop its client
and return auth. Both change security-relevant recovery behavior, so neither
was implemented in the commits measured above. The operator path was added
later and is qualified in
[Operator recovery after a provider-session close](#operator-recovery-after-a-provider-session-close).
The bounded shutdown grace is not implemented.

A shutdown before a session's work, while the host check runs, is
different. The native reviewer reports a trusted before-work failure, the
reserved session is refunded, and the job returns to `queued`.

## Operator recovery after a provider-session close

[`RecoverStoppedProvider`](GIT-STARTUP.md#stopped-provider-recovery) answers
the finding above with an explicit operator path; nothing runs it
automatically. `close-provider-recover` qualifies it end to end. It runs in a
new process attached to the root retained by `close-provider`, inside the
same lock hold, as `SFL_E2E_FOLLOWUP=close-provider-recover` in the run script.

1. Before startup, the job is `running` with its lease and one charged
   session. The only native launch is a Codex unit whose stopped audit
   records exported, unreturned auth. Codex auth is fenced and Claude auth is
   usable.
2. The service is opened, but not refreshed or run.
   `ProviderRecoveryCandidates` lists the job, its attempt ID, recorded
   stopped evidence and exactly the one original launch, whose stopped
   audit receipt binds that attempt ID. A root retained by a build whose
   receipts lack the attempt ID cannot be recovered.
3. A wrong attempt ID is refused with `ErrFenced`. The correct request without
   `RetireUnreturnedAuth` is refused with `ErrAuthRecoveryRequired`. After
   both, the lease, job state and auth fence are unchanged.
4. With consent, recovery returns `Auth: retired, State: auth_failed`. The job
   has no lease, no outstanding attempt, one consumed session, no artifact,
   no source attempt and one recovery receipt bound to the launch. A repeated
   call is refused. Codex auth is retired (unusable), and Claude auth is
   untouched. Neither startup nor recovery has started a source or native
   unit, and the interrupted launch is still the only one.
5. The test re-provisions a synthetic Codex seed, standing in for the
   operator's separate auth repair, then calls `Refresh` and `Run`. Once
   PR 2 is admitted, recovery is refused with `ErrLiveBlocked`.
6. The PR 2 `@sfl review` request is captured by one upload-pack wanting
   exactly its head and the base. It runs four native sessions, each in one
   completed launch with auth returned. It is published as one marked
   COMMENT with the inline comment at `source.go:1`, then confirmed.
7. PR 1 stays `auth_failed` with no new job, write or POST. PR 2 has exactly
   one review intent, and it is `confirmed`. The only GitHub POSTs were two
   role token mints and one review POST for PR 2. The owner journal holds two
   stopped source records, PR 1's retained attempt and PR 2's, each with one
   staged export. Final `Close` succeeded, `Run` returned only cancellation,
   and no unit remained.
8. The process observed exactly one source unit, PR 2's acquisition, and
   four native units, one per PR 2 session. Every one correlates with a
   recorded attempt or a launch directory this process created. The retained
   PR 1 attempt and interrupted launch belong to `close-provider` and are not
   counted. Both auth stores are usable at the end: the re-provisioned Codex
   auth was returned by PR 2's sessions.

Measured on mini on 2026-10-22 with code, test binary, stub and both images
from commit `098261f` (release digest `4861b9df…cfb6`), under the shared VM
lock with `GOMAXPROCS=2` and `nice -n 10`. Only this document changed
afterwards. `close-provider` passed in 14.5 s (15 checks). Then
`close-provider-recover` passed in 26.6 s (40 checks, no failure, no fake
GitHub violation). Startup took 191 ms. Recovery returned 35 ms after
startup completed, and PR 2 was confirmed 26.4 s after startup with receipt 5002. The
observer saw one source VM unit and four native units in the recovery
process, and none remained. Evidence is kept outside the repository in the
merge-mission directory `e2e-174/recovery/run-098261f-close-provider-recover2/`.
That run predates the per-case unit, source record, auth and sole-intent
checks in steps 4, 7 and 8. Its `close-provider-recover` evidence, and that of
the later runs at `483b17a` and `34c9d96`, meet them when re-evaluated with the
same helpers. Those runs did not take the step 4 observer sample, but in each
the first unit was first seen after recovery returned.
An earlier run at `ac57673` passed every check except one: its
running-service refusal raced `Run`'s start and got `ErrFenced`. The check
now waits for admission.

This qualifies only the recovery path for a stop proved in process. It does
not cover a host process kill before the stopped audit receipt. Recovery
refuses that case because the receipt is required evidence, and it remains
fenced. Re-provisioning provider auth after retirement is an operator action
outside the reviewer, and the test only simulates it with a synthetic seed.

## Limitations and remaining work

- Crash points use whole-service `Close` (cancellation), not `SIGKILL` at an
  arbitrary instruction. `close-provider-reopen` is a real process restart, but
  only after in-process cancellation has already stopped the unit. Mid-capture
  process loss is covered separately by the source VM `crash` case in
  [GIT-VM-SOURCE.md](GIT-VM-SOURCE.md), and native supervisor process loss by
  [NATIVE-VM-CRASH.md](NATIVE-VM-CRASH.md). A `SIGKILL` between session
  reservation and stopped receipt would leave the same durable fence as
  `close-provider`, without the in-process stop proof.
- Operator recovery covers a provider attempt stopped with a durable
  stopped audit receipt (see the recovery section). An attempt without that
  receipt, such as after a host process kill mid-session, stays fenced.
- Isolation is between sequential jobs because admission is serial. Both jobs
  share the per-provider auth stores by design; each session exports and
  returns them, and the case checks returns but cannot observe a concurrent
  crossover.
- The fake GitHub covers the endpoints this composition uses with single-page
  collections. It does not reproduce real App consistency, pagination at scale
  or rate limits. The canned model backends prove transport, protocol and
  source binding, not review quality.
- One host, small synthetic repositories, no resource pressure, no live
  credentials. Provider eligibility, App installation, trusted promotion and
  owner activation remain the separate gates in the
  [activation checklist](../docs/research/2026-10-21-alpha-activation-checklist.md).
