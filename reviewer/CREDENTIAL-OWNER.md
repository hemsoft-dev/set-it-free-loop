# Scoped GitHub credential owner

This offline alpha component composes the explicit [App JWT signer](GITHUB-INSTALLATION.md), [installation attempt journal](INSTALLATION-JOURNAL.md) and scoped HTTP transports. It does not start a controller or authorize installation, credential import, activation, live inference or spending.

`OpenGitHubCredentialOwner` accepts an existing journal, a caller-owned JWT signing callback, two explicit trusted HTTP transports and a read-only local eligibility callback. Construction takes a separate private service-directory lock. It performs no signing, HTTP or automatic recovery. Linux and Darwin use the existing OS file-lock implementation. Copies of the opaque owner share the same ownership state.

Every new owner starts unqualified. Persisted qualified journal receipts cannot recreate a token or readiness. Only an explicit `Refresh(ctx)` can acquire credentials:

1. Close admission and wait for every already admitted request, response body and acquisition-generation lease to finish. The phase has a three-minute ceiling and also honors the caller's earlier cancellation or deadline.
2. Check current local eligibility and both durable role latches before signing or issuing any new reader request. Hold the publisher journal's process lock from this preflight through reader qualification, so a standalone publisher acquisition cannot introduce uncertainty during that interval. The later publisher acquisition takes its own lock and rechecks its latch. An unresolved publisher acquisition blocks a new reader acquisition.
3. Sign once and pass the same bounded JWT buffer through the reader and publisher handshakes. Clear the transferred buffer on success and failure.
4. Recheck eligibility, then validate both new receipts against one current time under the handover lock. Both must match their enrolled role and remain more than one minute from expiry, with no backward clock or canceled phase. Only then replace both role transports together. Clear the old pair only after its admitted work has drained.

Failure leaves admission closed, including when the old pair still exists in memory. There is no fallback, background renewal, retry of an uncertain POST or implicit journal recovery. A failed partial pair is closed. Unknown mint outcomes remain latched in the journal across process restart. Recovery remains the journal's separate explicit operation with exact attempt identity and checked stopped-owner evidence. This owner does not authorize recovery.

`Reader()` and `Publisher()` return stable opaque role handles. They follow a successful pair replacement. The existing transport still enforces the fixed repository, HTTPS destination, caller-auth and trace refusal, role permissions and nonreplayable write requirements. The reader gains no Git upload-pack POST capability in this change.

`Ready(ctx)` runs the explicit local eligibility check and checks both receipts against the enrolled identities and current time. A time before qualification or within one minute of expiry refuses admission. Readiness and request handling never sign, mint or refresh. The caller must compose this check with the controller's release, provider, isolation, billing and activation gates. A successful local check is not evidence of those external qualifications.

At most 64 HTTP operations can hold leases simultaneously. A lease covers the delegated call and response body until EOF, another terminal read error or Close. A caller must consume or close its body. Rejected requests close their supplied bodies. The trusted underlying transports own delegated request bodies and must honor context cancellation. They must not hide credential fallback, redirect following or POST retries.

`Close(ctx)` closes admission, cancels qualification and admitted HTTP contexts, and waits for qualification plus all leased bodies to return. A timeout keeps the owner lock and token ownership. The caller must retain the owner, finish stopping its work and retry Close explicitly. It must not start a replacement while the lock is retained. Successful Close clears the owned token buffers and releases the lock. A copied or stale handle cannot reopen admission. This is local disposal, not remote token revocation or guaranteed erasure of copies made by TLS, the runtime or trusted callbacks.

## Acquisition generation ownership

The private factory method `retainReaderGeneration` pins the current pair for
one acquisition, separately from individual HTTP bodies. It requires a trusted
stop capability before admission. That capability must revoke and join the
broker and prove the exact owned acquisition unit stopped. No model, serialized
receipt, caller boolean or token value can provide it. Construction starts no
process and performs no signing or acquisition. This is lifecycle plumbing;
the production factory still needs to supply and qualify the actual stop path.

