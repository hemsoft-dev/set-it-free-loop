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
seams: deriving the source VM release from a measured image, nonsecret
projections of the source VM owner journal and archive, the production
provider receipt validators, and a restore probe.

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
| `close-provider` | Close the whole service while the first Codex session waits for a withheld inference response. | 1 / 1 | not asserted | Codex fenced, Claude usable |
| `close-provider-reopen` | A second process attached to the root retained by `close-provider` (`SFL_E2E_ATTACH`), as a restarted service. | 0 / 0 | 1 (retained) | Codex fenced, Claude usable |
| `close-publish` | The fake GitHub commits the review POST but withholds its response; close the whole service; reopen and reconcile. | 1 / 4 | 1 | both usable |

If `close-provider`'s `Close` proves completion instead of reporting
`ErrWorkerStopPending`, the case reopens and expects one source unit, one
native unit for the interrupted launch plus one per artifact of the reopened
job, and both auth stores usable. After the reopen it runs the same
completed-review checks as the other successful cases: exactly one confirmed
review intent, one POST, one marked COMMENT on the head with the inline
comment, four artifacts from four consumed sessions, stopped source receipts
with staged exports, and one completed native launch with returned auth per
artifact, with the interrupted launch as the only other launch. No host run
has taken this branch.

Every case also observes `systemctl --user list-units 'sfl-alpha-*'` every 200
ms, excluding units that existed before the run. At the end it requires a
successful listing that shows none of the units it saw. A failed listing never
counts as an empty one. It keeps the last successful residual, or every unit
seen before any successful final listing. If the 30-second grace ends without
a later successful listing, the run fails and records the error as
`UnitListingError`. Each observed unit must correlate with a launch the
run recorded: a source unit with a source VM owner journal record's unit, a
native unit with `sfl-alpha-<launch>.service` for a native launch directory
created by this process. Every launch recorded by this process must have been
observed, and the observed source and native counts must equal the case's
expectation in the table. An attached process may hold the earlier process's
source records, which it need not observe. Before `close-provider-reopen`
accepts an attached root, the root's PR 1 job must still hold its lease and
must have been admitted under the current release revision. A root retained
by another revision's `close-provider` is refused, so its interrupted job and
launch are never recorded as this revision's restart. Before
`close-provider-reopen` treats a native launch as the earlier process's, its
root must hold exactly one launch: captured and whole-unit stopped, with a
bound stopped audit that records the auth as exported and not returned and
the session as incomplete.
Its provider and session must be the retained job's outstanding attempt, and
the job must hold that attempt's stopped provider evidence. A missing or
unreadable launch directory fails setup, and a retained launch must still be
present at the end. Every check of source stop receipts
first requires the expected nonzero number of owner journal records, each with
exactly one archive export of the same attempt. As production recovery
requires, the export's request hash must be the digest of the record's source
request, and its binding (attempt, release hash, runtime hash and snapshot)
must equal the record's complete materialization binding. Each reviewed PR
must have exactly one review intent, and it must be `confirmed`. Once
`review`, `close-capture` and `close-publish` confirm the review, PR 1 must
retain exactly one request, the admitted automatic `opened` request its job
came from, and no other PR may retain any request. In `isolation`, PR 1 must
retain exactly its job's admitted `opened` request, PR 2 exactly its job's
admitted owner `comment` request, and no other PR any request. A further
request could otherwise still be admitted after the reviews are confirmed and
produce a duplicate review. After the provider close, PR 1 must retain
exactly one request: the admitted automatic `opened` request of its job. At
the end of `close-provider-reopen`, PR 1 must still retain only that request,
PR 2 exactly one unadmitted owner `comment` request, and no other PR any
request. A duplicate held back by the lease would otherwise become a second
job once the lease is released. Every case records the requests it judged as
`Requests` in the JSON result. The case reads each native launch's
`unit.json` and `native-audit.json` and checks provider auth disposition by
reopening the protected stores. A unit journal counts only
as version 1 for its own directory's launch. It must name
`sfl-alpha-<launch>.service`, and its binding must derive the containment ID
from its session and launch. The audit counts only as a version 1 receipt
with the same binding. Once auth was exported or the session completed, the
audit must carry provider evidence: the stopped receipt of that session under
the release's egress policy for that provider. The receipt must also pass the
production provider receipt validators under the session limits. Every
receipt must meet the stopped-receipt invariants that recovery requires before
recording a stopped attempt: consistent, nonnegative and in-budget request,
response and byte counters, and no exhaustion or denial without failure. The
receipt of a completed session must also pass the completed-receipt validation
an artifact requires: not failed or exhausted, at least one inference, every
forwarded request answered and no denial. The receipt's counters are recorded
as `Access` for each native unit. A copied, foreign, regenerated, failed or
impossible audit therefore proves nothing about the launch. In `isolation`,
every session from the first onward must carry one of the two job IDs, and
its PR and head must equal that job's snapshot.

