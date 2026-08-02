---
description: |
  Standalone full-spectrum pull request review triggered by the sfl-review
  label. Performs security, correctness and reliability, and quality and
  maintainability passes, posts one inline thread per finding, submits a
  consolidated review, and publishes the SFL Reviewer Approval check.

on:
  label_command:
    name: sfl-review
    events: [pull_request]
    remove_label: true

permissions:
  contents: read
  pull-requests: read

post-steps:
  - name: Require SFL review output
    if: always()
    shell: bash
    run: |
      node <<'NODE'
      const fs = require('fs');
      const path = '/tmp/gh-aw/agent_output.json';
      if (!fs.existsSync(path)) {
        throw new Error('SFL agent output file is missing');
      }
      const output = JSON.parse(fs.readFileSync(path, 'utf8'));
      const items = Array.isArray(output.items) ? output.items : [];
      const hasInventory = items.some(
        (item) => item.type === 'sfl_review_inventory'
      );
      const hasNoop = items.some((item) => item.type === 'noop');
      if (!hasInventory && !hasNoop) {
        throw new Error('SFL review emitted neither an inventory nor a noop');
      }
      NODE

engine:
  id: copilot
  env:
    COPILOT_PROVIDER_BASE_URL: https://openrouter.ai/api/v1
    COPILOT_PROVIDER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
    COPILOT_PROVIDER_TYPE: openai
    COPILOT_PROVIDER_WIRE_API: responses
    COPILOT_MODEL: moonshotai/kimi-k3

model: moonshotai/kimi-k3

models:
  default-ai-credits-pricing:
    input: 3
    output: 15

network:
  allowed:
    - openrouter.ai

tools:
  github:
    toolsets: [pull_requests, repos]
    github-app:
      client-id: ${{ vars.SFL_APP_CLIENT_ID }}
      private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}

