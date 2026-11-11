import { createWriteStream, openSync, fsyncSync, closeSync } from 'node:fs';
import { rename, unlink } from 'node:fs/promises';
import { pipeline } from 'node:stream/promises';
import { Transform, Readable } from 'node:stream';
import { hash, requireThat } from './store.mjs';

export class SlackError extends Error {
  constructor(code, { definite = false, retryAfter = 60 } = {}) {
    super(code); this.code = /^[a-z_]+$/i.test(code) ? code : 'SlackFailure';
    this.definite = definite; this.retryAfter = retryAfter;
  }
}

export class Slack {
  constructor({ botToken, userToken, appToken, fetcher = fetch }) { Object.assign(this, { botToken, userToken, appToken, fetcher }); }
  async api(method, args = {}, token = this.botToken) {
    const body = new URLSearchParams(Object.entries(args).map(([k, v]) => [k, typeof v === 'object' ? JSON.stringify(v) : String(v)]));
    let response;
    try { response = await this.fetcher(`https://slack.com/api/${method}`, { method: 'POST', body,
      headers: { Authorization: `Bearer ${token}` }, redirect: 'error', signal: AbortSignal.timeout(25000) }); }
    catch { throw new SlackError('transport_uncertain'); }
    if (response.status === 429) throw new SlackError('ratelimited', { definite: true, retryAfter: Number(response.headers.get('retry-after')) || 60 });
    if (!response.ok) throw new SlackError('http_failure');
    let value;
    try { value = await response.json(); } catch { throw new SlackError('response_uncertain'); }
    if (!value.ok) {
      const uncertain = ['fatal_error', 'internal_error', 'request_timeout', 'service_unavailable'].includes(value.error);
      throw new SlackError(value.error ?? 'api_failure', { definite: !uncertain });
    }
    Object.defineProperty(value, 'scopes', { value: response.headers.get('x-oauth-scopes')?.split(',') ?? [] });
    return value;
  }
  async identity(expected, { needsDMOpen = true } = {}) {
    const user = await this.api('auth.test', {}, this.userToken);
    const bot = await this.api('auth.test');
    const self = (await this.api('users.info', { user: user.user_id })).user;
    const profile = (await this.api('users.info', { user: bot.user_id })).user;
    requireThat(user.team_id === expected.team && bot.team_id === expected.team && user.user_id === expected.user &&
      bot.user_id === expected.bot && bot.bot_id === expected.botId && self?.id === expected.user &&
      self.real_name === expected.realName && !self.is_bot && !self.deleted && profile?.is_bot &&
      profile.profile?.api_app_id === expected.app, 'IdentityMismatch');
    requireThat(['im:history', 'im:read', 'files:read', 'reactions:write', 'chat:write', 'users:read'].every(s => bot.scopes.includes(s)), 'MissingBotScopes');
    requireThat(!needsDMOpen || bot.scopes.includes('im:write'), 'MissingDMOpenScope');
    return { ...expected, verifiedAt: new Date().toISOString() };
  }
  async verifyDM(channel, identity) {
    const info = (await this.api('conversations.info', { channel })).channel;
    requireThat(info?.id === channel && info.is_im === true && info.user === identity.user && !info.is_shared &&
      !info.is_ext_shared && !info.is_mpim, 'NotOwnerDM');
  }
  async reaction(source, name, remove = false) {
    try { await this.api(remove ? 'reactions.remove' : 'reactions.add', { channel: source.channel, timestamp: source.ts, name }); }
    catch (e) { if (!['already_reacted', 'no_reaction'].includes(e.code)) throw e; }
  }
  async download(id, path) {
    requireThat(/^F[A-Z0-9]+$/.test(id), 'InvalidFileId');
    const file = (await this.api('files.info', { file: id })).file;
    requireThat(file?.id === id && !file.is_external && !file.external_type && file.mode !== 'external', 'ExternalFileRefused');
    const url = new URL(file.url_private_download ?? file.url_private);
    requireThat(url.protocol === 'https:' && url.hostname === 'files.slack.com' && !url.username && !url.password && !url.port, 'UnsafeFileURL');
    const max = 25 * 1024 * 1024;
    requireThat(Number.isSafeInteger(file.size) && file.size >= 0 && file.size <= max, 'AttachmentTooLarge');
    const response = await this.fetcher(url, { headers: { Authorization: `Bearer ${this.botToken}` }, redirect: 'manual', signal: AbortSignal.timeout(60000) });
    requireThat(response.status === 200 && response.body, 'AttachmentDownloadFailed');
    const type = response.headers.get('content-type')?.split(';')[0];
    requireThat(type && type !== 'text/html' && type !== 'application/xhtml+xml', 'AttachmentLoginPage');
    const temporary = `${path}.download`;
    let size = 0; const chunks = [];
    const limiter = new Transform({ transform(chunk, _encoding, callback) {
      size += chunk.length;
      if (size > max) return callback(Object.assign(new Error('AttachmentTooLarge'), { code: 'AttachmentTooLarge' }));
      chunks.push(chunk); callback(null, chunk);
    } });
    try {
      await pipeline(Readable.fromWeb(response.body), limiter, createWriteStream(temporary, { flags: 'wx', mode: 0o600 }));
      const bytes = Buffer.concat(chunks);
      requireThat(size === file.size, 'AttachmentSizeMismatch');
      if (file.mimetype?.startsWith('image/')) {
        const valid = (file.mimetype === 'image/png' && bytes.subarray(0, 8).equals(Buffer.from([137,80,78,71,13,10,26,10]))) ||
          (file.mimetype === 'image/jpeg' && bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) ||
          (file.mimetype === 'image/gif' && /^GIF8[79]a/.test(bytes.subarray(0,6).toString())) ||
          (file.mimetype === 'image/webp' && bytes.subarray(0,4).toString() === 'RIFF' && bytes.subarray(8,12).toString() === 'WEBP');
        requireThat(valid, 'ImageSignatureMismatch');
      }
      const extension = ({ 'image/png':'.png', 'image/jpeg':'.jpg', 'image/gif':'.gif', 'image/webp':'.webp', 'application/pdf':'.pdf', 'text/plain':'.txt' })[file.mimetype] ?? '.bin';
      const finalPath = path + extension;
      const descriptor=openSync(temporary,'r+');try{fsyncSync(descriptor);}finally{closeSync(descriptor);}
      await rename(temporary, finalPath);
      return { id, name: file.name, mimetype: file.mimetype, size, sha256: hash(bytes), path: finalPath };
    } catch (error) { await unlink(temporary).catch(() => {}); throw error; }
  }
}

export function candidate(payload, identity) {
  const event = payload?.event;
  return payload?.type === 'event_callback' && payload.team_id === identity.team && payload.api_app_id === identity.app &&
    (!payload.authorizations || payload.authorizations.some(a => a.team_id === identity.team && a.user_id === identity.bot && a.is_bot)) &&
    event?.type === 'message' && event.channel_type === 'im' && /^D[A-Z0-9]+$/.test(event.channel ?? '') &&
    event.user === identity.user && !event.bot_id && !event.app_id && !event.hidden &&
    [undefined, 'file_share'].includes(event.subtype) && /^\d+\.\d+$/.test(event.ts ?? '') &&
    typeof event.text === 'string' && Array.isArray(event.files ?? []) &&
    (event.files ?? []).every(f => /^F[A-Z0-9]+$/.test(f.id ?? '')) &&
    (event.thread_ts === undefined || /^\d+\.\d+$/.test(event.thread_ts));
}
