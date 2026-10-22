# Inert Git source VM prerequisite probe

This diagnostic checks whether a measured guest can create the exact bounded
work mount and execute a sealed image. It is a possible environment for a future
VM source implementation under [#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
It does not implement that source factory or change the failed host path.

The static C guest runs its privileged probe only as guest PID 1 with UID 0. An
ordinary host invocation refuses before mount or device work. It creates a
256 MiB tmpfs with 65,536 inodes and `noswap,nosuid,nodev,noexec,mode=0700`, then
checks actual filesystem limits/flags and the kernel's `noswap` mount record.
It also executes the same mode 0500 memfd with all six seals through proc and
`execveat`. Each payload runs as UID 1203 in fresh mount/network/IPC/UTS/PID
namespaces, with zero effective/bounding capabilities and no-new-privileges.

The host builds only this fixed guest and uses explicit measured kernel, QEMU
and firmware pins. It runs KVM with no NIC, host share, monitor or TCG fallback.
The transient inert unit has 1 GiB memory, no swap, 64 tasks, one CPU quota and a
60-second ceiling. The runner observes kernel cgroup limits and current manager
identity, requires one bounded nonce-bound guest record, then checks shutdown
and an absent or empty original cgroup. It keeps `production_qualified` false.
These observations do not qualify resource exhaustion or an untrusted parser.
This diagnostic does not use the production source supervisor or its namespace
helper; that host confinement still needs independent qualification.

From a clean committed checkout on mini:

```text
python3 -B reviewer/scripts/diagnostics/git-vm-source/run.py --run-inert-only \
  --image /absolute/measured-native-image \
  --output /absolute/new-private-output
```

The measured image is used only for its pinned kernel/QEMU/firmware metadata.
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

The [captured positive record](testdata/README.md) is from the actual producer.
The [source design assessment](../../../GIT-VM-SOURCE.md) separates these narrow
observations from the source implementation and qualification still required.