safe-outputs:
  threat-detection:
    enabled: true
    max-ai-credits: -1
    post-steps:
      - name: Mint SFL validation token
        id: sfl-validation-token
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          app-id: ${{ vars.SFL_APP_ID }}
          private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
          permission-pull-requests: read
      - name: Validate SFL review verdict
        env:
          EXPECTED_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          EXPECTED_RUN_ID: ${{ github.run_id }}
          GH_TOKEN: ${{ steps.sfl-validation-token.outputs.token }}
          PR_NUMBER: ${{ github.event.pull_request.number }}
        shell: bash
        run: |
          # SFL_VERDICT_VALIDATOR_START
          node <<'NODE'
          const fs = require('fs');
          const { execFileSync } = require('child_process');

          const outputPath =
            process.env.SFL_AGENT_OUTPUT_PATH ||
            '/tmp/gh-aw/threat-detection/agent_output.json';
          const expectedHead = process.env.EXPECTED_HEAD_SHA || '';
          const expectedRunId = process.env.EXPECTED_RUN_ID || '';
          const fail = (message) => {
            console.error(`SFL verdict validation failed: ${message}`);
            process.exit(1);
          };

          if (!fs.existsSync(outputPath)) {
            fail(`agent output is missing: ${outputPath}`);
          }

          const output = JSON.parse(fs.readFileSync(outputPath, 'utf8'));
          const items = Array.isArray(output.items) ? output.items : [];
          const noops = items.filter((item) => item.type === 'noop');
          const triggeringRepo = String(process.env.GITHUB_REPOSITORY || '');
          const triggeringPr = Number(process.env.PR_NUMBER);
          const targetFields = [
            'pull_request_number',
            'pr_number',
            'pr',
            'pull_number',
          ];
          for (const item of items) {
            if (item.repo && item.repo !== triggeringRepo) {
              fail(`output target repo must be ${triggeringRepo}`);
            }
            for (const field of targetFields) {
              if (
                item[field] !== undefined &&
                Number(item[field]) !== triggeringPr
              ) {
                fail(`output target ${field} must be ${triggeringPr}`);
              }
            }
          }

          const loadPrState = () => {
            if (process.env.SFL_PR_STATE_PATH) {
              return JSON.parse(
                fs.readFileSync(process.env.SFL_PR_STATE_PATH, 'utf8')
              );
            }
            const [owner, repo] = String(
              process.env.GITHUB_REPOSITORY || ''
            ).split('/');
            const number = process.env.PR_NUMBER || '';
            if (!owner || !repo || !number || !process.env.GH_TOKEN) {
              fail('GitHub context for deterministic PR validation is incomplete');
            }
            const query = [
              'query($owner:String!,$repo:String!,$number:Int!,$after:String){',
              'repository(owner:$owner,name:$repo){',
              'pullRequest(number:$number){headRefOid reviewThreads(first:100,after:$after){',
              'nodes{isResolved comments(first:1){nodes{body author{login}}}} ',
              'pageInfo{hasNextPage endCursor}',
              '}}}}',
            ].join('');
            const nodes = [];
            let after = null;
            let headRefOid = '';
            try {
              do {
                const args = [
                  'api',
                  'graphql',
                  '-f',
                  `query=${query}`,
                  '-F',
                  `owner=${owner}`,
                  '-F',
                  `repo=${repo}`,
                  '-F',
                  `number=${number}`,
                ];
                if (after) {
                  args.push('-f', `after=${after}`);
                }
                const raw = execFileSync('gh', args, {
                  encoding: 'utf8',
                  env: process.env,
                  stdio: ['ignore', 'pipe', 'pipe'],
                });
                const response = JSON.parse(raw);
                if (
                  Array.isArray(response.errors) &&
                  response.errors.length > 0
                ) {
                  fail(
                    `GitHub GraphQL returned errors: ` +
                      response.errors
                        .map((error) => error.message || 'unknown error')
                        .join('; ')
                  );
                }
                const pullRequest = response?.data?.repository?.pullRequest;
                if (!pullRequest?.headRefOid) {
                  fail('GitHub returned no pull request state');
                }
                const reviewThreads = pullRequest.reviewThreads;
                if (
                  !Array.isArray(reviewThreads?.nodes) ||
                  typeof reviewThreads?.pageInfo?.hasNextPage !== 'boolean' ||
                  (reviewThreads.pageInfo.hasNextPage &&
                    !reviewThreads.pageInfo.endCursor)
                ) {
                  fail('GitHub returned incomplete review-thread data');
                }
                headRefOid = pullRequest.headRefOid;
                nodes.push(...reviewThreads.nodes);
                after = reviewThreads.pageInfo.hasNextPage
                  ? reviewThreads.pageInfo.endCursor
                  : null;
              } while (after);
              return { headRefOid, reviewThreads: { nodes } };
            } catch (error) {
              fail(`unable to query live pull request state: ${error.message}`);
            }
          };

          const prState = loadPrState();
          const liveHead = prState.headRefOid;
          if (liveHead !== expectedHead) {
            if (noops.length !== 1 || items.length !== 1) {
              fail('head drift requires exactly one noop output');
            }
            console.log('SFL verdict validation passed: verified head drift');
            process.exit(0);
          }
          if (noops.length > 0) {
            fail('noop is forbidden while the pull request head is unchanged');
          }

          const inventories = items.filter(
            (item) => item.type === 'sfl_review_inventory'
          );
          if (inventories.length !== 1) {
            fail('exactly one sfl_review_inventory output is required');
          }

          const inventory = inventories[0];
          const countFields = [
            'new_critical',
            'new_high',
            'new_medium',
            'new_low',
            'carried_critical',
            'carried_high',
            'carried_medium',
            'carried_low',
            'overflow',
          ];
          for (const field of countFields) {
            if (
              !Number.isInteger(inventory[field]) ||
              inventory[field] < 0
            ) {
              fail(`${field} must be a non-negative integer`);
            }
          }
          if (!expectedHead || inventory.head_sha !== expectedHead) {
            fail('inventory head_sha does not match the triggering PR head');
          }

          const severityOrder = ['critical', 'high', 'medium', 'low'];
          const prefixes = {
            critical: '**CRITICAL Finding**',
            high: '**HIGH Finding**',
            medium: '**MEDIUM Finding**',
            low: '**LOW Finding**',
          };
          const comments = items.filter(
            (item) => item.type === 'create_pull_request_review_comment'
          );
          const emitted = Object.fromEntries(
            severityOrder.map((severity) => [severity, 0])
          );
          for (const comment of comments) {
            const severity = severityOrder.find((candidate) =>
              String(comment.body || '').startsWith(prefixes[candidate])
            );
            if (!severity) {
              fail('every inline review comment must use an SFL severity prefix');
            }
            emitted[severity]++;
          }

          const newlyFound = Object.fromEntries(
            severityOrder.map((severity) => [
              severity,
              inventory[`new_${severity}`],
            ])
          );
          const carried = Object.fromEntries(
            severityOrder.map((severity) => [
              severity,
              inventory[`carried_${severity}`],
            ])
          );
          const actualCarried = Object.fromEntries(
            severityOrder.map((severity) => [severity, 0])
          );
          for (const thread of prState.reviewThreads?.nodes || []) {
            if (thread.isResolved) {
              continue;
            }
            const firstComment = thread.comments?.nodes?.[0];
            const author = String(firstComment?.author?.login || '');
            if (author !== 'sfl-app[bot]') {
              continue;
            }
            const severity = severityOrder.find((candidate) =>
              String(firstComment?.body || '').startsWith(prefixes[candidate])
            );
            if (severity) {
              actualCarried[severity]++;
            }
          }
          for (const severity of severityOrder) {
            if (carried[severity] !== actualCarried[severity]) {
              fail(
                `${severity} carried count must be ` +
                  `${actualCarried[severity]}, got ${carried[severity]}`
              );
            }
          }
          const newTotal = Object.values(newlyFound).reduce(
            (sum, count) => sum + count,
            0
          );
          const expectedOverflow = Math.max(0, newTotal - 20);
          if (inventory.overflow !== expectedOverflow) {
            fail(
              `overflow must be ${expectedOverflow}, got ${inventory.overflow}`
            );
          }

          let remaining = Math.min(newTotal, 20);
          const expectedEmitted = {};
          for (const severity of severityOrder) {
            expectedEmitted[severity] = Math.min(
              newlyFound[severity],
              remaining
            );
            remaining -= expectedEmitted[severity];
          }
          for (const severity of severityOrder) {
            if (emitted[severity] !== expectedEmitted[severity]) {
              fail(
                `${severity} emitted count must be ` +
                  `${expectedEmitted[severity]}, got ${emitted[severity]}`
              );
            }
          }

          const reviews = items.filter(
            (item) => item.type === 'submit_pull_request_review'
          );
          const checks = items.filter(
            (item) => item.type === 'create_check_run'
          );
          if (reviews.length !== 1 || checks.length !== 1) {
            fail('exactly one consolidated review and one check run are required');
          }

          const blockingFindings =
            newlyFound.critical +
              carried.critical +
              newlyFound.high +
              carried.high >
            0;
          const blocking = blockingFindings || inventory.overflow > 0;
          const expectedEvent = blocking ? 'REQUEST_CHANGES' : 'APPROVE';
          const expectedConclusion = blocking ? 'failure' : 'success';
          const expectedVerdict = blocking ? 'CHANGES_REQUESTED' : 'APPROVE';
          const actualEvent = String(reviews[0].event || '').toUpperCase();
          const actualConclusion = String(
            checks[0].conclusion || ''
          ).toLowerCase();

          if (actualEvent !== expectedEvent) {
            fail(
              `review event must be ${expectedEvent}, got ${actualEvent || 'empty'}`
            );
          }
          if (actualConclusion !== expectedConclusion) {
            fail(
              `check conclusion must be ${expectedConclusion}, got ` +
                `${actualConclusion || 'empty'}`
            );
          }

          const totals = Object.fromEntries(
            severityOrder.map((severity) => [
              severity,
              newlyFound[severity] + carried[severity],
            ])
          );
          const reviewBody = String(reviews[0].body || '');
          const reviewLines = new Set(
            reviewBody.split(/\r?\n/).map((line) => line.trim())
          );
          const requiredReviewFragments = [
            `SFL run ID: ${expectedRunId}`,
            `Head SHA: ${expectedHead}`,
            `Verdict: ${expectedVerdict}`,
            `| Critical | ${totals.critical} |`,
            `| High | ${totals.high} |`,
            `| Medium | ${totals.medium} |`,
            `| Low | ${totals.low} |`,
            `| Overflow | ${inventory.overflow} |`,
          ];
          if (!expectedRunId) {
            fail('workflow run ID is unavailable');
          }
          for (const fragment of requiredReviewFragments) {
            if (!reviewLines.has(fragment)) {
              fail(`consolidated review is missing: ${fragment}`);
            }
          }

          const checkSummary = String(checks[0].summary || '');
          const checkLines = new Set(
            checkSummary.split(/\r?\n/).map((line) => line.trim())
          );
          const requiredCheckFragments = [
            `Verdict: ${expectedVerdict}`,
            `Head SHA: ${expectedHead}`,
            `SFL run ID: ${expectedRunId}`,
            `Critical: ${totals.critical}`,
            `High: ${totals.high}`,
            `Medium: ${totals.medium}`,
            `Low: ${totals.low}`,
            `Overflow: ${inventory.overflow}`,
          ];
          for (const fragment of requiredCheckFragments) {
            if (!checkLines.has(fragment)) {
              fail(`check summary is missing: ${fragment}`);
            }
          }

          console.log('SFL verdict validation passed');
          // SFL_VERDICT_VALIDATOR_END
          NODE
  # Repository-owner-approved exception: shared SFL writes intentionally use
  # sfl-app[bot] so the reviewer has consistent attribution across HemSoft repos.
  github-app:
    client-id: ${{ vars.SFL_APP_CLIENT_ID }}
    private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
  scripts:
    sfl-review-inventory:
      description: Record the complete SFL finding inventory for deterministic verdict validation.
      inputs:
        head_sha:
          description: Triggering pull request head SHA.
          required: true
          type: string
        new_critical:
          description: Newly identified Critical findings.
          required: true
          type: number
        new_high:
          description: Newly identified High findings.
          required: true
          type: number
        new_medium:
          description: Newly identified Medium findings.
          required: true
          type: number
        new_low:
          description: Newly identified Low findings.
          required: true
          type: number
        carried_critical:
          description: Unresolved Critical findings carried from earlier SFL runs.
          required: true
          type: number
        carried_high:
          description: Unresolved High findings carried from earlier SFL runs.
          required: true
          type: number
        carried_medium:
          description: Unresolved Medium findings carried from earlier SFL runs.
          required: true
          type: number
        carried_low:
          description: Unresolved Low findings carried from earlier SFL runs.
          required: true
          type: number
        overflow:
          description: Newly identified findings omitted after the 20-comment cap.
          required: true
          type: number
      script: |
        return { success: true };
  create-pull-request-review-comment:
    side: RIGHT
    max: 20
    commit-id: ${{ github.event.pull_request.head.sha }}
  submit-pull-request-review:
    allowed-events: [APPROVE, REQUEST_CHANGES]
    supersede-older-reviews: true
    footer: always
    commit-id: ${{ github.event.pull_request.head.sha }}
  create-check-run:
    max: 1
    name: "SFL Reviewer Approval"
  noop:
    report-as-issue: false
