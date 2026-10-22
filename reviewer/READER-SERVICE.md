# Private source reader service

`cmd/sourcereader` is a bounded, separately launched reader program. It receives
one canonical `SourceReaderConfig` on a private stdin pipe, uses the immutable
object repository mounted at `/source`, and executes only the trusted
`/usr/bin/git`. It serves one captured MCP connection and exits. Nothing installs
or starts it automatically; existing live execution remains blocked.

The supervisor supplies a persisted 32-character lowercase hexadecimal launch
ID, captured Session, allowlisted paths, private socket root, expected reader UID,
different non-root worker UID and shared IPC GID. Session and manifest hashes plus
the launch ID bind the audit to that exact launch/configuration. The supervisor
must persist their mapping to its durable attempt and physical worker identity
before starting processes. Model output and per-PR commands do not choose these
fields.

The Linux service checks its actual UID/GID, the expected distinct worker UID,
and an existing reader-owned 0700 root with no symlink components. It creates a
new directory named by the full captured Session hash, initially private, then
creates a 0660 socket and exposes only group traversal with directory mode 0710.
It never replaces an existing session path. The worker gets only this job
directory through its mount namespace; the supervisor must arrange that mount
and shared IPC group before launching either role.

Before reading any MCP bytes, the service checks the accepted connection's
`SO_PEERCRED` UID against the captured worker UID. Eight rejected peers stop the
service. A fresh broker enforces the captured snapshot, path subset, source-call
and byte budgets. The supplied verifier cannot grant paths outside the manifest,
and the manifest cannot enlarge its verifier's allowlist. Reader activity stays
separate from citation verification. Existing MCP rules still reject arbitrary
jobs/revisions, traversal, symlinks, submodules, binaries and exhausted budgets.

Cancellation closes listener and active connection and joins source handlers.
Cleanup removes only the socket/directory inode created by this invocation. It
does not recursively remove directories or unlink a replacement file. An unknown
existing path requires recovery rather than destructive retry.

The program accepts only LANG, LC_ALL and TZ environment entries, with bounded
values. HOME, provider auth, GitHub tokens, sockets and other ambient variables
are rejected. The manifest is limited to 16 MiB and five seconds of intake before
the captured phase deadline applies. No arbitrary Git executable, remote URL,
provider endpoint or authentication selector is accepted. Git receives a cleared
environment and `--no-lazy-fetch`; missing promisor objects become omissions
without starting a fetch helper. Use Git 2.46 or newer, with 2.53.0 inspected here.
An unsupported global option fails closed instead of silently ignoring an
environment-only control. A network boundary is still required.

Cleanup atomically retires the entire job directory into a fresh private
quarantine through open directory descriptors, then checks its directory and
socket inodes there before unlinking. Canonical replacements cannot be deleted
by a stale pathname check. Unknown retired entries are restored with
`RENAME_NOREPLACE`; if recovery already recreated the canonical name, they stay
in quarantine for reconciliation. No recursive deletion is used. Concurrent
recovery must not enter another live reader's private quarantine. This does not
isolate a reader from a malicious process with the same UID. The qualified
supervisor must protect job roots and serialize their recovery. A required
cleanup failure also fails the service.

The reader's stdout is exclusively a supervisor audit channel, separate from
worker MCP. It emits a ready event and a stopped event containing actual
`SourceAccess`, bounded together to 2 MiB. A failure before readiness can emit
only a failed zero-tool stopped receipt. `DecodeSourceReaderAudit` requires exact
launch/session/manifest identities, endpoint and reader UID, canonical event
encoding, a newline after every frame including the last, legal lifecycle,
in-scope reads and captured limits. Duplicate,
truncated, stale or additional data fails. Events contain no raw source payload,
provider output, credential or backend diagnostic.

A stopped audit is not physical termination proof. The supervisor must reap the
reader and provider containment before accepting its receipt or releasing a
durable lease. Cancellation or a broken audit pipe can prevent a complete audit;
the launcher must retain uncertainty for recovery. The adapter's in-process broker is not
automatically replaced by an external receipt. The separate
[native VM assembly](NATIVE-VM.md) implements reader/worker role separation,
private object/audit channels and captured receipt validation. Complete physical
containment, actual-account compatibility and arbitrary-crash qualification
remain unproved under
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).

Build without installation or launch:

```sh
go -C reviewer build -o /tmp/sfl-sourcereader ./cmd/sourcereader
```

Run the offline checks:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

Tests measure real Unix peer rejection, protected permissions, captured source
reads/searches, audit binding, stale/ambiguous receipts, cancellation, ambient
credential rejection and preservation of replacement paths. Positive socket
protocol tests explicitly simulate the peer UID; they do not establish separate
OS identities. The local missing-promisor fixture first proves an uncontrolled
Git command invokes an inert local trap helper, then proves the protected reader
records an omission without invoking it. The helper makes no network request.

These fixtures do not qualify real UID provisioning, mount isolation, memory or
egress enforcement, official clients, provider account support or activation.
The supervisor must provide a separate credential-free reader identity, stripped
privileges/groups, read-only object/runtime mounts, no host/provider auth mounts,
no network, bounded resources and reliable process recovery. No App, credential,
retired automation, deployment or live inference is activated by this source.

Primary no-fetch references:

- [Git 2.53 global options](https://git-scm.com/docs/git/2.53.0.html) describes `--no-lazy-fetch` and its environment equivalent.
- [Git option handling](https://github.com/git/git/blob/v2.53.0/git.c#L204-L208) disables missing-object fetching and sets the environment control.
- [Promisor fetch guard](https://github.com/git/git/blob/v2.53.0/promisor-remote.c#L32-L40) returns before spawning fetch when the control is set.
