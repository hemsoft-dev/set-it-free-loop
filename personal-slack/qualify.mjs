#!/usr/bin/env node
// Post-review, bounded Home qualification. No restart, polling, new delegate,
// Slack send, production intake, or credential movement is performed here.
import { readFileSync, writeFileSync, unlinkSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID, randomInt } from 'node:crypto';
import { deflateSync } from 'node:zlib';
import { rootPath, validateQualification } from './run.mjs';
import { T3, admission } from './t3.mjs';
import { atomic, requireThat } from './store.mjs';

function crc(bytes) {
  let c = 0xffffffff;
  for (const b of bytes) { c ^= b; for(let k=0;k<8;k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; }
  const result = Buffer.alloc(4); result.writeUInt32BE((c ^ 0xffffffff) >>> 0); return result;
}
function chunk(name,data) {
  const length = Buffer.alloc(4); length.writeUInt32BE(data.length); const body=Buffer.concat([Buffer.from(name),data]);
  return Buffer.concat([length,body,crc(body)]);
}
function image(colors) {
  const rgb={red:[255,0,0],green:[0,255,0],blue:[0,0,255],yellow:[255,255,0]};
  const rows=[];
  for(let y=0;y<128;y++) { const row=[0]; for(let x=0;x<128;x++) row.push(...rgb[colors[(y>=64?2:0)+(x>=64?1:0)]]); rows.push(Buffer.from(row)); }
  const header=Buffer.alloc(13); header.writeUInt32BE(128,0);header.writeUInt32BE(128,4);header[8]=8;header[9]=2;
  return Buffer.concat([Buffer.from([137,80,78,71,13,10,26,10]),chunk('IHDR',header),chunk('IDAT',deflateSync(Buffer.concat(rows))),chunk('IEND',Buffer.alloc(0))]);
}

async function main(action) {
  const root=rootPath(); const config=JSON.parse(readFileSync(join(root,'config.json'),'utf8'));
  const t3=new T3(config.chief); await t3.initialize();
  const env=await t3.call('t3_environment_read',{});
  requireThat(env.label.toLowerCase()==='home' && env.platform.os==='windows','HomeIdentityMismatch');
  const path=join(root,'qualification-attempt.json');
  if(action==='start') {
    try { readFileSync(path); throw new Error('ExistingQualificationAttempt'); } catch(e) { if(e.code!=='ENOENT') throw e; }
    const {model}=await admission(t3,{...config,qualification:undefined});
    const key=randomUUID(); const colors=['red','green','blue','yellow'].sort(()=>randomInt(3)-1);
    const fixture=join(config.workspace,`qualification-${key}.png`); writeFileSync(fixture,image(colors),{flag:'wx'});
    const launch={projectId:config.projectId,title:`Personal Slack qualification ${key}`,clientRequestId:`personal-qualification-${key}`,modelSelection:model,workspaceStrategy:{type:'root'}};
    // Freeze before even an idle launch. A failed/ambiguous step is left for
    // the operator; do not start another attempt to recover an existing one.
    const attempt={key,environment:env.environmentId,serverVersion:env.serverVersion,provider:model.instanceId,sourceRevision:config.sourceRevision,launch,colors};
    atomic(path,JSON.stringify(attempt));
    const first=await t3.call('t3_thread_launch',launch,'human-request'); attempt.thread=first; atomic(path,JSON.stringify(attempt));
    const second=await t3.call('t3_thread_launch',launch,'human-request');
    requireThat(first.threadId===second.threadId,'LaunchDedupeFailed'); attempt.launchSameThread=true;
    const send={threadId:first.threadId,clientRequestId:`personal-qualification-send-${key}`,mode:'auto',message:
      `Bounded personal Slack qualification on Home. Do not delegate, contact anyone, change repositories/config/services, read credentials, or open UI. Use your native image read tool on ${JSON.stringify(fixture)}. Return ONLY JSON {"colors":[top-left,top-right,bottom-left,bottom-right]} naming the four quadrant colors. Do not read qualification-attempt.json or other files. No Slack messages. This is a capability check, not actual user admission.`};
    attempt.send=send; atomic(path,JSON.stringify(attempt));
    const a=await t3.call('t3_thread_send',send,'owner-continuation'); attempt.sent=a; atomic(path,JSON.stringify(attempt));
    const b=await t3.call('t3_thread_send',send,'owner-continuation');
    requireThat(a.messageId===b.messageId && a.runId===b.runId && a.threadId===b.threadId,'SendDedupeFailed');
    attempt.sendSameMessage=true; atomic(path,JSON.stringify(attempt));
    console.log(JSON.stringify({status:'pending actual image result',link:first.link,threadId:first.threadId})); return;
  }
  requireThat(action==='verify','UnsupportedQualificationAction');
  const attempt=JSON.parse(readFileSync(path,'utf8'));
  requireThat(attempt.serverVersion===env.serverVersion && attempt.environment===env.environmentId && attempt.sourceRevision===config.sourceRevision,'QualificationEnvironmentChanged');
  const view=await t3.call('t3_thread_read',{threadId:attempt.thread.threadId,limit:20,maxCharsPerItem:2000});
  requireThat(!view.hasMore,'QualificationResultReadIncomplete');
  const result=view.items?.filter(i=>i.type==='assistant_message' && i.status==='completed').at(-1);
  const text=result?.text?.replace(/^```(?:json)?\s*|\s*```$/g,'');
  requireThat(!view.thread.activeRunId && JSON.stringify(JSON.parse(text).colors)===JSON.stringify(attempt.colors),'NativeImageQualificationPendingOrFailed');
  const q={schema:1,homeEnvironment:env.environmentId,serverVersion:env.serverVersion,sourceRevision:config.sourceRevision,
    launchSameThread:attempt.launchSameThread,sendSameMessage:attempt.sendSameMessage,ambiguousPolicy:'reconcile-only',providers:[...new Set([
      ...(config.qualification?.serverVersion===env.serverVersion && config.qualification?.sourceRevision===config.sourceRevision ? config.qualification.providers : []),attempt.provider])],
    canaryThread:attempt.thread.threadId,canaryLink:attempt.thread.link,verifiedAt:new Date().toISOString()};
  validateQualification({...config,qualification:q},env);
  config.qualification=q; atomic(join(root,'config.json'),JSON.stringify(config));
  atomic(join(root,`qualification-completed-${attempt.key}.json`),JSON.stringify(attempt)); unlinkSync(path);
  console.log(JSON.stringify({qualification:q,activated:false}));
}
main(process.argv[2]).catch(e=>{console.error(JSON.stringify({error:/^[A-Za-z_]+$/.test(e.code??e.message)?(e.code??e.message):'QualificationFailed',activated:false}));process.exitCode=1;});
