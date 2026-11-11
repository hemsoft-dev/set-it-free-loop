import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { Store, enqueue, hash } from '../store.mjs';
import { Receiver, signOperation } from '../receiver.mjs';
import { ownerAvailable } from '../t3.mjs';
import { EXPECTED } from '../run.mjs';
import { SlackError } from '../slack.mjs';

const model={instanceId:'codex',model:'gpt-6.1-sol',options:[{id:'reasoningEffort',value:'high'}]};
const payload=(ts='1700000000.000001',extra={})=>({type:'event_callback',team_id:EXPECTED.team,api_app_id:EXPECTED.app,
  event:{type:'message',channel_type:'im',channel:'DOWNER',user:EXPECTED.user,text:'Actual request',ts,...extra}});
function fixture(t) {
  const local=mkdtempSync(join(tmpdir(),'sfl-round1-'));t.after(()=>rmSync(local,{recursive:true,force:true}));
  const root=join(local,'HemSoft','PersonalSlack');const calls=[];let now=1700000000000;
  const f={local,root,calls,settled:false,confirmSend:false,sendError:null,store:new Store(root),advance:()=>now+=60001};
  const config={projectId:'personal',workspace:root,helper:resolve('personal-slack/run.mjs'),qualification:{providers:['codex']}};
  const slack={verifyDM:async()=>{},reaction:async(...args)=>calls.push({reaction:args}),
    api:async(method,args)=>{calls.push({method,args:structuredClone(args)});return {ts:`1700000001.${String(calls.length).padStart(6,'0')}`};}};
  const t3={call:async(tool,args)=>{
    calls.push({tool,args:structuredClone(args)});
    if(tool==='t3_thread_read')return {thread:{threadId:args.threadId,projectId:'personal',settled:f.settled,archived:false,snoozed:false,pendingRequestCount:0},recentRuns:[],
      items:f.confirmSend?[{type:'user_message',text:f.originalSend}]:[]};
    if(tool==='t3_thread_send' && f.sendError)throw f.sendError;
    return {threadId:tool==='t3_thread_send'?args.threadId:`owner-${calls.filter(c=>c.tool==='t3_thread_launch').length}`,runId:'run-1'};
  }};
  const available=(...args)=>ownerAvailable(args[0],args[1],args[2],async()=>({allowed:true}),args[4]);
  f.restart=()=>{
    f.store=new Store(root);
    f.receiver=new Receiver({store:f.store,slack,t3,config,identity:EXPECTED,now:()=>now,gate:async()=>({model,claims:{}}),available});
  };
  f.slack=slack;f.t3=t3;f.restart();return f;
}
function queue(f,source,body) {
  const op={operation:'part-1',kind:'question',text:'First part',readFiles:[],...body,source,thread:f.store.state.requests[source].owner.threadId};
  const secret=readFileSync(join(f.root,'files',source,'owner.cap'));
  const item=enqueue(f.root,{...op,signature:signOperation(secret,op)});writeFileSync(item.path,item.bytes);return item.id;
}
async function rootRequest(f) {const key=f.receiver.accept(payload());await f.receiver.pump();return key;}
async function frozenContinuation(f) {
  await rootRequest(f);const key=f.receiver.accept(payload('1700000000.000002',{thread_ts:'1700000000.000001'}));
  const e=f.store.state.requests[key];await f.receiver.receipt(e);await f.receiver.freeze(e);f.originalSend=e.intent.args.message;return e;
}

