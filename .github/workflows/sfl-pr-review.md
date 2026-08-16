---
# HemSoft SFL reviewer platform v2
name: SFL PR Review
description: |
  Full-spectrum PR review dispatched automatically by the SFL PR review wrapper
  or manually through workflow_dispatch. Performs 3 evidence-based review
  passes (Security, Accuracy & Reliability, Quality & Maintainability) and
  posts inline review comments on specific lines with an overall summary.
source: HemSoft/set-it-free-loop/deployment/workflows/sfl-pr-review.md@main

on:
  workflow_dispatch:
    inputs:
      aw_context:
        description: gh-aw pull request context JSON
        required: true
        type: string
      item_number:
        description: Pull request number to review
        required: true
        type: string
      base_sha:
        description: Expected pull request base commit
        required: true
        type: string
      head_sha:
        description: Expected pull request head commit
        required: true
        type: string
      dispatch_id:
        description: Unique wrapper or recovery dispatch identifier
        required: false
        default: manual
        type: string
      retry_count:
        description: Missing-review-output retry counter
        required: false
        default: "0"
        type: string
      review_effort:
        description: Audit effort marker retained in provenance and recovery
        required: false
        default: low
        type: choice
        options:
          - low
          - medium
          - high

run-name: "SFL PR Review #${{ inputs.item_number }} ${{ inputs.base_sha }}:${{ inputs.head_sha }} retry=${{ inputs.retry_count }} dispatch=${{ inputs.dispatch_id }}"

concurrency:
  group: sfl-pr-review-${{ inputs.item_number }}
  cancel-in-progress: true

permissions:
  contents: read
  checks: read
  issues: read
  pull-requests: read

env:
  SFL_REVIEW_EFFORT: ${{ inputs.review_effort || 'low' }}

pre-steps:
  - name: Validate trusted review context
    env:
      AW_CONTEXT: ${{ inputs.aw_context }}
      BASE_SHA: ${{ inputs.base_sha }}
      DISPATCH_ID: ${{ inputs.dispatch_id }}
      HEAD_SHA: ${{ inputs.head_sha }}
      ITEM_NUMBER: ${{ inputs.item_number }}
    run: |
      set -euo pipefail
      if ! [[ "$ITEM_NUMBER" =~ ^[1-9][0-9]*$ ]]; then
        echo "::error::Invalid pull request number"
        exit 1
      fi
      if ! [[ "$BASE_SHA" =~ ^[0-9a-f]{40}$ ]] \
        || ! [[ "$HEAD_SHA" =~ ^[0-9a-f]{40}$ ]]; then
        echo "::error::Invalid pull request revision"
        exit 1
      fi
      if ! [[ "$DISPATCH_ID" =~ ^[A-Za-z0-9._-]{1,64}$ ]]; then
        echo "::error::Invalid dispatch identifier"
        exit 1
      fi
      if ! jq -e \
        --argjson item_number "$ITEM_NUMBER" \
        --arg base_sha "$BASE_SHA" \
        --arg head_sha "$HEAD_SHA" '
          . == {
            item_type: "pull_request",
            item_number: $item_number,
            base_sha: $base_sha,
            head_sha: $head_sha
          }
        ' <<< "$AW_CONTEXT" > /dev/null; then
        echo "::error::Untrusted or inconsistent review context"
        exit 1
      fi

# HemSoft runs the reviewer through Kimi K3 on its private OpenRouter route.

models:
  providers:
    github-copilot:
      models:
        "moonshotai/kimi-k3":
          cost:
            input: "3e-06"
            output: "1.5e-05"

engine:
  id: copilot
  env:
    COPILOT_MODEL: moonshotai/kimi-k3
    COPILOT_PROVIDER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
    COPILOT_PROVIDER_BASE_URL: https://openrouter.ai/api/v1
    COPILOT_PROVIDER_TYPE: openai
    COPILOT_PROVIDER_WIRE_API: responses

model: moonshotai/kimi-k3

network:
  allowed:
    - openrouter.ai

tools:
  github:
    lockdown: false
  bash: true

