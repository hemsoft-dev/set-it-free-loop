#!/usr/bin/env node
// Stage only merged source. This creates directories and a personal T3 project,
// never a checkout/worktree/clone or a Chief configuration/service mutation.
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdirSync, readFileSync, writeFileSync, existsSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { T3 } from './t3.mjs';
import { atomic, hash, requireThat } from './store.mjs';
import { rootPath } from './run.mjs';
const exec = promisify(execFile);

export async function stage({ repository, commit, pr, chief }) {
  requireThat(/^[a-f0-9]{40}$/.test(commit) && /^\d+$/.test(pr), 'ExactMergedSourceRequired');
  const git = async args => (await exec('git',args,{cwd:repository,windowsHide:true,encoding:'buffer'})).stdout;
  requireThat((await git(['status','--porcelain'])).length === 0 && (await git(['branch','--show-current'])).toString().trim() === 'main' &&
    (await git(['rev-parse','HEAD'])).toString().trim() === commit, 'CleanMainRequired');
  const result = await exec('ssh',['-T','-o','BatchMode=yes','mini',`gh pr view ${pr} --repo hemsoft-dev/set-it-free-loop --json state,mergeCommit,url`],{windowsHide:true});
  const merged = JSON.parse(result.stdout);
  requireThat(merged.state === 'MERGED' && merged.mergeCommit?.oid === commit, 'ReviewedMergeRequired');
  const root = rootPath(); mkdirSync(root,{recursive:true});
  // Own directory only. Remove inherited broad permissions before any source,
  // private queue, capability or downloaded attachment is placed here.
  const sid = (await exec('powershell.exe',['-NoProfile','-NonInteractive','-Command', '[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value'],{windowsHide:true})).stdout.trim();
  requireThat(/^S-1-5-[\d-]+$/.test(sid),'InvalidRuntimeOwner');
  await exec('icacls.exe',[root,'/inheritance:r','/grant:r',`*${sid}:(OI)(CI)F`,'*S-1-5-18:(OI)(CI)F'],{windowsHide:true});
  const release = join(root,'releases',commit); mkdirSync(release,{recursive:true});
  const files = (await git(['ls-tree','-r','--name-only',commit,'personal-slack'])).toString().trim().split('\n').filter(p => /\.(mjs|ps1)$/.test(p) && !p.includes('/test/'));
  const manifest = {};
  for (const path of files) {
    const relative = path.slice('personal-slack/'.length); const bytes = await git(['show',`${commit}:${path}`]);
    const destination = join(release,relative); mkdirSync(join(destination,'..'),{recursive:true});
    if (existsSync(destination)) requireThat(hash(readFileSync(destination)) === hash(bytes),'ReleaseDrift');
    else writeFileSync(destination,bytes,{flag:'wx',mode:0o600});
    manifest[relative] = hash(bytes);
  }
  atomic(join(release,'manifest.json'),JSON.stringify({commit,pr:merged.url,files:manifest}));
  const workspace = join(root,'workspace'); mkdirSync(workspace,{recursive:true});
  const t3 = new T3(chief); await t3.initialize();
  const projects = await t3.call('t3_project_list',{limit:100});
  requireThat(projects.nextCursor == null,'ProjectInventoryIncomplete');
  const normalize = p => p.replaceAll('\\','/').toLowerCase();
  const matches = projects.projects.filter(p => normalize(p.workspaceRoot) === normalize(workspace));
  requireThat(matches.length <= 1,'PersonalProjectAmbiguous');
  const project = matches[0] ?? await t3.call('t3_project_create',{title:'Franz personal Slack',workspaceRoot:workspace},'human-request');
  const priorPath = join(root,'config.json');
  const prior = existsSync(priorPath) ? JSON.parse(readFileSync(priorPath,'utf8')) : {};
  if (existsSync(priorPath)) atomic(join(root,`config-before-${commit}.json`),readFileSync(priorPath));
  const config = {chief,sourceRevision:commit,projectId:project.id,workspace,helper:join(release,'run.mjs'),
    installedAfter:prior.installedAfter ?? String(Number((1569939199447273506n >> 22n) + 1420070400000n)/1000), dm:prior.dm, qualification:undefined};
  atomic(join(root,'config.json'),JSON.stringify(config));
  if (!existsSync(join(root,'HOLD'))) writeFileSync(join(root,'HOLD'),'Staged, live qualification pending.\n',{flag:'wx'});
  console.log(JSON.stringify({root,release,projectId:project.id,staged:true,activated:false}));
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [repository,commit,pr,chief] = process.argv.slice(2);
  stage({repository,commit,pr,chief}).catch(() => { console.error('Personal deployment staging refused'); process.exitCode=1; });
}
