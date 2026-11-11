import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Store, enqueue, hash } from '../store.mjs';
import { Receiver, signOperation } from '../receiver.mjs';
import { candidate, SlackError } from '../slack.mjs';
import { EXPECTED, validateQualification } from '../run.mjs';

const model={instanceId:'codex',model:'gpt-6.1-sol',options:[{id:'reasoningEffort',value:'high'}]};
function payload(ts='1700000000.000001', event={}, extra={}) {
  return { type:'event_callback',team_id:EXPECTED.team,api_app_id:EXPECTED.app,event_id:'Ev1',event:{type:'message',channel_type:'im',channel:'DOWNER',user:EXPECTED.user,text:'Do the actual task',ts,...event},...extra };
}
function fixture(t,options={}) {
  const root=mkdtempSync(join(tmpdir(),'sfl-personal-test-')); t.after(()=>rmSync(root,{recursive:true,force:true}));
  const store=new Store(root); const calls=[]; let now=1700000000000;
  const slack={verifyDM:async()=>{},reaction:async(s,name,remove)=>calls.push({reaction:name,remove}),download:async()=>{},api:async(method,args)=>{calls.push({method,args});return {ts:'1700000001.000002'};},...options.slack};
  const t3={call:async(tool,args)=>{calls.push({tool,args:structuredClone(args)}); return {threadId:'owner-1',runId:'run-1',link:'t3-thread://owner-1'};},...options.t3};
  const config={projectId:'personal-project',workspace:root,helper:join(root,'run.mjs'),qualification:{providers:['codex']}};
  const receiver=new Receiver({store,slack,t3,config,identity:EXPECTED,now:()=>now,gate:async()=>({model,claims:{}}),available:async()=>({thread:{threadId:'owner-1'}}),...options.receiver});
  return {root,store,calls,slack,t3,receiver,advance:ms=>now+=ms};
}
function operation(f,key,body={}) {
  const op={operation:'result-1',kind:'success',text:'Actual completed outcome',readFiles:[],source:key,thread:'owner-1',...body};
  const secret=readFileSync(join(f.root,'files',key,'owner.cap'));
  const item=enqueue(f.root,{...op,signature:signOperation(secret,op)}); writeFileSync(item.path,item.bytes); return item.id;
}

test('admission accepts only verified owner DM event identity and safe subtypes',()=>{
  assert.equal(candidate(payload(),EXPECTED),true);
  assert.equal(candidate(payload(undefined,{subtype:'file_share',files:[{id:'F123',name:'../../escape.png'}]}),EXPECTED),true);
  for(const event of [{user:'UOTHER'},{bot_id:'B123'},{app_id:'A123'},{hidden:true},{channel_type:'channel'},{channel:'C123'},{subtype:'message_changed'},{subtype:'bot_message'},{files:[{id:'../../bad'}]}]) assert.equal(candidate(payload(undefined,event),EXPECTED),false);
  for(const extra of [{team_id:'TOTHER'},{api_app_id:'AOTHER'},{authorizations:[{team_id:EXPECTED.team,user_id:'UOTHER',is_bot:true}]}]) assert.equal(candidate(payload(undefined,{},extra),EXPECTED),false);
});

test('save precedes all effects; stable team/channel/timestamp dedupes event-id changes and restart',async t=>{
  const f=fixture(t); const key=f.receiver.accept(payload());
  assert.equal(f.calls.length,0);
  assert.equal(new Store(f.root).state.requests[key].source.text,'Do the actual task');
  f.receiver.accept({...payload(),event_id:'EvRetry'});
  assert.equal(Object.keys(f.store.state.requests).length,1);
  await f.receiver.pump();
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_launch').length,1);
  assert.equal(f.calls[0].reaction,'hourglass_flowing_sand');
  const restarted=new Receiver({store:new Store(f.root),slack:f.slack,t3:f.t3,config:f.receiver.config,identity:EXPECTED});
  restarted.accept(payload()); await restarted.pump();
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_launch').length,1);
});

test('failed durable save prevents acknowledgement/effects and poisons the writer',t=>{
  const f=fixture(t); f.store.failed=true;
  assert.throws(()=>f.receiver.accept(payload()),/StateWritePreviouslyFailed/);
  assert.equal(f.calls.length,0);
});

test('corrupt state is refused rather than silently recreating dedupe tombstones',t=>{
  const f=fixture(t);f.receiver.accept(payload());
  const data=JSON.parse(readFileSync(f.store.path,'utf8'));data.data.requests={};writeFileSync(f.store.path,JSON.stringify(data));
  assert.throws(()=>new Store(f.root),/StateChecksumMismatch/);
});

test('hold retains intake and receipt, independently releases two requests as two sessions',async t=>{
  const f=fixture(t);writeFileSync(join(f.root,'HOLD'),'hold');
  const a=f.receiver.accept(payload()); const b=f.receiver.accept(payload('1700000000.000002'));
  await f.receiver.pump(); assert.equal(f.calls.filter(c=>c.tool).length,0);
  assert.equal(f.store.state.requests[a].receipted,true);assert.equal(f.store.state.requests[b].receipted,true);
  rmSync(join(f.root,'HOLD'));f.advance(60001);await f.receiver.pump();
  const launches=f.calls.filter(c=>c.tool==='t3_thread_launch');assert.equal(launches.length,2);
  assert.notEqual(launches[0].args.clientRequestId,launches[1].args.clientRequestId);
});