test('Round 1 security: submit rejects owner internal fields before signing or publishing',async t=>{
  const f=fixture(t);const source=await rootRequest(f);
  const foreign={channel:'COTHER',text:'Injected destination'};
  for(const [field,value] of Object.entries({intent:foreign,intentHash:hash(JSON.stringify(foreign)),status:'uncertain',ts:'1.1',nextAttempt:1,id:'other',predecessor:'other',bodyHash:'other'})) {
    const path=join(f.local,'body.json');writeFileSync(path,JSON.stringify({operation:`forged-${field}`,kind:'question',text:'Valid text',readFiles:[],[field]:value}));
    const env={...process.env,LOCALAPPDATA:f.local};for(const name of Object.keys(env))if(name.startsWith('SFL_'))delete env[name];
    const result=spawnSync(process.execPath,[resolve('personal-slack/run.mjs'),'submit','--source',source,'--thread','owner-1','--file',path],{env,encoding:'utf8',windowsHide:true});
    assert.notEqual(result.status,0,`${field} must be rejected at submit`);
  }
  assert.deepEqual(readdirSync(join(f.root,'inbox')),[]);
  const path=join(f.local,'valid-body.json');writeFileSync(path,JSON.stringify({operation:'valid',kind:'question',text:'Valid UTF-8 reply: café',readFiles:[]}));
  const env={...process.env,LOCALAPPDATA:f.local};for(const name of Object.keys(env))if(name.startsWith('SFL_'))delete env[name];
  const valid=spawnSync(process.execPath,[resolve('personal-slack/run.mjs'),'submit','--source',source,'--thread','owner-1','--file',path],{env,encoding:'utf8',windowsHide:true});
  assert.equal(valid.status,0,valid.stderr);await f.receiver.pump();
  assert.equal(f.calls.find(c=>c.method==='chat.postMessage').args.text,'Valid UTF-8 reply: café');
});

test('Round 1 security: signed packets cannot supply daemon-owned fields',async t=>{
  const f=fixture(t);const source=await rootRequest(f);const foreign={channel:'COTHER',text:'Injected destination'};
  for(const [field,value] of Object.entries({intent:foreign,intentHash:hash(JSON.stringify(foreign)),status:'sent',ts:'1.1',nextAttempt:1,id:'other',predecessor:'other',bodyHash:'other',memoryKey:'not-a-memory-operation'}))
    queue(f,source,{operation:`forged-${field}`,[field]:value});
  await f.receiver.pump();assert.equal(Object.keys(f.store.state.operations).length,0);
  assert.equal(f.calls.filter(c=>c.method==='chat.postMessage').length,0);
});

test('Round 1 security: send revalidates a stored intent against the admitted source and body',async t=>{
  const f=fixture(t);const source=await rootRequest(f);const id=queue(f,source);await f.receiver.loadOperations();
  const op=f.store.state.operations[id];op.intent={channel:'COTHER',text:'Injected destination'};op.intentHash=hash(JSON.stringify(op.intent));f.store.save();
  await f.receiver.pump();assert.equal(f.calls.filter(c=>c.method==='chat.postMessage').length,0);assert.equal(op.status,'pending');
});

test('Round 1 settlement: a never-attempted frozen continuation recovers to a fresh session',async t=>{
  const f=fixture(t);const e=await frozenContinuation(f);const original=structuredClone(e.intent);f.settled=true;
  f.restart();await f.receiver.pump();const recovered=f.store.state.requests[e.key];
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_launch').length,2);
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_send').length,0);
  assert.equal(recovered.owner.threadId,'owner-2');assert.notEqual(recovered.intent.args.clientRequestId,original.args.clientRequestId);
  assert.deepEqual(recovered.intentHistory[0].intent,original);assert.equal(recovered.intentHistory[0].disposition,'definitely-unsent-owner-settled');
});

test('Round 1 settlement: a definite send refusal can recover after restart when its owner settles',async t=>{
  const f=fixture(t);const e=await frozenContinuation(f);const original=structuredClone(e.intent);
  f.sendError=Object.assign(new Error('LiveModeLaunchRefused'),{definite:true});await f.receiver.pump();assert.equal(e.status,'frozen');
  f.settled=true;f.sendError=null;f.advance();f.restart();await f.receiver.pump();
  const recovered=f.store.state.requests[e.key];assert.equal(recovered.owner?.threadId,'owner-2');assert.deepEqual(recovered.intentHistory[0].intent,original);
  assert.equal(recovered.intentHistory[0].lastAttempt.error,'LiveModeLaunchRefused');
});