safe-outputs:
  threat-detection:
    continue-on-error: false
    post-steps:
      - name: Generate SFL App token for final head verification
        id: sfl-final-head-token
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          client-id: ${{ vars.SFL_APP_CLIENT_ID }}
          private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
          owner: ${{ github.repository_owner }}
          repositories: ${{ github.event.repository.name }}
          permission-pull-requests: read
      - name: Verify pull request base and head before safe outputs
        env:
          EXPECTED_BASE: ${{ inputs.base_sha }}
          EXPECTED_HEAD: ${{ inputs.head_sha }}
          GH_TOKEN: ${{ steps.sfl-final-head-token.outputs.token }}
          PR_NUMBER: ${{ inputs.item_number }}
          REPOSITORY: ${{ github.repository }}
        run: |
          set -euo pipefail
          if [[ ! "${EXPECTED_HEAD}" =~ ^[0-9a-f]{40}$ ]]; then
            echo "::error::Invalid expected pull request head: ${EXPECTED_HEAD}"
            exit 1
          fi
          if [[ ! "${EXPECTED_BASE}" =~ ^[0-9a-f]{40}$ ]]; then
            echo "::error::Invalid expected pull request base: ${EXPECTED_BASE}"
            exit 1
          fi
          pull_request="$(gh api "repos/${REPOSITORY}/pulls/${PR_NUMBER}")"
          live_head="$(jq -r '.head.sha' <<< "$pull_request")"
          live_base="$(jq -r '.base.sha' <<< "$pull_request")"
          if [[ "${live_head}" != "${EXPECTED_HEAD}" ]]; then
            echo "::error::Pull request head changed from ${EXPECTED_HEAD} to ${live_head}; suppressing stale SFL outputs"
            exit 1
          fi
          if [[ "${live_base}" != "${EXPECTED_BASE}" ]]; then
            echo "::error::Pull request base changed from ${EXPECTED_BASE} to ${live_base}; suppressing stale SFL outputs"
            exit 1
          fi
  github-app:
    client-id: ${{ vars.SFL_APP_CLIENT_ID }}
    private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
  create-pull-request-review-comment:
    commit-id: "${{ inputs.head_sha }}"
    max: 30
    target: "${{ inputs.item_number }}"
  submit-pull-request-review:
    commit-id: "${{ inputs.head_sha }}"
    max: 1
    target: "${{ inputs.item_number }}"
  jobs:
    resolve-sfl-review-thread:
      description: Resolve one obsolete SFL finding thread after deterministic validation
      runs-on: ubuntu-slim
      needs: safe_outputs
      if: needs.safe_outputs.result == 'success'
      permissions:
        contents: write
        pull-requests: write
      inputs:
        thread_id:
          description: GraphQL node ID of the obsolete SFL review thread
          required: true
          type: string
      steps:
        - name: Generate SFL App token for thread validation
          id: validation-token
          uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
          with:
            client-id: ${{ vars.SFL_APP_CLIENT_ID }}
            private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
            owner: ${{ github.repository_owner }}
            repositories: ${{ github.event.repository.name }}
            permission-contents: read
            permission-pull-requests: read
        - name: Resolve SFL App identity
          id: app-identity
          uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
          with:
            github-token: ${{ steps.validation-token.outputs.token }}
            script: |
              const data = await github.graphql(`query { viewer { login } }`);
              const login = (data.viewer?.login || '')
                .replace(/\[bot\]$/i, '')
                .toLowerCase();
              if (!login) {
                core.setFailed('Could not resolve the SFL App identity');
                return;
              }
              core.setOutput('login', login);
        - name: Validate and resolve requested SFL threads
          uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
          env:
            SFL_APP_LOGIN: ${{ steps.app-identity.outputs.login }}
            SFL_EXPECTED_BASE: ${{ inputs.base_sha }}
            SFL_EXPECTED_HEAD: ${{ inputs.head_sha }}
            SFL_PR_NUMBER: ${{ inputs.item_number }}
          with:
            github-token: ${{ github.token }}
            script: |
              const fs = require('fs');
              const outputPath = process.env.GH_AW_AGENT_OUTPUT;
              const expectedBase = process.env.SFL_EXPECTED_BASE;
              const expectedHead = process.env.SFL_EXPECTED_HEAD;
              const pullNumber = Number(process.env.SFL_PR_NUMBER);
              if (!outputPath || !fs.existsSync(outputPath)) {
                core.setFailed('SFL agent output is unavailable for thread resolution');
                return;
              }
              if (!/^[0-9a-f]{40}$/.test(expectedHead || '')) {
                core.setFailed(`Invalid expected pull request head: ${expectedHead || 'missing'}`);
                return;
              }
              if (!/^[0-9a-f]{40}$/.test(expectedBase || '')) {
                core.setFailed(`Invalid expected pull request base: ${expectedBase || 'missing'}`);
                return;
              }
              if (!Number.isInteger(pullNumber) || pullNumber <= 0) {
                core.setFailed(`Invalid pull request number: ${process.env.SFL_PR_NUMBER}`);
                return;
              }

              const output = JSON.parse(fs.readFileSync(outputPath, 'utf8'));
              const requests = (Array.isArray(output.items) ? output.items : [])
                .filter(item => item.type === 'resolve_sfl_review_thread');
              const threadIds = [...new Set(requests.map(item => item.thread_id))];
              if (threadIds.length === 0) {
                core.info('No obsolete SFL review threads requested for resolution');
                return;
              }
              if (threadIds.length > 30) {
                core.setFailed(`Requested ${threadIds.length} thread resolutions; maximum is 30`);
                return;
              }

              const normalizeActorLogin = login =>
                (login || '').replace(/\[bot\]$/i, '').toLowerCase();
              const appLogin = normalizeActorLogin(process.env.SFL_APP_LOGIN);
              if (!appLogin) {
                core.setFailed('Could not resolve the SFL App identity');
                return;
              }
              const severityPrefix =
                /^(🔴 \*\*CRITICAL|🟠 \*\*HIGH|🟡 \*\*MEDIUM|⚪ \*\*LOW) —/;
              const unresolvedOutdatedThreadIds = [];

              for (const threadId of threadIds) {
                if (typeof threadId !== 'string' || !threadId.startsWith('PRRT_')) {
                  core.setFailed(`Invalid pull request review thread ID: ${threadId}`);
                  return;
                }
                const data = await github.graphql(
                  `query($threadId: ID!) {
                    node(id: $threadId) {
                      ... on PullRequestReviewThread {
                        id
                        isOutdated
                        isResolved
                        pullRequest {
                          number
                          baseRefOid
                          headRefOid
                          repository {
                            nameWithOwner
                          }
                        }
                        comments(first: 1) {
                          nodes {
                            author {
                              login
                              __typename
                            }
                            body
                            path
                          }
                        }
                      }
                    }
                  }`,
                  { threadId }
                );
                const thread = data.node;
                const rootComment = thread?.comments?.nodes?.[0];
                const body = rootComment?.body || '';
                const path = rootComment?.path;
                if (
                  !thread ||
                  thread.pullRequest?.repository?.nameWithOwner !==
                    `${context.repo.owner}/${context.repo.repo}` ||
                  thread.pullRequest?.number !== pullNumber ||
                  thread.pullRequest?.baseRefOid !== expectedBase ||
                  thread.pullRequest?.headRefOid !== expectedHead ||
                  rootComment?.author?.__typename !== 'Bot' ||
                  normalizeActorLogin(rootComment.author.login) !== appLogin ||
                  !severityPrefix.test(body) ||
                  !body.includes('SFL Reviewer') ||
                  typeof path !== 'string' ||
                  path.length === 0 ||
                  (!thread.isResolved && !thread.isOutdated)
                ) {
                  core.setFailed(
                    `Thread ${threadId} is not an obsolete SFL finding on PR #${pullNumber} at base ${expectedBase} and head ${expectedHead}`
                  );
                  return;
                }
                if (thread.isResolved) {
                  core.info(`SFL review thread ${threadId} is already resolved`);
                  continue;
                }
                try {
                  const { data: pathAtHead } = await github.rest.repos.getContent({
                    owner: context.repo.owner,
                    repo: context.repo.repo,
                    path,
                    ref: expectedHead,
                  });
                  if (Array.isArray(pathAtHead) || pathAtHead.type !== 'file') {
                    core.setFailed(
                      `Thread ${threadId} path ${path} is not a file at head ${expectedHead}`
                    );
                    return;
                  }
                } catch (error) {
                  if (error.status === 404) {
                    core.setFailed(
                      `Thread ${threadId} path ${path} no longer exists at head ${expectedHead}; human resolution is required`
                    );
                    return;
                  }
                  throw error;
                }
                unresolvedOutdatedThreadIds.push(threadId);
              }

              const failedThreadIds = [];
              for (const threadId of unresolvedOutdatedThreadIds) {
                let resolved = false;
                let lastError;
                for (let attempt = 1; attempt <= 3; attempt += 1) {
                  try {
                    const result = await github.graphql(
                      `mutation($threadId: ID!) {
                        resolveReviewThread(input: { threadId: $threadId }) {
                          thread {
                            id
                            isResolved
                          }
                        }
                      }`,
                      { threadId }
                    );
                    if (result.resolveReviewThread?.thread?.isResolved) {
                      resolved = true;
                      break;
                    }
                    lastError = new Error('mutation did not return a resolved thread');
                  } catch (error) {
                    lastError = error;
                  }
                  if (attempt < 3) {
                    await new Promise(resolve => setTimeout(resolve, attempt * 1000));
                  }
                }
                if (!resolved) {
                  core.error(
                    `Failed to resolve SFL review thread ${threadId}: ${lastError?.message || 'unknown error'}`
                  );
                  failedThreadIds.push(threadId);
                  continue;
                }
                core.info(`Resolved obsolete SFL review thread ${threadId}`);
              }
              if (failedThreadIds.length > 0) {
                core.setFailed(
                  `Failed to resolve ${failedThreadIds.length} obsolete SFL review thread(s)`
                );
              }

