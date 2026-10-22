# Source-only Git guest

The source VM guest has a separate purpose, release and request contract. It
contains the source guest entry, measured Git runtime and namespace helper. It
contains no subscription client or credential loader. The image builder builds
committed source offline and takes explicit kernel, firmware, Git and helper
pins. Its output remains an unqualified candidate.

Guest PID1 runs as root and owns three private virtio channels named
`sfl.source-control`, `sfl.source-read` and `sfl.source-export`. It verifies the
image release against the exact request whose hash is pinned in the boot command
line. Root mediates the bounded Git read channel. It creates the exact noswap
work filesystem and starts the sealed source worker under UID/GID 1203, inside
its original cgroup and private namespaces. Root holds the runtime and broker;
the worker receives no virtio descriptor, host socket or provider credential.

The namespace helper starts under the unprivileged outer identity. It executes
the worker as PID1, drops all capabilities and installs the fixed filter. The
worker authenticates root through the inherited original pidfd and a fresh
SO_PEERPIDFD connection. Root is unmapped inside the worker user namespace, so
the expected peer UID/GID is the kernel overflow identity, verified as 65534 by
guest setup. An overflow ID alone grants nothing. Root independently observes
the worker's original pidfd, identity, executable, runtime, mounts, descriptors,
capabilities and namespace state before sending readiness to the host.

No Git parser runs before the exact host grant. After that grant, only the
verified Git HTTP helper can borrow the root-owned read broker. Candidate Git
objects stay in the guest; the host receives bounded plain-object export frames.
The guest waits for natural worker exit, stops the original guest cgroup, drains
the broker and reports the export count/hash and broker audit. This record is a
guest observation. It proves neither host VM stop nor credential disposal. The
production host supervisor must separately retain and stop the original whole
VM, drain all host borrows, journal unknown outcomes and retain private staging
until those independent checks complete.

The root has a 110-second request deadline. The worker cgroup limits memory to
384 MiB, disables swap and limits tasks to 64 and CPU to one core. Work space is
256 MiB with 65,536 inodes and nosuid/nodev/noexec/noswap. The captured runtime
parent permits traversal for namespace-helper setup without permitting writes.
The worker standard streams are `/dev/null`; fixed root stage diagnostics carry
no child output. Host policy and the failed original host acquisition gates
remain unchanged.

## Reproduce the model-free guest test

Use a clean committed checkout and the explicit pins of trusted installed
inputs. Build with `python3 -B reviewer/scripts/git-source-vm-image.py --help` for
the required arguments. Select a new private output directory. The builder
limits Go parallelism to two, disables module/toolchain downloads, captures
QEMU runtime dependencies and emits `manifest.json` and `source.cpio.gz`.

Then run:

```sh
python3 -B reviewer/scripts/git-source-vm-fixture.py \
  --image /absolute/source-image \
  --output /absolute/new-fixture-output \
  --go /absolute/go \
  --module-cache /absolute/prefilled-module-cache
```

The runner requires the image's exact clean source revision, builds the fixture
from an immutable archive and uses one captured original diagnostic unit. It
bounds memory to 2 GiB, swap to zero, tasks to 96, CPU to one core and unit
lifetime to 140 seconds. QEMU uses KVM, one vCPU, 1,536 MiB, no NIC, no host
share and no monitor. Only trusted synthetic Git objects are parsed on the host
to implement the fake upload-pack backend. Real Git acquisition and export run
inside the guest. The test compares source bytes and the host-observed export
count/hash with the guest record. Cleanup uses the original captured cgroup,
never a reusable unit name.

The first passing inert run on October 22, 2026 at 08:14 EDT used source
`7007ce5` and measured two broker requests and a 10,109-byte export. Earlier
failures remain local evidence: wrong outer UID mapping, missing PID1 launch,
inaccessible runtime parent, a host-only final broker recheck and the refused
temporary diagnostic stderr pipe. The production stream contract keeps that
pipe refused. No provider, credential, App or live endpoint was used.

This is guest data-path qualification only. Production source-VM host ownership,
durable recovery, factory/startup composition and aggregate hostile/crash tests
remain open under [issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
The original host acquisition failure remains recorded in
[GIT-VM-SOURCE.md](GIT-VM-SOURCE.md). Provider eligibility and live activation
remain separate gates.
