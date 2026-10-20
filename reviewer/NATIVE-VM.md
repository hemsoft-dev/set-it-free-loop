# Native VM implementation in progress

This branch is implementing the native review path for
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The source includes a host reviewer and guest runtime, but is not a qualified VM
release. It has no activation command, live inference, App installation or
deployment. The separately required owner/provider/account gates stay closed.

`NativeVMManifest` uses purpose `native-review` and containment protocol 4.
Synthetic fixture purpose and protocol 2 stay separate. Manifest version 3 binds
the captured session, provider, full auth launch identity and six asset pins.
Ten one-use descriptors carry sealed QEMU/kernel/initrd/BIOS/runtime bytes, KVM,
three distinct private Unix streams and the original root-owned namespace helper.
Named bootstrap library pins travel in the bounded manifest. Old native protocol
3 journals remain readable for abort/recovery, but cannot start or grant work.

The runtime artifact contains a canonical manifest and ordered regular file
bodies. Every body has a path, size and SHA-256 pin. It permits only the fixed
ELF interpreter, named libraries and three ancillary boot ROM paths, with at most
128 files and 128 MiB of data. It contains no archive links, ownership records,
commands or filesystem selectors. Its program pin must match sealed QEMU.

The stub prepares a new private runtime directory, copies QEMU from its sealed
descriptor, verifies all bytes and mounts the directory read-only through the
original retained namespace helper. QEMU executes the fixed `/qemu` copy and uses
`/firmware`; BIOS, kernel, initrd, KVM and private channels use fixed descriptors.
The helper consumes and closes the host directory descriptor before QEMU exec.
Its fixed arguments create namespaces, disable nested user namespaces and supply
only private `/proc`, `/dev` and bounded `/tmp` mounts. The stub records directory ownership before file intake. After physical
whole-unit stop and valid auth persistence, host cleanup removes only the
recorded runtime inode. Recovery may retry cleanup through the same supervisor
gate. A missing ownership record or replaced directory is preserved and reported
as an error. A process-loss fixture covers reopening the recorded tree; full
crash/restart qualification remains separate.

The namespace helper is an explicit trusted host prerequisite. Its original
root-owned executable and named host interpreter/libraries are hash-checked
before launch and before auth export. Those host libraries are not sealed.
Metadata probes established that the original helper works under
`NoNewPrivileges`; an identical private copy failed on this host. QEMU memfd exec
inside that namespace also failed. The fixed verified copy route addresses the
observed failures without changing host policy. These probes do not establish
the precise policy cause or production isolation.

The private streams are control/auth/result, immutable source objects and
provider HTTP. Channel socket inodes must differ. Guest descriptor export is
one-use, closes the retained original ends and preserves EOF when QEMU closes.
SCM_RIGHTS receive sets CLOEXEC atomically, bounds data/ancillary descriptors,
closes rejected descriptors, and shares the tested framing reader with the
unchanged four-descriptor synthetic fixture.

The new `cmd/nativevmstub` is inert outside its owned systemd unit and before its
authenticated supervisor grant. It verifies the exact sealed assets, KVM API
and private stream types before executing the captured QEMU copy. QEMU has no NIC, host
share, monitor, interactive serial output or TCG fallback. Three fixed virtio
serial ports connect to the private streams. The guest init is fixed at
`/native-init` and the kernel command line binds the launch nonce.

The native unit has a proposed fixed 3 GiB host memory cap, no swap, 128 tasks,
one CPU quota and seven-minute outer runtime cap, with a 2 GiB guest. Short fake
reviews have sampled these bounds for both clients, with peaks near 1.35 GB and
nine tasks and no OOM events. These samples do not establish stress limits.
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
and configuration. Explicit QEMU, namespace helper and BIOS pins are also
required. The build captures QEMU libraries/ROMs and records host bootstrap
dependencies. Its kernel requires built-in virtio-console and isolation
facilities. It records named ELF dependencies including the interpreter and
generates a canonical role-specific release. No source checkout, auth profile,
shell or Git enters the image. It never opens KVM or runs a client or host unit.

Ordinary tests cover these protocols and hostile inputs using synthetic auth,
real local Unix transports and fake managers. Full measured resource limits,
interrupted refresh/crash recovery and broader guest capability qualification
remain unfinished. Both native clients have refreshed expired synthetic auth,
returned the changed tokens and reused them after reopening the store in a fresh
VM. Those bounded cases do not qualify interrupted refresh or production use.
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
transport and protocol, not review quality. Interrupted refresh/crash recovery and hostile
capability qualification remain separate requirements.

