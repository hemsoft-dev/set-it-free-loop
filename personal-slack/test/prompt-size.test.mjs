import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync,rmSync,readFileSync,writeFileSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {Receiver,signOperation} from '../receiver.mjs';
import {Store,hash,enqueue} from '../store.mjs';
import {EXPECTED} from '../run.mjs';

const LIMIT=120000;
const payload=(ts='1700000000.000001',extra={})=>({type:'event_callback',team_id:EXPECTED.team,api_app_id:EXPECTED.app,
  event:{type:'message',channel_type:'im',channel:'DOWNER',user:EXPECTED.user,ts,text:'Help with this message',...extra}});
const linked=()=>({finished:true,references:[],errors:[],snapshots:[{complete:true,finished:true,files:[],
  messages:Array.from({length:7},(_,i)=>({ts:`1700000000.00000${i}`,text:'x'.repeat(18000)}))}]});

function fixture(t,{settled=false,ambiguous=false}={}) {
  const root=mkdtempSync(join(tmpdir(),'sfl-packet-bound-'));t.after(()=>rmSync(root,{recursive:true,force:true}));
  const f={root,writes:[],replies:[],ambiguous};
  const config={projectId:'personal',workspace:root,chief:'C:/reviewed-chief',helper:join(root,'run.mjs'),qualification:{providers:['codex']}};
  const gate=async()=>({claims:{},model:{instanceId:'codex',model:'gpt-6.1-sol'}});
  const available=async()=>({thread:{threadId:'owner-1',settled,snoozed:false,pendingRequestCount:0}});
  const slack={verifyDM:async()=>{},reaction:async()=>{},api:async(method,args)=>{f.replies.push({method,args});return{ts:'1700000002.000003'};}};
  const t3={call:async(tool,args)=>{
    if(!['t3_thread_launch','t3_thread_send'].includes(tool))throw Error('No native scan authorized in this fixture');
    f.writes.push({tool,args:structuredClone(args)});
    // Native schema rejects before a message exists, but the reviewed transport
    // cannot establish definitely-unsent from an execute-stage error.
    if(args.message.length>LIMIT)throw Error('SchemaRejectedAtExecute');
    if(f.ambiguous)throw Error('T3TransportUncertain');
    return{threadId:'owner-1',runId:'run-1'};
  }};
  f.restart=()=>{f.store=new Store(root);f.receiver=new Receiver({store:f.store,config,identity:EXPECTED,slack,t3,gate,available});};
  f.request=(ts,extra={})=>{const key=f.receiver.accept(payload(ts,extra));const e=f.store.state.requests[key];e.linked=linked();return e;};
  f.restart();return f;
}

for(const mode of ['launch','continuation','settled'])test(`oversized ${mode} reaches an owner within native size bound without impossible uncertainty`,async t=>{
  const f=fixture(t,{settled:mode==='settled'});
  let root;
  if(mode!=='launch'){
    root=f.request();root.linked={finished:true,references:[],errors:[],snapshots:[]};await f.receiver.pump();
    if(mode==='settled'){
      root.linked=linked();root.outcome={kind:'success',operation:'old-answer'};
      f.store.state.operations['old-answer']={text:'a'.repeat(35000),status:'delivered'};f.receiver.save();
    }
  }
  const e=f.request(mode==='launch'?undefined:'1700000001.000002',mode==='launch'?{}:{thread_ts:root.source.ts});
  const original=JSON.stringify(e.linked);await f.receiver.pump();
  assert.ok(f.writes.every(w=>w.args.message.length<=LIMIT),`outgoing lengths ${f.writes.map(w=>w.args.message.length)}`);
  assert.equal(e.status,'delivered');assert.equal(e.deliveryState,'sent');
  assert.equal(JSON.stringify(e.linked),original);assert.equal(e.packetEvidence.complete,false);
  const bytes=readFileSync(e.packetEvidence.path);assert.equal(hash(bytes),e.packetEvidence.sha256);
  const evidence=JSON.parse(bytes);assert.deepEqual(evidence.linked,e.linked);
  assert.ok(e.packetEvidence.omittedSections.length>0);assert.match(e.intent.args.message,/OwnerPacketIncomplete/);
  assert.ok(e.intent.args.message.includes(JSON.stringify(e.packetEvidence.path)));assert.ok(e.intent.args.message.includes(e.packetEvidence.sha256));
  if(mode==='continuation')assert.ok(e.intent.args.message.startsWith('Validated same-task continuation'));
  if(mode==='settled')assert.equal(evidence.context.previousAnswer.length,35000);
});

test('small request stays complete and all attachment paths survive a packed contextual handoff',async t=>{
  const f=fixture(t,{settled:true});const root=f.request();const rootPath=join(f.root,'previous-image.png');writeFileSync(rootPath,'root image');
  root.files=[{id:'FROOT',path:rootPath,sha256:hash(readFileSync(rootPath)),size:10,mimetype:'image/png'}];
  root.linked={finished:true,references:[],errors:[],snapshots:[]};await f.receiver.pump();
  assert.equal(root.packetEvidence.complete,true);
  const e=f.request('1700000001.000002',{thread_ts:root.source.ts});const path=join(f.root,'native-image.png');writeFileSync(path,'fixture bytes');
  e.files=[{id:'FIMAGE',path,sha256:hash(readFileSync(path)),size:13,mimetype:'image/png'}];
  await f.receiver.pump();assert.equal(e.status,'delivered');assert.ok(e.intent.args.message.includes(JSON.stringify(path)));
  assert.deepEqual(e.files[0].path,path);assert.match(e.intent.args.message,/OPEN EVERY attachment/);
  assert.ok(e.intent.args.message.includes(JSON.stringify(rootPath)));assert.equal(e.context.files[0].id,'FROOT');
});

