const fs = require('node:fs');
const assert = require('node:assert/strict');
const source = fs.readFileSync(process.argv[2], 'utf8');
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const unindent = text => text.replace(/^ {12}/gm, '');
const block = name => {
  const begin = `// BEGIN TESTABLE ${name}`;
  const end = `// END TESTABLE ${name}`;
  const start = source.indexOf(begin), finish = source.indexOf(end, start);
  assert(start >= 0 && finish > start, name);
  return unindent(source.slice(start + begin.length, finish));
};
const head = 'a'.repeat(40), base = 'b'.repeat(40);
const token = 'sfl-codex-review:pull-context:at:123:456';
const marker = `<!-- sfl-codex-review:head=${head};base=${base};context=${token} -->`;
const exact = `@codex review\n\n${marker}`;
const commentURL = 'https://github.com/hemsoft-dev/fixture/pull/42#issuecomment-101';
const scriptStart = source.indexOf('          script: |\n');
const scriptEnd = source.indexOf('\n  observe:', scriptStart);
assert(scriptStart >= 0 && scriptEnd > scriptStart);
const invalidate = new AsyncFunction('github', 'context', 'core',
  unindent(source.slice(scriptStart + '          script: |\n'.length, scriptEnd)));

async function invalidationCase(name, body, edited, registered, oldTerminal, expected, action = edited ? 'edited' : 'created') {
  const comment = {id: 101, body, user: {login: 'HemSoft'}, html_url: commentURL,
    created_at: '2026-10-08T00:00:01Z',
    updated_at: edited ? '2026-10-08T00:00:02Z' : '2026-10-08T00:00:01Z'};
  const contextCheck = {app: {id: 15368}, id: 100, external_id: token,
    output: {text: JSON.stringify({schema: 1, pull_number: 42, action: 'opened', base_sha: base})}};
  const checks = [contextCheck];
  if (oldTerminal) checks.push({app: {id: 15368}, status: 'completed',
    conclusion: 'success', external_id: 'prior:request:101:at:1:artifact:c2'});
  // A prior pending check must not hide a later edit either.
  if (oldTerminal) checks.push({app: {id: 15368}, external_id: 'sfl-codex-review:request-pending:101'});
  const writes = [], failures = [];
  const github = {rest: {
    repos: {getCollaboratorPermissionLevel: async ({username}) =>
      ({data: {permission: 'admin', user: {login: username}}}),
      listCommitStatusesForRef: 'statuses', createCommitStatus: async data => writes.push(['status', data])},
    issues: {getComment: async () => {
      if (action === 'deleted') throw Object.assign(new Error('deleted'), {status: 404});
      return {data: comment};
    }},
    pulls: {get: async () => ({data: {state: 'open', head: {sha: head, repo: {full_name: 'hemsoft-dev/fixture'}},
      base: {sha: base, ref: 'main'}}})},
    checks: {listForRef: 'checks', create: async data => writes.push(['check', data])},
  }, paginate: async method => {
    if (method === 'checks') return checks;
    assert.equal(method, 'statuses');
    return registered ? [{context: 'SFL Codex Review Request Registry',
      creator: {login: 'HemSoft'}, target_url: commentURL}] : [];
  }};
  const context = {repo: {owner: 'hemsoft-dev', repo: 'fixture'}, payload: {
    issue: {number: 42}, comment, repository: {default_branch: 'main'}, action}};
  await invalidate(github, context, {info: () => {}, setFailed: text => failures.push(text)});
  if (expected === null) { assert.equal(writes.length, 0, name); return; }
  assert.equal(writes.length, 2, name);
  const check = writes.find(([kind]) => kind === 'check')[1];
  const status = writes.find(([kind]) => kind === 'status')[1];
  assert.equal(check.head_sha, head, name);
  assert.equal(check.conclusion, 'failure', name);
  assert.equal(status.sha, head, name);
  assert.equal(status.state, expected, name);
  assert.equal(failures.length, 1, name);
}

