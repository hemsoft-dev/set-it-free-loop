# Protected auth profiles

`AuthStore.LeaseProfile` gives a trusted supervisor a fresh private staging
profile for one phase. This is offline source for
[issue #174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174). Tests use inert
strings, not accounts or tokens. No launcher is enabled and no interactive
profile is opened or copied.

The supervisor first persists its containment recovery record, then passes its
opaque containment hash, launch nonce and session hash in `AuthBinding`. Hashes
must be canonical 32-byte hex strings. The launch nonce uses the same 16-byte
format as `SourceReaderConfig`. The provider-specific store holds an exclusive flock
through the phase and writes a durable `lease.json` before exposing credentials.
There is no timeout takeover. Closing the handle or losing the process releases
flock but leaves the journal, so both another lease and `Refresh` remain blocked.

The profile starts with exactly one allowlisted credential file:

| Provider | File beneath the staging profile |
| --- | --- |
| Codex | `.codex/auth.json` |
| Claude | `.claude/.credentials.json` |

The store treats these bytes as opaque and bounds them to 64 KiB. These filenames
are staging contracts, not proof that an authenticated pinned client works.
Identity inspection must still reject API-key auth, substituted models and
opposite-provider credentials. No settings, hooks, plugins, history, MCP config
or ambient HOME contents are imported. The reader and publisher receive no
profile handle or credential bytes.

This staging directory belongs to the supervisor. Workers must never mount the
host auth store. A production launcher must copy only the allowlisted file over
private control channels into its disposable worker profile, then return only
that file to staging. It must not transfer arbitrary worker profile contents.
The supervisor calls `Complete` only with an `AuthStopVerifier` that checks the
exact durable containment binding, confirms all descendants stopped and confirms
all profile mounts detached. A client exit code alone cannot supply this proof.
The [path-free auth transfer](AUTH-TRANSFER.md) supplies a bounded one-file
record and journaled return path. No concrete production launch/channel verifier
is supplied, and the VM worker must still provide the actual private transport.

After shutdown, `Complete` reads the returned file without following its final
symlink, rejects non-regular files, hard links, other owners, public modes, empty
files and oversized files, and checks that canonical state still matches the
leased seed. All paths use an anchored `os.Root`; external parent symlinks cannot
escape it. A private pending replacement and journal record protect the commit:

1. Persist the replacement and its hash before replacing canonical state.
2. Record the committing stage. After a crash, accept only the seed or that exact
   replacement as canonical state, then finish the same commit.
3. Persist the committed stage before removing the disposable profile and pending
   replacement. Clear the journal last, after syncing the directory.

`RecoverProfile` requires the original binding and provider. Recovery repeats
the shutdown check. It never starts a client. Missing final credentials,
unknown canonical state, a malformed journal or uncertain shutdown keeps the
lease blocked. `Retire` can invalidate known canonical credentials after the same
shutdown check when the final profile cannot be recovered. It removes state
instead of restoring the old seed. Retiring-stage crashes are recoverable by
calling `Retire` again.

Replacement files use a reserved private filename pattern. After obtaining the
lock, the store removes only owned regular single-link files matching that
pattern, including credential fragments left by a crash. Unknown files are not
used as credentials. An unsafe matching entry blocks access. Profile disposal
uses root-confined removal after the trusted shutdown check; it does not follow
links into an outside canary. Hostile processes sharing the supervisor UID are
outside this boundary. The store must live on a local filesystem supporting
flock, atomic rename and durable file/directory sync.

`Refresh` remains a trusted local state-update callback. It must not perform a
provider-side refresh or launch a process. Refresh inside an actual client uses
the leased profile and this journal. Importing real auth, exact-client refresh
qualification, isolated credential transfer, production containment recovery,
bounded egress and owner activation remain separate requirements of the issue.

Run the synthetic tests with:

```sh
go -C reviewer test -race -count=1 -run 'TestAuthProfile|TestRefresh' ./...
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The tests cover both providers, real subprocess loss without deferred cleanup,
all commit stages, cross-process lock release, simultaneous lease/refresh
exclusion, shutdown refusal, hostile credential files and fail-safe retirement.
