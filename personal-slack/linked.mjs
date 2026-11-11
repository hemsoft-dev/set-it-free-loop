import { requireThat } from './store.mjs';

export const LINK_WORKSPACE = Object.freeze({ host:'relias-engineering.slack.com', team:'T1KAG2BV5' });
export const LINK_LIMITS = Object.freeze({ links:3, pages:7, pageSize:15, messages:100, bytes:250000, files:10, fileBytes:100*1024*1024 });
const timestamp = value => typeof value === 'string' && /^\d{10,12}\.\d{6}$/.test(value);
const ticks = value => BigInt(value.replace('.',''));
const code = error => /^[a-z_]+$/i.test(error?.code ?? error?.message ?? '') ? error.code ?? error.message : 'LinkedReadUnavailable';

// Only authored sections/lists select reads. Never walk attachments, unfurls,
// forwarded messages, quotes, code, or a fetched message for more links.
export function authoredText(event) {
  const blocks = event.blocks?.filter(b=>b.type==='rich_text');
  const section = element => element.type==='rich_text_section' ? (element.elements ?? []).map(e=>
    e.style?.code ? '' : e.type==='link' ? ` <${e.url}> ` : e.type==='text' ? e.text : '').join('') :
    element.type==='rich_text_list' ? (element.elements ?? []).map(section).join('\n') : '';
  const text = blocks?.length ? blocks.flatMap(b=>(b.elements ?? []).map(section)).join('\n') : event.text;
  return (text ?? '').replace(/```[\s\S]*?(?:```|$)/g,'').replace(/`[^`\n]*(?:`|$)/g,'')
    .split('\n').filter(line=>!/^\s*(?:>|&gt;)/.test(line)).join('\n');
}

export function parsePermalink(raw) {
  requireThat(typeof raw==='string' && raw.length<=2048,'InvalidSlackLink');
  let url; try { url=new URL(raw.replaceAll('&amp;','&')); } catch { throw new Error('InvalidSlackLink'); }
  requireThat(url.protocol==='https:' && url.hostname===LINK_WORKSPACE.host && !url.port && !url.username && !url.password && !url.hash,'LinkedWorkspaceMismatch');
  const match=/^\/archives\/([CGD][A-Z0-9]+)\/p(\d{10,12})(\d{6})$/.exec(url.pathname);
  requireThat(match,'InvalidSlackLink');
  for(const key of url.searchParams.keys()) requireThat(['thread_ts','cid','team'].includes(key) && url.searchParams.getAll(key).length===1,'InvalidSlackLink');
  requireThat(!url.searchParams.has('team') || url.searchParams.get('team')===LINK_WORKSPACE.team,'LinkedWorkspaceMismatch');
  requireThat(!url.searchParams.has('cid') || url.searchParams.get('cid')===match[1],'LinkedChannelMismatch');
  const ts=`${match[2]}.${match[3]}`; const root=url.searchParams.get('thread_ts');
  requireThat(root===null || (timestamp(root) && ticks(root)<=ticks(ts)),'LinkedThreadMismatch');
  return {url:url.href,host:LINK_WORKSPACE.host,team:LINK_WORKSPACE.team,channel:match[1],ts,...(root ? {threadTs:root} : {})};
}

export function sourceLinks(event) {
  const references=[]; const errors=[];
  for(const match of authoredText(event).matchAll(/https?:\/\/[^\s<>|]+/g)) {
    const raw=match[0].replace(/[),.;!?]+$/,'');
    if(!/slack\.com|\/archives\//i.test(raw)) continue;
    try {
      const ref=parsePermalink(raw);
      if(references.some(r=>r.channel===ref.channel && r.ts===ref.ts && r.threadTs===ref.threadTs)) continue;
      if(references.length>=LINK_LIMITS.links) { errors.push({error:'LinkedSourceLimit'}); break; }
      references.push(ref);
    } catch(error) { errors.push({url:raw.slice(0,2048),error:code(error)}); if(errors.length>=LINK_LIMITS.links) break; }
  }
  return {schema:1,references,errors};
}

// read is a credential-local, fixed Slack API callback, never an arbitrary URL.
// Progress is persisted by the daemon after each page and before the next read.
export async function readLinked(ref, identity, read, progress=()=>{}) {
  const parsed=parsePermalink(ref.url);
  requireThat(JSON.stringify(parsed)===JSON.stringify(ref) && identity.team===ref.team,'LinkedWorkspaceMismatch');
  const result={reference:ref,messages:[],complete:false,targetFound:false,pages:0};
  const save=()=>progress(structuredClone(result));
  const add=message=>{
    requireThat(message?.type==='message' && timestamp(message.ts) && (!message.team || message.team===ref.team) &&
      (!message.channel || message.channel===ref.channel),'LinkedMessageMismatch');
    requireThat(message.ts===result.rootTs ? !message.thread_ts || message.thread_ts===result.rootTs : message.thread_ts===result.rootTs,'LinkedThreadMismatch');
    if(result.messages.some(m=>m.ts===message.ts)) return;
    requireThat(result.messages.length<LINK_LIMITS.messages && Buffer.byteLength(JSON.stringify([...result.messages,message]))<=LINK_LIMITS.bytes,'LinkedThreadLimit');
    result.messages.push(structuredClone(message));
    if(message.ts===ref.ts) result.targetFound=true;
  };
  const exact=async ts=>{
    const page=await read('conversations.history',{channel:ref.channel,oldest:ts,latest:ts,inclusive:true,limit:1});
    requireThat(Array.isArray(page.messages) && page.messages.length===1 && page.messages[0].ts===ts,'LinkedTargetUnavailable');
    return page.messages[0];
  };
  try {
    const first=await exact(ref.threadTs ?? ref.ts);
    result.rootTs=ref.threadTs ?? first.thread_ts ?? first.ts;
    requireThat(timestamp(result.rootTs) && ticks(result.rootTs)<=ticks(ref.ts),'LinkedThreadMismatch');
    if(ref.threadTs) requireThat(!first.thread_ts || first.thread_ts===ref.threadTs,'LinkedThreadMismatch');
    const root=first.ts===result.rootTs ? first : await exact(result.rootTs);
    add(root); if(first.ts!==root.ts) add(first); save();
    {
      let cursor; const seen=new Set();
      do {
        requireThat(result.pages<LINK_LIMITS.pages,'LinkedThreadLimit');
        const page=await read('conversations.replies',{channel:ref.channel,ts:result.rootTs,limit:LINK_LIMITS.pageSize,...(cursor ? {cursor} : {})});
        result.pages++;
        requireThat(Array.isArray(page.messages),'LinkedThreadUnavailable');
        for(const message of page.messages) add(message);
        cursor=page.response_metadata?.next_cursor || undefined;
        requireThat(!page.has_more || cursor,'LinkedThreadIncomplete');
        requireThat(!cursor || (typeof cursor==='string' && !seen.has(cursor)),'LinkedThreadIncomplete');
        if(cursor) seen.add(cursor);
        result.hasMore=Boolean(cursor || page.has_more); save();
      } while(cursor);
    }
    requireThat(root.reply_count===undefined || (Number.isSafeInteger(root.reply_count) && root.reply_count>=0 &&
      result.messages.filter(m=>m.ts!==result.rootTs).length>=root.reply_count),'LinkedThreadIncomplete');
    requireThat(result.targetFound,'LinkedTargetOutsideBound');
    result.complete=true;
  } catch(error) { result.error=code(error); }
  save(); return result;
}
