# Current-main fake-only hook qualification, October 27, 2026

The bounded `codex-settings` physical case passed against a fresh native image
and runner from main
[`951a4c93126deac84d1a355e510d8bf3fe84fd46`](https://github.com/hemsoft-dev/set-it-free-loop/commit/951a4c93126deac84d1a355e510d8bf3fe84fd46).
This replaces the unknown executable-path prerequisite in the latest
[owner approval](https://github.com/hemsoft-dev/set-it-free-loop/issues/174#issuecomment-6448029195)
with fresh measured evidence. It does not complete
[#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).

## Read-only asset recovery

The fixed client hashes in `reviewer/scripts/native-vm-image.py` matched:

| Asset | Existing input | SHA-256 |
| --- | --- | --- |
| Codex 0.163.0 | `/home/franz/.codex/packages/standalone/releases/0.163.0-x86_64-unknown-linux-musl/bin/codex` | `399ff69c431c066e40de318fba45c2b07f45a76b19939d247ee4828fc70c4215` |
| Claude 2.1.303 | `/tmp/sfl-current-head-qualified-prep-t2sg2hhj/current-native-image/claude-snapshot` | `375988f457756524234695cf2d8f64ace99c5f68682e47ea24862a843c191450` |
| Code host | `/home/franz/.codex/packages/standalone/releases/0.163.0-x86_64-unknown-linux-musl/bin/codex-code-mode-host` | `8724d73ec190536233ab4628d24a071bfbcf27e5124f08fe35129f26fe48b916` |

The original Claude path `/home/franz/.local/share/claude/versions/2.1.303`
is absent. The first attempt stopped on that missing file before any build or
VM run, preserving `/var/tmp/sfl-q27-neb2vqqs/receipt.json`. The next attempt
used the explicitly allowed prior native-image snapshot. No client download,
login, pin change or execution outside the fake-only fixture occurred.

The recovered kernel at
`/tmp/sfl-pinned-kernel-recovery-rcsm3bjh/vmlinuz-7.0.0-38-generic`
matched `9f55ef7253055a9cf68f8a04323c93dead2fca617386d842fbde9ca7b178b27e`.
Its retained config matched
`6cb18717c6914d4947489eedf8cb69ac23db3faa8ef9b9f563ff02c346dcd32d`.
The root-only boot file was not reopened. QEMU, SeaBIOS, bwrap and the three
separately located QEMU ROMs also matched the October 25 receipt. All selected
assets passed regular-file, single-link, ownership and writable-mode checks.

## Fresh physical result

The successful tranche ran October 27, 19:32:35 to 19:33:03 EDT. It held both
`sfl-vm.lock` and `sfl-native-fixture.lock`, verified KVM API 12 and at least
2.5 GiB available memory, and found no preexisting SFL VM units. Offline
compilation used a new private disk-backed cache copied from the prior cache,
nice 10, two Go workers and a 1536 MiB Go memory limit. It changed no host
quota, mount, resource, isolation or network policy. Both image and runner
explicitly bind main `951a4c9`.

- Native image build passed in 15.397 s. Initrd SHA-256 is
  `1eec11445e75cc128ee25585f75eafe8e33c79b026305cab9c738ddfbe13adc3`.
- The pinned denial helper SHA-256 is
  `6da88085307fdd1c59a2e933a3669cb5a897628c114a14f35a65937baac8dad0`.
- The selected physical case passed, 1 case, 0 skips, 0 failures, all 18 checks.
  Driver time was 6.501 s, matrix time 8.682 s, unit peak memory 1.1G.
- The session/request/image/helper-bound producer record reports
  `Attempts=1`, `Denied=1`, complete and not failed, entry `sh-deny-v1`, UID 1202.
  Auth returned; provider stopped; the captured unit stopped; runtime root
  removal and the matrix's independent unit-stop check passed.
- Raw result SHA-256 is
  `ec1ecbb7e1a30b13a3cd41dd08d3ef0bae15fad6a43b3cdbd8b7d355b4fc6ae7`.

The [sanitized receipt](2026-10-27-fake-hook-receipt.json) contains the asset
and result pins, exact revisions, checks and validator-control outcomes. Full
private build and fixture receipts remain under `/var/tmp/sfl-q27-jaapq3qq/`.
Synthetic auth and raw fixture transcripts were not uploaded.

## Validation and limits

Seven offline mutations of the actual result, absent, malformed, stale,
forged, wrong-helper, wrong-image and unsafe, all failed validation. These
are validator controls, not additional physical guest runs. The existing
hostile-matrix Python suite also passed, 29 tests. No production or test source
changed in this documentation tranche. The previous main vet/race evidence
remains historical; no fresh full Go race run or hosted CI pass is claimed.
Actions remains disabled.

This measurement identifies denial-helper execution. It does not attribute the
entry causally to the injected hook. Zero remains no observed attempt, and
denial before helper entry remains unproved. No source-VM physical run,
aggregate E2E/resource/crash qualification, real model-catalog check, live
subscription evaluation or release promotion occurred. The docs PR has
unchanged `reviewer/` bytes; its later head is not the measured image revision.

[#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173) stays open for
provider/account evidence. Its [ready-to-send support pack](2026-10-27-provider-support-send-pack.md)
is unsent. [#264](https://github.com/hemsoft-dev/set-it-free-loop/issues/264)
remains human-required. Work on
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176) and
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177) follows
[#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).
No live activation, login or billing change was performed or inferred.
