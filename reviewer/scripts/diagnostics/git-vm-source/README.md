# Inert Git source VM prerequisite probe

This diagnostic checks whether a measured guest can create the exact bounded
work mount and execute a sealed image. It is a possible environment for a future
VM source implementation under [#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
It does not implement that source factory or change the failed host path.

The static C guest runs its privileged probe only as guest PID 1 with UID 0. An
ordinary host invocation refuses before mount or device work. It creates a
256 MiB tmpfs with 65,536 inodes and `noswap,nosuid,nodev,noexec,mode=0700`, then
checks actual filesystem limits/flags, rejects read-only storage, observes
mode 0700 and UID/GID 1203, and parses the exact `noswap` superblock option
for `/work`. Each sealed payload must create, write, sync and remove a work
file as UID 1203.
It also executes the same mode 0500 memfd with all six seals through proc and
`execveat`. Each payload runs as UID 1203 in fresh mount/network/IPC/UTS/PID
namespaces, with zero effective/bounding capabilities and no-new-privileges.

The host builds only this fixed guest. It verifies bounded input files through
non-following descriptors and copies the measured kernel, firmware, QEMU,
captured libraries and ROMs into a fresh private runtime. Replacing an original
input path after capture cannot select the launched bytes. The owned unit
verifies the original runtime directory and every file again, then executes
the original opened root-owned namespace helper to bind the captured root
read-only. KVM runs with no NIC, host share, monitor or TCG fallback.
The transient inert unit has 1 GiB memory, no swap, 64 tasks, one CPU quota and a
60-second ceiling. The runner observes kernel cgroup limits and current manager
identity, requires one bounded nonce-bound guest record, then checks shutdown
and an absent or empty original cgroup. It keeps `production_qualified` false.
These observations do not qualify resource exhaustion or an untrusted parser.
The unit entry requires its nonce-named cgroup and manager invocation before
opening the runtime/helper/KVM. Cleanup never issues a name-targeted stop or
kill. It waits within a finite deadline for the original invocation to end
naturally or under its configured manager runtime ceiling. A replacement,
missing identity or expired wait is an unknown result. An already stopped
unit still needs the original cgroup proof. The parent independently verifies
the retained runner after shutdown before accepting any successful receipt. The original
namespace helper's host interpreter/libraries remain trusted bootstrap
prerequisites. Their list must contain 2-32 sorted, unique, complete pins,
including the loader and libc, checked before launch and after stop.

The diagnostic and unit require `-I -S -B`, suppressing site/profile imports
and bytecode writes. The unit fixes the locale and removes preload/library
and Python-path overrides. It also pins its resolved root-owned Python interpreter, all mapped
runtime files and file-backed stdlib modules and caches. It verifies the
actually executing interpreter and mapped device/inode identities before KVM
work, then checks those pins again after stop. Missing, changed, deleted or
unexpected runtime files refuse qualification. These remain trusted host
bootstrap prerequisites, separate from the read-only QEMU payload. The
diagnostic uses the
same read-only runtime binding mechanism as the native VM path, but does not
use the production source supervisor. Aggregate confinement and exhaustion
still need independent qualification.

From a clean committed checkout on mini:

```text
python3 -I -S -B reviewer/scripts/diagnostics/git-vm-source/run.py --run-inert-only \
  --image /absolute/measured-native-image \
  --output /absolute/new-private-output
```

The measured image supplies pinned kernel/QEMU/firmware metadata and its
captured QEMU runtime artifact at `host-runtime/native-qemu-runtime.bin`
inside the selected image directory. The recorded build-time absolute path is
provenance and is not reopened, so a complete image can be relocated.
No official model client or native image initrd runs. The diagnostic takes no
credential, provider or GitHub transport. The compiler is the installed,
root-owned `/usr/bin/cc` target, recorded with its hash. Retain `prepared.json`,
`observed.json`, `result.json`, source/program/image hashes and unit/serial logs.
Missing or refused evidence is not a pass. Investigate any cleanup refusal
using the retained original unit identity; never stop an unrelated replacement.

Ordinary tests compile only the inert guest and verify host refusal and record
validation. They open no manager, KVM, account, provider or GitHub connection:

```text
python3 -B -m unittest discover \
  -s reviewer/scripts/diagnostics/git-vm-source -p test_run.py
```

The [captured positive record and paired host receipt](testdata/README.md) are
from the actual producer. The earlier version 1 record is retained as an
unsupported historical artifact, with its missing evidence stated.
The [source design assessment](../../../GIT-VM-SOURCE.md) separates these narrow
observations from the source implementation and qualification still required.
