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
record and journaled return path. The [native VM host/guest](NATIVE-VM.md)
implements the private transport and captured stopped-unit verifier. Its
synthetic auth fixtures do not qualify production account use or aggregate
physical/crash recovery.

After shutdown, `Complete` reads the returned file without following any
symlink, rejects non-regular files, hard links, other owners, public modes, empty
files and oversized files, and checks that canonical state still matches the
leased seed. All paths are opened beneath an anchored `os.Root` and refuse every
symlink, including one whose target stays inside the store (see
[Symlink refusal beneath os.Root](#symlink-refusal-beneath-osroot)). A private
pending replacement and journal record protect the commit:

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

## Symlink refusal beneath os.Root

`os.Root.OpenFile` confines resolution to the root but follows a symlink whose
target stays inside it, even when the caller passes `O_NOFOLLOW`. Go 1.25.14 and
1.26.7 both do this; plain `os.OpenFile` returns `ELOOP`. A planted
`state.json -> real.json` was therefore judged by its target (issue #257).

Every production `*os.Root` open that asks for `O_NOFOLLOW` now goes through
`openRootNoFollow` (`root_nofollow_unix.go`). It takes the root's own directory
descriptor and opens each path component with `openat(O_NOFOLLOW)`, so the
kernel refuses an intermediate or final link with `ELOOP`. With `O_PATH`, the
call returns the link itself. `O_CREATE` without `O_EXCL` cannot create a
dangling link's target. `writeAuth` creates its replacement the same way and
renames it with `renameat` into a parent opened without following links. It
refuses a destination that is already a symlink. `os.Root.Rename` would have
followed an intermediate link. `TestRootOpenFileNoFollowSitesUseHelper` fails if
a new `*os.Root` `OpenFile` call passes `O_NOFOLLOW`.

| Site | Opens | Result |
| --- | --- | --- |
| `auth_profile_unix.go:86` | `refresh.lock` (create) | Refused; tested, including a dangling link |
| `auth_profile_unix.go:148` | `.replace-*` crash fragments | Refused; tested |
| `auth_profile_unix.go:177` (`readAuth`) | `state.json`, `lease.json`, `pending.json`, profile auth file and every other `readAuth` record | Refused at final and intermediate components; tested |
| `auth_profile_unix.go:213-225` (`writeAuth`) | Replacement file, destination parent and `renameat` | Refused at final and intermediate components; tested |
| `containment_stop_linux.go:71` | `cgroup.kill` | Refused; tested. `ELOOP` is not proof of removal, so it blocks. |
| `containment_supervisor_linux.go:91` | cgroup control files | Refused; tested |
| `git_acquisition_unit_linux.go:111` | `git-owner.lock` (create) | Refused; tested with a dangling link |
| `git_bootstrap_launch_linux.go:28` | `/tmp` root `.` | Uses the helper. `.` is the root itself and cannot be a link. |
| `git_bootstrap_launch_linux.go:73` | Just-created `git-work-*` | Refused; uses the helper. The real `/tmp` tmpfs check cannot run offline. |
| `git_descriptor_boundary_linux.go:95` | `/proc/PID/fdinfo/N` | Refused; procfs shape tested on the helper |
| `git_export_archive_linux.go:88` | `git-export.lock` (create) | Refused; tested with a dangling link |
| `git_export_archive_linux.go:368` | `payload.bin` (exclusive create) | Refused. `O_EXCL` never followed a final link. |
| `git_export_archive_linux.go:437`, `git_factory_recovery_linux.go:35` | `payload.bin` | Refused. `readGitExportRecord` already rejects non-regular entries. The helper closes the swap window after that check. |
| `git_factory_retirement_linux.go:110` | Retirement files | Refused; tested. `ReadDir` already rejects non-regular entries. |
| `git_factory_retirement_linux.go:280` | Retirement files before unlink | Refused. The identity and `Lstat` recheck already failed on a link. |
| `git_mount_boundary_linux.go:227` | Worker mount points (`O_PATH`) | Returns the link itself; non-directory and non-device types are rejected. Shape tested. |
| `git_mount_boundary_linux.go:254` | Worker `/work`, `/tmp` | Refused; shape tested |
| `git_process_boundary_linux.go:69` | Runtime pins through `/proc/self/fd/N` | Refused. Already verified by `verifyGitRuntimeDirectory`. |
| `git_process_observation_linux.go:96` | `/proc/PID/{stat,status,cgroup,maps,net/*}` | Refused; procfs shape tested on the helper |
| `git_quarantine_verify_linux.go:151` | Quarantine marker files | Refused; tested |
| `git_reviewer_startup_linux.go:330` | `reviewer-trigger-policy.json` | Refused; tested |
| `git_reviewer_startup_linux.go:352` | Policy replacement (exclusive create) | Refused. `O_EXCL` never followed a final link. |
| `git_runtime_directory_linux.go:57`, `:112` | Runtime metadata and pins | Refused; tested. The `Lstat` mode check already rejected a link. |
| `git_runtime_linux.go:351`, `:384` | Runtime files (exclusive create) | Refused at intermediate components; `O_EXCL` covers the final component |
| `git_vm_archive_lock_linux.go:67` | VM archive lock (create) | Refused; tested |
| `git_vm_owner_journal_linux.go:136` | `source-vm-units.json` | Refused; tested |
| `native_runtime_root_linux.go:85` | Runtime files (exclusive create) | Refused at intermediate components; `O_EXCL` covers the final component |
| `native_runtime_root_linux.go:168` | Runtime pins | Refused; tested |
| `native_runtime_root_linux.go:196` | Runtime root `.` | Uses the helper. `.` is the root itself and cannot be a link. |
| `native_vm_evaluation_recovery_linux.go:83` | Native evidence records | Refused; tested |
| `native_vm_guest_linux.go:573` (guest) | Returned provider auth file | Refused at final and intermediate components; tested with a cross-provider link |
| `release_promotion_linux.go:196` | `promotion.json` | Refused; tested |
| `release_promotion_linux.go:265` | `promotion.lock`, `startup.lock` (create) | Refused; tested |
| `vm_boot_fixture_linux.go:96`, `vm_grant_linux.go:360` | Serial logs (exclusive create) | Refused. `O_EXCL` never followed a final link. |

No audited site needs to follow an in-root link.

Run the synthetic tests with:

```sh
go -C reviewer test -race -count=1 -run 'TestAuthProfile|TestRefresh|NoFollow|TestAuthStoreRefusesInRootSymlinks' ./...
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The tests cover both providers, real subprocess loss without deferred cleanup,
all commit stages, cross-process lock release, simultaneous lease/refresh
exclusion, shutdown refusal, hostile credential files and fail-safe retirement.
