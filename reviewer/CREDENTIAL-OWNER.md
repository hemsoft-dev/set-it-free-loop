# Scoped GitHub credential owner

This offline alpha component composes the explicit [App JWT signer](GITHUB-INSTALLATION.md), [installation attempt journal](INSTALLATION-JOURNAL.md) and scoped HTTP transports. It does not start a controller or authorize installation, credential import, activation, live inference or spending.

`OpenGitHubCredentialOwner` accepts an existing journal, a caller-owned JWT signing callback, two explicit trusted HTTP transports and a read-only local eligibility callback. Construction takes a separate private service-directory lock. It performs no signing, HTTP or automatic recovery. Linux and Darwin use the existing OS file-lock implementation. Copies of the opaque owner share the same ownership state.

Every new owner starts unqualified. Persisted qualified journal receipts cannot recreate a token or readiness. Only an explicit `Refresh(ctx)` can acquire credentials:

1. Close admission and wait for every already admitted request and response body to finish. The phase has a three-minute ceiling and also honors the caller's earlier cancellation or deadline.
2. Check current local eligibility and both durable role latches before signing or issuing any new reader request. An unresolved publisher acquisition also blocks a new reader acquisition.
3. Sign once and pass the same bounded JWT buffer through the reader and publisher handshakes. Clear the transferred buffer on success and failure.
4. Recheck eligibility, then replace both role transports together. Clear the old pair only after its admitted work has drained.

Failure leaves admission closed, including when the old pair still exists in memory. There is no fallback, background renewal, retry of an uncertain POST or implicit journal recovery. A failed partial pair is closed. Unknown mint outcomes remain latched in the journal across process restart. Recovery remains the journal's separate explicit operation with exact attempt identity and checked stopped-owner evidence. This owner does not authorize recovery.

`Reader()` and `Publisher()` return stable opaque role handles. They follow a successful pair replacement. The existing transport still enforces the fixed repository, HTTPS destination, caller-auth and trace refusal, role permissions and nonreplayable write requirements. The reader gains no Git upload-pack POST capability in this change.

`Ready(ctx)` runs the explicit local eligibility check and checks both receipts against the enrolled identities and current time. A time before qualification or within one minute of expiry refuses admission. Readiness and request handling never sign, mint or refresh. The caller must compose this check with the controller's release, provider, isolation, billing and activation gates. A successful local check is not evidence of those external qualifications.

At most 64 HTTP operations can hold leases simultaneously. A lease covers the delegated call and response body until EOF, another terminal read error or Close. A caller must consume or close its body. Rejected requests close their supplied bodies. The trusted underlying transports own delegated request bodies and must honor context cancellation. They must not hide credential fallback, redirect following or POST retries.

`Close(ctx)` closes admission, cancels qualification and admitted HTTP contexts, and waits for qualification plus all leased bodies to return. A timeout keeps the owner lock and token ownership. The caller must retain the owner, finish stopping its work and retry Close explicitly. It must not start a replacement while the lock is retained. Successful Close clears the owned token buffers and releases the lock. A copied or stale handle cannot reopen admission. This is local disposal, not remote token revocation or guaranteed erasure of copies made by TLS, the runtime or trusted callbacks.

The signer, local checks, transports and clock are explicitly trusted bounded capabilities. `crypto.Signer` itself has no cancellation API. The owner cannot interrupt a callback that ignores its context, so it retains ownership when Close times out. Pre-admission readiness checks do not borrow token state and are not qualification leases. An uncooperative response body remains counted until its caller releases it. The private directory and file lock assume the host service identity is trusted. This component does not establish a hostile same-UID boundary or qualify worker containment.

## Validation

The tests use real local TLS handshakes, synthetic installation tokens and a generated RSA signing key. The actual signer qualifies both roles, and the actual publication producer posts an acknowledgment through the stable publisher handle. Other cases cover atomic replacement, old-buffer disposal, failed-pair admission, persisted unknown outcomes across reopening, no signing or HTTP before drain, EOF and Close retirement, the 64-operation limit, denied body closure, expiry and backward time, cancellation during qualification and HTTP, unreturned callbacks, timeout lock retention and explicit stop retries. A separate test process proves the owner lock blocks another process and releases after a successful stop.

Typed-nil transport and backward-clock regressions failed before their checks were added. These tests establish local fake-transport behavior. They do not establish native worker shutdown, host-crash recovery, production key custody, provider support, App eligibility or a comparative review result. Broader controller composition and those qualifications remain open in #176 and its related issues.
