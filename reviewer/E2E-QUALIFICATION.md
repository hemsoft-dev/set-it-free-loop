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
projections of the source VM owner journal and archive, and a restore probe.

1. **Startup.** The public `OpenGitReviewerService` is called with a version 2
   `GitReviewerRelease`: the measured source image and exact-head
   `gitsourcevmstub`, plus both native providers pinned to a measured native
   image. The public constructor uses the real user-systemd source VM owner
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

| Case | Scenario |
| --- | --- |
| `review` | PR 1 automatic request through capture, four sessions and one confirmed COMMENT. |
| `isolation` | PR 1 (automatic) and PR 2 (owner comment) pending together. |
| `close-capture` | Close the whole service after capture and admission, inside the first session's host check, before any VM launch. Reopen and finish. |
| `close-provider` | Close the whole service while the first Codex session waits for a withheld inference response. |
| `close-provider-reopen` | A second process attached to the root retained by `close-provider` (`SFL_E2E_ATTACH`), as a restarted service. |
| `close-publish` | The fake GitHub commits the review POST but withholds its response; close the whole service; reopen and reconcile. |

Every case also observes `systemctl --user list-units 'sfl-alpha-*'` every 200
ms, excluding units that existed before the run, and requires that every unit
it saw is gone at the end. It reads each native launch's `unit.json` and
`native-audit.json`, checks provider auth disposition by reopening the
protected stores, and writes a nonsecret JSON result to `SFL_E2E_EVIDENCE`.

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
the stub and test binary from the same head. Run each case under the shared
VM lock, with a bounded timeout:

```sh
go -C reviewer build -o "$OUT/gitsourcevmstub" ./cmd/gitsourcevmstub
chmod 0500 "$OUT/gitsourcevmstub"
go -C reviewer test -c -o "$OUT/reviewer.test" .
flock ~/.cache/sfl-vm.lock env GOMAXPROCS=2 \
  SFL_E2E_SOURCE_IMAGE="$SOURCE_IMAGE" SFL_E2E_SOURCE_STUB="$OUT/gitsourcevmstub" \
  SFL_E2E_NATIVE_IMAGE="$NATIVE_IMAGE" SFL_E2E_CASE=review SFL_E2E_EVIDENCE="$EVIDENCE" \
  timeout 600 nice -n 10 "$OUT/reviewer.test" -test.run '^TestE2EQualification$' -test.v -test.timeout 590s
```

For `close-provider`, run `close-provider-reopen` next in a new process with
`SFL_E2E_ATTACH` set to the `Root` recorded in `close-provider.json`, inside
the same lock hold. State lives under a fresh `/tmp/sfl-e2e-<nonce>` root,
which a passing run removes. `SFL_E2E_KEEP_FAILED=1` retains a failed root.

## Measured results

Measured on mini on 2026-10-22, one case at a time under the shared VM lock,
with `GOMAXPROCS=2` and `nice -n 10`. Code, test binary, stub and both images
come from commit `3140f0a`; that commit is also the release revision, and the
package digest is `12d1adf7…88d3`. Only this document changed afterwards.
Every case passed with no failed check and no fake GitHub violation. The
observer saw no remaining unit from any run.

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

### Rerun with tightened checks

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
