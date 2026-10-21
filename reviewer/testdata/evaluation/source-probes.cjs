const fs=require('node:fs'), assert=require('node:assert/strict');
const source=fs.readFileSync(process.argv[2],'utf8'),mode=process.argv[3];
if(mode==='edited-supersession'){
const name='REQUEST ELIGIBILITY',body=source.split('// BEGIN TESTABLE '+name)[1].split('// END TESTABLE '+name)[0];
const run=new Function('comments',`const baseMarker='<!-- sfl-codex-review:head=abc;base=def;',registeredRequestIds=new Set([1,2]);${body}\nreturn hasNewerRegisteredRequest(comments,registeredRequestIds,'ctx',{comment:{id:1},requestTime:Date.parse('2026-10-08T00:00:01Z')});`);
const comments=[{id:1,body:'@codex review\n\n<!-- sfl-codex-review:head=abc;base=def;context=ctx -->',created_at:'2026-10-08T00:00:01Z',updated_at:'2026-10-08T00:00:01Z'},{id:2,body:'erased request',created_at:'2026-10-08T00:00:02Z',updated_at:'2026-10-08T00:00:03Z'}];
assert.equal(run(comments),true,'edited newer registration must still supersede');
}else{
const needle='const headRepo = pull.head.repo && pull.head.repo.full_name;';
const start=source.indexOf(needle,source.indexOf('invalidate-pull-context:'));
assert(start>=0);const end=source.indexOf('const workflowRun =',start);assert(end>start);
const body=source.slice(start,end);const run=new Function('pull','context',`const owner='hemsoft-dev',repo='fixture',core={info:()=>{}};${body}\nreturn true;`);
assert.equal(run({head:{repo:{full_name:'hemsoft-dev/fixture'}},base:{ref:'release'}},{payload:{action:'edited',changes:{base:{ref:{from:'main'}}},repository:{default_branch:'main'}}}),true,'departure from default must invalidate');
}
