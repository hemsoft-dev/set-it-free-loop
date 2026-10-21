# Git worker and helper process evidence

The private `gitProcessBoundary` checks running worker and HTTP-helper process
evidence. It supplies part of the acquisition factory's admission policy. It
does not issue a source capability or qualify the complete acquisition unit.

Construction duplicates the original worker pidfd and expected root/executable
descriptors. It reconstructs the complete pinned runtime artifact from that root
before retaining the Git and HTTP-helper inodes and the executable-map allowlist.
The supervisor must independently authenticate the worker's sealed-image grant
and owning unit. Tests can supply explicitly synthetic executables to exercise
the process checks; those do not qualify a production worker.

Every observation checks the original live pidfd, boot and start ticks before
and after bounded procfs reads. It never reopens the broker peer's numeric PID.
It checks the actual process root and executable device/inode and executable
size/hash, six typed namespace identities, real/effective/saved/filesystem UIDs
and GIDs, all five capability sets, no-new-privileges, seccomp mode, cgroup,
executable mappings and network tables. Maps retain file identities and the two
recognized kernel mapping names, without raw filesystem paths. Failure returns
no partial observation.

The worker must have a read-only measured root, all six namespaces different
from the supervisor, no capabilities, no-new-privileges and filter mode. Helpers
and intermediate Git parents must match its namespaces and cgroup. Executable
mappings must belong to the measured runtime, the worker's own expected image,
or the kernel vDSO/vsyscall mappings. Writable executable mappings and unknown
anonymous executable mappings fail. Process observations are checked afresh;
a saved observation or its hash is not later admission authority.

The private network must expose only `lo`, no IPv4 routes and no IPv6 routes
that can leave loopback. On mini the actual inert namespace has two reject
defaults and one `::1/128` local route. Their exact observed forms are recognized
separately, with bounded counts. Changed destination, prefix, gateway, interface
or flags are not silently classified as harmless. The fixed final syscall
filter and launch chain still need independent qualification; `Seccomp: 2`
alone cannot prove which filter was installed.

Helper checks require the broker's live private SO_PEERPIDFD capability and
kernel UID/GID. Only the exact measured HTTP-helper inode/hash can connect.
The checker retains each intermediate parent pidfd and permits at most seven
measured Git ancestors leading to the original worker. There must be at least
one Git parent. It then re-observes every process and parent edge and rechecks
the original worker. A closed boundary, dead process, forged metadata, changed
inode, namespace, cgroup or ancestry refuses.

## Validation and remaining composition

`SFL_GIT_BOOTSTRAP_NAMESPACE=1 go test -race -count=1 -run
'^TestGitProcessObservation' .` runs actual inert namespace fixtures. They use
the installed runtime's real loader/library files with the Go test ELF explicitly
substituted at the Git and HTTP-helper paths. The executables contain no Git
parser and use no backend or credential. The fixed namespace helper and final
payload filter are used. The fixture verifies the read-only root, all six
private namespaces and capability/policy observations both before and after
dumpability is cleared. A measured helper/Git/worker chain passes. A direct
helper, forged peer and same bytes at the wrong executable inode fail. An actual
anonymous executable mapping makes the worker check fail; removing it restores
the process check.

Separate actual host-process tests reject wrong roots, copied executable inodes,
wrong hashes/sizes, mismatched original pidfds, changed start ticks, cancelled
checks and terminated processes. Borrowed handles stay open and both success
and refusal paths retain no temporary descriptors. Network regressions mutate
the captured kernel route table, alongside the fresh actual namespace test.

This is process/runtime evidence, not full sandbox qualification. The factory
still must bind the worker/helper to the exact manager-owned unit and captured
cgroup/limits, inspect the complete mount/device and inherited-descriptor policy,
prove the fixed filter/launch chain, and retain generation ownership through
broker shutdown and physical unit stop. The actual fixed systemd/tmpfs and
sealed inner-worker launch failures remain in [the launch report](GIT-LAUNCH-CONSTRAINTS.md).
Source import/publication and recovery need the [archive lifecycle](GIT-EXPORT-ARCHIVE.md)
and whole-unit stop receipts. No production factory, App activation or model
inference is enabled by these helpers.
