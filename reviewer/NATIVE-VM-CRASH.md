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
original root identity, seed hash and actual fake-backend counts.

Only after validating that record and the live captured native unit does the
parent send SIGKILL to its own child and join its real signal exit. The child
cannot run review cleanup. The parent reopens the original auth and unit journals
without constructing another reviewer or replaying a request. Fresh physical
stop checks and recorded runtime ownership govern shutdown and runtime cleanup.

The exported auth profile must remain fenced. Even after physical stop,
`Complete` must refuse a lost return, and canonical state must still match the
original synthetic seed hash. Reopening the auth store must preserve that fence.
The fixture then explicitly retires only its synthetic state through the actual
stopped verifier, and proves the old seed is gone. It does not seed a replacement.

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

Repeat with `--provider claude` and another new output directory. The fixed local
transport never dials a public provider. Existing roots, malformed/private-point
substitutions and mixed or unsupported providers refuse. Ordinary tests exercise
framing, source/session/policy/root binding, no existing-root adoption and actual
local TLS without opening a manager, KVM or native client.

Retain the source/image/build receipt, crash point, original private unit and
auth journals, physical-stop/runtime-cleanup evidence and final result. Failure
retains the state directory. A timeout is not proof of shutdown or permission to
restart, discard unknown state or reseed auth.

This is one controlled supervisor SIGKILL after refresh and before a response.
It does not qualify host power loss/reboot, arbitrary interruption boundaries,
resource pressure, real-account repair, aggregate protected Git acquisition,
controller budget accounting, actual benchmark quality or a live pilot. The
[existing Git launch failures](GIT-LAUNCH-CONSTRAINTS.md) and all separate
provider/account/owner gates remain open. No model inference occurs.
