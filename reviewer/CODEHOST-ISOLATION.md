# Offline code-host confinement

Pinned Codex 0.163.0 starts its sibling `codex-code-mode-host` with no arguments
and inherited client environment. Source-only code-mode calls use its stdio
transport. Running that process beside the client with access to its profile is
not auth isolation evidence.

The optional `--isolate-code-host` source case of `scripts/codex-fixture.py`
places a fixed relay at that sibling path. Before starting the client namespace,
the host starts one trusted broker outside it. The broker creates a separate
Bubblewrap namespace for the unmodified, hash-pinned code host. Nested namespace
creation failed under this host's enforced AppArmor child restrictions. The
sibling design needs no host policy change and has no same-profile fallback.

The client sees one job-local Unix socket and the relay. Its fixed prelude has
no executable, arguments, source paths, model, profile or credential selectors.
The broker accepts one connection, checks real Unix peer credentials against
the fixture owner, and checks the exact versioned prelude before launching its
fixed payload. A private temporary directory protects the socket. This is an
offline transport experiment, not a production launcher or an identity boundary
against other processes owned by the same host user.

The code-host process has fresh mount, PID, network, IPC and user namespaces,
dropped capabilities and a new root. Only the pinned code host, a trusted
pre-exec probe, procfs, minimal devices and disposable directories exist inside.
No profile, source socket/object store, schema, artifact directory, home or
provider route is mounted. The probe checks distinct PID/network namespace
identities, loopback-only interfaces, absent sensitive paths, dropped
capabilities and a read-only root. It closes every descriptor above stderr
before executing the pinned host with a fixed environment. Source tools reach
the official client through opaque code-host RPC, then its existing confined
MCP transport; the code host has no direct source socket mount.

Both relay and broker bound traffic to eight MiB per direction and forty seconds.
Both scan traffic, including chunk boundaries, for the synthetic fixture's
access/refresh/account/email canaries. A match fails the experiment. This proves
only that those tested plaintext values were absent, not that arbitrary secrets
are filtered. Receipts retain byte counts and SHA256, not raw IPC. The broker
retains its probe result and bounded stderr outside the client namespace.
The client may kill its relay during normal teardown, so that ephemeral process
cannot supply completion evidence. Only the separate broker's stopped, private
receipt is used with the existing client/provider/source assertions.

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
The host receipt records source/tree, official executable hashes, relay,
Bubblewrap and runtime library hashes. Success requires the existing source
Read/Search, provider guard, usage, metadata and report assertions, plus a clean
broker exit and receipt with nonempty bounded traffic in both directions.
Unsupported isolation/case combinations refuse before execution.

`testdata/actual-codehost-broker.json` was produced by the pinned native client
and code host, with client SHA256
`399ff69c431c066e40de318fba45c2b07f45a76b19939d247ee4828fc70c4215`
and host SHA256
`8724d73ec190536233ab4628d24a071bfbcf27e5124f08fe35129f26fe48b916`,
from committed source `2142b5fa89a582a032eb4f7364a9842d3a4b4544`, during a successful local source experiment.
The Python tests mutate its version, probe, bounds, canary and hash fields.
Go tests exercise real Unix peer credentials/preludes, opaque bytes, split
canaries, exact/overflow limits, write failures and host-execution refusal.

This does not supply the native VM `WorkerLauncher`, separate guest role UIDs,
protected profile transfer/refresh/return, full process-tree stop recovery, or
provider eligibility. Those remain required by issue #174. Namespace remapping
here does not prove the separate guest UID contract. The native VM payload must
adopt a qualified code-host boundary and bind its lifetime/private evidence to
worker containment. Neither a namespace probe nor a successful CLI exit is that
attestation. No App, service, actual account, live inference or paid usage is
activated by this experiment.