jobs:
  publish_review_provenance:
    if: always()
    runs-on: ubuntu-slim
    outputs:
      evidence-check-run-id: ${{ steps.initialize-evidence.outputs.check-run-id }}
    permissions:
      checks: write
      contents: read
      pull-requests: read
    steps:
      - name: Write immutable review dispatch provenance
        env:
          BASE_SHA: ${{ inputs.base_sha }}
          HEAD_SHA: ${{ inputs.head_sha }}
          PR_NUMBER: ${{ inputs.item_number }}
          PROVENANCE_PATH: ${{ runner.temp }}/sfl-review-run-provenance.json
          REPOSITORY: ${{ github.repository }}
          REVIEW_EFFORT: ${{ env.SFL_REVIEW_EFFORT }}
          RETRY_COUNT: ${{ inputs.retry_count }}
          RUN_ID: ${{ github.run_id }}
        run: |
          set -euo pipefail
          if ! [[ "$PR_NUMBER" =~ ^[1-9][0-9]*$ ]]; then
            echo "::error::Invalid pull request number: ${PR_NUMBER}"
            exit 1
          fi
          if ! [[ "$HEAD_SHA" =~ ^[0-9a-f]{40}$ ]]; then
            echo "::error::Invalid expected pull request head: ${HEAD_SHA}"
            exit 1
          fi
          if ! [[ "$BASE_SHA" =~ ^[0-9a-f]{40}$ ]]; then
            echo "::error::Invalid expected pull request base: ${BASE_SHA}"
            exit 1
          fi
          if ! [[ "$RETRY_COUNT" =~ ^[01]$ ]]; then
            echo "::error::Invalid review retry count: ${RETRY_COUNT}"
            exit 1
          fi
          case "$REVIEW_EFFORT" in
            low|medium|high) ;;
            *)
              echo "::error::Invalid review effort: ${REVIEW_EFFORT}"
              exit 1
              ;;
          esac
          jq -n \
            --arg repository "$REPOSITORY" \
            --arg workflow_run_id "$RUN_ID" \
            --argjson pr_number "$PR_NUMBER" \
            --arg base_sha "$BASE_SHA" \
            --arg head_sha "$HEAD_SHA" \
            --arg review_effort "$REVIEW_EFFORT" \
            --argjson retry_count "$RETRY_COUNT" \
            '{
              repository: $repository,
              workflow_run_id: $workflow_run_id,
              pr_number: $pr_number,
              base_sha: $base_sha,
              head_sha: $head_sha,
              review_effort: $review_effort,
              retry_count: $retry_count
            }' > "$PROVENANCE_PATH"
      - name: Initialize review evidence
        id: initialize-evidence
        uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
        env:
          SFL_BASE_SHA: ${{ inputs.base_sha }}
          SFL_HEAD_SHA: ${{ inputs.head_sha }}
          SFL_PR_NUMBER: ${{ inputs.item_number }}
          SFL_RUN_ID: ${{ github.run_id }}
        with:
          github-token: ${{ github.token }}
          script: |
            const baseSha = process.env.SFL_BASE_SHA;
            const headSha = process.env.SFL_HEAD_SHA;
            const pullNumber = Number(process.env.SFL_PR_NUMBER);
            const runId = Number(process.env.SFL_RUN_ID);
            if (!Number.isSafeInteger(runId) || runId <= 0) {
              core.setFailed(`Invalid workflow run ID: ${process.env.SFL_RUN_ID}`);
              return;
            }
            const { data: pullRequest } = await github.rest.pulls.get({
              owner: context.repo.owner,
              repo: context.repo.repo,
              pull_number: pullNumber,
            });
            if (
              pullRequest.base.sha !== baseSha ||
              pullRequest.head.sha !== headSha
            ) {
              core.setFailed(
                `Pull request changed from base ${baseSha} / head ${headSha} ` +
                `to base ${pullRequest.base.sha} / head ${pullRequest.head.sha}`
              );
              return;
            }
            const { data: evidence } = await github.rest.checks.create({
              owner: context.repo.owner,
              repo: context.repo.repo,
              name: 'SFL Review Evidence',
              head_sha: headSha,
              external_id:
                `sfl-review:${pullNumber}:${baseSha}:${headSha}:${runId}`,
              status: 'in_progress',
              started_at: new Date().toISOString(),
              output: {
                title: 'SFL review is in progress',
                summary: 'The immutable pull request state is being reviewed.',
                text: 'replacement_retry_eligible=false',
              },
            });
            core.setOutput('check-run-id', String(evidence.id));
      - name: Upload immutable review dispatch provenance
        if: always()
        uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1
        with:
          name: sfl-review-run-provenance
          path: ${{ runner.temp }}/sfl-review-run-provenance.json
          if-no-files-found: error
          retention-days: 1

  review_metadata:
    needs: [agent, publish_review_provenance, safe_outputs, resolve_sfl_review_thread]
    if: always()
    runs-on: ubuntu-slim
    permissions:
      checks: write
      contents: read
      pull-requests: read
    steps:
      - name: Verify expected pull request head
        id: resolve-head
        uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
        env:
          SFL_EXPECTED_BASE: ${{ inputs.base_sha }}
          SFL_EXPECTED_HEAD: ${{ inputs.head_sha }}
          SFL_PR_NUMBER: ${{ inputs.item_number }}
        with:
          github-token: ${{ github.token }}
          script: |
            const pullNumber = Number(process.env.SFL_PR_NUMBER);
            const expectedBase = process.env.SFL_EXPECTED_BASE;
            const expectedHead = process.env.SFL_EXPECTED_HEAD;
            if (!Number.isInteger(pullNumber) || pullNumber <= 0) {
              core.setFailed(`Invalid pull request number: ${process.env.SFL_PR_NUMBER}`);
              return;
            }
            if (!/^[0-9a-f]{40}$/.test(expectedHead || '')) {
              core.setFailed(`Invalid expected pull request head: ${expectedHead || 'missing'}`);
              return;
            }
            if (!/^[0-9a-f]{40}$/.test(expectedBase || '')) {
              core.setFailed(`Invalid expected pull request base: ${expectedBase || 'missing'}`);
              return;
            }

            const { data: pullRequest } = await github.rest.pulls.get({
              owner: context.repo.owner,
              repo: context.repo.repo,
              pull_number: pullNumber,
            });
            core.setOutput('head-sha', expectedHead);
            if (
              pullRequest.base.sha !== expectedBase ||
              pullRequest.head.sha !== expectedHead
            ) {
              core.setFailed(
                `Pull request changed from base ${expectedBase} / head ${expectedHead} to base ${pullRequest.base.sha} / head ${pullRequest.head.sha}`
              );
            }

      - name: Download safe output items
        if: needs.agent.result == 'success' && needs.safe_outputs.result == 'success' && (needs.resolve_sfl_review_thread.result == 'success' || needs.resolve_sfl_review_thread.result == 'skipped')
        uses: actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c # v8.0.1
        with:
          name: safe-outputs-items
          path: /tmp/sfl-review-safe-outputs

      - name: Download reviewer agent output
        if: needs.agent.result == 'success' && needs.safe_outputs.result == 'success' && (needs.resolve_sfl_review_thread.result == 'success' || needs.resolve_sfl_review_thread.result == 'skipped')
        uses: actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c # v8.0.1
        with:
          name: agent
          path: /tmp/gh-aw

      - name: Generate GitHub App token
        if: always()
        id: app-token
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          client-id: ${{ vars.SFL_APP_CLIENT_ID }}
          private-key: ${{ secrets.SFL_APP_PRIVATE_KEY }}
          owner: ${{ github.repository_owner }}
          repositories: ${{ github.event.repository.name }}
          permission-actions: read
          permission-contents: read
          permission-pull-requests: write
      - name: Finalize review metadata
        if: needs.agent.result == 'success' && needs.safe_outputs.result == 'success' && (needs.resolve_sfl_review_thread.result == 'success' || needs.resolve_sfl_review_thread.result == 'skipped')
        id: finalize-review
        uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
        env:
          SFL_AIC: ${{ needs.agent.outputs.aic }}
          SFL_EFFORT: ${{ env.SFL_REVIEW_EFFORT }}
          SFL_EXPECTED_BASE: ${{ inputs.base_sha }}
          SFL_EXPECTED_HEAD: ${{ inputs.head_sha }}
          SFL_AGENT_OUTPUT: /tmp/gh-aw/agent_output.json
          SFL_MODEL: ${{ needs.agent.outputs.model }}
          SFL_PR_NUMBER: ${{ github.event.pull_request.number || inputs.item_number }}
          SFL_RUN_ID: ${{ github.run_id }}
          SFL_SAFE_OUTPUT_ITEMS: /tmp/sfl-review-safe-outputs/safe-output-items.jsonl
        with:
          github-token: ${{ steps.app-token.outputs.token }}
          script: |
            // Keep these helpers inline so the HemSoft deployer can install a
            // self-contained workflow without additional script dependencies.
            function formatElapsed(startedAt, submittedAt) {
              if (!Number.isFinite(startedAt) || !Number.isFinite(submittedAt)) {
                return 'Unavailable';
              }
              const elapsedSeconds = Math.max(0, Math.round((submittedAt - startedAt) / 1000));
              const hours = Math.floor(elapsedSeconds / 3600);
              const minutes = Math.floor((elapsedSeconds % 3600) / 60);
              const seconds = elapsedSeconds % 60;
              return hours > 0
                ? `${hours}h ${minutes}m ${seconds}s`
                : `${minutes}m ${seconds}s`;
            }

            function formatModel(modelId) {
              const [baseModel, query = ''] = modelId.split('?', 2);
              const effort = new URLSearchParams(query).get('effort') ||
                (process.env.SFL_EFFORT || '').trim();
              const claude = baseModel.match(/^claude-(opus|sonnet|haiku)-(.+)$/);
              let displayName = baseModel;
              if (claude) {
                displayName = `${claude[1][0].toUpperCase()}${claude[1].slice(1)} ${claude[2]}`;
              } else if (baseModel.startsWith('gpt-')) {
                const parts = baseModel.slice(4).split('-');
                displayName = `GPT-${parts.shift()}${parts.length ? ` ${parts.map(part => part[0].toUpperCase() + part.slice(1)).join(' ')}` : ''}`;
              }
              return effort
                ? `${displayName} (${effort[0].toUpperCase()}${effort.slice(1)} effort)`
                : displayName;
            }

            function formatAic(rawAic) {
              const value = rawAic.trim() === '' ? Number.NaN : Number(rawAic);
              return Number.isFinite(value)
                ? value.toFixed(1).replace(/\.0$/, '')
                : 'Unavailable';
            }

            async function countUnresolvedSflFindings({
              owner,
              repo,
              pullNumber,
              reviewAuthor,
            }) {
              const normalizeActorLogin = login =>
                (login || '').replace(/\[bot\]$/i, '').toLowerCase();
              const severityPrefix =
                /^(🔴 \*\*CRITICAL|🟠 \*\*HIGH|🟡 \*\*MEDIUM|⚪ \*\*LOW) —/;
              let cursor = null;
              let unresolvedCount = 0;
              do {
                const data = await github.graphql(
                  `query($owner: String!, $repo: String!, $pullNumber: Int!, $cursor: String) {
                    repository(owner: $owner, name: $repo) {
                      pullRequest(number: $pullNumber) {
                        reviewThreads(first: 100, after: $cursor) {
                          nodes {
                            isResolved
                            comments(first: 1) {
                              nodes {
                                author {
                                  login
                                  __typename
                                }
                                body
                              }
                            }
                          }
                          pageInfo {
                            hasNextPage
                            endCursor
                          }
                        }
                      }
                    }
                  }`,
                  { owner, repo, pullNumber, cursor }
                );
                const threads = data.repository?.pullRequest?.reviewThreads;
                if (!threads) {
                  throw new Error('Pull request review threads were unavailable');
                }
                for (const thread of threads.nodes || []) {
                  const rootComment = thread.comments?.nodes?.[0];
                  const body = rootComment?.body || '';
                  if (
                    !thread.isResolved &&
                    rootComment?.author?.__typename === 'Bot' &&
                    normalizeActorLogin(rootComment.author.login) ===
                      normalizeActorLogin(reviewAuthor) &&
                    severityPrefix.test(body) &&
                    body.includes('SFL Reviewer')
                  ) {
                    unresolvedCount += 1;
                  }
                }
                cursor = threads.pageInfo.hasNextPage
                  ? threads.pageInfo.endCursor
                  : null;
              } while (cursor);
              return unresolvedCount;
            }

            const owner = context.repo.owner;
            const repo = context.repo.repo;
            const pullNumber = Number(process.env.SFL_PR_NUMBER);
            const runId = Number(process.env.SFL_RUN_ID);
            const expectedBase = process.env.SFL_EXPECTED_BASE;
            const expectedHead = process.env.SFL_EXPECTED_HEAD;
            if (!Number.isInteger(pullNumber) || pullNumber <= 0) {
              core.setFailed(`Invalid pull request number: ${process.env.SFL_PR_NUMBER}`);
              return;
            }
            if (!Number.isInteger(runId) || runId <= 0) {
              core.setFailed(`Invalid workflow run ID: ${process.env.SFL_RUN_ID}`);
              return;
            }
            if (!/^[0-9a-f]{40}$/.test(expectedHead || '')) {
              core.setFailed(`Invalid expected pull request head: ${expectedHead || 'missing'}`);
              return;
            }
            if (!/^[0-9a-f]{40}$/.test(expectedBase || '')) {
              core.setFailed(`Invalid expected pull request base: ${expectedBase || 'missing'}`);
              return;
            }

            const { data: pullRequest } = await github.rest.pulls.get({
              owner,
              repo,
              pull_number: pullNumber,
            });
            if (
              pullRequest.base.sha !== expectedBase ||
              pullRequest.head.sha !== expectedHead
            ) {
              core.setFailed(
                `Pull request changed from base ${expectedBase} / head ${expectedHead} to base ${pullRequest.base.sha} / head ${pullRequest.head.sha}`
              );
              return;
            }

            const fs = require('fs');
            const safeOutputItemsPath = process.env.SFL_SAFE_OUTPUT_ITEMS;
            let safeOutputItems;
            try {
              safeOutputItems = fs
                .readFileSync(safeOutputItemsPath, 'utf8')
                .split(/\r?\n/)
                .filter(Boolean)
                .map(line => JSON.parse(line));
            } catch (error) {
              core.setFailed(
                `Could not read safe-output review metadata: ${error.message}`
              );
              return;
            }
            let agentItems = [];
            let replacementRetryEligible = false;
            try {
              const agentOutput = JSON.parse(
                fs.readFileSync(process.env.SFL_AGENT_OUTPUT, 'utf8')
              );
              agentItems = Array.isArray(agentOutput)
                ? agentOutput
                : (agentOutput.items || []);
              if (!Array.isArray(agentItems)) {
                throw new Error('agent items are not an array');
              }
              replacementRetryEligible = !agentItems.some(item =>
                item.type === 'missing_data' ||
                item.type === 'missing_tool' ||
                item.type === 'report_incomplete'
              );
            } catch (error) {
              core.warning(
                `Could not inspect terminal review signals; suppressing replacement: ${error.message}`
              );
            }
            core.setOutput(
              'replacement-retry-eligible',
              replacementRetryEligible
            );
            const agentSubmittedReviews = agentItems.filter(
              item => item.type === 'submit_pull_request_review'
            );
            if (agentSubmittedReviews.length !== 1) {
              core.setFailed(
                `Expected exactly one immutable agent submitted-review item, found ${agentSubmittedReviews.length}`
              );
              return;
            }
            const [agentSubmittedReview] = agentSubmittedReviews;
            // gh-aw preserves the agent's raw tool arguments; on a
            // fixed-target workflow an omitted repo/pull_request_number means
            // the configured pull request. Default omitted targets while
            // still rejecting explicitly mismatched ones.
            const configuredRepo = `${owner}/${repo}`;
            if (
              (agentSubmittedReview.repo ?? configuredRepo) !== configuredRepo ||
              Number(agentSubmittedReview.pull_request_number ?? pullNumber) !== pullNumber
            ) {
              core.setFailed('Immutable agent submitted-review target does not match this pull request');
              return;
            }
            const severityPrefix =
              /^(🔴 \*\*CRITICAL|🟠 \*\*HIGH|🟡 \*\*MEDIUM|⚪ \*\*LOW) —/;
            const agentReviewComments = agentItems.filter(
              item => item.type === 'create_pull_request_review_comment'
            );
            if (agentReviewComments.some(item =>
              (item.repo ?? configuredRepo) !== configuredRepo ||
              Number(item.pull_request_number ?? pullNumber) !== pullNumber ||
              !severityPrefix.test(item.body || '')
            )) {
              core.setFailed('Immutable agent review comments contain an invalid target or finding format');
              return;
            }
            const publishedReviewComments = safeOutputItems.filter(
              item => item.type === 'create_pull_request_review_comment'
            );
            if (publishedReviewComments.some(item =>
              item.repo !== `${owner}/${repo}` ||
              Number(item.number) !== pullNumber
            )) {
              core.setFailed('Published review-comment manifest target does not match this pull request');
              return;
            }
            if (publishedReviewComments.length !== agentReviewComments.length) {
              core.setFailed(
                `Published review-comment count ${publishedReviewComments.length} does not match immutable agent count ${agentReviewComments.length}`
              );
              return;
            }
            const submittedReviews = safeOutputItems.filter(
              item => item.type === 'submit_pull_request_review'
            );
            if (submittedReviews.length !== 1) {
              core.setFailed(
                `Expected exactly one submitted-review manifest item, found ${submittedReviews.length}`
              );
              return;
            }
            const [submittedReview] = submittedReviews;
            if (
              submittedReview.repo !== `${owner}/${repo}` ||
              Number(submittedReview.number) !== pullNumber
            ) {
              core.setFailed('Submitted-review manifest target does not match this pull request');
              return;
            }
            const reviewId = Number(submittedReview.metadata?.review_id);
            if (!Number.isInteger(reviewId) || reviewId <= 0) {
              core.setFailed('Submitted-review manifest does not contain a valid review ID');
              return;
            }
            const { data: review } = await github.rest.pulls.getReview({
              owner,
              repo,
              pull_number: pullNumber,
              review_id: reviewId,
            });
            if (!review.commit_id || review.commit_id !== expectedHead) {
              core.setFailed(
                `SFL review commit ${review.commit_id || 'missing'} does not match expected head ${expectedHead}`
              );
              return;
            }

            const { data: run } = await github.rest.actions.getWorkflowRun({
              owner,
              repo,
              run_id: runId,
            });
            const startedAt = Date.parse(run.run_started_at || run.created_at);
            const submittedAt = Date.parse(review.submitted_at);
            const elapsed = formatElapsed(startedAt, submittedAt);
            const model = formatModel(process.env.SFL_MODEL || 'unknown');
            const aic = formatAic(process.env.SFL_AIC || '');

            const originalBody = agentSubmittedReview.body || '';
            for (const placeholder of [
              'SFL_MODEL_PENDING',
              'SFL_TIME_PENDING',
              'SFL_AIC_PENDING',
            ]) {
              const occurrences = originalBody.split(placeholder).length - 1;
              if (occurrences !== 1) {
                core.setFailed(
                  `Review body must contain ${placeholder} exactly once; found ${occurrences}`
                );
                return;
              }
            }
            const templatePlaceholders = [
              '{verdict_icon}',
              '{VERDICT}',
              '{verdict_reason}',
              '{security_critical_or_dash}',
              '{security_high_or_dash}',
              '{security_medium_or_dash}',
              '{security_low_or_dash}',
              '{accuracy_critical_or_dash}',
              '{accuracy_high_or_dash}',
              '{accuracy_medium_or_dash}',
              '{accuracy_low_or_dash}',
              '{quality_maintainability_critical_or_dash}',
              '{quality_maintainability_high_or_dash}',
              '{quality_maintainability_medium_or_dash}',
              '{quality_maintainability_low_or_dash}',
              '{total_critical_or_dash}',
              '{total_high_or_dash}',
              '{total_medium_or_dash}',
              '{total_low_or_dash}',
              '{Top 3 most impactful current findings with their severity prefixes, or "No findings."}',
            ];
            const unresolvedPlaceholder = templatePlaceholders.find(placeholder =>
              originalBody.includes(placeholder)
            );
            if (unresolvedPlaceholder) {
              core.setFailed(
                `Review body contains unresolved template placeholder ${unresolvedPlaceholder}`
              );
              return;
            }

            const body = originalBody
              .replaceAll('SFL_MODEL_PENDING', model)
              .replaceAll('SFL_TIME_PENDING', elapsed)
              .replaceAll('SFL_AIC_PENDING', aic);

            const verdictApproved = body.includes('## ✅ Verdict: APPROVE');
            const verdictNeedsWork = body.includes('## ⚠️ Verdict: NEEDS WORK');
            if (verdictApproved === verdictNeedsWork) {
              core.setFailed('Review body must contain exactly one recognized SFL verdict');
              return;
            }

            const prAuthor = pullRequest.user?.login;
            const reviewAuthor = review.user?.login;
            if (!reviewAuthor) {
              core.setFailed('SFL review author identity is unavailable');
              return;
            }
            const selfAuthored = Boolean(prAuthor) && prAuthor === reviewAuthor;
            const expectedReviewEvent = verdictApproved && !selfAuthored
              ? 'APPROVE'
              : 'COMMENT';
            const expectedReviewState = verdictApproved && !selfAuthored
              ? 'APPROVED'
              : 'COMMENTED';
            if ((agentSubmittedReview.event || '').toUpperCase() !== expectedReviewEvent) {
              core.setFailed(
                `Immutable agent review event ${agentSubmittedReview.event || 'missing'} does not match expected event ${expectedReviewEvent}`
              );
              return;
            }
            if (
              (submittedReview.metadata?.review_event || '').toUpperCase() !==
                expectedReviewEvent ||
              submittedReview.metadata?.review_state !== expectedReviewState
            ) {
              core.setFailed('Published review manifest does not match the immutable agent verdict');
              return;
            }
            if (review.state !== expectedReviewState) {
              core.setFailed(
                `Review state ${review.state || 'missing'} does not match expected state ${expectedReviewState}`
              );
              return;
            }

            // GitHub accepts body-only updates to submitted COMMENTED reviews
            // when authenticated as the app that authored the review.
            await github.request('PUT /repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}', {
              owner,
              repo,
              pull_number: pullNumber,
              review_id: review.id,
              body,
            });

            let publishedFindingCount;
            try {
              const reviewComments = await github.paginate(
                'GET /repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}/comments',
                {
                  owner,
                  repo,
                  pull_number: pullNumber,
                  review_id: review.id,
                  per_page: 100,
                }
              );
              const inlineRunMarker = `[Run ${runId}]`;
              publishedFindingCount = reviewComments.filter(comment =>
                severityPrefix.test(comment.body || '') &&
                comment.body?.includes(inlineRunMarker)
              ).length;
            } catch (error) {
              core.setFailed(`Could not enumerate SFL findings: ${error.message}`);
              return;
            }
            const immutableFindingCount = agentReviewComments.length;
            if (publishedFindingCount !== immutableFindingCount) {
              core.setFailed(
                `Published finding count ${publishedFindingCount} does not match immutable agent count ${immutableFindingCount}`
              );
              return;
            }

            let unresolvedFindingCount;
            try {
              unresolvedFindingCount = await countUnresolvedSflFindings({
                owner,
                repo,
                pullNumber,
                reviewAuthor,
              });
            } catch (error) {
              core.setFailed(`Could not enumerate unresolved SFL findings: ${error.message}`);
              return;
            }

            const gateApproved =
              verdictApproved &&
              immutableFindingCount === 0 &&
              unresolvedFindingCount === 0;
            core.setOutput('head-sha', expectedHead);
            core.setOutput('gate-approved', gateApproved);
            core.setOutput('finding-count', unresolvedFindingCount);
            core.setOutput('new-finding-count', immutableFindingCount);
            core.setOutput('review-matches-head', true);
            core.setOutput('review-state', review.state);
      - name: Finalize SFL review evidence
        if: always()
        uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
        env:
          SFL_BASE_SHA: ${{ inputs.base_sha }}
          SFL_EVIDENCE_CHECK_RUN_ID: ${{ needs.publish_review_provenance.outputs.evidence-check-run-id }}
          SFL_HEAD_SHA: ${{ inputs.head_sha }}
          SFL_GATE_APPROVED: ${{ steps.finalize-review.outputs.gate-approved }}
          SFL_FINDING_COUNT: ${{ steps.finalize-review.outputs.finding-count }}
          SFL_REPLACEMENT_RETRY_ELIGIBLE: ${{ steps.finalize-review.outputs.replacement-retry-eligible }}
          SFL_REVIEW_MATCHES_HEAD: ${{ steps.finalize-review.outputs.review-matches-head }}
          SFL_REVIEW_STATE: ${{ steps.finalize-review.outputs.review-state }}
          SFL_PR_NUMBER: ${{ inputs.item_number }}
          SFL_RUN_ID: ${{ github.run_id }}
          SFL_UPSTREAM_RESULT: agent=${{ needs.agent.result }}, safe_outputs=${{ needs.safe_outputs.result }}, thread_resolution=${{ needs.resolve_sfl_review_thread.result }}
        with:
          github-token: ${{ github.token }}
          script: |
            const baseSha = process.env.SFL_BASE_SHA;
            const headSha = process.env.SFL_HEAD_SHA;
            if (!/^[0-9a-f]{40}$/.test(baseSha || '')) {
              core.setFailed(`Cannot publish SFL approval gate for invalid base SHA: ${baseSha || 'missing'}`);
              return;
            }
            if (!/^[0-9a-f]{40}$/.test(headSha || '')) {
              core.setFailed(`Cannot publish SFL approval gate for invalid head SHA: ${headSha || 'missing'}`);
              return;
            }
            const pullNumber = Number(process.env.SFL_PR_NUMBER);
            const runId = Number(process.env.SFL_RUN_ID);
            if (!Number.isSafeInteger(runId) || runId <= 0) {
              core.setFailed(`Invalid workflow run ID: ${process.env.SFL_RUN_ID}`);
              return;
            }
            const { data: pullRequest } = await github.rest.pulls.get({
              owner: context.repo.owner,
              repo: context.repo.repo,
              pull_number: pullNumber,
            });
            const baseMatchesExpected = pullRequest.base.sha === baseSha;
            const headMatchesExpected = pullRequest.head.sha === headSha;
            const gateApproved = process.env.SFL_GATE_APPROVED === 'true';
            const replacementRetryEligible =
              process.env.SFL_REPLACEMENT_RETRY_ELIGIBLE === 'true';
            const evidenceCheckRunId =
              Number(process.env.SFL_EVIDENCE_CHECK_RUN_ID);
            const gateExternalId =
              `sfl-review:${pullNumber}:${baseSha}:${headSha}:${runId}`;
            if (!Number.isSafeInteger(evidenceCheckRunId) || evidenceCheckRunId <= 0) {
              core.setFailed('Cannot finalize missing SFL review evidence');
              return;
            }
            const { data: initializedEvidence } = await github.rest.checks.get({
              owner: context.repo.owner,
              repo: context.repo.repo,
              check_run_id: evidenceCheckRunId,
            });
            if (
              initializedEvidence.name !== 'SFL Review Evidence' ||
              initializedEvidence.head_sha !== headSha ||
              initializedEvidence.external_id !== gateExternalId ||
              initializedEvidence.app?.id !== 15368
            ) {
              core.setFailed('Initialized SFL review evidence does not match this run');
              return;
            }
            const publishEvidence = async ({ approved, summary }) => {
              let lastError;
              for (let attempt = 1; attempt <= 3; attempt += 1) {
                try {
                  return await github.rest.checks.update({
                    owner: context.repo.owner,
                    repo: context.repo.repo,
                    check_run_id: evidenceCheckRunId,
                    name: 'SFL Review Evidence',
                    external_id: gateExternalId,
                    status: 'completed',
                    conclusion: approved ? 'success' : 'failure',
                    completed_at: new Date().toISOString(),
                    output: {
                      title: approved
                        ? 'SFL zero-finding approval gate satisfied'
                        : 'SFL Reviewer approval is required',
                      summary,
                      text:
                        `replacement_retry_eligible=${replacementRetryEligible}`,
                    },
                  });
                } catch (error) {
                  lastError = error;
                  if (attempt < 3) {
                    core.warning(
                      `Evidence finalization attempt ${attempt} failed: ${error.message}`
                    );
                    await new Promise(resolve => setTimeout(resolve, attempt * 1000));
                  }
                }
              }
              throw lastError;
            };
            const publishFailure = async ({ reason }) => {
              await publishEvidence({
                approved: false,
                summary:
                  `${reason}. The review cannot satisfy the approval gate. ` +
                  `Upstream results: ${process.env.SFL_UPSTREAM_RESULT}.`,
              });
              core.setFailed(reason);
            };
            if (!baseMatchesExpected || !headMatchesExpected) {
              await publishFailure({
                reason:
                  `Pull request changed from base ${baseSha} / head ${headSha} ` +
                  `to base ${pullRequest.base.sha} / head ${pullRequest.head.sha}`,
              });
              return;
            }
            const summary = gateApproved
              ? 'The latest SFL review approved this head with zero inline findings.'
              : `The latest SFL review state is ${process.env.SFL_REVIEW_STATE || 'unavailable'} with ${process.env.SFL_FINDING_COUNT || 'unknown'} unresolved SFL finding thread(s), expected-head review match ${process.env.SFL_REVIEW_MATCHES_HEAD || 'unknown'}, live-base match ${baseMatchesExpected}, and live-head match ${headMatchesExpected}, so it does not satisfy the zero-finding approval gate. Upstream results: ${process.env.SFL_UPSTREAM_RESULT}.`;
            await publishEvidence({
              approved: gateApproved,
              summary,
            });
            if (!gateApproved) {
              core.setFailed(summary);
            }