---

<!-- sfl:
  status: active
  version: "1.0.0"
  category: review
  risk-class: trivial
  target-labels: [sfl-review]
  outcome-definition: |
    The triggering pull request receives a current-head structured review,
    one inline thread per finding, and an SFL Reviewer Approval check.
  acceptance-criteria:
    - The sfl-review label triggers exactly one current-head review run
    - The trigger label is consumed during authorized activation
    - Security, correctness/reliability, and quality/maintainability are reviewed
    - Every finding is an inline thread classified Critical, High, Medium, or Low
    - The review body reports the run ID, head SHA, verdict, and severity counts
    - Critical or High findings fail the approval check and request changes
    - Medium or Low findings do not fail the approval check
    - Zero findings produce an approving review and successful approval check
  source-repo: HemSoft/set-it-free-loop
-->

# SFL Review - Full-Spectrum Pull Request Review

Review only the pull request that triggered this workflow. The reviewed commit
must be `${{ github.event.pull_request.head.sha }}` and the SFL run ID is
`${{ github.run_id }}`.

Use the GitHub pull request tools to read the triggering PR, its changed files,
and the complete diff. Before creating comments, list existing review comments
and unresolved threads from every SFL review run whose comment begins with an
exact SFL severity prefix, so you do not repeat a finding and can carry
unresolved findings into the current verdict.

