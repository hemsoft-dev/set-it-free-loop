import test from 'node:test';
import assert from 'node:assert/strict';
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

for(const [name,answer,accepted,mismatch=''] of [
  ['final answer under a newest-first T3 default',['red','green','blue','yellow'],true],
  ['native lime name for the exact bright-green quadrant',['red','lime','blue','yellow'],true],
  ['lime in the wrong quadrant',['lime','red','blue','yellow'],false],
  ['a different green hue',['red','teal','blue','yellow'],false],
  ['an incomplete answer',['red','lime','blue'],false],
  ['an attempt for a different installed source',['red','lime','blue','yellow'],false,'source'],
  ['an attempt for a different server',['red','lime','blue','yellow'],false,'server'],
  ['a canary from a different project',['red','lime','blue','yellow'],false,'project'],
  ['a canary from a different provider',['red','lime','blue','yellow'],false,'provider'],
  ['a different canary thread',['red','lime','blue','yellow'],false,'thread'],
]) test(`qualification CLI verifies ${name} without another launch`,t=>{
  const local=mkdtempSync(join(tmpdir(),'sfl-qualification-order-'));t.after(()=>rmSync(local,{recursive:true,force:true}));
  const root=join(local,'HemSoft','PersonalSlack'),release=join(root,'release');mkdirSync(release,{recursive:true});
  for(const file of ['qualify.mjs','run.mjs','store.mjs','slack.mjs','linked.mjs','receiver.mjs','operations.mjs'])
    copyFileSync(resolve('personal-slack',file),join(release,file));
  const source='a'.repeat(40),colors=['red','green','blue','yellow'];
  const originalConfig=JSON.stringify({sourceRevision:source,chief:'fixture-only',projectId:'fixture-project'});
  writeFileSync(join(root,'config.json'),originalConfig);
  writeFileSync(join(root,'qualification-attempt.json'),JSON.stringify({key:'retained-attempt',environment:'fixture-home',
    serverVersion:mismatch==='server'?'old-server':'current',sourceRevision:mismatch==='source'?'b'.repeat(40):source,
    provider:'codex',launch:{projectId:'fixture-project',modelSelection:{instanceId:'codex'}},
    thread:{threadId:'fixture-canary',link:'t3-thread://fixture'},
    colors,launchSameThread:true,sendSameMessage:true}));
  writeFileSync(join(release,'t3.mjs'),`
    export const CHIEF_PIN='fixture-only';
    export async function admission(){throw Error('No new attempt authorized');}
    export async function ownerAvailable(){throw Error('No owner mutation authorized');}
    export class T3 {
      async initialize(){}
      async call(tool,args){
        if(tool==='t3_environment_read')return {environmentId:'fixture-home',serverVersion:'current',label:'home',platform:{os:'windows'}};
        if(tool!=='t3_thread_read')throw Error('Unexpected mutation');
        const items=[{type:'user_message',status:'completed',text:'Native-image canary'},
          {type:'assistant_message',status:'completed',text:'I will open the image.'},
          {type:'assistant_message',status:'completed',text:JSON.stringify({colors:${JSON.stringify(answer)}})}];
        return {thread:{activeRunId:null,
          threadId:${JSON.stringify(mismatch==='thread'?'other-canary':'fixture-canary')},
          projectId:${JSON.stringify(mismatch==='project'?'other-project':'fixture-project')},
          providerInstanceId:${JSON.stringify(mismatch==='provider'?'claudeAgent':'codex')}},
          items:args.afterPosition===-1?items:items.reverse(),hasMore:false};
      }
    }
  `);
  const env={...process.env,LOCALAPPDATA:local};for(const key of Object.keys(env))if(key.startsWith('SFL_'))delete env[key];
  const frozenAttempt=readFileSync(join(root,'qualification-attempt.json'),'utf8');
  const result=spawnSync(process.execPath,[join(release,'qualify.mjs'),'verify'],{env,encoding:'utf8',windowsHide:true});
  if(!accepted){
    assert.equal(result.status,1);assert.match(result.stderr,['source','server'].includes(mismatch)?/QualificationEnvironmentChanged/:
      mismatch?/QualificationOwnerMismatch/:/NativeImageQualificationPendingOrFailed/);
    assert.equal(readFileSync(join(root,'config.json'),'utf8'),originalConfig);
    assert.equal(readFileSync(join(root,'qualification-attempt.json'),'utf8'),frozenAttempt);
    assert.equal(existsSync(join(root,'qualification-completed-retained-attempt.json')),false);return;
  }
  assert.equal(result.status,0,result.stderr);
  const config=JSON.parse(readFileSync(join(root,'config.json'),'utf8'));
  assert.equal(config.qualification.serverVersion,'current');assert.equal(config.qualification.sourceRevision,source);
  assert.equal(readFileSync(join(root,'qualification-completed-retained-attempt.json'),'utf8'),frozenAttempt);
});
