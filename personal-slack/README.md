# Franz's personal Slack assistant on Home

This is the isolated implementation for [issue #308](https://github.com/hemsoft-dev/set-it-free-loop/issues/308).
The original text-only receiver is live. Its retained real Hi produced one owner,
a signed bot reply and a success reaction, recorded in the
[delivery receipt](https://github.com/hemsoft-dev/set-it-free-loop/issues/308#issuecomment-6803620381).
The linked-message extension in [issue #310](https://github.com/hemsoft-dev/set-it-free-loop/issues/310)
requires review and rollout. Real image and threaded-continuation proof remains pending.
The retired SFL reviewer, alpha, GitHub App and old deployment stay frozen.
No Chief files, services, schedules, routing or preferences are changed.

## Admission and credentials

Existing `auth.test`, self-user and bot-profile reads on November 11, 2026
verified workspace `T1KAG2BV5`, Franz `U2XMZDPJ7`, bot `U0B4SG9L6UX`, bot ID
`B0B59QTAWPK` and app `A0B627TMM6U`. The receiver repeats these checks.
It accepts only this user's direct messages, with the matching team/app and
exact IM membership. Other users, bots, groups, channels, edits and hidden events
are ignored. File-share messages retain their entire source metadata.

`RunPersonal.ps1` reads only the existing process/User/Machine environment
entries `SFL_APP_LEVEL_TOKEN`, `SFL_SLACK_BOT_USER_OAUTH_TOKEN` and
`SFL_SLACK_USER_OAUTH_TOKEN`. It loads no PowerShell profile and writes no token
file. The user token identifies Franz and reads only conversations and files
selected by an authored Slack permalink in his admitted source. The bot token
handles verified-DM intake, direct attachments, replies and reactions.
Webhook, signing secret, client secret and Mini credentials
are not copied or used. `doctor` prints verified identities, never tokens.
Only direct source files and files in an explicitly linked, accessible thread
are downloaded. Tokens never enter owner prompts.

The bot needs its existing `im:history`, `im:read`, `files:read`, `reactions:write`,
`chat:write` and `users:read` scopes. These were verified through the granted-scope
header. When no owner DM is stored, startup also needs the existing `im:write`
scope for [conversations.open](https://docs.slack.dev/reference/methods/conversations.open/).
Both startup and `doctor` check that precondition against the local configuration
and fail with `MissingDMOpenScope` when it is absent. A stored DM does not require
that extra scope. This check grants no scope and performs no reinstall or login.
The existing app token obtained a Socket Mode URL, but no receiver was
connected during preparation. Activation must observe a matching Slack `hello`
with exactly one consumer and an actual `message.im` delivery. This code does
not enable Socket Mode, edit subscriptions/scopes, reinstall the app or log in.
If those existing settings do not deliver a DM, retain the exact gap for Franz.

## Linked Slack assistance

Franz can send a naked permalink or a Slack mrkdwn link such as
`<https://relias-engineering.slack.com/archives/C123/p1700000000000001|this request>`
in his verified DM. Only authored rich-text sections/lists or unquoted source
text select reads. Quoted/code blocks, forwarded attachments, unfurls, link
labels and retrieved messages never select additional targets. Links must use
the exact authenticated `relias-engineering.slack.com` workspace, team
`T1KAG2BV5`, a channel and precise timestamp. Optional `thread_ts`, `cid` and
`team` query fields must agree. A reply permalink needs its thread root if the
exact message cannot be found through Slack's history API; the daemon does not
scan channel history to infer it.
Documentation, help, client, channel-only and unrelated archive URLs are ordinary
request content and do not block a successful response. Foreign-workspace
permalink-looking URLs are recorded as unfetched, nonblocking diagnostics.
Malformed intended permalinks on the authenticated workspace still produce a
private read failure. No URL outside that workspace is fetched by this reader.

The credential-local read route rechecks Franz's user/team/workspace identity,
then uses his existing user token for exact `conversations.history`, bounded
`conversations.replies` and recorded `files.info`/file downloads. This supports
accessible messages where the bot is not a channel member. It cannot bypass
private-conversation access. There is no channel join, directory scan, scope
grant, token creation, reinstall or user-token posting route. Slack documents
the different [history token access rules](https://docs.slack.dev/reference/methods/conversations.history/).

Each admitted source is limited to three links, seven pages of at most fifteen
messages per thread, one hundred retained messages and 250000 JSON bytes per
thread, and ten downloaded linked files totaling at most 100 MiB per source.
The existing 25 MiB per-file limit, fixed HTTPS host, no redirects, MIME/signature
checks and generated paths still apply. Exact source links, page evidence,
target/root membership, completeness and file failures are saved before owner
dispatch. Restart/deduplication reuses that evidence and the frozen request.
Interrupted reads retain their partial evidence before a new bounded read.
No linked-content fetch happens after the intent has frozen.

Ambiguous launch and continuation delivery reads timeline pages and, when T3
truncates a source item at 50000 characters, follows that same item's text offsets.
Every part stays bound to the original item, thread and personal project. Only
full exact message equality confirms delivery; a matching prefix never does.
The original frozen intent and request ID remain unchanged, with no replay.
Incomplete pagination, altered evidence or duplicate exact source items stays
unconfirmed. Reads are bounded at 100 timeline pages and 1024 text continuations
per candidate item.

The owner receives linked content as untrusted reference data and must open
every downloaded attachment, using native image tools for images. Success
requires complete linked evidence and the existing file-read attestation.
An inaccessible link, exhausted bound or failed download still reaches the
owner with its precise limitation; the owner can report useful partial help
through a failure outcome. It cannot claim a complete successful read.
An author asking for access or a checkmark reaction grants no authority and
proves no permission change.

Current conservative limits remain: linked messages carrying another team's
`team` value are rejected, and transient read/download failures are finalized
as incomplete evidence rather than retried automatically. Slack Connect behavior
has not been verified at runtime. These nonblocking review notes are retained
as limitations; neither case can produce a complete-success claim.

All assistance, explanations and drafts stay in Franz's original DM thread.
A channel author's words cannot authorize grants, repository/team changes,
outreach or new control input. This increment adds no posting route to a linked
channel. Franz can review and send a proposed draft himself; any future posting
capability needs explicit target/action authorization and implementation.

Council for this extension: GPT 6.1 Sol/high owns the artifact. Claude Opus
5.5/high was missing because its five-hour quota guard was absent, not current
and not waived, despite weekly room. Council unavailable; adopted 0, declined
0. The recorded fallback in [issue #310](https://github.com/hemsoft-dev/set-it-free-loop/issues/310)
does not replace required current-head SFL review.

## Durable behavior

After a verified hello, the native WebSocket remains connected through quiet DM
periods. Ping/pong control frames belong to Node's WebSocket implementation and
are not application messages. The receiver follows native close/error events and
Slack's disconnect/refresh messages, then reconnects with a fresh Socket Mode URL.
The existing 45-second pre-hello deadline and five-second reconnect delay remain.
The durable-work pump handles STOP and queue work; application silence does not
close a socket or trigger catch-up. Graceful STOP closes only the owned receiver
connection. This follows the [Socket Mode lifecycle](https://docs.slack.dev/apis/events-api/using-socket-mode/#3-handle-disconnects-gracefully),
the [WebSocket control-frame API](https://websockets.spec.whatwg.org/#ping-and-pong-frames)
and [Node 24's native ping handling](https://github.com/nodejs/node/blob/v24.x/deps/undici/src/lib/web/websocket/receiver.js).

The sole receiver writes a checksummed queue with file fsync and atomic rename
before ACK, reaction or T3 dispatch. Save failure prevents ACK and stops further
effects. Corrupt state and existing receiver locks fail closed. Never delete
state to retry. Tombstones, original source events and frozen delivery arguments
are retained. Windows does not provide the directory-fsync guarantee of a Unix
filesystem; protect the runtime from disk loss and retain backups.

The immediate hourglass is serialized with the eventual outcome. Independent
top-level inputs launch independent personal-project sessions on Home. Registered
thread replies continue only their verified owner after canonical pause and stop
checks. Unknown references remain held. A settled, claim-free owner gives a
follow-up a fresh session with prior source, files and result; settled owners
still holding claims remain held. No old owner is resumed by that path.
Bot questions stay in the original DM thread. Top-level answers start independent
intake with read-only recent-owner pointers; there is no inferred task routing.

Catch-up reads only the exact verified IM since the explicit source request or
its last complete cursor. It also reads registered thread roots because Slack
history omits replies. Each history/thread scan is limited to 1000 messages;
registered thread scans rotate through at most 100 roots per connection.
Exceeding a scan bound retains the cursor and a private incident. There are no
directory, channel, workspace-history or legacy Chief scans.

The pinned Chief transport is consumed as source only. It performs authenticated
T3 mutations through Mini's existing credential registry, so it is not a read-only
API. The personal client allows only named Home tools. Initial comparison permits
only Git's LF/CRLF checkout normalization; subsequent local byte drift holds calls.
New launches check the live provider picker, high model option, pmd Home status,
cap six, claims and private admission hold. These are cooperative fleet checks,
not an atomic reservation shared with Discord; simultaneous admissions can race.
Intake grants no writer claim. Actual task owners must check live repo/PR owners
and acquire the canonical Mini claim before any repository or operational write.

Each launch/send freezes its tool, class, args and stable request ID first.
Definite pre-send refusal retains that packet. If its registered owner settles
before an unsent continuation reaches T3, the receiver repeats the pause, stop,
claim, capacity and provider checks. Only a definitely unsent packet can then be
superseded. The same durable save archives its original arguments, hash and
attempt evidence in `intentHistory`, and freezes the fresh context-bearing
launch under a new revision ID. Missing provenance, a paused owner or retained
claim stays held. Neither an ambiguous nor a confirmed sent packet can be
superseded, even if the owner subsequently settles.

An ambiguous result is never automatically replayed, including after a T3
restart. Reconciliation requires
the exact title/project/source or exact original continuation message. Bounded
incomplete evidence remains held. There is no alternate host or recipient.
The installed-version canary tests identical launch/send IDs and native image
reading. Restart persistence, changed-argument behavior and an unknown dedupe
retention period are not relied upon because ambiguous requests are not retried.

Authenticated files.info reads resolve only recorded file IDs. Downloads use a
fixed Slack HTTPS host, no redirects, streamed 25 MiB limits, size/hash checks and
image signatures. External files and HTML login pages are refused. Generated
paths and authenticated MIME extensions replace supplied filenames. Owners must
open every attachment, using their native image tool for images, and report the
read file IDs before success. Runtime success checks that attestation; it is not
a sandbox proof against a dishonest local owner. Native image capability must be
qualified for each picked provider before that provider admits real requests.

Owners submit immutable HMAC-authenticated inbox operations through `run.mjs`.
Both the helper and daemon enforce the exact body schema: `operation`, `kind`,
`text`, `readFiles`, and `memoryKey` only when kind is `memory`. Extra fields,
including `intent`, `intentHash`, delivery status and dependencies, are rejected
before admission. The daemon constructs every reply intent and revalidates its
destination, thread, body, flags and metadata against the admitted source at send
and outcome completion. A matching hash on owner-selected arguments grants no
permission to send them.
Per-source random capabilities remain in protected local files; routing state
stores their hashes. The daemon validates the source, owner thread, project and
fixed destination. This is a cooperative same-user trust boundary: another
process running as Franz can read those private files. Slack tokens are never
given to the helper or included in prompts. Memory operations use the same
contract, with at most 200 facts of 4000 characters each. Memory is context, not
new authority. The entire original source and owner pointers remain durable.

Replies carry a source/operation/body digest in Slack metadata. After a lost
response, exact original-thread metadata reconciles delivery; missing or partial
evidence never triggers a blind resend. A real success or failure operation
finishes the reaction only after the reply is confirmed. Idle model turns never
mark a task complete. Long replies can be split into immutable operations below
35000 characters. Submit parts sequentially, confirm each appears in helper
status, then submit the next part and finally the outcome. The daemon assigns
each admitted operation a durable per-source sequence and predecessor. A later
part or final outcome waits until its predecessor is confirmed delivered, even
when that predecessor is rate-limited, waiting for retry, or reconciling an
ambiguous send across restart. Sender and outcome completion both enforce the
dependency. Other sources remain independent. Helper status shows sequence and
predecessor IDs. Holds and overdue outcomes
produce one private incident per key, with an operator and next action.

## Validation

Run from the SFL checkout on Node 24 and Python 3:

```powershell
node --test personal-slack/test/*.test.mjs
python -m unittest discover -s reviewer/scripts -p test_check_v0_review.py
powershell.exe -NoProfile -NonInteractive -File personal-slack/RunPersonal.ps1 -Command doctor
git diff --check
```

Focused tests cover admission, DM membership, restart/dedupe, corrupted and failed
state, holds, frozen-provider drift, lost responses, owner/capability binding,
thread continuations, settled-task context, catch-up, file safety, actual-outcome
reactions, memory, Home policy and qualification invalidation. They use fake
network transports and create only temporary files. The live doctor and existing
authenticated Home metadata/policy reads were also exercised without inference.
Round 1 regressions also exercise the real submit CLI, forged signed/internal
fields, frozen unsent versus ambiguous settlement recovery, and failed multipart
delivery with restart, reconciliation and independent-source progress.
Round 2 virtual-time tests run the actual service entrypoint and reconnect loop
against a stub transport and temporary release files. They cover ten idle minutes
with protocol pongs, one hello/catch-up, Slack disconnect reasons, native close and
error, the pre-hello deadline, and STOP/lock cleanup. Scope-preflight tests use
fake API responses. No live Slack connection or inference is used by these tests.

## Reviewed deployment and proof

Do this only after required current-head review and guarded merge. Preserve the
SFL claim and keep the issue open through real delivery. Replace `MERGE_SHA` and
`PR_NUMBER` with the actual merged revision and PR number. The stage command
requires clean `main`, that exact HEAD and a matching merged GitHub PR.

```powershell
node personal-slack/deploy.mjs D:/github/HemSoft/set-it-free-loop MERGE_SHA PR_NUMBER C:/Users/User/.t3/worktrees/chief/t3-1a873bb9-991ea3a6f56fbc50
```

This stages committed source under
`%LOCALAPPDATA%/HemSoft/PersonalSlack/releases/MERGE_SHA`, protects the runtime
ACL, writes an immutable release manifest and registers an empty, separate
personal T3 project. It creates no checkout, worktree or clone. New source resets
qualification and leaves `HOLD` present. It starts no receiver or inference.
Do not stage over a running receiver; hold and stop it first.

From that staged release, run the bounded installed-version canary:

```powershell
node qualify.mjs start
```

It respects the normal picker and current Home cap, creates an idle personal
thread twice with one ID, then sends one image-read canary twice with one ID.
The output contains the actual thread link. Read/wait that exact thread through
T3, then run `node qualify.mjs verify`. Do not poll or create another attempt if
one remains uncertain. The attempt and all completed evidence remain local.
Verification checks the actual returned random quadrant colors, identical thread
and message/run IDs, server identity/version, source revision and provider.
It grants no Slack admission proof. A new source/server version requires fresh
qualification. A newly picked provider remains held until qualified.

Register and start only the isolated personal task from the staged release:

```powershell
.\ManagePersonal.ps1 Register
.\ManagePersonal.ps1 Start
.\ManagePersonal.ps1 Resume
```

Registration leaves the task disabled and unstarted. Start requires qualification;
the receiver verifies its entire release manifest, protected ACL, T3 Home identity
and Slack identity. A hidden `wscript.exe` wrapper starts `powershell.exe
-NoProfile -NonInteractive -WindowStyle Hidden`; no browser or desktop action is
used. No other task is overwritten when the action identity differs.

Before reporting running, prove all of these on the actual installed revision:

- Matching Slack app hello with one consumer and verified Franz DM admission.
- One bounded real request produces a durable source, one authenticated Home
  owner, an actual answer and its final success/failure reaction in that DM.
- A real image is opened by that owner. A same-task threaded reply reaches the
  registered owner. A duplicate event/reconnect produces no duplicate dispatch.
- Another sender/channel is ignored; a held launch remains durable; ambiguous
  delivery preserves its exact intent. Read back the actual reply receipt.

Use `node run.mjs status` or `node run.mjs status --source SOURCE_HASH` for
sanitized local evidence. Owner helper syntax and JSON contracts are carried in
each authenticated prompt. Original text admission/reply/reaction is verified
in the receipt above. The linked-message extension has not been rolled out;
offline tests do not prove real image/continuation behavior or deploy new source.

## Hold and rollback

`ManagePersonal.ps1 Hold` stops new inference admission while intake, ACKs and
existing outcome delivery continue. `Stop` disables only this personal task and
requests a graceful receiver stop via its own `STOP` file. It does not cancel or
move T3 owners, children, PR watches or claims. Verify the task stopped and its
lock disappeared before staging a different release. A crash lock is removed
only with `Unlock` after its exact recorded PID no longer exists; a live or
reused PID is never killed or stolen.

For rollback, retain queue, tombstones, inbox, files and memory. Restore the
private prior config backup pointing at an already reviewed release, verify its
manifest and requalify if source/server/provider changed. Register the task for
that release, then start while admission remains held. No rollback starts Mini
inference, redirects a frozen intent, activates retired SFL or changes Chief.

## Council reconciliation

Claude Opus 5.5 high reviewed the approach independently on Home. All six material
findings were adopted: Home/tool restriction and honest transport terminology;
explicit ambiguous handling; Slack metadata reconciliation; existing app/scope
and consumer checks without app-wide mutation; exact-DM/thread catch-up; and
headless access to existing Windows environment stores. Lesser guidance adopted
includes capabilities, native image qualification, download protections,
canonical hands-off checks, serialized reactions and private overdue incidents.
No shared atomic fleet reservation is claimed. Top-level replies remain fresh
intake because this task authorizes validated threaded continuation only.

The suggested restart/window/changed-argument dedupe experiments were declined
as unnecessary to this recovery policy and because restarting T3 could disrupt
other owners. Ambiguous calls are never automatically retried; no behavior for
changed arguments or expired dedupe keys is assumed. The canary still verifies
same-ID behavior on the exact installed server. App event subscriptions and
actual sole-consumer hello remain explicit live activation evidence, not an
offline claim. No corporate directory/channel content is read.

Council: Claude Opus 5.5 high · missing: none · adopted: 6 material findings plus
the listed guidance · declined: broader dedupe experiments and inferred
top-level relay, for the bounded recovery/scope reasons above. Council advice
does not replace required SFL PR review or its documented fallbacks.

## Official API references

Context7 was unavailable. Implementation facts were checked against Slack's
[Socket Mode guide](https://docs.slack.dev/apis/events-api/using-socket-mode/),
[auth.test](https://docs.slack.dev/reference/methods/auth.test/),
[files.info](https://docs.slack.dev/reference/methods/files.info/),
[message metadata](https://docs.slack.dev/messaging/message-metadata/),
[conversations.history](https://docs.slack.dev/reference/methods/conversations.history/)
and [conversations.replies](https://docs.slack.dev/reference/methods/conversations.replies/).