test('lost T3 response persists immutable intent and never blindly relaunches',async t=>{
  const f=fixture(t);let attempted=0;
  f.t3.call=async(tool,args)=>{
    if(tool==='t3_thread_launch'){attempted++;throw new Error('T3TransportUncertain');}
    if(tool==='t3_thread_list')return {threads:[],nextCursor:null};
    throw new Error('unexpected');
  };
  const key=f.receiver.accept(payload());await f.receiver.pump();
  const frozen=JSON.stringify(f.store.state.requests[key].intent);
  assert.equal(new Store(f.root).state.requests[key].status,'uncertain');
  f.advance(60001);await f.receiver.pump();assert.equal(attempted,1);
  assert.equal(JSON.stringify(f.store.state.requests[key].intent),frozen);
});

test('ambiguous launch reconciles exact title/project/source without a second launch',async t=>{
  const f=fixture(t);let launch=0;
  f.t3.call=async(tool)=>{
    const e=Object.values(f.store.state.requests)[0];
    if(tool==='t3_thread_launch'){launch++;throw new Error('T3TransportUncertain');}
    if(tool==='t3_thread_list')return {threads:[{threadId:'owner-1',title:e.intent.args.title,projectId:'personal-project'}],nextCursor:null};
    if(tool==='t3_thread_read')return {thread:{threadId:'owner-1',projectId:'personal-project',latestRunId:'run-1'},items:[{createdBy:'agent',type:'user_message',text:e.intent.args.message}]};
  };
  const key=f.receiver.accept(payload());await f.receiver.pump();f.advance(60001);await f.receiver.pump();
  assert.equal(launch,1);assert.equal(f.store.state.requests[key].status,'delivered');
});

test('definite pre-send refusal retries the identical packet; frozen provider changes hold',async t=>{
  const f=fixture(t);let packets=[];
  f.t3.call=async(_tool,args)=>{packets.push(JSON.stringify(args));if(packets.length===1)throw Object.assign(new Error('LiveModeLaunchRefused'),{definite:true});return {threadId:'owner-1'};};
  f.receiver.accept(payload());await f.receiver.pump();f.advance(60001);await f.receiver.pump();
  assert.equal(packets.length,2);assert.equal(packets[0],packets[1]);
  const g=fixture(t);g.receiver.accept(payload());await g.receiver.receipt(Object.values(g.store.state.requests)[0]);
  const e=Object.values(g.store.state.requests)[0];await g.receiver.freeze(e);
  g.receiver.gate=async()=>({model:{...model,model:'different'},claims:{}});await g.receiver.pump();
  assert.equal(e.error,'FrozenProviderHeld');assert.equal(g.calls.filter(c=>c.tool).length,0);
});

test('thread continuation binds recorded owner; unknown or paused references stay held',async t=>{
  const f=fixture(t);const root=f.receiver.accept(payload());await f.receiver.pump();
  const next=f.receiver.accept(payload('1700000000.000002',{thread_ts:'1700000000.000001',text:'Same task clarification'}));await f.receiver.pump();
  const sends=f.calls.filter(c=>c.tool==='t3_thread_send');assert.equal(sends.length,1);assert.equal(sends[0].args.threadId,'owner-1');
  assert.equal(f.store.state.requests[next].root,root);
  const unknown=f.receiver.accept(payload('1700000000.000003',{thread_ts:'1600000000.000001'}));await f.receiver.pump();assert.equal(f.store.state.requests[unknown].error,'UnknownThreadOwner');
  f.receiver.available=async()=>{throw new Error('OwnerPaused');};
  const paused=f.receiver.accept(payload('1700000000.000004',{thread_ts:'1700000000.000001'}));await f.receiver.pump();assert.equal(f.store.state.requests[paused].error,'OwnerPaused');
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_send').length,1);
});

test('owner reply signs source and thread; only actual outcome clears hourglass',async t=>{
  const f=fixture(t);const key=f.receiver.accept(payload());await f.receiver.pump();
  assert.equal(f.store.state.requests[key].outcome,undefined);
  const id=operation(f,key);await f.receiver.pump();
  assert.equal(f.store.state.operations[id].status,'delivered');assert.equal(f.store.state.requests[key].outcome.kind,'success');
  const post=f.calls.find(c=>c.method==='chat.postMessage');assert.equal(post.args.channel,'DOWNER');assert.equal(post.args.thread_ts,'1700000000.000001');
  const reactions=f.calls.filter(c=>c.reaction);assert.equal(reactions.at(-2).reaction,'white_check_mark');assert.equal(reactions.at(-1).remove,true);
});