Immediately before producing any safe output, fetch the pull request again and
compare its current head SHA with `${{ github.event.pull_request.head.sha }}`.
If they differ, emit no review comments, review, or check run. Call `noop` with
the stale-head reason and stop. The safe-output handlers pin review comments
and the consolidated review to the triggering SHA as a second fail-closed guard.

## Required review passes

Perform all three evidence-based passes independently before producing output.

1. **Security**
   - Injection, unsafe command or path construction, XSS, SSRF, and deserialization
   - Authentication, authorization, privilege boundaries, and secret exposure
   - Dependency, workflow, and supply-chain risks
2. **Correctness and Reliability**
   - Logic errors, regressions, incorrect assumptions, null and boundary cases
   - Error handling, races, resource leaks, data loss, and compatibility
   - Whether tests cover every meaningful new or changed behavior
3. **Quality and Maintainability**
   - Excessive complexity, duplication, coupling, unclear ownership, and dead code
   - Type safety, performance regressions, operational risk, and repository conventions
   - Whether the implementation is the smallest complete and defensible change

## Finding policy

Classify every finding into exactly one severity:

- **CRITICAL** - exploitable security issue, data loss, production crash,
  public API break, race, or deadlock
- **HIGH** - serious correctness, authorization, reliability, or operational defect
- **MEDIUM** - material bug avenue, missing logic-branch tests, performance
  regression, or maintainability problem