test('incomplete packed evidence cannot claim success through admission or retained-operation validation',async t=>{
  const f=fixture(t);const e=f.request();await f.receiver.pump();
  const body={operation:'packed-answer',kind:'success',text:'Complete answer',readFiles:[],source:e.key,thread:e.owner.threadId};
  const secret=readFileSync(join(f.root,'files',e.key,'owner.cap'));
  const item=enqueue(f.root,{...body,signature:signOperation(secret,body)});writeFileSync(item.path,item.bytes);
  await f.receiver.loadOperations();assert.equal(f.store.state.operations[item.id],undefined);
  assert.equal(f.store.state.incidents[item.id].reason,'OwnerPacketIncomplete');assert.equal(f.replies.length,0);
  const {source,thread,...operation}=body;
  assert.throws(()=>f.receiver.validateReply({...operation,source,thread,id:item.id,bodyHash:hash(JSON.stringify(operation))}),/OwnerPacketIncomplete/);
  const failure={...body,operation:'useful-partial-answer',kind:'failure',text:'Useful partial assistance; retained evidence exceeds this packet.'};
  const partial=enqueue(f.root,{...failure,signature:signOperation(secret,failure)});writeFileSync(partial.path,partial.bytes);
  await f.receiver.pump();assert.equal(f.store.state.operations[partial.id].status,'delivered');
  assert.equal(e.outcome.kind,'failure');assert.equal(f.replies.length,1);assert.equal(f.replies[0].args.channel,'DOWNER');
});

test('quote-heavy Unicode context is retained intact while its encoded packet stays bounded',async t=>{
  const f=fixture(t);f.store.state.memory.reference={text:'"\\\u{1f600}'.repeat(50000)};
  const e=f.request();await f.receiver.pump();assert.equal(e.status,'delivered');
  assert.ok(e.intent.args.message.length<=LIMIT);const evidence=JSON.parse(readFileSync(e.packetEvidence.path));
  assert.deepEqual(evidence.memory,f.store.state.memory);assert.deepEqual(evidence.source,e.source);
  for(const marker of ['PERSONAL Slack assistant','NO repository writer claim','OPEN EVERY attachment','submit --source',EXPECTED.user])
    assert.ok(e.intent.args.message.includes(marker));
});

test('the exact native 120000-code-unit dispatch boundary is inclusive',async t=>{
  const f=fixture(t);const e=f.request();e.linked={finished:true,references:[],errors:[],snapshots:[]};
  await f.receiver.receipt(e);await f.receiver.freeze(e);
  e.intent.args.message='x'.repeat(LIMIT);e.intentHash=hash(JSON.stringify(e.intent));
  await f.receiver.deliver(e);assert.equal(e.status,'delivered');assert.equal(f.writes[0].args.message.length,LIMIT);
});

test('mandatory attachment metadata exceeding the bound cannot freeze or dispatch a packet',async t=>{
  const f=fixture(t);const e=f.request();const path=join(f.root,'image.png');writeFileSync(path,'bytes');
  e.files=[{id:'FIMAGE',path,sha256:hash(readFileSync(path)),name:'x'.repeat(LIMIT),size:5}];
  await f.receiver.pump();assert.equal(f.writes.length,0);assert.equal(e.intent,undefined);
  assert.equal(e.status,'pending');assert.equal(e.error,'OwnerPacketTooLarge');assert.equal(e.lastAttempt,undefined);
});

test('legacy oversized frozen or uncertain intent is never sent, replaced or reattributed',async t=>{
  const f=fixture(t);const e=f.request();
  for(const status of ['frozen','uncertain']){
    e.intent={tool:'t3_thread_launch',args:{message:'x'.repeat(LIMIT+1),clientRequestId:'retained-original'}};
    e.intentHash=hash(JSON.stringify(e.intent));e.status=status;e.deliveryState=status==='frozen'?'unsent':'uncertain';
    const original=JSON.stringify(e.intent);await assert.rejects(f.receiver.deliver(e),/OwnerPacketTooLarge/);
    assert.equal(JSON.stringify(e.intent),original);assert.equal(e.status,status);assert.equal(f.writes.length,0);
  }
});

test('bounded ambiguous packet stays immutable across restart with the same clientRequestId and evidence bytes',async t=>{
  const f=fixture(t,{ambiguous:true});const e=f.request();await f.receiver.pump();
  assert.equal(e.status,'uncertain');assert.ok(e.intent.args.message.length<=LIMIT);
  const original=JSON.stringify(e.intent),proof=e.packetEvidence,bytes=readFileSync(proof.path);f.restart();
  const restored=f.store.state.requests[e.key];assert.equal(JSON.stringify(restored.intent),original);
  assert.deepEqual(readFileSync(restored.packetEvidence.path),bytes);assert.equal(f.writes.length,1);
  writeFileSync(restored.packetEvidence.path,'changed evidence');
  await assert.rejects(f.receiver.deliver(restored),/PacketEvidenceChanged/);
  assert.equal(JSON.stringify(restored.intent),original);assert.equal(restored.status,'uncertain');assert.equal(f.writes.length,1);
});
