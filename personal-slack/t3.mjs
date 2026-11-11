import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { hash, requireThat } from './store.mjs';
const exec = promisify(execFile);
const sourceDigest = bytes => hash(new TextDecoder('utf-8',{fatal:true,ignoreBOM:true}).decode(bytes).replaceAll('\r\n','\n'));
export const CHIEF_PIN = '478ed2ead9e9f042acba9120c7b67268ea491eca';
const TOOLS = new Set(['t3_thread_launch', 't3_thread_send', 't3_thread_read', 't3_thread_list',
  't3_project_list', 't3_project_read', 't3_project_create', 't3_environment_read', 'orchestrator_capabilities']);
const NOT_SENT = new Set(['InvalidArguments', 'UnknownEndpoint', 'ToolUnavailable', 'LiveModeLaunchRefused']);

// This client reads the reviewed Chief transport source without modifying it.
// The transport can mutate T3. The personal client narrows it to Home and these tools.
export class T3 {
  constructor(chief) { this.chief = chief; }
  async initialize() {
    this.hashes = {};
    for (const script of ['chief-remote.mjs', 'chief-requests.mjs']) {
      const { stdout } = await exec('git', ['show', `${CHIEF_PIN}:scripts/${script}`], { cwd: this.chief, encoding: 'buffer', windowsHide: true });
      const local=readFileSync(join(this.chief, 'scripts', script));
      requireThat(sourceDigest(stdout) === sourceDigest(local), 'ChiefTransportDrift');
      this.hashes[script]=hash(local); // Subsequent changes, even line endings, hold.
    }
    this.remote = (await import(pathToFileURL(join(this.chief, 'scripts/chief-remote.mjs')).href)).invokeRemote;
  }
  async call(tool, args, launchClass = 'advisory-readonly') {
    requireThat(TOOLS.has(tool) && !args.environmentId, 'PersonalToolRefused');
    requireThat(!['t3_thread_launch','t3_project_create'].includes(tool) || launchClass==='human-request','PersonalLaunchClassRequired');
    requireThat(tool!=='t3_thread_send' || launchClass==='owner-continuation','PersonalContinuationClassRequired');
    requireThat(this.remote, 'TransportNotInitialized');
    for(const [script,digest] of Object.entries(this.hashes)) requireThat(hash(readFileSync(join(this.chief,'scripts',script)))===digest,'ChiefTransportDrift');
    // invokeRemote is synchronous, so run the owning script in a hidden subprocess
    // to keep Slack intake/ACK independent of network waits. UTF-8 bytes on stdin.
    const input = JSON.stringify(args);
    const child = await import('node:child_process');
    const result = await new Promise((resolve, reject) => {
      const p = child.spawn(process.execPath, [join(this.chief, 'scripts/chief-remote.mjs'), 'mcp', 'home', tool,
        '--args-file', '-', '--launch-class', launchClass], { windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] });
      const out = []; let bytes = 0;
      const timer = setTimeout(() => { p.kill(); reject(Object.assign(new Error('T3TransportUncertain'), { code: 'T3TransportUncertain' })); }, 250000);
      p.stdout.on('data', b => { bytes += b.length; if (bytes > 16 * 1024 * 1024) p.kill(); else out.push(b); });
      p.stderr.resume(); // Never expose remote bodies, prompt text, or credentials.
      p.on('error', () => { clearTimeout(timer); reject(new Error('T3TransportUncertain')); });
      p.on('close', () => {
        clearTimeout(timer);
        try { resolve(JSON.parse(Buffer.concat(out).toString('utf8'))); } catch { reject(new Error('T3TransportUncertain')); }
      });
      p.stdin.on('error', () => {}); p.stdin.end(Buffer.from(input, 'utf8'));
    });
    if (!result.ok) {
      const code = result.error?.name ?? 'T3TransportUncertain';
      throw Object.assign(new Error(code), { code, definite: result.error?.stage === 'parse' || NOT_SENT.has(code) });
    }
    return result.result;
  }
}

// All Mini helpers execute where their credentials already live. Only JSON evidence
// returns Home. Script bytes go to SSH stdin, never through a PowerShell pipeline.
const MINI_POLICY = `import sys, json, importlib.util, pathlib, subprocess, base64
root=pathlib.Path('/home/franz/github/hemsoft/chief/scripts')
def load(name):
 spec=importlib.util.spec_from_file_location(name.replace('-','_'), root/(name+'.py'))
 module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); return module
req=json.loads(base64.b64decode(sys.argv[1]))
if req['operation']=='snapshot':
 gate=load('routine-gate'); token=gate.TOKEN.read_text().strip()
 machines=gate.pmd('/v1/machines',token)['machines']
 activity=gate.pmd('/v1/fleet/activity',token)['machines']
 claims=subprocess.run(['python3',str(root/'repo-claim.py'),'list'],capture_output=True,check=True).stdout
 pick=subprocess.run(['python3',str(root/'pick-provider.py'),'--json'],capture_output=True,check=True).stdout
 print(json.dumps({'machines':[{'name':m['name'],'maxSessions':m['maxSessions']} for m in machines if m['name']=='home'],'activity':[{'name':m['name'],'state':m.get('state'),'activeCount':m.get('activeCount')} for m in activity if m['name']=='home'],'claims':json.loads(claims),'model':json.loads(pick)}))
elif req['operation']=='unpaused':
 pause=load('t3_paused'); data=pause.load()
 assert isinstance(data.get('paused'),dict) and isinstance(data.get('resumable'),dict)
 view=req['view']; thread=view['thread']; run=(view.get('recentRuns') or [{}])[-1]
 reason=pause.hands_off('home',thread['threadId'],run.get('status',thread.get('status')),run.get('completedAt') or thread.get('updatedAt'),data)
 print(json.dumps({'allowed':reason is None}))
else: raise ValueError('UnsupportedPersonalPolicyOperation')
`;

