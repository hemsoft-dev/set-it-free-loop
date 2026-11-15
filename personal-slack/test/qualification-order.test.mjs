import test from 'node:test';
import assert from 'node:assert/strict';
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

test('qualification CLI verifies the final native answer under a newest-first T3 default',t=>{
  const local=mkdtempSync(join(tmpdir(),'sfl-qualification-order-'));t.after(()=>rmSync(local,{recursive:true,force:true}));
  const root=join(local,'HemSoft','PersonalSlack'),release=join(root,'release');mkdirSync(release,{recursive:true});
  for(const file of ['qualify.mjs','run.mjs','store.mjs','slack.mjs','linked.mjs','receiver.mjs','operations.mjs'])
    copyFileSync(resolve('personal-slack',file),join(release,file));
  const source='a'.repeat(40),colors=['red','green','blue','yellow'];
  writeFileSync(join(root,'config.json'),JSON.stringify({sourceRevision:source,chief:'fixture-only'}));
  writeFileSync(join(root,'qualification-attempt.json'),JSON.stringify({key:'retained-attempt',environment:'fixture-home',
    serverVersion:'current',sourceRevision:source,provider:'codex',thread:{threadId:'fixture-canary',link:'t3-thread://fixture'},
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
          {type:'assistant_message',status:'completed',text:JSON.stringify({colors:${JSON.stringify(colors)}})}];
        return {thread:{activeRunId:null},items:args.afterPosition===-1?items:items.reverse(),hasMore:false};
      }
    }
  `);
  const env={...process.env,LOCALAPPDATA:local};for(const key of Object.keys(env))if(key.startsWith('SFL_'))delete env[key];
  const result=spawnSync(process.execPath,[join(release,'qualify.mjs'),'verify'],{env,encoding:'utf8',windowsHide:true});
  assert.equal(result.status,0,result.stderr);
  const config=JSON.parse(readFileSync(join(root,'config.json'),'utf8'));
  assert.equal(config.qualification.serverVersion,'current');assert.equal(config.qualification.sourceRevision,source);
  assert.equal(JSON.parse(readFileSync(join(root,'qualification-completed-retained-attempt.json'),'utf8')).key,'retained-attempt');
});