---

## Full-Spectrum PR Review

## Required Configuration

This workflow uses a GitHub App for safe-outputs (posting inline review
comments). The following must be configured in the target repository:

| Kind | Name | Description |
| ---- | ---- | ----------- |
| Variable | `SFL_APP_CLIENT_ID` | Client ID of the GitHub App used for safe-outputs authentication |
| Secret | `SFL_APP_PRIVATE_KEY` | Private key (PEM) of the GitHub App used to mint installation tokens |
| Secret | `OPENROUTER_API_KEY` | HemSoft OpenRouter credential used to run Kimi K3 |

Set these under **Settings → Secrets and variables → Actions** in the target
repository. If they are missing, the token minting step will fail and review
comments will not be posted.

**Required Permissions**: Automatic reviews are dispatched by
`sfl-pr-review-auto.yml` with an SFL GitHub App installation token. Humans can
request an explicit rerun with:

```bash
gh workflow run sfl-pr-review.lock.yml \
  -f item_number=PR_NUMBER \
  -f base_sha=PULL_REQUEST_BASE_SHA \
  -f head_sha=PULL_REQUEST_HEAD_SHA \
  -f review_effort=low \
  -f 'aw_context={"item_type":"pull_request","item_number":PR_NUMBER,"base_sha":"PULL_REQUEST_BASE_SHA","head_sha":"PULL_REQUEST_HEAD_SHA"}'
```

