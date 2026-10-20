# Offline code-host confinement

Pinned Codex 0.163.0 starts its sibling `codex-code-mode-host` with no arguments
and inherited client environment. The source-only code-mode calls use its stdio
transport. Running that process beside the client with access to its profile is
not auth isolation evidence.

The optional `--isolate-code-host` mode of `scripts/codex-fixture.py` places a
fixed relay at that sibling path and retains the unmodified, hash-pinned host at
a different path. The relay accepts no arguments or model-selected executable.
It requires the synthetic fixture marker and expected host hash before launch.
This command is an offline transport experiment, not a production launcher.

The inner Bubblewrap process creates fresh mount, PID, network, IPC and user
namespaces, drops capabilities and uses a new root. Only the pinned code host,
a trusted pre-exec probe, procfs, minimal devices and disposable directories
exist inside. No profile, source socket/object store, schema, artifact directory,
home or provider route is mounted. The probe checks the distinct PID/network
namespace identities, loopback-only interfaces, absent sensitive paths, dropped
capabilities and read-only root. It closes every descriptor above stderr before
executing the pinned host with a fixed environment.

The parent relay forwards opaque stdio bytes, bounded to eight MiB per direction
and forty seconds. It scans both directions, including chunk boundaries, for the
synthetic fixture's access/refresh/account/email canaries. A match fails the
experiment. This scan is evidence about these synthetic values, not a general
credential filter. Local private receipts retain counts and hashes, not raw IPC.
The trusted probe's result is retained through the relay's private stderr sink.
A provider report cannot replace that result. Failure never falls back to the
original same-profile execution.

From clean committed source, use previously verified official binary snapshots:

```sh
python3 reviewer/scripts/codex-fixture.py \
  --client /absolute/pinned/codex \
  --code-host /absolute/pinned/codex-code-mode-host \
  --output /absolute/new/private-artifacts \
  --case source --isolate-code-host \
  --go /absolute/pinned/go
```

The outer fixture has its own network namespace and only a local fake backend.
All auth state is freshly synthesized. No real profile or account is read.
The host receipt records source/tree, both official executable hashes, relay,
Bubblewrap and runtime library hashes. Success additionally requires the existing
source/read/search, provider guard, usage and metadata assertions, plus a clean
code-host relay receipt with nonempty bounded traffic in both directions.

This does not supply the native VM `WorkerLauncher`, separate guest role UIDs,
protected profile transfer/refresh/return, full process-tree stop recovery, or
provider eligibility. Those remain required by issue #174. In particular,
namespace remapping in this experiment does not prove the separate guest UID
contract. The native VM payload must adopt a qualified code-host boundary and
bind its lifetime and private evidence to the worker's containment. Neither a
passing namespace probe nor a successful CLI exit is that attestation.