- **LOW** - actionable improvement with concrete value and low implementation risk

Do not report style preferences, speculative concerns, or findings without
specific evidence from the changed code.

For each finding, call `create-pull-request-review-comment` on the most precise
changed line. The comment body must begin with one of these exact prefixes:

- `**CRITICAL Finding**`
- `**HIGH Finding**`
- `**MEDIUM Finding**`
- `**LOW Finding**`

After the prefix, state the defect, impact, evidence, and a concrete fix.
Create exactly one inline thread per finding. If there are no findings, create
no inline comments.

Build the complete finding inventory before emitting comments. At most 20
inline comments can be published. If more than 20 findings exist, publish the
20 highest-severity findings, report the overflow count in the consolidated
review, and force `REQUEST_CHANGES` with a failing approval check. Never approve
a review whose complete finding inventory exceeded the inline-comment limit.

Before emitting the consolidated review or check, call
`sfl_review_inventory` exactly once with the triggering head SHA; the complete
new and carried counts for every severity; and the number of newly identified
findings omitted by the 20-comment cap. The deterministic validation step
checks this inventory against the emitted inline comments, review event, and
check conclusion. Any mismatch blocks all safe outputs.

## Approval policy

Count all newly identified findings plus unresolved SFL findings from earlier
runs by severity. Do not duplicate an unresolved finding as a new inline
comment, but carry it into the current counts and verdict until its GitHub
review thread is resolved.

- If any Critical or High finding remains unresolved, or the complete finding
  inventory exceeded 20 comments, submit `REQUEST_CHANGES` and create the
  `SFL Reviewer Approval` check with conclusion `failure`.
- If only Medium or Low findings exist and the complete finding inventory did
  not exceed 20 comments, submit `APPROVE` and create the check with conclusion
  `success`.
- If no findings exist, submit `APPROVE` and create the check with conclusion
  `success`.

Submit exactly one consolidated review with this body:

```markdown
## SFL Full-Spectrum Review

SFL run ID: ${{ github.run_id }}
Head SHA: ${{ github.event.pull_request.head.sha }}
Verdict: APPROVE

| Severity | Count |
| --- | ---: |
| Critical | 0 |
| High | 0 |
| Medium | 0 |
| Low | 0 |
| Overflow | 0 |

### Review passes

- Security: complete
- Correctness and Reliability: complete
- Quality and Maintainability: complete

### Summary

Concise evidence-based summary of the review result.
```

Replace the verdict and counts with the actual result. Use
`Verdict: CHANGES_REQUESTED` when Critical or High findings exist or the
complete finding inventory exceeded 20 comments.

Create exactly one check run named `SFL Reviewer Approval` with:

- `title`: `SFL full-spectrum review complete`
- `summary`: exactly these lines with actual values:
  - `Verdict: APPROVE`
  - `Head SHA: ${{ github.event.pull_request.head.sha }}`
  - `SFL run ID: ${{ github.run_id }}`
  - `Critical: 0`
  - `High: 0`
  - `Medium: 0`
  - `Low: 0`
  - `Overflow: 0`
- `conclusion`: the approval-policy result above

Do not modify code, branches, pull request labels, or pull request metadata.