---

You are an expert code reviewer performing a comprehensive 3-pass analysis of
this pull request. Your goal is to surface **real issues** — bugs, security
vulnerabilities, logic errors, and architectural concerns — not style nits.

## Setup

1. Get the PR diff and changed files:

   ```bash
   gh pr diff ${{ inputs.item_number }} --repo ${{ github.repository }}
   ```

2. Get the list of changed files:

   ```bash
   gh pr view ${{ inputs.item_number }} --repo ${{ github.repository }} --json files --jq '.files[].path'
   ```

3. Read the full content of each changed file to understand context beyond the
   diff (imports, class structure, related functions).

4. Treat `${{ inputs.base_sha }}` and `${{ inputs.head_sha }}` as the immutable
   expected base and head for this run. Read the live pull request base and head
   before reviewing. If either differs, stop without posting review output; the
   changed pull request state requires its own SFL run.

5. List the pull request's unresolved review threads before creating comments.
   Re-evaluate every unresolved SFL finding against the expected head. A prior
   finding is an unresolved inline comment authored by the SFL reviewer whose
   body starts with one of the exact severity prefixes below and contains the
   `SFL Reviewer` run footer. Carry forward findings that still apply. Exclude
   resolved findings. For every unresolved SFL finding that no longer applies
   and whose thread GitHub reports as outdated, call
   `resolve_sfl_review_thread` with its thread ID before submitting the current
   review. Never request resolution for a live unresolved thread; carry it
   forward until a maintainer resolves it or a later commit makes it outdated.