The root is owned as soon as it is created (or, for an attached run, once its
layout is checked), before the unit observer starts. A setup failure after
that point still stops the observer, closes the fake servers after any open
service and removes a fresh root unless `SFL_E2E_KEEP_FAILED` is set. A
failed SQLite close or checkpoint of any store handle is a failed check. Each
run first removes an earlier `<case>.json` from `SFL_E2E_EVIDENCE`, and does
not start if it cannot. It writes the nonsecret JSON result only after cleanup
has closed every handle and removed (or deliberately retained) the root, so a
cleanup failure is recorded as `Passed: false`. The result goes to a synced
temporary in the same directory, which is renamed over `<case>.json`; a failed
write leaves no result rather than an earlier one.

## Admission rule

The controller admits one invocation at a time. `selectWork` returns nothing
while any durable job holds a lease, and `Poll` starts at most one background
worker, so a second pending request is captured and reviewed only after the
first job's invocation ends. Publication of the first job runs in a later
outbox cycle and can overlap the second job's work. The `isolation` case
therefore proves isolation between sequential jobs, not concurrent ones.

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

## Measured at `e144bc5`

On October 23, 2026, each case ran on mini under the shared VM lock, one VM at
a time, from source and native images, a source stub and a `reviewer.test` all
built from `e144bc5`. Both binaries carried `vcs.revision=e144bc5…` and
`vcs.modified=false`, and the release hash was `6364636bb5e9`. The first pass
of `close-publish` and `close-capture` was killed when the shared host ran
out of memory, with these guests running beside other workloads; both were
re-run at the same head under a check that at least 2.5 GB was available, and
passed. Every case passed with no
failures, every observed unit matched a recorded launch, every native receipt
passed the production provider-receipt validation, and no unit or E2E root
remained afterwards. Each PR held exactly its expected requests: PR 1 its
admitted automatic `opened` request in every case, and PR 2 an unadmitted owner
`comment` request after the `close-provider` restart and an admitted one in
`isolation`. Only this document changed after `e144bc5`.

| Case | Passed | Seconds | Checks | Source / native units |
| --- | --- | --- | --- | --- |
| `review` | yes | 29.6 | 32 | 1 / 4 |
| `close-publish` | yes | 28.6 | 36 | 1 / 4 |
| `close-provider` | yes | 13.6 | 22 | 1 / 1 |
| `close-provider-reopen` | yes | 38.6 | 23 | 0 / 0 |
| `close-capture` | yes | 28.6 | 38 | 1 / 4 |
| `isolation` | yes | 56.8 | 62 | 2 / 8 |

Earlier runs at `c29e563` and `3bbf701`, before the later checks, also passed
every case. `close-provider` took the branch where `Close` reports
`ErrWorkerStopPending`; the branch where it proves completion has not run on a
host. The sections below are pre-rebase history only: the commits they name
are not ancestors of this branch, and their checks predate the attestation and
binding rules above.

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
  explicit trusted supervisor could release the lease. The service composition
  never calls it, and the Codex auth fence would still need an explicit
  stopped-return or retirement path.

Without an operator recovery path, any service stop or crash between a
session's reservation and its stopped receipt would leave the reviewer
offline. Before activation, that path needs to be specified and qualified as a
separate gate: native stopped-attempt verification, a lease release that
records a charged session, auth return or retirement, and job disposition. The
alternative is a bounded grace on shutdown that lets the guest stop its client
and return auth. Both change security-relevant recovery behavior, so neither is
implemented here.

A shutdown before a session's work, while the host check runs, is
different. The native reviewer reports a trusted before-work failure, the
reserved session is refunded, and the job returns to `queued`.

## Limitations and remaining work

- Crash points use whole-service `Close` (cancellation), not `SIGKILL` at an
  arbitrary instruction. `close-provider-reopen` is a real process restart, but
  only after in-process cancellation has already stopped the unit. Mid-capture
  process loss is covered separately by the source VM `crash` case in
  [GIT-VM-SOURCE.md](GIT-VM-SOURCE.md), and native supervisor process loss by
  [NATIVE-VM-CRASH.md](NATIVE-VM-CRASH.md). A `SIGKILL` between session
  reservation and stopped receipt would leave the same durable fence as
  `close-provider`, without the in-process stop proof.
- No in-service recovery path exists for an uncertain provider lease (see
  Findings).
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
