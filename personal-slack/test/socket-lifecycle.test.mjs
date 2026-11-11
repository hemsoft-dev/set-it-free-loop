import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, writeFileSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { hash } from '../store.mjs';

// Only this isolated node:test worker sees these fake values. No credential
// store is read, and every network-capable method/transport below is stubbed.
for(const key of ['SFL_APP_LEVEL_TOKEN','SFL_SLACK_BOT_USER_OAUTH_TOKEN','SFL_SLACK_USER_OAUTH_TOKEN'])process.env[key]='test-only';
const flush=async()=>{for(let n=0;n<20;n++)await Promise.resolve();};

async function fixture(t,{autoHello=true}={}) {
  const root=mkdtempSync(join(tmpdir(),'sfl-socket-test-'));const release=join(root,'release');mkdirSync(release);
  // Run the real serve entrypoint, release verification, reconnect loop, STOP
  // handling and lock cleanup. Plain fixture files are not a checkout/worktree.
  const files={};
  for(const name of readdirSync(resolve('personal-slack')).filter(n=>n.endsWith('.mjs'))) {
    const bytes=readFileSync(resolve('personal-slack',name));writeFileSync(join(release,name),bytes);files[name]=hash(bytes);
  }
  const commit='0'.repeat(40);writeFileSync(join(release,'manifest.json'),JSON.stringify({commit,files}));
  const {serve,EXPECTED}=await import(pathToFileURL(join(release,'run.mjs')).href);
  const {Slack}=await import(pathToFileURL(join(release,'slack.mjs')).href);
  const {T3}=await import(pathToFileURL(join(release,'t3.mjs')).href);
  const {Receiver}=await import(pathToFileURL(join(release,'receiver.mjs')).href);
  const sockets=[];const counts={opens:0,catchups:0,threadCatchups:0};
  t.mock.method(globalThis,'fetch',async()=>{throw new Error('UnexpectedLiveNetwork');});
  t.mock.method(T3.prototype,'initialize',async()=>{});
  t.mock.method(T3.prototype,'call',async tool=>{
    assert.equal(tool,'t3_environment_read');return {environmentId:'test-home',serverVersion:'test-version',label:'home',platform:{os:'windows'}};
  });
  t.mock.method(Slack.prototype,'identity',async()=>EXPECTED);
  t.mock.method(Slack.prototype,'api',async method=>{
    assert.equal(method,'apps.connections.open');counts.opens++;return {url:'wss://test.slack.com/socket'};
  });
  t.mock.method(Receiver.prototype,'pump',async()=>{});
  t.mock.method(Receiver.prototype,'catchUp',async()=>{counts.catchups++;});
  t.mock.method(Receiver.prototype,'catchUpThreads',async()=>{counts.threadCatchups++;});
  const NativeSocket=globalThis.WebSocket;
  class StubSocket {
    constructor(){this.readyState=1;this.closes=0;this.pongs=0;sockets.push(this);
      if(autoHello)queueMicrotask(()=>this.message({type:'hello',connection_info:{app_id:EXPECTED.app},num_connections:1}));}
    message(body){this.onmessage?.({data:JSON.stringify(body)});}
    protocolPing(){if(this.readyState===1)this.pongs++;} // Native pong, no application message event.
    close(){if(this.readyState>=2)return;this.closes++;this.readyState=2;queueMicrotask(()=>this.serverClose());}
    serverClose(){this.readyState=3;this.onclose?.({code:1000});}
    error(){this.onerror?.({type:'error'});}
  }
  globalThis.WebSocket=StubSocket;
  const listeners=Object.fromEntries(['SIGINT','SIGTERM'].map(name=>[name,new Set(process.listeners(name))]));
  t.mock.timers.enable({apis:['Date','setTimeout','setInterval'],now:1700000000000});
  const config={helper:join(release,'run.mjs'),sourceRevision:commit,chief:'unused',dm:'DOWNER',installedAfter:'1700000000',
    qualification:{schema:1,homeEnvironment:'test-home',serverVersion:'test-version',sourceRevision:commit,
      launchSameThread:true,sendSameMessage:true,ambiguousPolicy:'reconcile-only',providers:['codex']}};
  const running=serve(root,config);running.catch(()=>{});
  let stopping;
  const stop=()=>stopping ??= (async()=>{writeFileSync(join(root,'STOP'),'test stop');t.mock.timers.tick(5000);await flush();await running;})();
  t.after(async()=>{
    try {await stop();} finally {
      globalThis.WebSocket=NativeSocket;
      for(const name of Object.keys(listeners))for(const fn of process.listeners(name))if(!listeners[name].has(fn))process.removeListener(name,fn);
      rmSync(root,{recursive:true,force:true});
    }
  });
  await flush();assert.equal(sockets.length,1);
  return {root,sockets,counts,stop};
}

test('Round 2: protocol-live socket remains open during ten minutes without app messages or repeated catch-up',async t=>{
  const f=await fixture(t);assert.equal(f.counts.catchups,1);
  for(let n=0;n<20;n++){f.sockets.at(-1).protocolPing();t.mock.timers.tick(30000);await flush();}
  assert.equal(f.counts.opens,1,'idle traffic must not request another Socket Mode URL');
  assert.equal(f.sockets[0].closes,0,'application-message silence is not a transport failure');
  assert.equal(f.sockets[0].pongs,20);assert.equal(f.counts.catchups,1);assert.equal(f.counts.threadCatchups,1);
});

for(const reason of ['refresh_requested','warning','link_disabled'])test(`Socket Mode disconnect ${reason} still reconnects and catches up`,async t=>{
  const f=await fixture(t);f.sockets[0].message({type:'disconnect',reason});await flush();t.mock.timers.tick(5000);await flush();
  assert.equal(f.counts.opens,2);assert.equal(f.sockets[0].closes,1);assert.equal(f.counts.catchups,2);
});

for(const event of ['close','error'])test(`real WebSocket ${event} still reaches the reconnect loop`,async t=>{
  const f=await fixture(t);if(event==='close')f.sockets[0].serverClose();else f.sockets[0].error();
  await flush();t.mock.timers.tick(5000);await flush();assert.equal(f.counts.opens,2);assert.equal(f.counts.catchups,2);
});

test('pre-hello deadline still closes a connection that never identifies itself',async t=>{
  const f=await fixture(t,{autoHello:false});t.mock.timers.tick(45000);await flush();assert.equal(f.sockets[0].closes,1);
  t.mock.timers.tick(5000);await flush();assert.equal(f.counts.opens,2);assert.equal(f.counts.catchups,0);
});

test('graceful STOP closes the owned socket, removes its lock and never reconnects',async t=>{
  const f=await fixture(t);await f.stop();assert.equal(f.sockets[0].closes,1);assert.equal(existsSync(join(f.root,'receiver.lock')),false);
  t.mock.timers.tick(3600000);await flush();assert.equal(f.counts.opens,1);assert.equal(f.counts.catchups,1);
});
