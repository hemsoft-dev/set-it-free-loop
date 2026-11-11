import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Slack, SlackError } from '../slack.mjs';
import { admission, ownerAvailable, T3 } from '../t3.mjs';
import { EXPECTED } from '../run.mjs';
const model={instanceId:'codex',model:'gpt-6.1-sol',options:[{id:'reasoningEffort',value:'high'}]};
const json=value=>new Response(JSON.stringify(value),{headers:{'content-type':'application/json'}});

test('identity preflight requires im:write only when startup must open the owner DM',async()=>{
  const base=['im:history','im:read','files:read','reactions:write','chat:write','users:read'];let scopes=[...base];
  const methods=[];
  const slack=new Slack({botToken:'test-bot',userToken:'test-user',fetcher:async(url,args)=>{
    const method=new URL(url).pathname.split('/').at(-1);methods.push(method);
    if(method==='auth.test')return new Response(JSON.stringify({ok:true,team_id:EXPECTED.team,
      user_id:args.headers.Authorization==='Bearer test-user'?EXPECTED.user:EXPECTED.bot,bot_id:EXPECTED.botId}),
      {headers:{'content-type':'application/json','x-oauth-scopes':scopes.join(',')}});
    assert.equal(method,'users.info');const self=args.body.get('user')===EXPECTED.user;
    return json({ok:true,user:{id:self?EXPECTED.user:EXPECTED.bot,real_name:EXPECTED.realName,is_bot:!self,deleted:false,
      profile:{api_app_id:EXPECTED.app}}});
  }});
  await assert.rejects(slack.identity(EXPECTED),/MissingDMOpenScope/);
  await slack.identity(EXPECTED,{needsDMOpen:false});
  scopes=[...base,'im:write'];await slack.identity(EXPECTED);
  assert.ok(methods.every(method=>['auth.test','users.info'].includes(method)),'preflight neither grants scopes nor opens a DM');
});

test('Slack uses bearer headers and form fields; rate limits are definite and transport failures uncertain',async()=>{
  const slack=new Slack({botToken:'FAKE',fetcher:async(url,args)=>{
    assert.equal(url,'https://slack.com/api/conversations.info');assert.equal(args.headers.Authorization,'Bearer FAKE');
    assert.equal(args.body.get('channel'),'DOWNER');assert.equal(args.redirect,'error');return json({ok:true,channel:{id:'DOWNER'}});
  }});
  await slack.api('conversations.info',{channel:'DOWNER'});
  slack.fetcher=async()=>new Response('',{status:429,headers:{'retry-after':'120'}});
  await assert.rejects(slack.api('chat.postMessage'),e=>e instanceof SlackError && e.definite && e.retryAfter===120);
  slack.fetcher=async()=>{throw new Error('sensitive response never forwarded');};
  await assert.rejects(slack.api('chat.postMessage'),e=>!e.definite && !e.message.includes('sensitive'));
  slack.fetcher=async()=>json({ok:false,error:'fatal_error'});
  await assert.rejects(slack.api('chat.postMessage'),e=>!e.definite);
});

test('exact IM membership blocks shared channels and other users',async()=>{
  const slack=new Slack({fetcher:async()=>json({ok:true,channel:{id:'DOWNER',is_im:true,user:EXPECTED.user}})});
  await slack.verifyDM('DOWNER',EXPECTED);
  for(const value of [{id:'DOWNER',is_im:false,user:EXPECTED.user},{id:'DOWNER',is_im:true,user:'UOTHER'},{id:'DOWNER',is_im:true,user:EXPECTED.user,is_ext_shared:true}]) {
    slack.fetcher=async()=>json({ok:true,channel:value});await assert.rejects(slack.verifyDM('DOWNER',EXPECTED),/NotOwnerDM/);
  }
});

