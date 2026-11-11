import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { Store } from '../store.mjs';
import { Receiver } from '../receiver.mjs';
import { EXPECTED } from '../run.mjs';

const payload=(ts='1700000000.000001',extra={})=>({type:'event_callback',team_id:EXPECTED.team,api_app_id:EXPECTED.app,
  event:{type:'message',channel_type:'im',channel:'DOWNER',user:EXPECTED.user,ts,
    text:'Help with https://relias-engineering.slack.com/archives/CTARGET/p1700000000000001',...extra}});

function fixture(t,mode,fault) {
  const root=mkdtempSync(join(tmpdir(),'sfl-reconcile-large-'));t.after(()=>rmSync(root,{recursive:true,force:true}));
  const f={root,mode,fault,capture:mode==='launch',writes:[],reads:[],store:new Store(root)};
  const config={projectId:'personal',workspace:root,helper:join(root,'run.mjs'),qualification:{providers:['codex']}};
  const slack={verifyDM:async()=>{},reaction:async()=>{},readLinked:async reference=>({reference,complete:true,targetFound:true,
    messages:[{type:'message',ts:'1700000000.000001',text:'large untrusted evidence '.repeat(2800)+'\u{1f600}'}]})};
  const thread=()=>({threadId:f.threadId,projectId:'personal',latestRunId:'run-exact',link:'t3-thread://exact-owner'});
  const chunk=(offset=0)=>{
    let text=f.message;
    if(f.fault==='tail')text=text.slice(0,-1)+'!';
    const end=Math.min(offset+50000,text.length);
    return {itemId:f.fault==='item'&&offset?'wrong-item':'source-item',messageId:'message-exact',sourceThreadId:f.fault==='source'?'foreign-thread':f.threadId,
      type:'user_message',createdBy:'agent',status:'completed',position:1,text:text.slice(offset,end),
      textTruncated:end<text.length,nextTextOffset:f.fault==='offset'?offset:(end<text.length?end:null)};
  };
  const t3={call:async(tool,args)=>{
    if(['t3_thread_launch','t3_thread_send'].includes(tool)) {
      f.writes.push({tool,args:structuredClone(args)});
      if(!f.capture)return {threadId:'owner-1',runId:'initial'};
      f.threadId=args.threadId ?? 'owner-exact';f.message=args.message;
      throw new Error('T3TransportUncertain');
    }
    if(tool==='t3_thread_list')return {threads:[{threadId:f.threadId,projectId:'personal',title:f.entry.intent.args.title}],nextCursor:null};
    assert.equal(tool,'t3_thread_read');assert.equal(args.maxCharsPerItem,50000);f.reads.push(args);
    if(args.itemId) {
      assert.equal(args.itemId,'source-item');
      return {thread:{...thread(),...(f.fault==='project'?{projectId:'other-project'}:{})},items:[chunk(args.textOffset)],hasMore:false};
    }
    if(args.afterPosition===undefined && mode==='launch')return {thread:thread(),items:[chunk()],hasMore:false,nextPosition:1};
    if(args.afterPosition===undefined)return {thread:thread(),items:[{type:'assistant_message',text:'Unrelated old activity',position:0}],hasMore:true,nextPosition:0};
    assert.equal(args.afterPosition,0);
    return {thread:thread(),items:[chunk()],hasMore:false,nextPosition:1};
  }};
  const available=async()=>({thread:{threadId:'owner-1',settled:mode==='settled',snoozed:false,pendingRequestCount:0}});
  const gate=async()=>({claims:{},model:{instanceId:'codex',model:'gpt-6.1-sol'}});
  f.restart=()=>{ f.store=new Store(root);f.receiver=new Receiver({store:f.store,slack,t3,config,identity:EXPECTED,available,gate}); };
  Object.defineProperty(f,'entry',{get:()=>f.store.state.requests[f.key]});
  f.prepare=async()=>{
    if(mode!=='launch') { f.receiver.accept(payload());await f.receiver.pump();f.capture=true; }
    f.key=f.receiver.accept(mode==='launch'?payload():payload('1700000001.000002',{thread_ts:'1700000000.000001'}));
    await f.receiver.pump();assert.equal(f.entry.status,'uncertain');assert.ok(f.message.length>50000);
  };
  f.restart();return f;
}

for(const mode of ['launch','continuation','settled'])test(`large ${mode} source reconciles after restart through timeline and text pagination without replay`,async t=>{
  const f=fixture(t,mode);await f.prepare();const original=JSON.stringify(f.entry.intent);const writes=f.writes.length;
  f.restart();await f.receiver.reconcile(f.entry);
  assert.equal(f.entry.status,'delivered');assert.equal(f.entry.owner.threadId,f.threadId);
  assert.equal(JSON.stringify(f.entry.intent),original);assert.equal(f.writes.length,writes);
  if(mode!=='launch')assert.ok(f.reads.some(r=>r.afterPosition===0));
  assert.ok(f.reads.some(r=>r.itemId==='source-item'&&r.textOffset===50000));
});

for(const fault of ['tail','item','project','offset','source'])test(`large source ${fault} evidence mismatch cannot confirm a shared prefix`,async t=>{
  const f=fixture(t,'launch',fault);await f.prepare();const original=JSON.stringify(f.entry.intent);const writes=f.writes.length;
  f.restart();await assert.rejects(f.receiver.reconcile(f.entry));
  assert.equal(f.entry.status,'uncertain');assert.equal(f.entry.owner,undefined);assert.equal(f.writes.length,writes);
  assert.equal(JSON.stringify(f.entry.intent),original);
});
