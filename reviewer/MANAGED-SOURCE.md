# Managed reader execution

`ManagedSourceReviewer` implements the existing `Reviewer` and `SourceReviewer`
interfaces. Configure it with an `Adapter` whose concrete `NativeDriver` has the
trusted provider launcher, a separate `SourceReaderLauncher`, and the expected
worker UID/shared IPC GID. `Adapter.Reader` remains an independent citation
verifier. Its trusted path allowlist supplies the reader manifest. The worker
receives no in-process source capability in this path.

Every phase creates a fresh random reader launch ID and captures the complete
Session, allowed paths, endpoint root and identity requirements. The provider's
normal preflight checks run before starting the reader. A reader process must
arrive with private stdin/audit pipes, and its launcher must persist the
manifest/process recovery binding before launch. A non-nil returned process
always requires shutdown, including a launch error. A nil failure attests that
no reader started.

The supervisor sends one bounded canonical manifest and closes its pipe. Before
starting the provider it requires the exact newline-terminated reader ready
event, including launch/session/manifest hashes, socket and reader UID. The
native driver still performs the pinned client identity and Claude control
checks. Model/native output cannot supply private reader events.

After the native exchange, both role stop requests run even if one role cannot
attest termination. `SourceReaderProcess.Stop` must permit the normal final audit
to drain while the provider and proxy descendants stop. It must reap the whole
reader containment, honor its context and preserve the parent's audit pipe for
EOF consumption. `Wait` has one owner; implementations must coordinate it with
concurrent `Stop`. The caller closes the private input exactly once and closes
the audit after its bounded collection or cancellation.

The reader's complete private audit must pass `DecodeSourceReaderAudit`, and a
successful receipt must agree with the actual reader exit status. No missing,
truncated, mismatched or failed receipt becomes a measured zero-read success.
Both roles must have stopped before an accepted report. Physical uncertainty
returns `ErrWorkerStopPending`; missing/invalid final audit returns
`ErrSourceAuditPending`. The existing durable runner retains the attempt charge
and lease for both. A valid failed-reader receipt remains available alongside
the failure. Report/citation validation failure also retains valid worker access
evidence. Authoritative reader omissions are merged into accepted reports under
the captured schema/output limits.

`DurableReviewer` already records `SourceReviewer` evidence against the reserved
attempt ID before checkpointing or releasing a stopped lease. Supplying a
`ManagedSourceReviewer` through `Engine.Reviewers` therefore stores external
worker reads separately from citation validation. No new model field, global
receipt cache or fallback receipt is involved. The existing adapter's in-process
broker remains useful for fake transports; it is not the separate reader path.

The fixtures use fake reader roles with actual private pipes and source broker
calls, plus separate fake native-provider executables. A four-session durable
run records all actual source accesses and completes one fake GitHub publication.
Negative cases cover stale/truncated audits, reader exit inconsistency, failed
readiness, client launch failure, cancellation and uncertainty in either role.
They do not provision identities or qualify an OS containment mechanism.

No production OS launcher or provider credentials are selected by this module.
Qualified reader/client launchers, persisted OS recovery records, protected auth
leases, disposable profiles, memory/egress/request limits and exact-client
fake-backend qualification remain implementation or qualification work in #174.
Actual account eligibility, App installation, live inference and activation are
separate owner/provider gates. Merging this code activates none of them.
