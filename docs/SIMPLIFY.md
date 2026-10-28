# Get SFL reviewing PRs

Proposal for Franz, October 27, 2026. This PR changes no runtime, issue or
existing contract. Plan approval alone grants no activation or deletion.

## Start with zero new code

Use connected Codex GitHub review as SFL's first reviewer. Franz posts
`@codex review` on an allowed PR. Codex reads the diff and posts its native
review under his connected subscription. One Codex reviewer, no SFL-orchestrated
second model. Humans decide what to fix and merge. No fix commands, approvals
or autonomous merges. [OpenAI documents the route](https://learn.chatgpt.com/docs/third-party/github)
and its [subscription usage](https://learn.chatgpt.com/docs/pricing).
It already reviewed [SFL's latest merged PR](https://github.com/hemsoft-dev/set-it-free-loop/pull/283#issuecomment-6456266599).
This proves connectivity only.

This product replacement accepts Codex authorship, OpenAI's model selection and
review format, and manual requests for v0. If an SFL-bot identity or exact model
is essential, decide before building. Automatic triggers wait.

## Where scope grew

Survey at main `e4d85f3`: four Go modules. The offline `reviewer/` alone has
51,371 non-test Go lines, 63,049 test Go lines, 22 command directories and
80 Markdown files. These are tracked physical lines including blanks, comments
and fixture programs. Python adds 5,697 lines; ten research documents add 1,544. The last 15 merged PRs added 13,590 lines and
deleted 816, median 840 changed lines, largest 2,845. No PR is currently open.

Nine alpha subsystems: polling/SQLite controller, Git acquisition VM, reader,
dual CLI workers, auth transfer, egress proxy, App publisher, release registry
and six-arm evaluator. Six activation gate groups remain open. Qualification
has 20 hostile, 13 exhaustion and eight E2E scenarios. Kernel/QEMU pins, VM
images, native-hook evidence and provider-policy packs support that service.
Native requests need none of this local machinery.
See the [architecture](../reviewer/README.md),
[qualification](../reviewer/E2E-QUALIFICATION.md) and
[gate inventory](research/2026-10-21-alpha-activation-checklist.md).

## Cut or park, after approval

Parked issues stay open with a reason and revival condition; work stops. Preserve
code and failed evidence. Revival requires the original unresolved gates. This
cuts scope and maintenance commitments before deleting code.

| All six open issues | Proposed disposition and reason |
| --- | --- |
| [#172](https://github.com/hemsoft-dev/set-it-free-loop/issues/172), dual alpha | Close as superseded after explicit approval of the replacement scope, never as completed. |
| [#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173), eligibility | Park unattended CLI and custom-App questions. Verify only the selected native account, repository access and billing boundary. |
| [#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174), adapters/isolation | Park `reviewer/`, VMs, kernel pins, native hooks, egress and qualification. SFL runs no local model process. |
| [#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176), controller/publisher | Park polling, SQLite jobs, auth leases, custom App writes and promotion. Native Codex publishes its own review. |
| [#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177), evaluation | Park comparative benchmarks and the two-week gate. Franz judges usefulness and noise on three real reviews. |
| [#264](https://github.com/hemsoft-dev/set-it-free-loop/issues/264), missing stop receipt | Park with its worker; no credential recovery or policy decision is implied. |

Keep retired `central-reviewer/`, `gh-sfl/` and deployment code frozen. Supersede
the alpha README and activation checklist for v0; retain other research. Actions
is disabled. The ready
[provider-question pack](research/2026-10-27-provider-support-send-pack.md)
remains unsent and relevant only if the unattended CLI service returns. Native
success establishes no CLI-policy exception.

## Keep only essential controls

- Owner-operated pilot, exact repository ID `1169772257`, same-repository heads.
  Approve PR-content disclosure to OpenAI. Add no local secrets, auth files or
  logs; inspect for accidental secrets.
- Manual budget: one outstanding request, three/day maximum. Record request URL,
  head and time. Reconcile uncertain POSTs before retrying. Reuse an identifiable
  current-head review. Unclear/moved heads stay pending. No parser or branch gate.
- Verify included-plan usage cannot draw purchased credits or paid overflow.
  Record account evidence or stop. No API-key fallback or credit purchase.
- Stop new requests to stop the pilot. Account connector revocation is the
  stronger access stop; neither proves in-flight cancellation. Operator controls
  cannot bound other authorized users or automatic reviews.
- Hostile text can still mislead the hosted reviewer. Keep output advisory and
  verify its reviewed head manually. Existing checks and merge rules continue.
  For this plan PR's required review, wait about 60 minutes, re-request once,
  then follow the existing cross-model fallback if still hung after another hour.
  Pilot failures stop without substitution.

## Ordered small PRs

1. **This plan.** Approve the replacement scope, including manual-only v0 and
   native identity/model selection. No live permission follows from merging it.
2. **Pilot evidence, 1–2 hours after separate explicit pilot permission and
   account checks.** Manually request one real open PR. Record head, request,
   result, turnaround, advisory review state and useful findings or noise.
   Franz judges three reviews before expansion. First review may take minutes;
   provider latency is not guaranteed.
3. **Scope cleanup, 1–2 hours after plan approval.** Add the two supersession
   notices and issue dispositions above. No deletion or credential recovery.
4. **Optional requester, 3–5 hours only if manual use proves painful.** One
   Python command in `scripts/`, local receipts and `gh` stubs. Cap code plus
   tests at 300 lines; stop and keep manual requests if it grows beyond that.
   Its local allowlist, rate cap and disabled flag govern only its own requests.

Allow half a day plus approval/review waits; the optional command is no
prerequisite. Franz approves the replacement and
bounded pilot, then verify the [connected review settings](https://chatgpt.com/codex/settings/code-review)
and account billing controls, including any automatic code/security reviews.
Head/result checks remain an operator task. No App, credential, provider-contact
or organization rollout work is needed.

Council: active GPT 6.1 Sol · consulted Muse Wonder Full 1.0 · missing Claude
Opus 5.5, over pace · adopted 6 · declined 0. Muse prompted the zero-code pilot,
explicit product change, honest operator controls, defined parking, bounded
optional command and ongoing owner duties. Capacity checked October 27 at
20:52 EDT: Claude red/deferred; Muse both windows ok, yellow pacing, gate open.
