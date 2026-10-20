# Native VM implementation in progress

This branch is implementing the native review path for
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The source includes a host reviewer and guest runtime, but is not a qualified VM
release. It has no activation command, live inference, App installation or
deployment. The separately required owner/provider/account gates stay closed.

`NativeVMManifest` uses purpose `native-review` and containment protocol 3.
Synthetic fixture purpose and protocol 2 stay separate. The manifest binds the
captured session, provider, full auth launch identity and sealed QEMU/kernel/initrd
pins, plus a separately sealed BIOS pin. Manifest version 2 requires all four
assets. It accepts no executable path, command, environment or host filesystem
selector. Eight one-use descriptors carry those immutable assets, KVM and three
distinct connected Unix streams in fixed order. QEMU reads the BIOS from its
granted descriptor rather than a host package pathname.

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
one CPU quota and seven-minute outer runtime cap, with a 2 GiB guest. Short fake
reviews have sampled these bounds for both clients, with peaks near 1.22 GB and
eight tasks and no OOM events. These samples do not establish stress limits.
The synthetic unit retains
its original 1 GiB/64-task/two-minute profile. `NativeVMSupervisor` reuses durable
unit identity, start settlement, revoked grants, whole-cgroup stop and fresh
recovery. Its stop proof applies only to the distinct native payload.

Ordinary tests use inert sealed assets, real local Unix descriptor transfers and
fake systemd/kernel observations. They never open KVM, start a host unit or run a
provider client. The separate opt-in fixture has completed native QEMU reviews
with both pinned clients against fake TLS, including accepted source-backed
reports, returned synthetic auth and whole-unit stop. This is a bounded fixture
result, not production isolation or full resource qualification.

The private object stream now supplies the reader with captured revisions and
paths, an independent byte/call budget and canonical finite records. The private
provider HTTP stream has one connection with no reconnect or outside dial. It
serializes HTTP/1 through response EOF, including the local WebSocket rejection;
ordinary and Upgrade requests cannot select separate connection pools. A partial
response close retires the stream, while a cancelled queued request leaves the
active response intact. It
requires the captured model and strict source-tool response policy, and retains
a stopped host-side provider receipt. Control request/ready/finished envelopes
bind the full launch, session and request digest; the host must still validate
their actual source audit/auth return before accepting a report.

The host waits for the first private byte within the job's timeout before
starting its five-second HTTP header timer. VM boot and client initialization
cannot consume that header budget. A latched host guard failure adds a private
fatal marker, causing the guest to stop its owned CLI and return auth promptly.
The marker never reaches the CLI and cannot be forged by upstream response
headers. Normal upstream auth/quota responses and the local WebSocket refusal
remain available to the client's refresh and error handling.

`NativeVMReviewer` implements `AuditedReviewer`. Its explicit trusted `Check`
must establish all activation/eligibility gates; construction chooses no default
gate or credential path. It checks peers on the host, starts and grants the
concrete native containment, then waits for a matching private guest ready
record before leasing auth. Export verifies the still-captured granted unit.
After successful whole-unit stop, the object server consumes peer EOF before
the host closes its local channels. Failed or uncertain shutdown revokes intake
immediately. Every return drains object/provider channels, validates
private source/provider evidence and persists a nonsecret audit. A valid auth
return is committed after physical stop even when model output is invalid.
A missing return preserves recovery and cannot recommit a possibly stale seed.

`cmd/nativevmguest` is the sealed `/native-init`. Normal host invocation refuses
before mounting or opening a device. Guest PID1 verifies the release and boot
nonce, prepares separate chroots/namespaces/cgroups, and starts the UID1201
reader on the private object port. Only this session's socket directory is
bound into the client jail. The trusted root client proxy owns its private
provider port and loopback listener. It checks the pinned CLI's no-auth version
and absence of remaining probe processes before ready, then waits for a private
start signal. The CLI uses UID1202 for Codex or UID1204 for Claude and receives
neither private control nor provider descriptors.

Auth seed intake accepts only the selected subscription-file schema. It writes
one fixed file into the disposable client profile. After all roles stop, guest
PID1 checks owner, permissions, link count, size and confinement of the returned
file, then emits a bound `AuthTransfer` return. API-key and opposite-provider
formats are refused. This parsing does not establish entitlement.

Codex's code-mode executable hands its anonymous stdin/stdout pipes to a trusted
guest PID1 broker through a version-3 descriptor protocol. The broker requires
the UID1202 peer in the exact client cgroup, validates distinct anonymous pipes
and their access directions, and adopts pollable CLOEXEC duplicates. Files,
sockets, aliases, old frames and incomplete handoffs are rejected. The launcher
performs no forwarding and its EOF cannot attest completion.

The trusted broker owns both bounded, canary-checked copies and joins them on
completion or cancellation. A private version-2 code-host receipt requires local
relay completion, clean pinned-child exit and physical stop. The actual code
host runs under UID1203 in separate PID/network/mount/cgroup confinement without
a profile, source socket or provider route. Launcher teardown does not discard
the broker's pipe ownership. Intake closes after client shutdown while adopted
exchanges can drain within the cleanup deadline.

Private proxy descriptors regain CLOEXEC immediately after intake, before any
version probe or client exec. Canary copies remain in guest PID1. Final reports
are checked against seed and returned credential canaries.

`scripts/native-vm-image.py` builds only from a clean committed source archive,
the exact pinned Codex/Claude/code-host snapshots, and explicit hashed kernel
and configuration. Its kernel requires built-in virtio-console and isolation
facilities. It records named ELF dependencies including the interpreter and
generates a canonical role-specific release. No source checkout, auth profile,
shell or Git enters the image. It never opens KVM or runs a client or host unit.

Ordinary tests cover these protocols and hostile inputs using synthetic auth,
real local Unix transports and fake managers. Full measured resource limits,
native refresh/restart behavior and broader guest capability qualification
remain unfinished. Happy-path auth return has passed for both native clients.
No source-level test or built image closes the parent issue.

`scripts/native-vm-fake.py --run-fake-only` is the separate opt-in qualification
path. It compiles its driver from a committed archive and records driver and
image revisions separately. Each run requires a new private state root, generates
only synthetic auth, and fixes the upstream to a loopback TLS server. Source and
forbidden-tool cases exist for both providers. Existing profile directories are
never adopted. Driver timeout retains the exact state path and launch journals
for recovery; a missing stopped/auth receipt cannot pass. The driver does not
qualify live eligibility, even if all fake checks pass.

The same script accepts `--provider both --case adversarial` to exercise the
actual `Engine` with four sequential native sessions. `--peer-rounds 2` selects
the configured six-session protocol. Both providers retain their separate auth
stores across fresh VM launches. Every phase binds its captured packet and
prior report hashes, verifies real source read/search exchanges against fake
TLS, and checks auth recovery. Each private unit must record completion, auth
return and whole-unit stop. A completed engine checkpoint must resume without
another VM or a changed ledger. The canned findings and peer judgments test
transport and protocol, not review quality. Native refresh/restart and hostile
capability qualification remain separate requirements.

The BIOS descriptor fix does not qualify the complete host QEMU runtime. The
[x86 loader in QEMU 10.2.1](https://raw.githubusercontent.com/qemu/qemu/v10.2.1/hw/i386/x86-common.c)
also selects an ancillary Linux boot ROM from the QEMU data directory. That ROM
and host dynamic runtime capture remain source qualification work before live
use. The separately required trusted `Check` cannot treat these fixtures as a
production release qualification.
