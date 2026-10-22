# Alpha activation evidence still required

Status snapshot: October 21, 2026 at 20:07 EDT. This reconciles
[#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173),
[#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174),
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176) and
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177) against the
[alpha contract](https://github.com/hemsoft-dev/set-it-free-loop/issues/172).
It records project evidence and missing decisions, not a new interpretation of
provider terms or permission to activate anything.

Main at this snapshot is `bd5cd59ec873ec18c849ad96bd179d5242bef9cf`.
[PR #230](https://github.com/hemsoft-dev/set-it-free-loop/pull/230) delivered
native single-provider controls, and
[PR #231](https://github.com/hemsoft-dev/set-it-free-loop/pull/231) delivered
explicit release decisions.
[PR #232](https://github.com/hemsoft-dev/set-it-free-loop/pull/232) delivered
durable six-arm evaluation and stopped native recovery. Its reviewed head was
`7dd5d612016b9ea0bd396230f0f5fc6342943d88`; its synthetic qualification used
that source. This snapshot does not claim that all offline implementation or
whole-host qualification is complete.

## Chosen triggers and scope

Franz selected two polling triggers on October 21 at 01:19 EDT:
an original authorized **`@sfl review`** PR comment, and automatic review when a
PR opens or its head changes. Manual requests retain the real commenter's
immutable identity; automatic runs retain PR-event or explicit polling-observation
provenance and the enrolled owner policy. No human pusher or webhook event is
invented. Native Reviewers-picker assignment is no longer a prerequisite.

The alpha remains limited to repository ID **1169772257** and the selected
installation, with owner-only commands, immutable snapshots/configuration,
generation fences and advisory COMMENT publication. See the
[implemented trigger contract](../../reviewer/GITHUB-TRIGGERS.md). This is not
organization-wide activation, a machine-user design or an Actions deployment.

## Six unsatisfied activation gates

Each row remains open. Source implementation, a passing fake test or a normal
code-review merge supplies no activation grant.

| Gate | Evidence already retained | Required before live use |
| --- | --- | --- |
| Provider permission | The [October 19 eligibility report](2026-10-19-alpha-eligibility.md) records documentary/account observations and an unresolved unattended public-target question. No applicable provider answer for the selected workflow is recorded. | Applicable official documentation or written provider confirmation covering each unmodified pinned CLI, the owner's account, this public repository, unattended comment/automatic polling, initial review and peer critique. Scriptability and native Codex GitHub code review are separate mechanisms. |
| Account, auth and paid overflow | Existing-login inspection and synthetic auth/refresh/return tests establish limited facts. No worker-account entitlement or enforceable paid-overflow result is established. | Account-specific model/subscription entitlement, supported worker auth and refresh/expiry, separately owned serialized auth state, refusal of API-key/cross-provider fallback, and evidence that paid overflow is disabled or enforceably blocked. Preserve finite session, time, tool, byte, memory, retry and daily limits. Never publish tokens or auth files. |
| Aggregate containment and recovery | Source readers, native workers, egress/audit, auth transfer, Git acquisition ownership and startup components exist. PR #232 records 39 synthetic native phases with stopped-result faults, physical-stop/auth/runtime checks and request-bound recovery. The [hostile capability fixtures](../../reviewer/HOSTILE-FIXTURES.md) record 20 native forbidden-tool, source, code-mode and metadata cases. Codex model metadata restored direct tools in the client. The host refused their use, but live Codex use is blocked. Codex code mode awaits an owner decision. | Qualify the complete pinned acquisition/reader/provider/publisher assembly: exact protected identities/descriptors, immutable executable and source binding, bounded storage with required mount flags, hostile filesystem/network/escape cases, resource exhaustion, whole-worker stop, process/host crash and unknown-attempt recovery. Synthetic transport success does not prove this aggregate. |
| Benchmark, trusted release and pilot | The frozen corpus has 20 selected historical cases: 14 clean, 6 defect, none unlabeled, hash `807196990ee0adda9c8a2f87b0c9b6a5819baf886fa4196b58f6b102d7ea4910`. Source/scoring/control tooling exists; PR #232's native fixture runs one synthetic case and leaves 19 explicitly missing. | Independently approve a qualified trusted release and frozen evaluation plan; obtain actual six-arm outputs and independent finding assessments, quality/error/usage/latency denominators and uncertainty. Preserve natural 4+2+1+1+4+4 session counts and the owner's dual default. Poor/inconclusive results stop rollout. After separate owner authorization, run the bounded two-week pilot. Historical model exposure is unknown; no model-unseen or actual quality result is claimed. |
| Final App, triggers and rendering | The [October 21 model-free report](2026-10-21-model-free-app-eligibility.md) qualified new App/installation scope and canned COMMENT authorship; native request creation failed and was superseded. Comment/automatic discovery and publication races have offline tests. | Under separately applicable authority, revalidate selected App/installation/permissions, qualify the chosen triggers and attribution, and capture actual GitHub clean/defect, dispute/late, incomplete/quota/failure, cancelled and superseded states. Include unknown-POST reconciliation and old-A/new-B behavior. Rendered screenshots/recording must name their source/fixture provenance. |
| Explicit owner activation | Scoped offline implementation, tests, documentation and guarded merges are authorized. The bounded model-free App experiment completed. Trigger selection and agent labeling changed only their stated policies. | A separate owner decision permitting the exact provider credential use, public-target inference and live activation after the preceding evidence passes. Name the release/package, repository/installation, account/model/auth tuples, budgets, triggers, duration and verified stop/rollback procedure. Paid use, wider scope, retired automation and host-policy changes require their own authority. |

### Known host failures remain failures

The [Git launch constraints](../../reviewer/GIT-LAUNCH-CONSTRAINTS.md) retain
two distinct prerequisites that failed on mini: creation of the exact bounded
`noswap` work mount, and sealed anonymous executable launch under mandatory
confinement. The inert comparison recorded ordinary-file success but `EACCES`
for sealed proc/direct `execveat`; fixed anonymous-inode naming failed before
payload execution. A successful collector exit is not a successful exec.

Do not replace the sealed executable with its ordinary control, substitute an
unbounded/ordinary mount, detach cancellation or change privileges/policy to turn
these results green. A suitable environment or reviewed design revision needs
its own measured evidence and applicable authority. Real credential-bearing
qualification still waits for its permission and owner gates.

### Source review and release approval are separate

PR #232 passed current-head review, thread resolution, validation and guarded
merge. Future source increments must pass the same gates. A merge never promotes
a build.
The release registry requires an independent operator decision and verifies
claims through an explicit trusted authorizer; its stored fields are not proof
of provider permission or human approval by themselves. See
[release promotion and rollback](../../reviewer/RELEASE-PROMOTION.md).

Before the alpha reviews its own later changes, independently establish manual
bootstrap review/approval and that the pinned running artifact predates each
candidate PR. Candidate code, prompts, runtime and auth changes remain inert
review input. Normal connected-Codex review of these implementation PRs is not
an alpha self-review experiment or benchmark result.

## Updated provider questions, prepared only

The October 19 report retains its original native-request question as historical
evidence. The following drafts reflect the replacement triggers. Neither has
been sent; preparing them grants no authority to contact a provider or accept
terms on Franz's behalf.

**OpenAI.** May the owner use the pinned unmodified Codex CLI with their own
ChatGPT subscription login on their own isolated private host, unattended, to
review PRs in their own public `hemsoft-dev/set-it-free-loop` repository? Work
would start from an owner-authorized `@sfl review` comment or the owner's enrolled
policy for PR-open/head-push polling, and would include initial review, peer
critique and bounded offline-comparison runs. Which supported auth, renewal,
model entitlement, individual-use and paid-credit controls apply to this exact
workflow? Does the public-target limitation recorded in the October 19 report
permit this private-host service? No API-billing fallback, account sharing,
repository visibility change or native GitHub-review API equivalence is assumed.

**Anthropic.** May the owner use the pinned unmodified Claude Code CLI with their
own individual Claude subscription on that isolated host for the same unattended
public-repository comment/automatic polling workflow, including initial review,
peer critique and bounded comparison? Which supported subscription credential,
renewal, model entitlement and individual-use conditions apply, and what
account-specific evidence or enforceable control prevents extra paid usage?
No API-billing fallback or shared credential is assumed.

Retain any answer's date, provider identity, exact account/workflow scope and
supporting link or sanitized written evidence. A successful model request is not
permission evidence. Freeze a real evaluation plan and publish executable live
run instructions only after their applicable gates and owner authority pass.

## Evidence boundaries and next steps

The [model-free App report](2026-10-21-model-free-app-eligibility.md) records
App **5406096**, Bot **345646943**, selected installation **173628126** and exactly
repository **1169772257**, with metadata/contents read and pull_requests write.
Its eight uploaded screenshots/API receipts and closed-unmerged
[PR #221](https://github.com/hemsoft-dev/set-it-free-loop/pull/221) are historical
model-free evidence. The automated-Chromium caveat remains. Existing-reviewer
display does not prove requestability, and no new native-picker test is required
by the chosen trigger design.

Keep provider/account, physical host, actual benchmark, final GitHub and owner
decisions as distinct missing evidence. Complete authorized source work while
they remain open. Resolve each with an explicit pass/fail/blocked receipt rather
than closing its issue from a fake result or silently changing the requested
product. No provider contact, credential recreation, inference, deployment,
App modification, paid use or activation occurred in preparing this checklist.
