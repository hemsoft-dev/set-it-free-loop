# Offline measured Git runtime

`BuildGitRuntime` builds an immutable Linux x86-64 runtime from explicitly pinned,
trusted installed Git and `git-remote-http` executables. It reads ELF metadata
with Go's `debug/elf`; it does not run `ldd`, a shell, Git or any candidate code.
It imports no auth and starts no unit. Producing an artifact is neither runtime
qualification nor trusted-release promotion.

The builder validates root-owned regular inputs that are not group/world writable through opened
no-follow descriptors, requires single links, hashes all bytes and follows only
bounded ELF `DT_NEEDED` dependencies in fixed system library directories. It
rejects alternate interpreters, unsupported architectures and ambient loader
search/audit/filter/auxiliary paths. The fixed loader is
`lib64/ld-linux-x86-64.so.2`; executables occupy `usr/bin/git` and
`usr/lib/git-core/git-remote-http`. Library paths and names have a closed allowlist.
No host environment or executable discovery selects a runtime at worker startup.

Bounds are 128 files, 64 MiB per input, 128 MiB total file data and a 64 KiB canonical
manifest. The `SFL-GIT-RUNTIME-1` wire artifact contains a length-framed manifest
and ordered file bodies, with exact EOF. Release pins bind Git, HTTP helper and
the complete artifact's byte counts and SHA256. Per-file hashes include all
transitive libraries, not merely the entry points.

Private preparation copies the artifact into an owned measured `git-runtime`
root, rechecks metadata/order/hash/size and exact program pins, and creates only
fixed directories/files. Programs/loader are 0555, libraries 0444, directories 0555;
proc/dev/work/broker mount points begin empty. The anchored private root reuses
the existing immutable-root ownership and foreign-path cleanup protections, with
a distinct directory selector. Failure or closure cannot adopt a replacement
root or follow a foreign symlink. These are private factory inputs, not a public
constructor that declares a downloaded object store qualified.

## Build command

```sh
go -C reviewer run ./cmd/gitruntime \
  --git /absolute/trusted/git --git-sha256 EXPECTED_SHA256 --git-bytes EXPECTED_BYTES \
  --http-helper /absolute/trusted/git-remote-http \
  --http-sha256 EXPECTED_SHA256 --http-bytes EXPECTED_BYTES \
  --output-dir /absolute/new/private-output
```

Arguments are trusted release inputs. The command creates a new 0700 output
directory, writes/fsyncs 0444 `git-runtime.bin` and `manifest.json`, and syncs the
directory. It refuses an existing output, even when its contents match. A wrong
input pin refuses before creating output. A filesystem failure leaves private
partial output for inspection; it does not publish or promote it. The metadata
states `built-only`, `runtime_qualified=false`, `trusted_release_promoted=false`.
No automatic download, default pin, credential import or service start exists.

An actual October 21 offline CLI smoke on mini built 35 files into 28,611,451 bytes.
The entry-point pins were Git 4,547,768 bytes/SHA256
`5516c9f362c29376ab9a499a33082f9f611941d8c75930c880e30ad109e39c9a`,
HTTP helper 986,272 bytes/SHA256
`1c7399430d74282f68062612acefd5740d8f9d3e07a559bf1029bf54b1be8f02`;
artifact SHA256 was
`64a2e640e0545fcf09747a303222b740658ae51bdc12259de67a342003d13516`.
Existing-output and wrong-pin refusals preserved the first artifact. These pins
are observed build provenance, not approval to select a live release.

## Validation limits

Tests cover actual installed ELF production, deterministic artifact verification,
mutated/truncated/reordered metadata/bodies, bounds, substituted inputs, private
root preparation/closure and foreign-path preservation. Optional startup checks:

```sh
SFL_GIT_RUNTIME_NAMESPACE=1 go -C reviewer test -race -count=1 \
  -run '^TestGitRuntime|^TestNativeRuntime' ./
```

The actual bubblewrap check unshares all namespaces, clears the environment and
mounts only the measured root plus private proc/dev/work. Git reports its version
and the HTTP helper reaches its expected `remote-curl` usage error without host
libraries. It is a startup/linker check, not an aggregate fetch/confinement or
resource-pressure qualification. The fixed usage assertion was corrected to the
actual pinned helper's output; fsck/object-validation checks were not relaxed.

The concrete acquisition worker/factory must still authenticate its private
channels and helper peers, inspect actual namespaces/mounts/resource limits,
perform all Git parsing/materialization inside the measured unit, revoke/drain
the broker and prove whole-unit termination before publication. Durable plain
export reopening, uncertain-attempt recovery, credential-generation leases and
release/controller composition remain open. See [unit ownership](GIT-UNIT.md),
[broker](GIT-BROKER.md), [quarantine checks](GIT-QUARANTINE.md) and
[bounded materialization](GIT-MATERIALIZATION.md). No live inference or deployment
was used for the builder, smoke or namespace startup check.
