# Franz's personal Slack assistant on Home

This is the isolated implementation for [issue #308](https://github.com/hemsoft-dev/set-it-free-loop/issues/308).
It is prepared for review. It has not been activated or proved with a live DM.
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
file. The user token is used only to identify Franz; message/file API operations
use the bot token. Webhook, signing secret, client secret and Mini credentials
are not copied or used. `doctor` prints verified identities, never tokens.
Only files voluntarily submitted in the verified DM are downloaded.

The bot needs its existing `im:history`, `im:read`, `files:read`, `reactions:write`,
`chat:write` and `users:read` scopes. These were verified through the granted-scope
header. The existing app token obtained a Socket Mode URL, but no receiver was
connected during preparation. Activation must observe a matching Slack `hello`
with exactly one consumer and an actual `message.im` delivery. This code does
not enable Socket Mode, edit subscriptions/scopes, reinstall the app or log in.
If those existing settings do not deliver a DM, retain the exact gap for Franz.

## Durable behavior

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
Definite pre-send refusal retains that packet. An ambiguous result is **never
automatically replayed**, including after a T3 restart. Reconciliation requires
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
35000 characters; a final summary closes the outcome. Holds and overdue outcomes
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
each authenticated prompt. These deployment/live steps have not yet been run.

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
