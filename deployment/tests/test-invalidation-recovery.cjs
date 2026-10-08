const fs = require('node:fs');
const assert = require('node:assert/strict');
const source = fs.readFileSync(process.argv[2], 'utf8');
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
function job(name, next) {
  const start = source.indexOf(`\n  ${name}:`);
  const end = next ? source.indexOf(`\n  ${next}:`, start) : source.length;
  const script = source.indexOf('          script: |\n', start);
  assert(start >= 0 && script > start && end > script);
  return new AsyncFunction('github', 'context', 'core',
    source.slice(script + '          script: |\n'.length, end).replace(/^ {12}/gm, ''));
}
const head = 'a'.repeat(40), base = 'b'.repeat(40), changedBase = 'c'.repeat(40);
const created = '2026-10-08T00:00:00Z', runId = 456;
const pull = {number:42,state:'open',head:{sha:head,repo:{full_name:'hemsoft-dev/fixture'}},
  base:{ref:'release',sha:base},html_url:'https://github.com/hemsoft-dev/fixture/pull/42'};
const context = {repo:{owner:'hemsoft-dev',repo:'fixture'},runId,ref:'refs/heads/main',
  payload:{action:'edited',repository:{default_branch:'main'},pull_request:pull,
    changes:{base:{ref:{from:'main'}}}}};
async function run(name, checks, live, baseAdvance=false) {
  const writes=[];
  const github={rest:{actions:{getWorkflowRun:async()=>({data:{created_at:created}})},
    checks:{listForRef:()=>{},create:async v=>{writes.push({kind:'check',...v});}},
    pulls:{get:async()=>({data:live}),list:()=>{}},
    repos:{createCommitStatus:async v=>{writes.push({kind:'status',...v});}}},
    paginate:async fn=>fn===github.rest.pulls.list?[live]:checks};
  await job(name,baseAdvance?null:'invalidate-base-advance')(github,context,{info:()=>{}});
  return writes;
}
(async()=>{
  const failures=[];
  async function verify(name, test){try{await test();console.log('PASS',name);}catch(error){failures.push(name);console.error('FAIL',name,error.message);}}
  await verify('departure still invalidates after a second nondefault base edit',async()=>{
    const writes=await run('invalidate-pull-context',[],{...pull,base:{ref:'other',sha:changedBase}});
    assert(writes.some(x=>x.kind==='status'&&x.state==='failure'&&x.sha===head));
  });
  await verify('existing pull invalidation repairs missing blocking status',async()=>{
    const checks=[{app:{id:15368},external_id:`sfl-codex-review:pull-context:at:${Date.parse(created)}:${runId}`}];
    const writes=await run('invalidate-pull-context',checks,pull);
    assert(writes.some(x=>x.kind==='status'&&x.state==='failure'));
    assert(!writes.some(x=>x.kind==='check'),'existing check is retained');
  });
  await verify('existing base invalidation repairs missing blocking status',async()=>{
    const live={...pull,base:{ref:'main',sha:base}};
    const checks=[{app:{id:15368},external_id:`sfl-codex-review:base-advance:at:${Date.parse(created)}:${runId}:42`}];
    const writes=await run('invalidate-base-advance',checks,live,true);
    assert(writes.some(x=>x.kind==='status'&&x.state==='failure'));
    assert(!writes.some(x=>x.kind==='check'),'existing check is retained');
  });
  await verify('unsupported nondefault retarget stays read-only',async()=>{
    const saved=context.payload.changes.base.ref.from;
    context.payload.changes.base.ref.from='other';
    try {assert.deepEqual(await run('invalidate-pull-context',[],pull),[]);}
    finally {context.payload.changes.base.ref.from=saved;}
  });
  await verify('departure does not invalidate a different live head',async()=>{
    assert.deepEqual(await run('invalidate-pull-context',[],{...pull,head:{...pull.head,sha:'d'.repeat(40)}}),[]);
  });
  for (const [name,next,baseAdvance] of [['invalidate-pull-context','invalidate-base-advance',false],['invalidate-base-advance',null,true]]) {
    await verify(`${name} preserves a newer successful review`,async()=>{
      const live=baseAdvance?{...pull,base:{ref:'main',sha:base}}:pull;
      const token=baseAdvance?`sfl-codex-review:base-advance:at:${Date.parse(created)}:${runId}:42`:`sfl-codex-review:pull-context:at:${Date.parse(created)}:${runId}`;
      const checks=[{app:{id:15368},external_id:token},{app:{id:15368},conclusion:'success',
        external_id:`sfl-codex-review:pull:42:base:${base}:context:${encodeURIComponent(token)}:request:101:at:1:artifact:c2`}];
      assert.deepEqual(await run(name,checks,live,baseAdvance),[]);
    });
    await verify(`${name} preserves a superseding context`,async()=>{
      const live=baseAdvance?{...pull,base:{ref:'main',sha:base}}:pull;
      const checks=[{app:{id:15368},external_id:`sfl-codex-review:pull-context:at:${Date.parse(created)+1}:${runId+1}`}];
      assert.deepEqual(await run(name,checks,live,baseAdvance),[]);
    });
  }
  await verify('delayed base event does not invalidate a pull opened afterward',async()=>{
    const live={...pull,created_at:'2026-10-08T00:00:01Z',base:{ref:'main',sha:base}};
    assert.deepEqual(await run('invalidate-base-advance',[],live,true),[]);
  });
  await verify('base event still invalidates a previously open pull',async()=>{
    const live={...pull,created_at:'2026-10-07T23:59:59Z',base:{ref:'main',sha:base}};
    const writes=await run('invalidate-base-advance',[],live,true);
    assert(writes.some(x=>x.kind==='status'&&x.state==='failure'));
  });
  await verify('existing registered pending check repairs its missing commit status',async()=>{
    const token=`sfl-codex-review:pull-context:at:${Date.parse(created)}:${runId}`;
    const url=pull.html_url+'#issuecomment-101';
    const comment={id:101,user:{login:'HemSoft'},created_at:created,updated_at:created,html_url:url,
      body:`@codex review\n\n<!-- sfl-codex-review:head=${head};base=${base};context=${token} -->`};
    const checks=[{id:100,app:{id:15368},external_id:token,output:{text:JSON.stringify({schema:1,pull_number:42,action:'opened',base_sha:base})}},
      {id:101,app:{id:15368},external_id:'sfl-codex-review:request-pending:101'}];
    const writes=[];
    const github={rest:{issues:{getComment:async()=>({data:comment})},
      pulls:{get:async()=>({data:{...pull,base:{ref:'main',sha:base}}})},
      checks:{listForRef:'checks',create:async v=>writes.push({kind:'check',...v})},
      repos:{getCollaboratorPermissionLevel:async()=>({data:{permission:'admin',user:{login:'HemSoft'}}}),
        listCommitStatusesForRef:'statuses',createCommitStatus:async v=>writes.push({kind:'status',...v})}},
      paginate:async method=>method==='checks'?checks:[{context:'SFL Codex Review Request Registry',creator:{login:'HemSoft'},target_url:url}]};
    await job('invalidate-review-request','observe')(github,{repo:context.repo,payload:{action:'created',issue:{number:42},comment,repository:{default_branch:'main'}}},{info:()=>{},setFailed:()=>{}});
    assert(writes.some(x=>x.kind==='status'&&x.state==='pending'));
    assert(!writes.some(x=>x.kind==='check'),'existing pending check is retained');
  });
  if(failures.length)throw new Error(`${failures.length} invalidation recovery cases failed`);
})().catch(error=>{console.error(error);process.exitCode=1;});
