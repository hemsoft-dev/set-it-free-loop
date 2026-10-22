# Model-free native supervisor process loss

`cmd/nativevmcrashfixture` tests a controlled loss of the host process owning one
native review. It creates synthetic expired auth, uses the pinned official CLI
inside the measured native VM and permits only a local canned TLS backend.
It imports no account/profile and supplies no App, paid-usage or activation path.

The parent launches its own child executable. That child exclusively creates a
new private root, so an existing root or profile cannot be adopted. It opens the
normal native reviewer, source reader and protected auth store. After the actual
canned producer validates the first inference request using refreshed synthetic
auth, it withholds the successful SSE response. No response bytes reach the
client. A bounded private pipe record binds the source/image, session/policy,
original root identity, seed hash and actual fake-backend counts. The producer
closes that pipe after its one canonical newline-terminated record; trailing
bytes, additional records or a missing EOF refuse qualification.

Only after validating that record and the live captured native unit does the
parent send SIGKILL to its own child and join its real signal exit. The child
cannot run review cleanup. The parent reopens the original auth and unit journals
without constructing another reviewer or replaying a request. Fresh physical
stop checks and recorded runtime ownership govern shutdown and runtime cleanup.

The exported auth profile must remain fenced. Even after physical stop,
`Complete` must refuse a lost return, and canonical state must still match the
original synthetic seed hash. Reopening the auth store must preserve that fence.
Before retirement, the fixture saves a bounded nonsecret copy of the original
exported auth journal, bound to the provider, launch and seed hash. Its SHA-256
enters the final result. A failed snapshot leaves the original recovery state
intact. The fixture then explicitly retires only its synthetic state through
the actual stopped verifier and proves the old seed is gone. It does not seed
a replacement. Supervisor closure must succeed before a pass is written.

The fresh root also records the exact driver source/tree, configuration digest binding a
random fixture-instance nonce, and original directory identity before any auth
store or launch is created. A separate `--recover-owned` command must verify
that record before opening the original unit. It cannot start a review, complete
a return, retire a credential or seed auth. Missing/substituted state remains
unknown; a vanished root is not shutdown evidence.

## Run the opt-in fixture

Use clean committed source and an explicitly measured native image. The receipt
records the driver source and image source separately, together with all existing
asset pins. Each provider uses a new private state directory.

```text
python3 reviewer/scripts/native-vm-crash-fake.py --run-fake-only \
  --provider codex --image /absolute/measured-native-image \
  --output /absolute/new-private-receipt-directory \
  --go /absolute/pinned-go --module-cache /absolute/offline-module-cache
```

Repeat with `--provider claude` and another new output directory. Add
`--force-driver-timeout` for a separate fresh run to kill both host driver
processes after the validated crash point. The Python supervisor owns a new
process group, kills and observes its members, then runs the same pinned binary
in separately bounded recovery mode to stop the original systemd unit and
remove its recorded runtime. Go also binds the child to parent death. This
forced case must preserve the exported auth recovery fence and unchanged seed;
it intentionally leaves those synthetic auth journals for inspection.

An ordinary timeout always fails qualification, even if cleanup succeeds. Only
the explicitly selected forced-timeout case can pass after observing its exact
fault point and independently bound cleanup. Unproven group or unit cleanup
is recorded as unknown and retains the state directory. The fixed local
transport never dials a public provider. Existing roots, malformed/private-point
substitutions and mixed or unsupported providers refuse. Ordinary tests exercise
framing, source/session/policy/root binding, no existing-root adoption and actual
local TLS without opening a manager, KVM or native client.

Retain the source/image/build receipt, crash point, original private unit
journal, retained auth metadata copy/hash for retired cases, original fenced
auth journal for timeout cases, physical-stop/runtime-cleanup evidence and final
result. Failure
retains the state directory. A timeout is not proof of shutdown or permission to
restart, discard unknown state or reseed auth.

These cases cover one controlled interruption point after refresh and before
a response, with either the child supervisor or both host driver processes lost.
It does not qualify host power loss/reboot, arbitrary interruption boundaries,
resource pressure, real-account repair, aggregate protected Git acquisition,
controller budget accounting, actual benchmark quality or a live pilot. The
[existing Git launch failures](GIT-LAUNCH-CONSTRAINTS.md) and all separate
provider/account/owner gates remain open. No model inference occurs.
