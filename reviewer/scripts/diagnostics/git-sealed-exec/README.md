# Inert sealed-executable diagnostic

This opt-in tool compares proc-path exec and direct `execveat` using the same
sealed static ELF, with ordinary-file controls inside and outside mandatory
bubblewrap confinement. It also attempts a fixed read-only name for the same
inode. It runs no Git parser, provider client, credential flow or policy change.
The generated filter comes from the checkout's committed production
`gitPayloadSeccomp` source. Its bytes and producer, driver, compiler, helper and
probe hashes are recorded separately.

```text
python3 reviewer/scripts/diagnostics/git-sealed-exec/run.py --run-inert-only \
  --output /absolute/new-private-output \
  --go /absolute/pinned-go --module-cache /absolute/offline-module-cache
```

The tool needs an existing user systemd manager, bubblewrap, a static C compiler
and cached Go dependencies. `--cc` and `--bwrap` accept explicit absolute paths.
Production filter, `go.mod` and `go.sum` bytes must match the recorded commit.
The driver and C source have their own hashes, including during an unpublished
source qualification. Output directories are never reused.

One inert user unit has a ten-second runtime, 256 MiB memory, zero swap and
32-task limits. The script records intent before starting it and stops the
exact unit on success or failure. These diagnostic limits are not the complete
production unit. In particular, its temporary workspace does not qualify the
required bounded `noswap` acquisition mount.

Exit zero means controls, same-inode/seal observations and terminal unit
collection succeeded. It does not mean sealed execution succeeded. Inspect
`exec_compatible`, each matrix cell's stages/errno and `settled` in `result.json`.
`qualified` and `noswap_qualified` always remain false. A pre-payload bind failure
must be reported separately from an exec failure. Never use this tool's exit
code as an admission grant or replace the sealed worker with its ordinary-file
control.
