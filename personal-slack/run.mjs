#!/usr/bin/env node
import { existsSync, readFileSync, writeFileSync, linkSync, unlinkSync, mkdirSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomUUID } from 'node:crypto';
import { Store, atomic, enqueue, hash, requireThat } from './store.mjs';
import { Slack } from './slack.mjs';
import { Receiver, signOperation } from './receiver.mjs';
import { T3 } from './t3.mjs';
import { operationBody } from './operations.mjs';

export const EXPECTED = Object.freeze({ team: 'T1KAG2BV5', user: 'U2XMZDPJ7', realName: 'Franz Hemmer',
  bot: 'U0B4SG9L6UX', botId: 'B0B59QTAWPK', app: 'A0B627TMM6U' });
export const rootPath = () => join(process.env.LOCALAPPDATA ?? '', 'HemSoft', 'PersonalSlack');
const json = path => JSON.parse(readFileSync(path, 'utf8'));
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

export function validateQualification(config, environment) {
  const q = config.qualification;
  requireThat(q?.schema === 1 && q.homeEnvironment === environment.environmentId && q.serverVersion === environment.serverVersion &&
    q.launchSameThread === true && q.sendSameMessage === true && q.ambiguousPolicy === 'reconcile-only' &&
    Array.isArray(q.providers) && q.providers.length > 0 && q.providers.every(p => ['codex','claudeAgent'].includes(p)) &&
    q.sourceRevision === config.sourceRevision, 'LiveQualificationRequired');
}

function credentials() {
  const botToken = process.env.SFL_SLACK_BOT_USER_OAUTH_TOKEN;
  const userToken = process.env.SFL_SLACK_USER_OAUTH_TOKEN;
  const appToken = process.env.SFL_APP_LEVEL_TOKEN;
  requireThat(botToken && userToken && appToken, 'ExistingEnvironmentCredentialsMissing');
  return { botToken, userToken, appToken };
}

export async function serve(root, config) {
  const release=dirname(config.helper); const manifest=json(join(release,'manifest.json'));
  requireThat(resolve(config.helper)===fileURLToPath(import.meta.url) && manifest.commit===config.sourceRevision,'PersonalReleaseMismatch');
  for(const [path,digest] of Object.entries(manifest.files)) requireThat(hash(readFileSync(join(release,path)))===digest,'PersonalReleaseDrift');
  const store = new Store(root); const unlock = store.lock();
  const slack = new Slack(credentials()); const t3 = new T3(config.chief);
  let socket; let stopped = false;
  const stop = () => { stopped = true; socket?.close(); };
  process.once('SIGINT', stop); process.once('SIGTERM', stop);
  try {
    await t3.initialize();
    const environment = await t3.call('t3_environment_read', {});
    requireThat(environment.label.toLowerCase() === 'home' && environment.platform.os === 'windows', 'HomeIdentityMismatch');
    validateQualification(config, environment);
    const identity = await slack.identity(EXPECTED, { needsDMOpen:!config.dm });
    store.state.identity = identity; store.save();
    if (!config.dm) {
      // Open only the verified owner's DM. This sends no message and discovers
      // no directory/channel inventory. Subsequent connections use this ID only.
      const dm = await slack.api('conversations.open',{ users:identity.user, return_im:true });
      requireThat(/^D[A-Z0-9]+$/.test(dm.channel?.id ?? ''),'OwnerDMUnavailable');
      await slack.verifyDM(dm.channel.id,identity);
      config.dm=dm.channel.id; atomic(join(root,'config.json'),JSON.stringify(config));
    }
    const receiver = new Receiver({ store, slack, t3, config, identity });
    const pump = () => {
      if (existsSync(join(root,'STOP'))) stop();
      return receiver.pump().catch(() => { if (store.failed) stop(); });
    };
    const timer = setInterval(pump, 5000);
    try {
      while (!stopped) {
        try {
          const { url } = await slack.api('apps.connections.open', {}, slack.appToken);
          const endpoint = new URL(url);
          requireThat(endpoint.protocol === 'wss:' && endpoint.hostname.endsWith('.slack.com') && !endpoint.username && !endpoint.password, 'UnsafeSocketURL');
          let hello = false;
          await new Promise((resolveConnection, rejectConnection) => {
            socket = new WebSocket(url);
            const deadline = setTimeout(() => socket.close(), 45000);
            // After hello, application silence is normal. Native WebSocket owns
            // ping/pong control frames; reconnect on real lifecycle events only.
            socket.onmessage = event => {
              try {
                const message = JSON.parse(event.data);
                if (message.type === 'hello') {
                  requireThat(message.connection_info?.app_id === identity.app && message.num_connections === 1, 'SocketIdentityOrConsumerConflict');
                  hello = true; clearTimeout(deadline);
                  store.state.socket = { app: identity.app, connectedAt: Date.now(), consumers: message.num_connections }; store.save();
                  // No channel discovery. Catch up only the exact previously verified
                  // DM, from installation/last complete private cursor, at most 1000.
                  void (async () => {
                    const channels = new Set(Object.values(store.state.requests).filter(r => r.verified).map(r => r.source.channel));
                    if (config.dm) channels.add(config.dm);
                    for (const channel of channels) {
                      await receiver.catchUp(channel, store.state.cursors[channel] ?? config.installedAfter);
                      await receiver.catchUpThreads(channel,config.installedAfter);
                    }
                    await pump();
                  })().catch(() => receiver.incident('catchup', 'CatchUpHeld'));
                  return;
                }
                if (message.type === 'disconnect') { socket.close(); return; }
                requireThat(hello, 'SocketHelloRequired');
                if (message.type === 'events_api') {
                  const key = receiver.accept(message.payload);
                  if (key) void receiver.receipt(store.state.requests[key]).catch(() => {});
                }
                if (typeof message.envelope_id === 'string') socket.send(JSON.stringify({ envelope_id: message.envelope_id }));
                void pump();
              } catch {
                // Persistence/identity failures never ACK. Stop on state failure;
                // reconnect on protocol failure without logging payload or ticket.
                socket.close(); if (store.failed) stopped = true;
              }
            };
            socket.onclose = () => { clearTimeout(deadline); resolveConnection(); };
            socket.onerror = () => { clearTimeout(deadline); socket.close(); rejectConnection(new Error('SocketUnavailable')); };
          });
        } catch { if (!stopped) { store.state.socketError = 'SocketUnavailable'; store.save(); } }
        if (!stopped) await sleep(5000);
      }
    } finally { clearInterval(timer); await receiver.running; }
  } finally { socket?.close(); unlock(); }
}

