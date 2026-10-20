# Durable containment launch gate

`ContainmentGate` is the Linux supervisor's first concrete launch component.
It starts no process. Its current fixed stub accepts only `FakeGateManifest`,
a bounded synthetic marker, and writes the marker's digest after grant. There
is no command, executable, credential, profile, mount or destination field.

Prepare the gate before asking a manager to start an inert stub. Preparation
creates a private launch directory named by `AuthBinding.LaunchID`, fsyncs its
parent and writes a private versioned journal using atomic replacement plus
file/directory fsync. The binding includes the session and containment hashes.
An existing launch directory is never reused, including after preparation fails.

A manager must obtain the exact stub identity from its own observations.
`GrantFake` compares that identity with `SO_PEERCRED`, a live `SO_PEERPIDFD`,
process start ticks and the host boot ID. The hello must also match the complete
binding, manager invocation ID and captured manifest digest. Frames reject
unknown, duplicate and missing fields and have a 4 KiB limit and short deadline.
The stub independently verifies its supervisor's kernel PID and UID, and accepts
only a grant for its own exact process identity and expected manifest digest.

The journal changes from `prepared` to `granted` before the manifest is written.
That persisted grant consumes the launch even if delivery fails. The grant's
cross-process flock remains held through its bounded socket write. No background
writer can send work later. A second grant cannot replay the manifest.

`Abort` takes the same flock and fsyncs `aborted` before closing the listener and
all accepted connections held by that supervisor. Registration under the local
mutex closes a connection that raced with abort. Socket names and journal
tombstones remain in place to prevent a delayed stub finding a new endpoint.
Cleanup/disposal must preserve nonce uniqueness for all retained auth bindings.

Recovery calls `OpenAbortedContainmentGate`. It reopens and checks the exact
private journal, records permanent revocation and creates no listener. A live
old supervisor that still owns its original socket must recheck that journal
under the same flock and cannot grant after recovery. Revocation can precede
physical shutdown; it does not prove an old socket or process has disappeared.

## Evidence and remaining integration

Tests use real Unix sockets, kernel peer credentials/pidfds and a real child
process running the same inert stub entry point. They cover grant delivery,
spent nonces, connected and delayed peers, old-supervisor recovery, changed
PID/start ticks/boot identity, changed invocation/manifest, duplicate or oversized
frames, binding mismatch and journal symlinks. Ordinary tests start no systemd
unit, VM, official client, provider session or network backend.

This component does not implement `AuthStopVerifier`. Its successful abort is
an authorization-revocation receipt, never evidence to release an auth lease.
The next manager adapter must bind the supplied invocation ID to the actual
unit and cgroup, settle every asynchronous start and helper, prove the exact
cgroup subtree empty, and prove profile mounts detached. A missing unit or a
successful stop command alone cannot establish that proof. Unknown launches
remain recovery work, and recovery never grants their old launch IDs again.

The fixed QEMU payload, resource enforcement, native/source-reader role wiring,
protected auth transfer and refresh/disposal, private provider bridge binding
and whole-containment recovery still require implementation and qualification.
The fake-only gate does not enable reviewer activation or change any account,
capability or production qualification flag.