The captured host runtime includes the ancillary Linux boot ROM selected by the
[x86 loader in QEMU 10.2.1](https://raw.githubusercontent.com/qemu/qemu/v10.2.1/hw/i386/x86-common.c).
The separately required trusted `Check` must still establish runtime, policy and
account eligibility. Building or exercising this fixture cannot authorize live
use.

Port discovery also waits for connected writability before framed intake. The
[Linux 7.0 virtio-console implementation](https://raw.githubusercontent.com/torvalds/linux/v7.0/drivers/char/virtio_console.c)
can return EOF and POLLHUP after a device node appears but before the host-open
notification arrives. The bounded readiness wait rejects HUP even when another
readiness bit is set. Fixed boot-control/object/provider/request progress
records remain separate from the authenticated Ready record and cannot export
auth. The complete pre-Ready stream remains bounded to 24 records.

A separate owned serial socket sends early boot output only to a bounded host
parser. It retains fixed setup-stage, panic and OOM enums, discards raw bytes,
and never publishes transcripts. Guest client and reader jails have no UART
device. This channel cannot establish Ready or authorize auth export. It
exists to distinguish setup failure from private virtio-stream intake failure.

Guest PID1 explicitly creates and validates its fixed bootstrap mountpoints
before the kernel mount calls. It rejects symlinks, non-directories, writable
mountpoints and wrong ownership. Repeated native runs caught a missing `/sys`
mountpoint before any auth export; bootstrap no longer assumes its presence.
Runtime files still require their full pinned-byte checks before Ready.

Repeated native boots exposed kernel initramfs write failures with an exhausted
root filesystem before auth export. The fixed command line now supplies
`initramfs_options=size=1024m,nr_inodes=4096`. Guest PID1 requires the actual
tmpfs byte and inode limits to match before proceeding. This accommodates the
roughly 700 MB runtime while preserving explicit bounds inside the 2 GiB guest.
Linux 7.0 [accepts early rootfs options](https://raw.githubusercontent.com/torvalds/linux/v7.0/fs/namespace.c)
and [defaults ordinary tmpfs mounts to half of total RAM](https://raw.githubusercontent.com/torvalds/linux/v7.0/mm/shmem.c).
The prior implicit boot-time capacity was insufficient in failed runs.


When the captured policy declares refresh routes, the root client supervisor
creates a session-only CA and a distinct server certificate and retains its private key in memory.
The official CLI receives a fixed public CA file and an HTTPS proxy on its
existing private loopback listener. CONNECT permits only declared refresh and metadata
names on port 443, requires matching SNI and HTTP Host, and terminates TLS
inside the disposable namespace. The decoded request still passes the same
method/path/body budgets and private host egress guard. This is not an opaque
TCP tunnel. Other authorities, IP literals, ports and host overrides fail closed.

Four incomplete TLS handshakes are the maximum. Header/handshake deadlines and
cancellation bound intake; shutdown closes and joins the owned connections and
requests. The CA file is root-owned and read-only; its key never enters a file,
CLI, source reader or host report. No system trust or interactive profile is
modified. No refresh route means no TLS proxy or new trust environment.

The opt-in fake fixture adds `--case refresh` for each single provider. It starts
with expired synthetic auth, requires a counted refresh and changed returned
tokens, reopens the protected store, and starts a fresh native job that must use
the returned tokens without another refresh. Both jobs must produce source-backed
fixture reports, return auth, stop their whole units and remove runtime roots.
The exact pinned clients passed this path against fake TLS. Hand-built OAuth
responses and TLS unit tests remain supporting evidence; the native runs are
required to establish that the real clients consumed and persisted those
responses. This does not establish real account support or provider permission.

The CA signs a separate non-CA leaf certificate, since
[WebPKI rejects a CA used as an end entity](https://github.com/rustls/webpki/issues/200).
Claude refresh also fetches its exact declared OAuth profile endpoint; its fake
response contains only synthetic account/subscription metadata. This route has
an independent metadata allowance and uses the rotated bearer token. CONNECT's
HTTP Host and selected route must agree; adding metadata cannot redirect a
request with the same path to another captured authority.

A client may close its own HTTP/TLS connection after refresh. The sidecar removes
ordinary close/keep-alive Connection headers before the separate private hop,
while preserving the local WebSocket negotiation contract. Other nominated
connection tokens are refused. A real Unix-stream regression reproduced the old
502 on the next metadata request; both pinned clients now complete refresh and
subsequent source review without retiring that private stream.

An idle reader receives trusted SIGTERM cancellation, emits its failed audit and
is still checked/killed through its owned cgroup. This avoids spending the whole
cleanup deadline waiting for a client that never connected. Native failed-client
runs now return stopped auth without implying a valid source report. A separate
process fixture verifies cancellation before the deadline and retains the
physical-stop enforcement.

The client supervisor's private readiness pipe remains available for one bound
failure classification after Ready. Only fixed enums travel there, with launch
and request hashes. The pipe is CLOEXEC before any client/version exec. A status
cannot establish Ready, auth export or successful work. Both EOF and malformed
status have bounded handling. Stderr scanning retains a small transient suffix
and publishes no raw messages; failed phases can report a fixed client-error
stage without losing their auth recovery evidence.

After any attempted refresh, the host commits returned auth only when its
stopped provider receipt is complete, has no failure or exhausted budget, and
the returned file differs from the exported seed. A lost reply or unchanged
file retains the exported recovery journal. Model output failure alone does
not discard a changed return after fully captured successful HTTP. This rule
is conservative: an unrelated later transport failure also retains recovery.

The fake-only `--case refresh-uncertain` accepts one synthetic refresh then
closes the loopback TLS connection before returning response bytes. It requires
both the current store and a reopened store to refuse further auth mutation,
no substantive report, whole-unit stop, and runtime-root removal. It launches
one VM, leaves its private recovery state for inspection, and never reseeds it.
This bounded response-loss case does not qualify arbitrary host crashes,
operator recovery or production account behavior.

Reader SIGTERM is a graceful intake stop. It closes an idle listener immediately,
but an accepted source connection retains its bounded phase context through
queued work and clean EOF so it can emit a complete audit. The hard phase
context still closes active work, and guest PID1 still enforces cgroup kill and
empty-group checks within its cleanup deadline. Plaintext loopback requests can
reach the intended inference and metadata routes, but a refresh route requires
the CONNECT/TLS path and its captured authority, SNI and inner Host checks.
