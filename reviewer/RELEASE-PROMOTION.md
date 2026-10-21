# Explicit trusted-release decisions

`ReleasePromotionRegistry` records a separate operator decision to bootstrap,
promote or roll back a pinned reviewer package. A merged PR never invokes it.
The registry contains no credential, GitHub writer, runtime, deployment hook or
model capability. None of its operations grants live activation.

This implements the local decision and recovery mechanism required by
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176) and
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177). Actual owner
approval, human-review bootstrap and host/provider qualification remain evidence
the operator must supply before use.

## Separate creation and opening

Create a preexisting private, canonical directory dedicated to this registry,
separate from credentials, unit records and controller storage. An explicit
`CreateReleasePromotionRegistry` writes only an empty `promotion.json`. Empty
state qualifies no package. It refuses an existing record or foreign directory
contents. `OpenReleasePromotionRegistry` requires an existing valid record; it
never reconstructs missing history as an empty registry.

The registry binds the exact repository, installation, App, bot and owner
identity through `Enrollment`. Every operation checks the original directory
identity before creating a lock and again after acquiring it, before cleaning
reserved temporary files or reading the record. This repeats the ownership,
private permissions, named directory identity and bounded inventory checks after
any lock wait. It uses private cross-process locks, canonical bounded JSON and atomic fsync/rename
replacement. A symlink, replaced directory, foreign enrollment, malformed or
noncanonical record refuses. Unknown directory entries are preserved and cause
refusal. Interrupted reserved replacement files can be discarded under the
original lock; only the committed record is authoritative.

## Explicit decisions

`Snapshot` returns the history and its hash. A `ReleasePromotionRequest` names:

- A unique 16-byte hex `OperationID` and exact `ExpectedStateHash`.
- `bootstrap`, `promote` or `rollback`, the previous revision, selected revision
  and exact canonical `GitReviewerRelease` package hash.
- The owner ID, decision time, owner decision evidence and a hash of the prior
  service's stopped evidence.
- For a new approval, a manual-review URL and PR, reviewed revision, owner ID,
  build/validation artifact hashes, owner approval and review/approval times.

`Apply` also receives the actual package and a mandatory trusted authorizer.
Before calling it, the registry checks package structure, exact tuple, common
Codex/Claude availability, provenance shape, state hash, ordering and bounds.
The authorizer must independently verify the owner decision, human/manual
review, exact build and validation artifacts, and completed shutdown of the
previous service. These supplied fields are not self-authenticating evidence.
For bootstrap, it must establish that no prior service is active and that the
manual review occurred before this alpha could review itself.

The authorizer is a read-only, cancellation-aware local capability. Its call
holds the startup and metadata locks and has a 30-second context ceiling. It must not reenter
the registry. Denial or cancellation writes no decision. An unreturned callback
continues to hold ownership; a context deadline does not establish its return.
There is no default authorizer, approval from model text, merge-event listener
or automatic deployment route.

A revision has one immutable package hash and approval. A changed package needs
a new revision and independent approval. At most eight approved revisions and
64 decisions are retained. Reaching a bound refuses further changes; history
is never silently pruned. No migration or rollover is implemented.

## Promotion, rollback and retained jobs

Promotion approves a new package and selects it for the next explicit startup.
Older approved packages remain eligible for jobs already bound to their exact
release. They do not inherit the new package's approval or assets.

Rollback selects a previously approved, nonrevoked package. It also revokes the
replaced release, including for retained jobs. Those jobs must fail their
original release check; they cannot run under the rollback target. Revoked
approvals and their decisions stay in the record and cannot be revived by a
later rollback or by replacing their package bytes.

Stop and close the previous service before applying a change. Read the committed
selection, supply its exact package and provider hosts to
`OpenPromotedGitReviewerService`, and retain the independent activation,
eligibility and quota checks in the supplied options. The wrapper verifies the
committed current revision and every configured release's approval, revocation
status and package hash before opening a manager. An invalid unused catalog
entry also refuses construction. It binds all
later checks to the captured decision epoch, checking that epoch before and
after the independent local gate. Any later registry change fences this service
until the operator performs a fresh startup. `registry.Check` also matches
`GitReviewerServiceOptions.Check`; it verifies enrollment, revision and package
hash on every call, including older retained releases, but alone does not bind
a service to the selected current epoch. Neither check replaces the independent
gates. Exclude revoked packages from a new startup's catalog. A stale catalog
containing one refuses construction rather than quietly dropping it.

The wrapper holds `startup.lock` through the entire service constructor,
including partial construction and cleanup. `Apply` takes that same lock before
`promotion.lock`, so a cooperating decision cannot commit during construction.
Snapshots and epoch checks take only `promotion.lock`; the independent check
must remain read-only and must not call `Apply` or recursively start a service.
If a constructor returns an owned cleanup handle with an error, retain and close
that handle before authorizing a new decision. Releasing the construction lock
does not prove that a returned service has stopped; the trusted authorizer still
must verify completed shutdown. Later decisions continue to fence the original
service through its captured epoch.

Registry selection never rewrites an admitted job's release. Changing a record
does not start, refresh or stop a service. Candidate source, prompts, auth and
runtime files remain inert until separately built, reviewed and explicitly
approved by the trusted operator. Evidence that a running artifact predates a
candidate PR must still come from the trusted build/review provenance and live
qualification; the registry does not infer Git ancestry or PR creation times.

## Uncertain writes and recovery

A completed operation can be retried with the exact original request and
package. Its `OperationID` and full request must match the retained decision;
the registry returns the current state without invoking the authorizer again
or appending a duplicate. A reused ID with different data refuses. Competing
operators must obtain a fresh snapshot after a state-hash conflict.

A replacement failure returns `ErrReleasePromotionUnknown`. Preserve the
original operation and inspect `Snapshot`. An atomic rename may already have
committed before a later fsync error, so do not invent another operation ID to
make a retry succeed. Reusing the exact request either reconciles a committed
decision or retries against its original expected state. Lost or corrupt
history remains a recovery problem, never permission to bootstrap over it.

## Model-free validation

Tests use synthetic approvals, packages and stopped evidence. They exercise
bootstrap, promotion, rollback, persistent revocation, reopen, stale compare and
swap, idempotency, cancellation, directory replacement, foreign ownership and
malformed records. A real permission failure after authorization verifies that
atomic replacement failure preserves the committed record and original retry
identity. An unencodable retry mutation is rejected before hashing or writing.

A startup integration test uses the existing fake manager/kernel and inert
measured Git package. Unapproved startup is refused before signing. Approved
construction still cannot `Run` without explicit credential qualification, and
an undrained service cannot pass the test authorizer. Additional cases refuse a
stale current selection, fence a previously constructed service after a changed
decision, and detect a noncooperating record change inside the independent
eligibility gate. Lock-wait regressions replace or make the directory public or
add a foreign entry while an operation waits, then verify refusal without
deleting a reserved fragment. Startup tests refuse both unapproved and revoked
unused catalog entries before opening a manager. A blocked constructor proves
that a concurrent promotion cannot commit before construction returns.
No model is invoked.
These tests do not supply actual release approval or qualify the host's known
remaining physical containment and recovery gates.