The lease's private borrow returns only the pinned reader, a context tied to
the acquisition, and an exactly-once completion function. The typed Git exchange
must hold that borrow through response-body completion. At most 64 HTTP borrows
can coexist with the one acquisition lease. A borrowed reader cannot gain the
publisher role. Existing admitted acquisition work may finish while Refresh
waits for it; new ordinary admission stays closed. Expiry, backward time,
withdrawn eligibility, Close or a failed refresh drain cancels acquisition work.
Cancellation and HTTP EOF retain the whole-generation lease.

The factory explicitly calls the lease's stop operation. It cancels further
work, waits for all borrowed HTTP operations/bodies, and invokes the retained
stop capability once at a time under the caller's deadline and a 30-second
ceiling. Only a successful stopped result releases the generation. A failed,
uncertain or unreturned stop keeps the credential pair and service lock owned.
Concurrent stop callers cannot replay the in-progress proof. A later explicit
stop retry is allowed after that proof returns; there is no automatic credential
retry or fallback. The stop caller returns at its deadline even if the trusted
capability ignores cancellation. The one in-progress proof retains ownership
until it returns. Its late success cannot release an expired stop operation.
Close cancels the acquisition and waits for this ownership to be released. It
cannot turn cancellation into physical-stop proof.

The proof callback never releases a generation. Its caller accepts a returned
successful proof under the owner lock and commits the release itself. Once that
acceptance succeeds, the call returns success even if cancellation arrives
during the commit. A caller that selects its deadline or rejects a late result
retains the generation. The proof remains single-flight until both the callback
and its caller's acceptance decision have completed.

These leases are local ownership, not a durable unit journal. After a controller
restart, the composing service must recover every unknown acquisition from its
durable unit/archive records before its eligibility check can permit Refresh.
The old lease, process death or lock availability cannot qualify that recovery.
Complete factory/startup recovery and broker/unit composition remain in #176.
No production stop capability or credential-bearing service is activated here.

Close timeouts before disposal retain ownership. Once all owned work has drained
and lock-descriptor closure has been attempted, the owner is terminal even if
that close reports an error. The initial Close reports the error, clears the
disposed handle and credentials, and later Close is idempotent. An already
released or uncertain descriptor is never represented as a retained flock.

The signer, local checks, transports and clock are explicitly trusted bounded capabilities. `crypto.Signer` itself has no cancellation API. The owner cannot interrupt a callback that ignores its context, so it retains ownership when Close times out. Pre-admission readiness checks do not borrow token state and are not qualification leases. An uncooperative response body remains counted until its caller releases it. The private directory and file lock assume the host service identity is trusted. This component does not establish a hostile same-UID boundary or qualify worker containment.

## Validation

The tests use real local TLS handshakes, synthetic installation tokens and a generated RSA signing key. The actual signer qualifies both roles, and the actual publication producer posts an acknowledgment through the stable publisher handle. Other cases cover atomic replacement, old-buffer disposal, failed-pair admission, persisted unknown outcomes across reopening, no signing or HTTP before drain, EOF and Close retirement, the 64-operation limit, denied body closure, expiry and backward time, cancellation during qualification and HTTP, unreturned callbacks, timeout lock retention and explicit stop retries. A separate test process proves the owner lock blocks another process and releases after a successful stop.

Generation tests add actual inert child processes and original pidfds. A live
child makes the stop capability fail after its HTTP body has completed; refresh
and Close then retain the pair and lock without signing or minting. Only actual
termination permits release. Other cases cover pinned requests during refresh
drain, cancellation of an HTTP call that has not returned, concurrent stop calls,
an unreturned stop capability, expiry during the eligibility check, backward
time and revoked eligibility. These tests qualify ownership behavior with a
test stop capability, not a complete protected cgroup or broker shutdown path.

Typed-nil transport and backward-clock regressions failed before their checks were added. These tests establish local fake-transport behavior. They do not establish native worker shutdown, host-crash recovery, production key custody, provider support, App eligibility or a comparative review result. Broader controller composition and those qualifications remain open in #176 and its related issues.