export async function miniPolicy(operation, view) {
  // Code is raw UTF-8 LF bytes on SSH stdin. The only command argument is
  // base64 JSON, whose alphabet cannot introduce remote-shell syntax.
  const encoded = Buffer.from(JSON.stringify({ operation, view }), 'utf8').toString('base64');
  const command = `python3 - ${encoded}`;
  const { spawn } = await import('node:child_process');
  return new Promise((resolve, reject) => {
    const p = spawn('ssh', ['-T', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', 'mini', command], { windowsHide: true, stdio: ['pipe','pipe','pipe'] });
    const chunks = []; let size = 0;
    const timer = setTimeout(() => { p.kill(); reject(new Error('PolicyUnavailable')); }, 180000);
    p.stdout.on('data', b => { size += b.length; if (size > 4 * 1024 * 1024) p.kill(); else chunks.push(b); }); p.stderr.resume();
    p.on('error', () => { clearTimeout(timer); reject(new Error('PolicyUnavailable')); });
    p.on('close', code => { clearTimeout(timer); try { requireThat(code === 0, 'PolicyUnavailable'); resolve(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch { reject(new Error('PolicyUnavailable')); } });
    p.stdin.on('error', () => {}); p.stdin.end(Buffer.from(MINI_POLICY, 'utf8'));
  });
}

export async function admission(t3, config, policy = miniPolicy) {
  if(config.qualification) {
    const environment=await t3.call('t3_environment_read',{});
    requireThat(environment.environmentId===config.qualification.homeEnvironment && environment.serverVersion===config.qualification.serverVersion,
      'LiveQualificationRequired');
  }
  const snapshot = await policy('snapshot');
  requireThat(snapshot.claims && typeof snapshot.claims === 'object' && !Array.isArray(snapshot.claims), 'ClaimsUnavailable');
  requireThat(Object.entries(snapshot.claims).every(([repo, claim]) => /^[\w.-]+\/[\w.-]+$/.test(repo) && typeof claim?.owner === 'string'), 'ClaimsUnavailable');
  const machine = snapshot.machines?.find(m => m.name === 'home');
  const activity = snapshot.activity?.find(m => m.name === 'home');
  requireThat(Number.isInteger(machine?.maxSessions) && machine.maxSessions > 0 && activity?.state === 'ok' &&
    Number.isInteger(activity.activeCount) && activity.activeCount < Math.min(machine.maxSessions, 6), 'HomeCapacityHeld');
  const model = snapshot.model;
  requireThat(['codex','claudeAgent'].includes(model?.instanceId) && typeof model.model === 'string' &&
    model.options?.some(o => ['reasoningEffort','effort'].includes(o.id) && o.value === 'high'), 'PickerHeld');
  const caps = await t3.call('orchestrator_capabilities', { search: model.model });
  const provider = caps.providers?.find(p => p.providerInstanceId === model.instanceId);
  const selected = provider?.models?.find(m => m.id === model.model);
  const effort = model.options.find(o => ['reasoningEffort','effort'].includes(o.id));
  requireThat(provider?.canRunChildTask && selected?.options?.some(o => o.id === effort.id && o.values.includes('high')), 'ProviderUnavailable');
  const project = await t3.call('t3_project_read', { projectId: config.projectId });
  requireThat(project.id === config.projectId && !project.deletedAt && !project.archivedAt && project.workspaceRoot.replaceAll('\\','/').toLowerCase() === config.workspace.replaceAll('\\','/').toLowerCase(), 'PersonalProjectMismatch');
  return { model, claims: snapshot.claims };
}

export async function ownerAvailable(t3, threadId, config, policy = miniPolicy, { resultOnly = false } = {}) {
  const view = await t3.call('t3_thread_read', { threadId, limit: 1, maxCharsPerItem: 1000, runLimit: 5 });
  const t = view.thread;
  requireThat(t?.threadId === threadId && t.projectId === config.projectId && !t.archived &&
    (resultOnly || (!t.settled && !t.snoozed && !t.pendingRequestCount)), 'OwnerHeld');
  const minimal = { thread: Object.fromEntries(['threadId','status','updatedAt'].map(k => [k,t[k]])),
    recentRuns: view.recentRuns?.sort((a,b) => a.ordinal-b.ordinal).map(r => ({ status: r.status, completedAt: r.completedAt })) };
  requireThat((await policy('unpaused', minimal)).allowed === true, 'OwnerPaused');
  return view;
}
