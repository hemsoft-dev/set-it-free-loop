# Trusted reviewer startup

`OpenGitReviewerService` is the explicit Linux operator API for the new alpha.
It consumes pinned release packages, already opened controller/installation
metadata, explicit provider/GitHub capabilities and local eligibility checks.
It does not install or start a daemon, select an account, import credentials,
refresh, run inference, promote a release or deploy anything.

## Package and host inputs

`GitReviewerRelease` binds one source revision to the Git artifact, Git/HTTP
program pins, worker and namespace helper, plus each provider's pinned identity,
VM assets, runtime/bootstrap libraries and egress policy. Use
`EncodeGitReviewerRelease` to produce canonical JSON and its SHA-256 digest.
Decoding checks bounded JSON, duplicate/unknown fields, structural pins and
the supported identities, then requires exact equality with the canonical bytes.
Whitespace, reordered fields or other alternative encodings refuse; the returned
digest always covers the supplied bytes. Re-encoding is stable. The Git artifact is produced
by `BuildGitRuntime`; startup measures and seals its actual bytes before
preparing the read-only runtime.

Package paths are trusted host configuration and participate in the digest.
Packages contain no auth store, transport, signing callback or provider account
hints. Those capabilities are supplied separately through
`GitReviewerServiceOptions` and `GitReviewerProviderHost`. Their diagnostic
representations are redacted.

Supply one to eight explicit releases, a current revision, and exactly matching
provider-host entries. The current release must contain both Codex and Claude
to serve the default adversarial review and every per-PR mode. An older retained
release may contain one provider for its original single-provider jobs.
The package identity fixes the model, client and auth
mode. Each native session must match that identity and pass the host's local
check. Its stub byte count and hash are measured before auth; the native VM
path still verifies its other assets and containment before exporting auth.
Neither a package digest nor successful decoding proves model access or
containment.

The release check receives the enrolled repository/installation identity,
revision and canonical package digest. It must establish current approval of
that exact tuple from trusted evidence. It is a read-only, cancellation-aware
callback. Do not approve multiple different packages under an immutable job's
revision. Older retained jobs select their original explicit catalog entry,
with the old entry's own digest checked again. Missing or revoked entries fail
without a fallback. Creating the descriptor does not approve it for use.

## Startup order

1. Validate and copy package descriptors and host capabilities. A denied local
   release or credential-eligibility check opens no directories or manager.
2. Validate four preexisting private, canonical, disjoint directories for unit
   records, exports, runtime preparation and source work. Nested roots, aliases
   and symlinked paths refuse. No default path is inferred.
3. Hold a separate startup lock under the installation metadata directory and
   preserve the enrollment-bound trigger policy.
4. Seal the Git artifact, retain the measured helper, prepare a private runtime,
   open the export archive and fixed user-systemd unit owner, then compose the
   [internal service](GIT-SERVICE.md). The public API uses the concrete fixed
   user-manager transport. A private opener seam exists only for fake tests.
5. Finish source recovery before the internal service can qualify credentials
   or poll. Construction never calls `Refresh` or `Run`.

The initial `Triggers.EnabledAt` is persisted as a UTC instant, together with
the exact enrollment. It survives restarts and release changes. A different
epoch, different enrollment, malformed record or insecure file refuses instead
of rewriting the policy. The record may already exist after a failed startup.
Changing it needs a separately specified migration, not a retry that resets the
epoch. Persistence of this record supplies no activation authority.
An epoch outside JSON's supported year range refuses before a policy file is
installed. Atomic writes use the metadata lock's existing reserved replacement
filenames. After a crash before rename, the next lock acquisition validates and
removes the abandoned temporary without changing the committed policy or
unrelated entries.

## Work, close and failed construction

Only after all independent owner and evidence gates pass may an operator
explicitly call `Refresh` and then `Run`. Neither call is made by this package's
constructor. `Recover` delegates to the retained internal service and never
refreshes. A failure before that service exists can only be closed and retried
as a fresh explicit startup; it does not silently reconstruct missing resources.

Any nonnil handle returned with an error must be retained and closed. Failed
construction attempts a bounded five-second disposal, keeping the handle and
remaining owners if disposal cannot prove completion. Unknown old acquisitions
retain the unit, runtime, credential and startup locks. A subsequent explicit
recovery may complete their stop evidence; it cannot reactivate a closing
service.

Close first cancels and joins the internal service, then disposes any still
owned startup resources. Runtime removal checks the opened container's original
identity. The outer startup lock is released last. A failed earlier step retains
later owners. Close has a 30-second ceiling or the caller's earlier deadline.
An unreturned callback keeps ownership; a timeout is not a stop receipt.
Git runtime disposal also retains its original directory descriptors and open
state after an identity, chmod or removal failure. Repairing the original path
allows an explicit retry on the same owner. The code never treats a failed close
as completion, replaces the runtime owner or skips a cached cleanup error.

Each startup uses its own random private runtime container. Successful close
removes only that container. Unknown entries from an interrupted prior startup
are preserved, never recursively deleted by name. At 128 retained entries the
runtime parent refuses new preparation before opening a unit or signing.
Operators must retain and investigate such artifacts; automatic crash-artifact
collection is not implemented. This bound and retained artifacts belong in the
full host disk/resource qualification.

## Qualification limits

Positive startup tests use fake manager/kernel evidence and local TLS with
synthetic credentials. A separate positive case consumes an actual
`BuildGitRuntime` artifact from the installed trusted Git producer. They prove
packaging and ownership behavior, with no model inference. Public denied-gate
tests return before opening a real manager. They do not qualify whole-unit
physical containment or a live subscription.

Trusted promotion/rollback decisions, provider policy/entitlement and quota
evidence, complete containment/resource/recovery qualification, native control
and evaluation work, final GitHub rendering evidence and explicit owner
activation remain tracked in
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176) and
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177).
The existing failed physical qualifications remain failed. No activation,
credential or App exception follows from this API or its tests.