test('Round 1 settlement control: ambiguous sends retain their original owner through settlement and reconciliation',async t=>{
  const f=fixture(t);const e=await frozenContinuation(f);const original=structuredClone(e.intent);
  f.sendError=new Error('T3TransportUncertain');await f.receiver.pump();assert.equal(e.status,'uncertain');
  f.settled=true;f.sendError=null;f.advance();f.restart();await f.receiver.pump();
  assert.equal(f.calls.filter(c=>c.tool==='t3_thread_launch').length,1);assert.deepEqual(f.store.state.requests[e.key].intent,original);
  assert.equal(f.store.state.requests[e.key].status,'uncertain');
  await assert.rejects(f.receiver.freeze(f.store.state.requests[e.key]),/FrozenIntentExists/);
  await assert.rejects(f.receiver.freeze(f.store.state.requests[e.key],{supersedeUnsent:true}),/IntentRecoveryRefused/);
  f.confirmSend=true;f.advance();await f.receiver.pump();
  assert.equal(f.store.state.requests[e.key].owner.threadId,'owner-1');assert.equal(f.store.state.requests[e.key].status,'delivered');
  await f.receiver.pump();assert.equal(f.calls.filter(c=>c.tool==='t3_thread_send').length,1);assert.equal(f.calls.filter(c=>c.tool==='t3_thread_launch').length,1);
});

test('Round 1 settlement: claim and pause holds preserve an unsent original intent until recovery is safe',async t=>{
  const f=fixture(t);const e=await frozenContinuation(f);const original=structuredClone(e.intent);f.settled=true;
  f.receiver.gate=async()=>({model,claims:{'owner/repo':{owner:'thread:owner-1'}}});await f.receiver.pump();
  assert.equal(e.error,'SettledOwnerStillClaims');assert.deepEqual(e.intent,original);assert.equal(e.intentHistory,undefined);
  f.advance();f.receiver.available=async()=>{throw new Error('OwnerPaused');};await f.receiver.pump();
  assert.equal(e.error,'OwnerPaused');assert.deepEqual(e.intent,original);assert.equal(f.calls.filter(c=>c.tool==='t3_thread_launch').length,1);
});

for(const failure of ['ratelimited','transport_uncertain'])test(`Round 1 multipart: ${failure} retains all parts before a final outcome across restart`,async t=>{
  const f=fixture(t);const source=await rootRequest(f);const part=queue(f,source);await f.receiver.loadOperations();
  const final=queue(f,source,{operation:'final',kind:'success',text:'Final actual result'});let failed=false;let confirm=false;const delivery=[];
  f.slack.api=async(method,args)=>{
    if(method==='chat.postMessage') {
      delivery.push(args.text);
      if(args.text==='First part'&&!failed){failed=true;throw new SlackError(failure,{definite:failure==='ratelimited'});}
      return {ts:'1700000001.000002'};
    }
    if(method==='conversations.replies')return {messages:confirm?[{ts:'1700000001.000001',user:EXPECTED.bot,
      metadata:{event_type:'sfl_personal_reply',event_payload:{source,operation:part,body:hash('First part')}}}]:[]};
  };
  await f.receiver.pump();assert.deepEqual(delivery,['First part']);assert.equal(f.store.state.requests[source].outcome,undefined);
  f.advance();f.restart();
  if(failure==='transport_uncertain') {
    await f.receiver.pump();assert.deepEqual(delivery,['First part']);assert.equal(f.store.state.operations[final].status,'pending');
    f.advance();confirm=true;
  }
  await f.receiver.pump();
  assert.deepEqual(delivery,failure==='ratelimited'?['First part','First part','Final actual result']:['First part','Final actual result']);
  assert.equal(f.store.state.operations[part].status,'delivered');assert.equal(f.store.state.operations[final].status,'delivered');
  assert.equal(f.store.state.requests[source].outcome.operation,final);
});

test('Round 1 multipart: an earlier held part blocks its own final, while another source can finish',async t=>{
  const f=fixture(t);const source=await rootRequest(f);
  const other=f.receiver.accept(payload('1700000000.000003'));await f.receiver.pump();
  const first=queue(f,source,{operation:'part',text:'Held part'});await f.receiver.loadOperations();
  queue(f,source,{operation:'end',kind:'success',text:'Must wait'});await f.receiver.loadOperations();
  queue(f,other,{operation:'other-result',kind:'success',text:'Independent actual result'});
  const sends=[];f.slack.api=async(method,args)=>{
    if(method==='chat.postMessage'){sends.push(args.text);if(args.text==='Held part')throw new SlackError('ratelimited',{definite:true});return {ts:'1700000001.000005'};}
  };
  await f.receiver.pump();assert.deepEqual(sends,['Held part','Independent actual result']);
  assert.equal(f.store.state.requests[source].outcome,undefined);assert.equal(f.store.state.operations[first].status,'pending');
  assert.equal(f.store.state.requests[other].outcome.kind,'success');
});