test('a settled claim-free task gets fresh context and later replies follow its registered new owner',async t=>{
  const f=fixture(t);f.receiver.accept(payload());await f.receiver.pump();
  f.receiver.available=async(_t,id)=>({thread:{threadId:id,settled:id==='owner-1'}});
  f.t3.call=async(tool,args)=>{f.calls.push({tool,args});return {threadId:tool==='t3_thread_launch'?'owner-2':args.threadId};};
  const second=f.receiver.accept(payload('1700000000.000002',{thread_ts:'1700000000.000001',text:'Follow up'}));await f.receiver.pump();
  assert.equal(f.store.state.requests[second].owner.threadId,'owner-2');assert.ok(f.store.state.requests[second].context.source);
  f.receiver.accept(payload('1700000000.000003',{thread_ts:'1700000000.000001',text:'Clarify that'}));await f.receiver.pump();
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_send').at(-1).args.threadId,'owner-2');
});

test('thread reply catch-up uses only registered roots and saves its own watermark',async t=>{
  const f=fixture(t);f.receiver.accept(payload());await f.receiver.pump();
  const seen=[];f.slack.api=async(method,args)=>{seen.push({method,args});return {messages:[payload('1700000000.000002',{text:'Reply during outage'}).event]};};
  await f.receiver.catchUpThreads('DOWNER','1699999999.000000');
  assert.equal(Object.keys(f.store.state.requests).length,2);assert.equal(seen[0].method,'conversations.replies');
  assert.equal(seen[0].args.ts,'1700000000.000001');assert.equal(Object.keys(f.store.state.threadCursors).length,1);
});

test('forged capability, mismatched thread and unread-image success never send',async t=>{
  const f=fixture(t);const key=f.receiver.accept(payload());await f.receiver.pump();
  const id=operation(f,key,{thread:'other-owner'});await f.receiver.pump();assert.equal(f.store.state.operations[id],undefined);
  const file=join(f.root,'inbox',`${id}.json`);const op=JSON.parse(readFileSync(file,'utf8'));op.thread='owner-1';writeFileSync(file,JSON.stringify(op));
  await f.receiver.pump();assert.equal(f.store.state.operations[id],undefined);
  rmSync(file);f.store.state.requests[key].files=[{id:'FIMAGE',mimetype:'image/png'}];operation(f,key);await f.receiver.pump();
  assert.equal(f.calls.filter(c=>c.method==='chat.postMessage').length,0);
});

test('ambiguous Slack post is reconciled from metadata, never sent twice',async t=>{
  const f=fixture(t);const key=f.receiver.accept(payload());await f.receiver.pump();const id=operation(f,key);let sends=0;
  f.slack.api=async(method)=>{
    if(method==='chat.postMessage'){sends++;throw new SlackError('transport_uncertain');}
    if(method==='conversations.replies')return {messages:[{ts:'1700000001.000001',user:EXPECTED.bot,text:'Slack normalized the links',metadata:{event_type:'sfl_personal_reply',event_payload:{source:key,operation:id,body:hash('Actual completed outcome')}}}]};
  };
  await f.receiver.pump();assert.equal(f.store.state.operations[id].status,'uncertain');
  f.advance(60001);await f.receiver.pump();assert.equal(sends,1);assert.equal(f.store.state.operations[id].status,'delivered');
});

test('explicit failure and memory use the same validated durable owner contract',async t=>{
  const f=fixture(t);const key=f.receiver.accept(payload());await f.receiver.pump();
  const mem=operation(f,key,{operation:'remember',kind:'memory',memoryKey:'preference',text:'Verified preference'});await f.receiver.pump();
  assert.equal(new Store(f.root).state.memory.preference.text,'Verified preference');assert.equal(f.store.state.operations[mem].status,'delivered');
  operation(f,key,{operation:'failed',kind:'failure',text:'Actual failure, result not delivered'});await f.receiver.pump();assert.equal(f.store.state.requests[key].outcome.kind,'failure');
});

test('bounded catch-up reads only exact owner DM and commits cursor after all durable intake',async t=>{
  const f=fixture(t);let calls=[];
  f.slack.api=async(method,args)=>{calls.push({method,args});return {messages:[payload('1700000000.000002').event,payload().event]};};
  await f.receiver.catchUp('DOWNER','1699999999.000000');
  assert.equal(Object.keys(new Store(f.root).state.requests).length,2);assert.equal(f.store.state.cursors.DOWNER,'1700000000');
  assert.equal(calls[0].args.channel,'DOWNER');assert.equal(calls[0].args.oldest,'1699999999.000000');
});

test('qualification is invalid after a server/source/environment change',()=>{
  const config={sourceRevision:'abc',qualification:{schema:1,homeEnvironment:'home',serverVersion:'v1',sourceRevision:'abc',launchSameThread:true,sendSameMessage:true,ambiguousPolicy:'reconcile-only',providers:['codex']}};
  validateQualification(config,{environmentId:'home',serverVersion:'v1'});
  assert.throws(()=>validateQualification(config,{environmentId:'home',serverVersion:'v2'}),/LiveQualificationRequired/);
});
