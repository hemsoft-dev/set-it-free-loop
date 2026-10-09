# Organization App reviewer pilot

The SFL GitHub App is installed once on `hemsoft-dev`. This Linux service receives
its signed webhooks and publishes the `SFL PR Reviewer` check as App 4448946.
Consumer repositories need no SFL Actions workflow or SFL model/App secret. Native
Codex remains the review engine, using its existing organization installation and
[automatic review configuration](https://learn.chatgpt.com/docs/third-party/github).
The service neither calls a model API nor stores a human GitHub credential.

The current pilot permits only `hemsoft-dev/hs-buddy`, repository ID 1229335234,
installation 169090497. Configuration cannot widen that scope. `codexbar` and
CodexBar iOS remain excluded. The check is advisory during qualification; the
existing CI, CodeQL, review-thread and branch protections remain intact. Old
SFL-only workflow gates are removed under Franz's rollback instruction. Making
this new App check required, supporting other repositories, and autonomous SFL
workflows are outside this pilot.

## Review contract

Only a new, same-repository PR opened after the service's first startup is enrolled.
Existing PRs belonging to other work are ignored. Forks are ignored. An observed
new head on an enrolled PR can begin another review, provided its signed event
matches the live head, base SHA and default branch.

The service verifies Codex bot ID 199175422, its login, App ID 1144995, slug and
owner. A legacy unedited clean comment must identify the current commit. A current
automatic summary must have one completed Code Review row naming that commit,
a completion time after the observed context, a fresh native thumbs-up, and no
native eyes reaction or current-head findings. GitHub GraphQL verifies the summary
editor as the immutable Codex bot. Human-edited summaries never establish success.

A head associated with another PR is rejected, including a closed PR. A changed
base, retarget, close/reopen, draft conversion or observed default-branch push
invalidates the current head permanently. Push a substantive new commit before
seeking a fresh result. Returning to an earlier head cannot restore its review.
This conservative rule avoids claiming a native result proves a base when the
provider identifies only a reviewed head. Titles and unrelated user comments do
not initiate reviews. `@codex review` continues to belong to native Codex; the old
`gh sfl review` workflow registration protocol is retired for this deployment.

Before and after publication, the service checks the live PR context and queued
context changes. It periodically reconciles enrolled PRs because GitHub has no
reaction-created webhook. It never uses a thumbs-up alone as review proof. A lost
synchronize event cannot turn a new head green; its signed delivery is required.

## Setup on mini

1. Build and validate the reviewed source:

   ```sh
   cd central-reviewer
   gofmt -l .
   go vet ./...
   go test -race -count=1 ./...
   go build -trimpath -o /home/franz/.local/bin/sfl-reviewer .
   ```

2. Keep configuration and credentials under `/home/franz/.config/sfl-reviewer`,
   directory mode 0700 and file mode 0600. Start from `config.example.json`.
   A separate App key authenticates this service; existing keys and consumers
   remain unchanged. Generate a random webhook secret of at least 32 bytes.
   Neither credential belongs in Git, a PR, command arguments or logs.

3. Install the user service from `sfl-reviewer.service`. Start with
   `enabled=false`. State is under `/home/franz/.local/state/sfl-reviewer`.
   The endpoint binds only `127.0.0.1:3791`.

4. Expose only that port through a dedicated Tailscale Funnel port. Preserve the
   existing mini serve/funnel configuration. Configure the
   [SFL App webhook](https://github.com/organizations/hemsoft-dev/settings/apps/sfl-app)
   to use the HTTPS `/webhook` endpoint and the locally stored secret. Subscribe
   to `pull_request`, `pull_request_review`, `issue_comment` and `push`, preserving
   App permissions. Validate the actual GitHub ping delivery.

5. Merge the reviewed hs-buddy deployment removal, inspect its default-branch
   workflow tree, and verify its existing Codex automatic-review setting.
   Set `enabled=true` only for the qualification window. Open a substantive new
   pilot PR after setup, retaining its immutable head/base and delivery IDs.
   Prove an authentic clean result and publisher ID 4448946. Exercise a real
   context change. Synthetic negative controls are separate from live receipts.
   Pause again after qualification. Organization `SFL_ENABLED=false` stays in
   place to protect the legacy workflows.

## Operation and recovery

`GET /healthz` exposes only enablement and queue/dead-letter counts. A dead letter
returns HTTP 503. The configuration enable flag is reread before processing and
publication; invalid or broadened configuration pauses processing. Paused webhook
receipts are acknowledged without processing, so qualification starts with a new
PR after enablement.

Verified deliveries are durably acknowledged after an atomic mode-0600 state
write, file fsync, rename and directory fsync. One Linux process holds the state
lock. Startup reloads queued work. GitHub checks are recovered by App and external
identity after a lost creation response. Duplicate delivery IDs are retained for
the latest 10,000 completed deliveries; checks remain idempotent after that window.
At most 1,000 jobs can queue. Each failed job gets five attempts with exponential
backoff, then remains visible as a dead letter. Pending context jobs prevent success.

An API verification error attempts to withdraw any prior green check before durable
retry. If GitHub also rejects the withdrawal, recovery remains visible in the queue
and health response; a write cannot be guaranteed while GitHub is unavailable.

A public webhook accepts at most 1 MiB. Bad signatures never enqueue work.
Installation tokens are short-lived, stay in memory, and are restricted to the
single pilot repository with Checks write and Contents, Issues, Pull requests read.
The service does not execute PR code. GitHub response bodies and credential values
are excluded from error logs.

To pause, set `enabled=false` in the local configuration, then inspect `/healthz`.
To stop transport, stop only `sfl-reviewer.service` and its dedicated Funnel port;
do not reset the machine's complete Tailscale configuration. Back up state before
recovering a failed delivery. Investigate its recorded HTTP/operation failure,
restore the missing access or resource, then reset only that job's attempt/dead
fields with the service stopped. Keep original failure receipts. Do not remove
state, reset head history or edit a GitHub check to manufacture a clean result.

The central implementation and consumer rollback are tracked in
[issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139). Earlier
per-repository pilot reports document the retired architecture and do not prove
this service's live qualification.