test('file downloads refuse external hosts, redirect/login pages and invalid image signatures',async t=>{
  const root=mkdtempSync(join(tmpdir(),'sfl-file-test-'));t.after(()=>rmSync(root,{recursive:true,force:true}));
  const file={id:'F123',name:'../../bad.png',mimetype:'image/png',size:8,url_private:'https://files.slack.com/path'};
  let fetchCount=0;
  const slack=new Slack({botToken:'FAKE',fetcher:async(url,args)=>{
    fetchCount++;if(String(url).includes('/api/'))return json({ok:true,file});
    assert.equal(new URL(url).hostname,'files.slack.com');assert.equal(args.redirect,'manual');
    return new Response(Buffer.from([137,80,78,71,13,10,26,10]),{headers:{'content-type':'image/png'}});
  }});
  const result=await slack.download('F123',join(root,'F123.asset'));assert.equal(readFileSync(result.path).length,8);assert.ok(result.path.endsWith('.png'));
  file.url_private='https://attacker.example/private';await assert.rejects(slack.download('F123',join(root,'bad')),/UnsafeFileURL/);assert.equal(fetchCount,3);
  file.url_private='https://files.slack.com/path';file.is_external=true;await assert.rejects(slack.download('F123',join(root,'bad')),/ExternalFileRefused/);delete file.is_external;
  slack.fetcher=async(url)=>String(url).includes('/api/')?json({ok:true,file}):new Response('login',{headers:{'content-type':'text/html'}});
  await assert.rejects(slack.download('F123',join(root,'bad')),/AttachmentLoginPage/);
  slack.fetcher=async(url)=>String(url).includes('/api/')?json({ok:true,file}):new Response('invalid!',{headers:{'content-type':'image/png'}});
  await assert.rejects(slack.download('F123',join(root,'bad')),/ImageSignatureMismatch/);
});

test('live admission obeys Home cap, canonical claims and high provider catalog',async()=>{
  const config={projectId:'personal',workspace:'C:/private/workspace'};
  const snapshot={machines:[{name:'home',maxSessions:6}],activity:[{name:'home',state:'ok',activeCount:3}],claims:{'owner/repo':{owner:'other-source'}},model};
  const t3={call:async(tool,args)=>tool==='orchestrator_capabilities'?{providers:[{providerInstanceId:'codex',canRunChildTask:true,models:[{id:model.model,options:[{id:'reasoningEffort',values:['high']}]}]}]}:{id:'personal',workspaceRoot:config.workspace}};
  const result=await admission(t3,config,async()=>snapshot);assert.deepEqual(result.claims,snapshot.claims);
  snapshot.activity[0].activeCount=6;await assert.rejects(admission(t3,config,async()=>snapshot),/HomeCapacityHeld/);
  snapshot.activity[0].activeCount=3;snapshot.activity[0].state='unavailable';await assert.rejects(admission(t3,config,async()=>snapshot),/HomeCapacityHeld/);
  snapshot.activity[0].state='ok';snapshot.claims={'bad':{}};await assert.rejects(admission(t3,config,async()=>snapshot),/ClaimsUnavailable/);
});

test('continuation requires the recorded personal project, no stop/pending request, and canonical hands-off permission',async()=>{
  const thread={threadId:'owner',projectId:'personal',archived:false,settled:false,snoozed:false,pendingRequestCount:0};
  const t3={call:async()=>({thread,recentRuns:[]})};const config={projectId:'personal'};
  await ownerAvailable(t3,'owner',config,async()=>({allowed:true}));
  await assert.rejects(ownerAvailable(t3,'owner',config,async()=>({allowed:false})),/OwnerPaused/);
  thread.settled=true;await assert.rejects(ownerAvailable(t3,'owner',config,async()=>({allowed:true})),/OwnerHeld/);
  await ownerAvailable(t3,'owner',config,async()=>({allowed:true}),{resultOnly:true});
  thread.projectId='chief';await assert.rejects(ownerAvailable(t3,'owner',config,async()=>({allowed:true}),{resultOnly:true}),/OwnerHeld/);
});

test('personal transport rejects unlisted tools and environment overrides before executing',async()=>{
  const t3=new T3('unused');
  await assert.rejects(t3.call('t3_thread_interrupt',{}),/PersonalToolRefused/);
  await assert.rejects(t3.call('t3_thread_read',{environmentId:'mini'}),/PersonalToolRefused/);
});