## Review Passes

Execute all 3 passes sequentially. For each finding, post an **inline review
comment** on the specific line using the `create_pull_request_review_comment`
tool.

### Severity Accounting

Every inline comment must represent **exactly one finding** and begin with
exactly one of these prefixes, replacing `{Pass}` with `Security`,
`Accuracy`, or `Quality & Maintainability`:

| Severity | Prefix | Definition |
| ---------- | -------- | ------------ |
| Critical | `🔴 **CRITICAL — {Pass}**` | Exploitable security issue, data loss, systemic outage, or release-blocking defect |
| High | `🟠 **HIGH — {Pass}**` | Likely production defect, serious security weakness, or major regression |
| Medium | `🟡 **MEDIUM — {Pass}**` | Material correctness, maintainability, or operational issue that should be fixed |
| Low | `⚪ **LOW — {Pass}**` | Minor but actionable improvement with limited risk |

Apply this same severity scale to all three passes. Do not combine multiple
findings into one inline comment. Maintain a ledger containing both new
findings successfully posted in this run and still-applicable unresolved SFL
findings carried from earlier runs, grouped by pass and severity. Do not post a
duplicate inline comment for a carried finding.

For each new finding, pass `side: RIGHT` for an added or context line and
`side: LEFT` for a deleted line. Always pass the side explicitly. End every new
inline finding with this run marker:

