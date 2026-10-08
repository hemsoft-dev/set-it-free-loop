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
const invalidate = new AsyncFunction('github', 'context', 'core', 'setTimeout',
  unindent(source.slice(scriptStart + '          script: |\n'.length, scriptEnd)));

async function invalidationCase(name, body, edited, registered, oldTerminal, expected, action = edited ? 'edited' : 'created', permission = 'admin', expectedLookups = null) {
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
  const writes = [], failures = [], timers = [];
  let statusLookups = 0;
  const github = {rest: {
    repos: {getCollaboratorPermissionLevel: async ({username}) =>
      ({data: {permission, user: {login: username}}}),
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
    statusLookups += 1;
    return registered ? [{context: 'SFL Codex Review Request Registry',
      creator: {login: 'HemSoft'}, target_url: commentURL}] : [];
  }};
  const context = {repo: {owner: 'hemsoft-dev', repo: 'fixture'}, payload: {
    issue: {number: 42}, comment, repository: {default_branch: 'main'}, action}};
  await invalidate(github, context, {info: () => {}, setFailed: text => failures.push(text)},
    (resolve, duration) => { timers.push(duration); resolve(); });
  if (expectedLookups !== null) {
    assert.equal(statusLookups, expectedLookups, name);
    assert.equal(timers.length, 0, name);
  }
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


function titleForComment(id, body = 'ordinary comment') {
  const expression = /run-name: >-\n\s*\$\{\{([\s\S]*?)\}\}/.exec(source);
  assert(expression);
  return new Function('github', 'format', 'contains', `return ${expression[1]};`)(
    {event_name: 'issue_comment', event: {action: 'created', issue: {number: 42}, comment: {id, body}}},
    (pattern, ...values) => pattern.replace(/\{([0-9]+)\}/g, (_, index) => String(values[Number(index)])),
    (value, needle) => String(value || '').includes(needle));
}
async function activeCases() {
  let start = source.indexOf('function invalidationRequestCommentId(');
  if (start < 0) start = source.indexOf('function isActiveInvalidationRun(');
  const end = source.indexOf('function requestGateExternalIdPrefix(', start);
  assert(start >= 0 && end > start);
  const classify = new AsyncFunction('fixture', `
    const owner='hemsoft-dev',repo='fixture',pullNumber=42,currentHead='${head}',context={runId:99};
    const requesterAllowed=async login=>fixture.allowed.includes(login);
    const github={rest:{actions:{listWorkflowRuns:'runs'},repos:{listCommitStatusesForRef:'statuses'}},
      paginate:async(method,args)=>method==='runs'?(args.status==='queued'?fixture.runs:[]):fixture.statuses};
    ${unindent(source.slice(start,end))}
    ${block('ACTIVE INVALIDATIONS')}
    return (await activeInvalidationRuns()).map(run=>run.id);`);
  const run = (actor, id=101, markerIntent=false) => ({id:1,event:'issue_comment',status:'queued',actor:{login:actor},display_title:titleForComment(id, markerIntent ? exact : 'ordinary comment')});
  const registration = (creator='author',url=commentURL) => ({context:'SFL Codex Review Request Registry',creator:{login:creator},target_url:url});
  const cases = [
    ['ordinary administrator comment', [run('admin')], [], ['admin'], []],
    ['writer request before registry visibility', [run('admin',101,true)], [], ['admin'], [1]],
    ['reader marker before registry visibility', [run('reader',101,true)], [], [], []],
    ['registered administrator request', [run('admin')], [registration('admin')], ['admin'], [1]],
    ['revoked registered author', [run('author')], [registration()], [], [1]],
    ['unprivileged other actor', [run('reader')], [registration()], [], []],
    ['administrator deleting another author request', [run('admin')], [registration()], ['admin'], [1]],
    ['different PR registration', [run('admin')], [registration('admin',commentURL.replace('/42#','/43#'))], ['admin'], []],
    ['unregistered comment ID', [run('admin',102)], [registration('admin')], ['admin'], []],
    ['blank registration creator', [run('admin')], [registration('')], ['admin'], []],
    ['push and target invalidations', [
      {id:2,event:'push',status:'queued'},
      {id:3,event:'pull_request_target',status:'queued',pull_requests:[{number:42}]},
      {id:4,event:'pull_request_target',status:'queued',pull_requests:[{number:43}]},
      {id:99,event:'push',status:'queued'}], [], [], [2,3]],
    ['completed comment run', [{...run('admin'),status:'completed'}], [registration('admin')], ['admin'], []],
    ['legacy writer run', [{...run('admin'),display_title:'SFL Codex review request #42'}], [], ['admin'], [1]],
  ];
  for (const [name,runs,statuses,allowed,want] of cases) {
    assert.deepEqual(await classify({runs,statuses,allowed}),want,name);
  }
}
function retargetCases() {
  const supported = new Function('pull','context','headRepo','owner','repo',
    `${block('SUPPORTED PULL CONTEXT')}\nreturn supportedPullContext;`);
  const event = (ref, from, headRepo='hemsoft-dev/fixture') => supported(
    {base:{ref}}, {payload:{action:'edited',changes:{base:{ref:{from}}},repository:{default_branch:'main'}}},headRepo,'hemsoft-dev','fixture');
  assert.equal(event('release-b','release-a'),false,'nondefault-to-nondefault retarget');
  assert.equal(event('release','main'),true,'departure from default invalidates its previous review');
  assert.equal(event('main','release'),true,'retarget to default remains supported');
  assert.equal(event('release',undefined),false,'missing original base cannot grant admission');
  assert.equal(event('release','main','fork/fixture'),false,'fork remains unsupported');
}

const repairStart = source.indexOf('const terminalState = await publicationState();');
const repairEnd = source.indexOf('const detailsURL = artifact.html_url', repairStart);
assert(repairStart >= 0 && repairEnd > repairStart);
const repairFlow = new AsyncFunction('fixture', 'snapshot', `
const owner='hemsoft-dev',repo='fixture',currentHead='${head}',currentBase='${base}',pullNumber=42,contextToken='none';
const latestRequestChecks=fixture.checks,latestRequestStatuses=fixture.statuses,externalIdPrefix=fixture.repairPrefix;
const artifact={html_url:'https://github.com/hemsoft-dev/fixture/pull/42'},pull={data:artifact};
const core={info:()=>{},setFailed:reason=>fixture.failures.push(reason)};
const github={rest:{repos:{createCommitStatus:async data=>{
  fixture.writes.push(data);
  if(data.state==='failure'&&fixture.blockingErrors?.length)throw fixture.blockingErrors.shift();
  if(data.state==='success'&&fixture.onSuccess)fixture.onSuccess();
  if(data.state==='success'&&fixture.successWriteError)throw fixture.successWriteError;
}}}};
const publicationState=async()=>{
  fixture.snapshots++;
  if(fixture.beforeSnapshot)fixture.beforeSnapshot(fixture.snapshots);
  return await snapshot(fixture);
};
${block('REQUIRED GATE REPAIR')}
${unindent(source.slice(stateEnd, repairStart))}
${unindent(source.slice(repairStart, repairEnd))}`);

async function repairCases(baseFixture, first, selected) {
  const repairPrefix=`sfl-codex-review:pull:42:base:${base}:context:none:request:2:at:${Date.parse(selected.created_at)}`;
  const make=()=>({...baseFixture,comments:[first,selected],writes:[],failures:[],snapshots:0,repairPrefix,
    checks:[...baseFixture.checks,{app:{id:15368},status:'completed',conclusion:'success',
      external_id:repairPrefix+':artifact:c2',completed_at:'2026-10-08T00:00:04Z'}]});
  const stable=make();await repairFlow(stable,snapshot);
  assert.deepEqual(stable.writes.map(write=>write.state),['success'],'stable terminal repair');
  const alreadyCorrect=make();alreadyCorrect.statuses=[...alreadyCorrect.statuses,
    {context:'SFL Reviewer Gate Runner',creator:{login:'github-actions[bot]'},state:'success'}];
  await repairFlow(alreadyCorrect,snapshot);assert.equal(alreadyCorrect.writes.length,0,'already-correct status remains untouched');
  for(const [name,comments] of [
    ['prior registered request edited',[{...first,updated_at:'2026-10-08T00:00:06Z'},selected]],
    ['prior registered request deleted',[selected]],
    ['selected registered request deleted',[first]],
    ['selected registered request erased',[first,{...selected,body:'erased'}]],
  ]) {
    const during=make();during.onSuccess=()=>{during.comments=comments;};
    await repairFlow(during,snapshot);
    assert.deepEqual(during.writes.map(write=>write.state),['success','failure'],name);
    assert.equal(during.failures.length,1,name);
    assert(during.snapshots>=2,name);
  }
  for(const transientFailures of [0,1,2,3]) {
    const broken=make();
    const originalError=new Error('actual post-write snapshot API failure');
    broken.onSuccess=()=>{broken.comments=[selected];};
    broken.beforeSnapshot=count=>{if(count===3)throw originalError;};
    broken.blockingErrors=Array.from({length:transientFailures},()=>new Error('blocking status API failure'));
    await assert.rejects(repairFlow(broken,snapshot),error=>error===originalError,
      'preserve the post-write verification failure');
    assert.deepEqual(broken.writes.map(write=>write.state),
      ['success',...Array(Math.min(transientFailures+1,3)).fill('failure')],
      'restore a blocking status with bounded retries after API rejection');
    assert.equal(broken.failures.length,1,'verification errors cannot become success');
  }
  const ambiguousWrite=make();
  const originalWriteError=new Error('success status accepted but response failed');
  ambiguousWrite.successWriteError=originalWriteError;
  await assert.rejects(repairFlow(ambiguousWrite,snapshot),error=>error===originalWriteError);
  assert.deepEqual(ambiguousWrite.writes.map(write=>write.state),['success','failure'],
    'an ambiguous success write must restore the blocking status');
  assert.equal(ambiguousWrite.failures.length,1);
  const before=make();before.beforeSnapshot=count=>{
    if(count===2)before.comments=[selected];
  };
  await repairFlow(before,snapshot);assert.equal(before.writes.length,0,'changed history before repair must prevent the success write');
}

const normalEnd=source.indexOf('\n  invalidate-pull-context:',repairEnd);
assert(normalEnd>repairEnd);
const normalFlow=new AsyncFunction('fixture','snapshot','Buffer',`
const owner='hemsoft-dev',repo='fixture',currentHead='${head}',currentBase='${base}',pullNumber=42,contextToken='none';
const artifact={html_url:'https://github.com/hemsoft-dev/fixture/pull/42'},pull={data:artifact};
const result={action:'success',reason:'authenticated clean'},title='clean',externalId='fixture';
const context={payload:{repository:{id:123}},runId:456,sha:currentBase,eventName:'issue_comment'};
const core={info:()=>{},setFailed:reason=>fixture.failures.push(reason)};
const github={rest:{checks:{create:async data=>({data:{id:789}}),update:async data=>{
  fixture.events.push('check:'+data.conclusion);
  if(data.conclusion==='success'&&fixture.successCheckError)throw fixture.successCheckError;
  if(data.conclusion==='failure'&&fixture.failureCheckError)throw fixture.failureCheckError;
}},repos:{createCommitStatus:async data=>{
  fixture.writes.push(data);fixture.events.push('status:'+data.state);
  if(data.state==='failure'&&fixture.blockingErrors?.length)throw fixture.blockingErrors.shift();
  if(data.state==='success'&&fixture.onSuccess)fixture.onSuccess();
  if(data.state==='success'&&fixture.successWriteError)throw fixture.successWriteError;
  return {data:{id:890}};
}}}};
const publicationState=async()=>{
  fixture.snapshots++;
  if(fixture.beforeSnapshot)fixture.beforeSnapshot(fixture.snapshots);
  return await snapshot(fixture);
};
${block('REQUIRED GATE TARGET')}
${unindent(source.slice(stateEnd,repairStart))}
${unindent(source.slice(repairEnd,normalEnd))}`);

async function publicationCases(baseFixture,first,selected) {
  const make=()=>({...baseFixture,comments:[first,selected],writes:[],events:[],failures:[],snapshots:0});
  const stable=make();await normalFlow(stable,snapshot,Buffer);
  assert.deepEqual(stable.writes.map(x=>x.state),['pending','success']);
  for(const retries of [0,1,2,3]) {
    const broken=make(),original=new Error('post-success GitHub snapshot failure');
    broken.onSuccess=()=>{broken.comments=[selected];};
    broken.beforeSnapshot=count=>{if(count===2)throw original;};
    broken.blockingErrors=Array.from({length:retries},()=>new Error('blocking status API failure'));
    broken.failureCheckError=new Error('failure audit API failure');
    await assert.rejects(normalFlow(broken,snapshot,Buffer),error=>error===original);
    assert.deepEqual(broken.writes.map(x=>x.state),['pending','success',...Array(Math.min(retries+1,3)).fill('failure')]);
    assert(broken.events.indexOf('status:failure')<broken.events.indexOf('check:failure'),
      'audit API failures cannot prevent required-status invalidation');
    assert.equal(broken.failures.length,1);
  }
  const changed=make();changed.onSuccess=()=>{changed.comments=[selected];};
  await normalFlow(changed,snapshot,Buffer);
  assert.deepEqual(changed.writes.map(x=>x.state),['pending','success','failure']);
  assert.equal(changed.failures.length,1);
  const auditFailure=make(),auditError=new Error('success check update 404');
  auditFailure.successCheckError=auditError;
  await assert.rejects(normalFlow(auditFailure,snapshot,Buffer),error=>error===auditError);
  assert.deepEqual(auditFailure.writes.map(x=>x.state),['pending','failure']);
  const ambiguous=make(),writeError=new Error('success status accepted but response failed');
  ambiguous.successWriteError=writeError;
  await assert.rejects(normalFlow(ambiguous,snapshot,Buffer),error=>error===writeError);
  assert.deepEqual(ambiguous.writes.map(x=>x.state),['pending','success','failure']);
}

(async () => {
  const mode = process.argv[3] || 'all';
  assert(['all', 'probe-invalidation', 'probe-history', 'probe-revoked', 'probe-active', 'probe-retarget', 'probe-polling', 'probe-empty', 'probe-repair', 'probe-publication'].includes(mode));
  if (['all', 'probe-polling'].includes(mode)) {
    for (const action of ['created', 'edited', 'deleted']) {
      await invalidationCase(`unprivileged marker ${action}`, exact, action === 'edited', false, false, null, action, 'read', 1);
    }
  }
  if (['all', 'probe-active'].includes(mode)) await activeCases();
  if (['all', 'probe-retarget'].includes(mode)) retargetCases();
  if (['all', 'probe-revoked'].includes(mode)) {
    for (const [action, body] of [['edited', 'erased request'], ['deleted', exact], ['created', exact]]) {
      await invalidationCase(`revoked registered author ${action}`, body, action === 'edited', true, true, 'failure', action, 'read');
    }
    await invalidationCase('revoked unregistered author', 'ordinary comment', false, false, false, null, 'created', 'read');
  }
  if (mode.startsWith('probe-') && !['probe-invalidation', 'probe-history', 'probe-empty', 'probe-repair', 'probe-publication'].includes(mode)) return;
  if (!['probe-history', 'probe-empty', 'probe-repair', 'probe-publication'].includes(mode)) {
  if (mode === 'all') {
  assert.match(source, /issue_comment:\s*types: \[created, edited, deleted\]/);
  const title = /run-name: >-\n\s*\$\{\{([\s\S]*?)\}\}/.exec(source);
  assert(title);
  const titleFor = new Function('github', 'format', 'contains', `return ${title[1]};`);
  const format = (pattern, ...values) => pattern.replace(/\{([0-9]+)\}/g, (_, index) => String(values[Number(index)]));
  for (const action of ['created', 'edited', 'deleted']) {
    for (const body of [exact, ` ${exact}`, 'erased request']) {
      const github = {event_name: 'issue_comment', event: {action, issue: {number: 42}, comment: {id: 101, body}}};
      assert.equal(titleFor(github, format, (value, needle) => String(value || '').includes(needle)),
        body.includes('<!-- sfl-codex-review:head=') ? 'SFL Codex comment #42 request 101' : 'SFL Codex comment #42 comment 101');
    }
  }
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
  assert.equal((await snapshot({...fixture, comments: [selected]})).requestStillAuthorized, false,
    'deleted historical registration must block a fresh snapshot');
  for (const changed of [{...first, updated_at: '2026-10-08T00:00:06Z'},
    {...first, body: ` ${first.body}`}, {...first, body: 'erased request'}]) {
    assert.equal((await snapshot({...fixture, comments: [changed, selected]})).requestStillAuthorized, false);
  }
  if (['all','probe-empty'].includes(mode)) {
    for(const context of ['', ' ', '\t']) {
      const malformed={...first,body:first.body.replace('context=none',`context=${context}`)};
      assert.equal((await snapshot({...fixture,comments:[malformed,selected]})).requestStillAuthorized,false,
        'empty or whitespace historical context must block a later request');
    }
  }
  if (['all','probe-repair'].includes(mode)) await repairCases(fixture,first,selected);
  if (['all','probe-publication'].includes(mode)) await publicationCases(fixture,first,selected);
  console.log('Production invalidation and fresh historical-publication regressions passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