const stateStart = source.indexOf('const publicationState = async () => {');
const stateEnd = source.indexOf('const publicationChanged = state =>', stateStart);
assert(stateStart >= 0 && stateEnd > stateStart);
const prefixStart = source.indexOf('function requestGateExternalIdPrefix(');
const prefixEnd = source.indexOf('function requestGateExternalId(', prefixStart);
assert(prefixStart >= 0 && prefixEnd > prefixStart);
const snapshot = new AsyncFunction('fixture', `
const owner='hemsoft-dev',repo='fixture',pullNumber=42,currentHead='${head}',currentBase='${base}',contextToken='none';
const baseMarker='<!-- sfl-codex-review:head=${head};base=${base};';
const requestTargetPrefix='https://github.com/hemsoft-dev/fixture/pull/42#issuecomment-';
const triggerComments=fixture.initial,request=fixture.request,artifactTime=fixture.artifactTime;
const registeredRequestIds=new Set([1,2]);
const isOwnerRequest=()=>true,refreshRequesterPermissions=async()=>{};
const openPullsForCurrentHead=async()=>[{number:42}],pullLifecycleToken=async()=> 'same';
const initialLifecycleToken='same',activeInvalidationRuns=async()=>[];
const reviewContextToken=()=> 'none',reviewContextUnambiguous=()=>true,hasNewerMatchingRequest=()=>false;
const github={rest:{pulls:{get:async()=>({data:{state:'open',head:{sha:currentHead},base:{sha:currentBase}}})},
  issues:{listComments:'comments'},checks:{listForRef:'checks'},repos:{listCommitStatusesForRef:'statuses'}},
  paginate:async method=>fixture[method]};
${unindent(source.slice(prefixStart, prefixEnd))}
${block('REQUEST REGISTRATION')}
${block('REQUEST ELIGIBILITY')}
if(!eligibleReviewRequests(triggerComments,fixture.checks,artifactTime,registeredRequestIds).some(c=>c.comment.id===2)){
  throw new Error('Initial request history did not qualify the selected request');
}
${unindent(source.slice(stateStart, stateEnd))}
return await publicationState();`);

(async () => {
  const mode = process.argv[3] || 'all';
  assert(['all', 'probe-invalidation', 'probe-history'].includes(mode));
  if (mode !== 'probe-history') {
  if (mode === 'all') {
  assert.match(source, /issue_comment:\s*types: \[created, edited, deleted\]/);
  const condition = /invalidate-review-request:[\s\S]*?if: >-\n([\s\S]*?)\n    runs-on:/.exec(source);
  assert(condition);
  const admits = new Function('github', 'vars', `return Boolean(${condition[1]});`);
  for (const action of ['created', 'edited', 'deleted']) {
    const github = {event_name: 'issue_comment', event: {action, issue: {pull_request: {}}, comment: {body: 'erased'}}};
    assert.equal(admits(github, {SFL_ENABLED: 'true'}), true);
    assert.equal(admits(github, {SFL_ENABLED: 'false'}), false);
  }
  }
  await invalidationCase('exact new request', exact, false, true, false, 'pending');
  await invalidationCase('exact completed request replay', exact, false, true, true, null);
  await invalidationCase('ordinary unregistered comment', 'ordinary comment', false, false, false, null);
  for (const [name, body, edited] of [['leading whitespace', ` ${exact}`, false],
    ['trailing whitespace', `${exact}\n`, false], ['edited exact body', exact, true],
    ['erased marker', 'erased request', true]]) {
    await invalidationCase(name, body, edited, true, true, 'failure');
  }
  await invalidationCase('deleted registered request', exact, false, true, true, 'failure', 'deleted');
  if (mode === 'probe-invalidation') return;
  }
  const requestComment = (id, second) => ({id, user: {login: 'HemSoft'},
    body: `@codex review\n\n<!-- sfl-codex-review:head=${head};base=${base};context=none -->`,
    created_at: `2026-10-08T00:00:0${second}Z`, updated_at: `2026-10-08T00:00:0${second}Z`});
  const first = requestComment(1, 1), selected = requestComment(2, 3);
  const prefix = `sfl-codex-review:pull:42:base:${base}:context:none:request:1:at:${Date.parse(first.created_at)}`;
  const fixture = {initial: [first, selected], request: {comment: selected},
    artifactTime: Date.parse('2026-10-08T00:00:05Z'),
    checks: [{app: {id: 15368}, status: 'completed', completed_at: '2026-10-08T00:00:02Z',
      external_id: `${prefix}:artifact:c1`}], statuses: [1, 2].map(id => ({
      context: 'SFL Codex Review Request Registry', creator: {login: 'HemSoft'},
      target_url: `https://github.com/hemsoft-dev/fixture/pull/42#issuecomment-${id}`}))};
  assert.equal((await snapshot({...fixture, comments: [first, selected]})).requestStillAuthorized, true);
  for (const changed of [{...first, updated_at: '2026-10-08T00:00:06Z'},
    {...first, body: ` ${first.body}`}, {...first, body: 'erased request'}]) {
    assert.equal((await snapshot({...fixture, comments: [changed, selected]})).requestStillAuthorized, false);
  }
  console.log('Production invalidation and fresh historical-publication regressions passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