```markdown
---
*SFL Reviewer • {Pass} • {Severity} • [Run ${{ github.run_id }}](${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }})*
```

The run marker makes findings from repeated reviews distinguishable. New
findings use the current run ID; carried findings retain their original run
marker and are counted through the unresolved-finding ledger.

---

### Pass 1: 🔒 Security Review

Analyze for security vulnerabilities using OWASP Top 10 and CWE Top 25 as
the baseline. Look for:

| Category | Examples |
| ---------- | ---------- |
| Injection | SQL injection, command injection, XSS, SSTI |
| Authentication | Broken auth flows, weak token handling, session issues |
| Authorization | Missing access control, privilege escalation, IDOR |
| Data exposure | PII leaks, sensitive data in logs, missing encryption |
| Configuration | Hardcoded secrets, debug mode in prod, insecure defaults |
| Dependencies | Known vulnerable packages, outdated critical deps |
| Cryptography | Weak algorithms, improper key management, predictable random |
| Input validation | Missing validation, type confusion, buffer overflow |

Only report findings with **>80% confidence** of exploitability. Do not
flag theoretical risks that require implausible attack vectors.

---

### Pass 2: ✅ Accuracy & Reliability Review

Trace the changed behavior from inputs and state through outputs and failure
paths. Look for demonstrable defects:

| Area | What to check |
| ------ | --------------- |
| Logic | Incorrect conditions, calculations, ordering, or control flow |
| Edge cases | Empty, null, boundary, overflow, timeout, and partial-success behavior |
| State | Invalid transitions, stale state, race conditions, or non-idempotent retries |
| Error handling | Swallowed errors, incorrect recovery, misleading success, or missing cleanup |
| Data integrity | Lost, duplicated, corrupted, or partially committed data |
| Contracts | Implementations that violate explicit API, type, or behavioral contracts |

Only report a finding when you can explain a concrete input/state sequence and
the incorrect result it produces. Do not speculate about hypothetical runtime
conditions without a credible path through the changed code.

---

### Pass 3: 🧰 Quality & Maintainability Review

Evaluate whether the change remains understandable, modifiable, and testable:

| Area | What to check |
| ------ | --------------- |
| Cohesion | Functions or modules with unrelated responsibilities |
| Coupling | Hidden dependencies, brittle cross-module knowledge, or leaky abstractions |
| Duplication | Independently maintained logic that can drift or contradict itself |
| Complexity | Control flow or state handling that obscures behavior and increases defect risk |
| API clarity | Ambiguous contracts, surprising side effects, or inconsistent interfaces |
| Testability | Design choices that prevent important behavior from being isolated or verified |

Do **not** report formatting, import ordering, bracket placement, comment style,
or naming that follows repository conventions. Do not calculate or estimate
CRAP, coverage, scorecard, or other numeric scores without measured repository
artifacts.

### Conditional Evidence Checks

Use these only when the changed files provide direct evidence:

- **Tests**: Flag a gap only when changed behavior can be mapped to an existing
  test seam and a concrete regression case is missing.
- **Compatibility**: Check only when the PR changes a public API, schema,
  configuration key, serialized format, or persisted data contract.
- **Performance**: Report only concrete static problems such as N+1 queries,
  unbounded work, blocking I/O on a known hot path, or avoidable large
  allocations.
- **Migrations/rollout**: Check only when migration or deployment artifacts are
  changed and a specific forward/rollback failure is visible.

Classify applicable findings under Accuracy or Quality & Maintainability. If
evidence is unavailable, do not create a finding.

---

## Communication quality

Run the embedded Unslop pass on every user-facing sentence before each
`create_pull_request_review_comment` or `submit_pull_request_review` call. This
includes inline findings, the verdict reason, key findings, and any explanation
of the self-approval exception. Preserve the technical meaning, evidence,
severity, location, remediation, counts, and verdict.

Protocol literals override the Unslop rules. Keep the required severity
prefixes, verdict headings, summary heading, table labels, placeholders, run
markers, and footer exactly as specified, including their emoji, bold text, and
em dashes. Apply the rules below only to the natural-language prose around
those literals.

### Embedded Unslop pass

1. Scan the draft for the patterns below.
2. Rewrite it without changing technical meaning or evidence.
3. Add a direct human voice where it helps clarity.
4. Ask, "What makes this obviously AI generated?" Fix any remaining tells.

Use these writing rules:

- State what happened. Cut puffery, promotional language, vague claims, and
  formulaic framing.
- Name the source of a claim or remove the attribution. Do not write "experts
  believe" or "reports suggest" without a source.
- Remove superficial `-ing` phrases that add no evidence.
- Prefer plain words. Avoid AI stock words such as "additionally", "crucial",
  "delve", "enhance", "foster", "intricate", "pivotal", "showcase",
  "tapestry", "testament", "underscore", and abstract uses of "landscape".
