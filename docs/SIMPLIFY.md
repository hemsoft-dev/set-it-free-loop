# SFL v0: ask our agent to review

Franz corrected the plan on October 28, 2026 at 00:47 EDT: "I thought the
whole point was for us to be doing the review in an agent and not use Codex."
This supersedes the native-Codex product framing merged in
[#284](https://github.com/hemsoft-dev/set-it-free-loop/pull/284).
Native `@codex review` remains a delivery check on our implementation PRs.

## Explicit owner authorization

Franz must explicitly authorize live deployment and pilot activation, including
setting the poller's `enabled` flag to `true`, before those steps happen. A clean
review, merged plan, merged runner or this runbook alone grants no activation
permission. Record the owner go and its scope before acting.

The Chief recorded Franz's authorization for this v0 on October 28, 2026:

- At 00:40 EDT, Franz approved the plan and authorized this v0 pilot.
- At 00:47 EDT, Franz corrected the reviewer choice to our T3 agent.
- v0 was enabled under that authority for only `hemsoft-dev/set-it-free-loop`,
  owner-comment triggers, one session at a time and three attempts per Eastern
  day, using the existing SFL App and Chief subscriptions.
- The [live request](https://github.com/hemsoft-dev/set-it-free-loop/pull/286#issuecomment-6461413661)
  was posted at 01:26:09 EDT. Exactly one
  [SFL App review](https://github.com/hemsoft-dev/set-it-free-loop/pull/286#pullrequestreview-5678160241)
  posted at 01:27:47 EDT, 98 seconds later, for head
  `10f24a8367c9586996fc40fed46c96f3026f7179`. Its review thread was archived.

This is a narrow owner-authorized exception for the pmd v0 route. The retired
systems and parked alpha remain frozen.

Each further live activation or scope change needs a new explicit owner go.
Before enabling the poller, widening the repository allowlist, raising the daily
cap or adding an automatic PR-open/head-push trigger, obtain and record Franz's
authorization for that specific step. The current pilot's approval does not
cover more repositories, a higher cap, auto-triggers or other scope expansion.

## The whole loop

1. HemSoft posts exactly `@sfl review` in an open PR's Conversation tab.
2. A light poll in project-manager checks allowed open PRs every 60 seconds,
   independently of its 30-minute reconciliation sweep. It uses ETags and a
   durable comment cursor. No GitHub App event checkbox is needed.
3. One T3 review session on mini examines the exact base and head commits.
   Chief's `scripts/pick-provider.py --priority P1 --json` selects the provider.
   The prompt permits only reading GitHub commit data and reporting findings.
4. The existing `hemsoft-sfl-reviewer` App, installation `173628126`, posts one
   advisory PR review with event `COMMENT` and the reviewed head SHA. Humans
   decide what to fix and merge. A newer head requires a new request.

The implementation lives in project-manager, including its small Python runner.
The external MCP credential uses T3's launch path because `delegate_task`
requires an active parent provider session. A later tick reads the launched
session's result. There is no second review model or custom job database.

## Pilot controls

- Allow only `hemsoft-dev/set-it-free-loop`, repository ID `1169772257`.
  Adding a repository requires a new explicit owner go before activation.
  Require both the PR base and head repository IDs to match. No fork heads.
- Pin requester `HemSoft`, numeric user ID `8227352`, type `User`. An org
  membership or `OWNER` association alone is insufficient. Review bodies and
  inline comments do not trigger runs. Edited old comments do not become new
  requests. On first enable, establish a creation-time baseline.
- Permit one review in flight globally, which also enforces one per PR.
  Default maximum three launch attempts per Eastern calendar day. Raising this
  cap requires a new explicit owner go. Reserve the
  attempt in a locked, atomically replaced state file before calling T3.
  Capacity refusals consume no launch. Busy requests and duplicate requests
  for an already reviewed head require a fresh comment.
- Save the session/run IDs and survive pmd restarts. An uncertain launch stays
  blocked for operator reconciliation. Never automatically launch it again.
  Mark a publication attempt before POST, and reconcile an uncertain POST by
  looking for the request/head marker under the SFL App identity. Never repeat
  an uncertain POST automatically.
- Recheck the live PR and head before launch and publication. Require the
  session result's reviewed SHA and base SHA to match the reservation. If the
  head moved, retain the result locally and require a fresh request. The App
  review uses `commit_id` equal to the reserved SHA.
- Keep App and T3 credentials in the parent process, outside prompts, state
  and logs. Use only the existing subscriptions. No API-key fallback or paid
  overflow. Refuse expired T3 credentials before reserving an attempt.
- The review session must not edit, execute PR code, push, approve, merge,
  publish, or delegate. T3 enforces no read-only sandbox, so these are prompt
  instructions for this owner-only pilot. This is not hostile-code isolation.
- Set the runner's `enabled` flag to false to stop new launches and posts.
  Known active sessions are interrupted on the next tick. Keep uncertain state
  until reconciled. Removing future trigger comments stops future requests;
  it does not cancel an accepted request.

## Keep parked work parked

Preserve code and evidence. Do not delete modules or revive these issues.

| Issue | v0 disposition |
| --- | --- |
| [#172](https://github.com/hemsoft-dev/set-it-free-loop/issues/172), dual alpha | Superseded by this single-session pilot; no dual-model work. |
| [#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173), eligibility | The remaining native-account/provider-question slice is not needed for v0. Existing Chief T3 sessions and the existing App supply the path. The unattended CLI service remains parked. |
| [#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174), isolation | VMs, kernel pins, native hooks and hostile-fixture qualification remain parked. |
| [#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176), controller | SQLite jobs, auth leases, promotion and the alpha publisher remain parked. The tiny pmd runner does not activate them. |
| [#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177), evaluation | Comparative benchmarks remain parked. Judge usefulness on the live pilot. |
| [#264](https://github.com/hemsoft-dev/set-it-free-loop/issues/264), stop receipt | Remains parked with its worker. |

Keep `reviewer/`, retired `central-reviewer/`, `gh-sfl/` and deployment
automation frozen. This v0 supersedes the alpha README and activation checklist
only as the delivery plan. It grants no exception for the parked CLI service.

## Delivery evidence

Use small PRs for this correction and project-manager's runner. Each needs a
clean current-head delivery review via `@codex review`, required checks, and a
guarded merge. If Codex hangs about one hour, re-request once; after another
hour, use a Claude `audit-pr` fallback.

Before live deployment, enabling the poller or running the live acceptance test,
verify that Franz's recorded explicit go covers the repository, trigger mode and
daily cap. Without it, stop those live steps; offline implementation, reviews and
guarded merges may continue. This v0's go is recorded above. Back up pmd before
deploying with `bash deploy/install.sh`, verify `/healthz`, and fast-forward only
clean main checkouts on mini and home. Set `enabled` to `true` only within the
explicitly authorized scope.

After the authorization gate, open a tiny real PR here. Ask the Chief or Franz
to post `@sfl review`, or use the HemSoft account when explicitly authorized.
Record the request URL, exact head, T3 session, SFL App review URL and elapsed
time. Verify exactly one review, correct App identity and SHA. Repeat polling
must not duplicate it. The first posted review is the acceptance test.

Council: GPT 6.1 Sol active, Claude Opus 5.5 consulted; Muse Wonder Full 1.0
missing because its quota guard was unknown. Adopted six material fixes:
short restart-safe ticks, a local MCP client, credential-expiry checks,
exact-commit result validation, mention neutralization and new-comment rules.
Claude's delegate-status polling suggestion is implemented with T3's external
launch/read path because delegates require an active parent session. The
suggestion to publish after a head move was declined to preserve the fresh-head
request rule. No material objection to the overall direction.