export async function main(argv) {
  const [action, ...rest] = argv; const root = rootPath();
  requireThat(process.env.LOCALAPPDATA && resolve(root).toLowerCase().startsWith(resolve(process.env.LOCALAPPDATA).toLowerCase()), 'PrivateRuntimeRequired');
  if (action === 'doctor') {
    const configFile=join(root,'config.json');
    const needsDMOpen=!existsSync(configFile) || !json(configFile).dm;
    const identity = await new Slack(credentials()).identity(EXPECTED, { needsDMOpen });
    console.log(JSON.stringify({ identity, credentials: 'verified in process; values omitted' })); return;
  }
  if (action === 'submit') {
    const flags = Object.fromEntries(rest.reduce((a, x, i) => i % 2 ? a : [...a, [x,rest[i+1]]], []));
    requireThat(rest.length === 6 && flags['--source'] && flags['--thread'] && flags['--file'] && /^[a-f0-9]{64}$/.test(flags['--source']), 'InvalidSubmitArguments');
    const body = operationBody(json(flags['--file']));
    const op = { ...body, source: flags['--source'], thread: flags['--thread'] };
    const state = json(join(root, 'state.json')).data;
    const entry = state.requests[op.source];
    requireThat(entry?.owner?.threadId === op.thread && entry.status === 'delivered', 'OperationOwnerMismatch');
    const secret = readFileSync(join(root,'files',op.source,'owner.cap'));
    requireThat(hash(secret) === entry.capHash, 'InvalidCapability');
    const packet = { ...op, signature: signOperation(secret,op) };
    const item = enqueue(root,packet); const temporary = `${item.path}.${randomUUID()}.tmp`;
    atomic(temporary,item.bytes);
    try { linkSync(temporary,item.path); }
    catch (e) { requireThat(e.code === 'EEXIST' && readFileSync(item.path,'utf8') === item.bytes, 'OperationConflict'); }
    finally { unlinkSync(temporary); }
    console.log(JSON.stringify({ operation:item.id,status:'durably queued, external delivery unconfirmed' })); return;
  }
  if (action === 'status') {
    const state = json(join(root,'state.json')).data;
    const source = rest[0] === '--source' ? rest[1] : undefined;
    requireThat(!source || /^[a-f0-9]{64}$/.test(source), 'InvalidSource');
    console.log(JSON.stringify({ held:existsSync(join(root,'HOLD')), socket:state.socket,
      requests:Object.values(state.requests).filter(e => !source || e.key === source).map(e => ({ source:e.key,status:e.status,deliveryState:e.deliveryState,intentRevision:e.intentRevision,error:e.error,owner:e.owner,outcome:e.outcome })),
      operations:Object.values(state.operations).filter(o => !source || o.source === source).map(o => ({ id:o.id,sequence:o.sequence,predecessor:o.predecessor,status:o.status,error:o.error })),
      incidents:state.incidents })); return;
  }
  if (action === 'serve') { await serve(root,json(join(root,'config.json'))); return; }
  throw new Error('UnsupportedPersonalCommand');
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).catch(e => {
    // Codes only. No API responses, token values, websocket URLs or message text.
    console.error(JSON.stringify({ error: /^[A-Za-z_]+$/.test(e.code ?? e.message) ? (e.code ?? e.message) : 'PersonalSlackFailed' })); process.exitCode = 1;
  });
}