- Use "is" and "has" instead of inflated substitutes such as "serves as",
  "stands as", "boasts", or "features".
- Do not use "not just X, but Y", forced groups of three, synonym cycling, or
  false "from X to Y" ranges.
- Do not use em dashes in free-form prose. Split the sentence or use a comma.
- Use colons only when they introduce a real list or example.
- Avoid decorative bold text, decorative emoji, title-case headings, and curly
  quotes in free-form prose.
- Remove chatbot filler, cutoff disclaimers, sycophancy, generic conclusions,
  and phrases such as "I hope this helps", "Of course", or "Great question".
- Cut filler. Use "to" instead of "in order to" and "because" instead of "due
  to the fact that". Remove "it is important to note that".
- Reduce hedging to the uncertainty the evidence requires.
- Replace abstract metaphor jargon such as "substrate", "wedge", "vector",
  "locus", "nexus", "primitive", "harness", "surface", "bedrock",
  "scaffolding", "modality", "paradigm", "gold-plating", "ratchet",
  "evacuate", "endgame", "north star", and "flywheel" with concrete terms.
- Say what the code does and what fails. Do not substitute mood or feeling for
  a mechanism, condition, path, command, or measured result.
- Keep one idea per sentence. Split sentences that require rereading.
- Prefer active voice. Name the actor when it matters.
- Cut adverbs that prop up weak verbs.
- Be specific and opinionated when the evidence supports it. Use "I" only when
  it sounds natural in a review comment.
- Vary sentence length, but do not make the prose chatty or theatrical.

Run this pass after the technical content is complete. It is a communication
filter, not permission to soften, omit, invent, or reclassify a finding.

---

## Summary

This is a hard completion contract. Do not end the run with prose-only output.
Before exiting, make exactly one successful `submit_pull_request_review` call.
If a defensible verdict cannot be produced because required evidence is
unavailable, call `missing_data` with the specific blocker instead; never call
`noop` and never silently stop.

After all 3 passes, submit a **pull request review** using
`submit_pull_request_review` with:

- **Event**:
  - Use `APPROVE` when the verdict is `APPROVE` and the pull request author is
    not `sfl-app[bot]`.
  - Use `COMMENT` when the verdict is `NEEDS WORK`.
  - Use `COMMENT` when the verdict is `APPROVE` but the pull request was authored
    by `sfl-app[bot]`, because GitHub does not allow an app to approve
    its own pull request. Explain this exception in the verdict reason.
- **Body**: A structured summary:

```markdown
## {verdict_icon} Verdict: {VERDICT}

{verdict_reason}

## 🔨 Full-Spectrum Review Summary

> **Review done by:** SFL_MODEL_PENDING<br>**Review time:** SFL_TIME_PENDING<br>**Token count:** SFL_AIC_PENDING AIC

| Pass | 🔴 Critical | 🟠 High | 🟡 Medium | ⚪ Low |
|------|------------:|--------:|----------:|-------:|
| 🔒 Security | {security_critical_or_dash} | {security_high_or_dash} | {security_medium_or_dash} | {security_low_or_dash} |
| ✅ Accuracy | {accuracy_critical_or_dash} | {accuracy_high_or_dash} | {accuracy_medium_or_dash} | {accuracy_low_or_dash} |
| 🧰 Quality & Maintainability | {quality_maintainability_critical_or_dash} | {quality_maintainability_high_or_dash} | {quality_maintainability_medium_or_dash} | {quality_maintainability_low_or_dash} |
| **Total** | **{total_critical_or_dash}** | **{total_high_or_dash}** | **{total_medium_or_dash}** | **{total_low_or_dash}** |

### Key Findings
{Top 3 most impactful current findings with their severity prefixes, or "No findings."}

---
*HemSoft full-spectrum review triggered automatically or by workflow dispatch • 3 evidence-based passes • [Run details](${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}) • SFL run ID: ${{ github.run_id }}*
```

Choose the verdict deterministically:

- If any new or still-applicable unresolved Critical, High, Medium, or Low
  finding exists, use
  `## ⚠️ Verdict: NEEDS WORK` and explain that every actionable finding must be
  resolved before merge.
- Use `## ✅ Verdict: APPROVE` only when all four severity totals are zero.

Before submitting, reconcile the ledger against new inline comments and
carried unresolved findings:

1. Count each successfully posted current-run inline finding exactly once.
2. Count each still-applicable unresolved SFL finding from an earlier run
   exactly once without duplicating its inline comment.
3. Resolve every earlier SFL thread that no longer applies and GitHub reports
   as outdated. Never request resolution for a live unresolved thread. Never
   approve while any SFL finding thread remains unresolved.
4. Never count resolved findings or duplicate reports of the same defect.
5. Every counted finding must have exactly one severity prefix.
6. Each table cell must equal the union of new and carried findings with that
   exact pass and severity prefix.
7. The Total row must equal the column sums and the complete current finding
   set.
8. Do not count positive observations, failed comment attempts, or summary-only
   notes as findings.
9. Render every zero count as an em dash (`—`). Render only nonzero counts as
   numbers. Keep the underlying numeric totals for verdict calculation.
10. Copy the `SFL_MODEL_PENDING`, `SFL_TIME_PENDING`, and `SFL_AIC_PENDING`
   placeholders exactly. A post-review job replaces them with finalized values.
11. Replace every other `{...}` template field with its actual value. Never
    submit literal brace or angle-bracket placeholders.
12. **Match event to verdict**: An `APPROVE` verdict must submit an `APPROVE`
   event unless GitHub's self-approval restriction applies. Never submit a
   `COMMENT` event for an eligible `APPROVE` verdict.

## Rules

1. **Be precise**: Comment on exact lines, not general areas.
2. **Be actionable**: Every finding must include a specific fix or recommendation.
3. **One comment, one finding**: Never combine independently actionable issues.
4. **Severity is mandatory**: Begin every inline finding with exactly one
   severity prefix from the Severity Accounting table.
5. **Be honest**: If confidence is below the pass threshold, do not post it.
6. **No false positives**: Better to miss a minor issue than flag a non-issue.
7. **Cross-reference**: If a finding in one pass relates to another, mention it.
8. **Respect context**: Read the full file, not just the diff. Understand the
   codebase patterns before flagging "violations."
9. **Pin the review**: All comments and the summary review must target
   `${{ inputs.head_sha }}` and apply only to base `${{ inputs.base_sha }}`.
   Never target a later pull request head or a different base.
10. **Close obsolete threads**: Resolve prior SFL threads only after verifying
    their findings no longer apply to the expected head and GitHub reports the
    thread as outdated. Never request resolution for a live unresolved thread.
11. **Accepted gate trust boundary**: The reviewer initializes and finalizes
    one `SFL Review Evidence` check on the immutable head with the
    repository-scoped `GITHUB_TOKEN` and exact
    `sfl-review:<pr>:<base>:<head>:<run>` locator. Because check fields are
    caller-controlled, neither App ID nor `external_id` authorizes a merge. The
    branch protection requires the GitHub Actions-owned native
    immutable-head `SFL Reviewer Approval` check; the default-branch
    `sfl-pr-review-auto.yml` gate runner authenticates the exact reviewer
    through the Actions API and publishes that check with the
    repository-scoped `GITHUB_TOKEN` only when the exact run succeeds.
    Maintainers who can modify workflows on the protected default branch are
    explicitly trusted under this first-version model. Defending against those
    maintainers requires a central publisher or OIDC broker and is deliberately
    out of scope. Still file any change that weakens this boundary, such as
    trusting check fields without Actions-run validation, dropping PR/base/head
    matching, or replacing the required workflow with a reusable head status.
