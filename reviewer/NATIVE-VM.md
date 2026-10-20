# Native VM implementation in progress

This branch is implementing the native review path for
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The current host boundary is not a complete reviewer or a qualified VM release.
It has no activation command, guest implementation, provider credential import,
live inference, App installation or deployment.

`NativeVMManifest` uses purpose `native-review` and containment protocol 3.
Synthetic fixture purpose and protocol 2 stay separate. The manifest binds the
captured session, provider, full auth launch identity and sealed QEMU/kernel/initrd
pins. It accepts no executable path, command, environment or host filesystem
selector. Seven one-use descriptors carry those three immutable assets, KVM and
three distinct connected Unix streams in fixed order.

The private streams are control/auth/result, immutable source objects and
provider HTTP. Channel socket inodes must differ. Guest descriptor export is
one-use, closes the retained original ends and preserves EOF when QEMU closes.
SCM_RIGHTS receive sets CLOEXEC atomically, bounds data/ancillary descriptors,
closes rejected descriptors, and shares the tested framing reader with the
unchanged four-descriptor synthetic fixture.

The new `cmd/nativevmstub` is inert outside its owned systemd unit and before its
authenticated supervisor grant. It verifies the exact sealed assets, KVM API
and private stream types before executing retained QEMU. QEMU has no NIC, host
share, monitor, interactive serial output or TCG fallback. Three fixed virtio
serial ports connect to the private streams. The guest init is fixed at
`/native-init` and the kernel command line binds the launch nonce.

The native unit has a proposed fixed 3 GiB host memory cap, no swap, 128 tasks,
one CPU quota and seven-minute outer runtime cap, with a 2 GiB guest. These bounds
have not yet been measured with both native clients. The synthetic unit retains
its original 1 GiB/64-task/two-minute profile. `NativeVMSupervisor` reuses durable
unit identity, start settlement, revoked grants, whole-cgroup stop and fresh
recovery. Its stop proof applies only to the distinct native payload.

Ordinary tests use inert sealed assets, real local Unix descriptor transfers and
fake systemd/kernel observations. They never open KVM, start a host unit or run a
provider client. Native QEMU/guest execution and resource qualification are still
missing. A fake manager's stop result is not production isolation evidence.

The private object stream now supplies the reader with captured revisions and
paths, an independent byte/call budget and canonical finite records. The private
provider HTTP stream has one connection with no reconnect or outside dial. It
requires the captured model and strict source-tool response policy, and retains
a stopped host-side provider receipt. Control request/ready/finished envelopes
bind the full launch, session and request digest; the host must still validate
their actual source audit/auth return before accepting a report.

The next implementation work must connect the private channels to a pinned
guest runtime with separate provider, reader and code-host identities; implement
native read-only client execution, exclusive bounded provider egress and private
source/provider receipts; transfer only the allowlisted auth record and return it
before shutdown; then accept reports and commit refreshed auth only after a
fresh whole-VM stop proof. Host and guest result framing, image assembly,
`AuditedReviewer` composition and exact-client fake-backend KVM qualification are
unfinished. No partial host implementation closes the parent issue.
