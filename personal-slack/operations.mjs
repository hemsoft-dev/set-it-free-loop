import { requireThat } from './store.mjs';

const BODY_FIELDS = ['operation', 'kind', 'text', 'readFiles'];
// Shared by the CLI and receiver. Owner input never includes transport intent,
// delivery state, ordering, hashes, timestamps or other daemon-owned fields.
export function operationBody(value) {
  requireThat(value && typeof value === 'object' && !Array.isArray(value), 'InvalidOperationSchema');
  const fields = value.kind === 'memory' ? [...BODY_FIELDS, 'memoryKey'] : BODY_FIELDS;
  requireThat(Object.keys(value).length === fields.length && fields.every(k => Object.hasOwn(value, k)), 'InvalidOperationSchema');
  requireThat(/^[a-zA-Z0-9_-]{1,80}$/.test(value.operation ?? '') && ['question','success','failure','memory'].includes(value.kind), 'InvalidOperation');
  requireThat(typeof value.text === 'string' && value.text.length > 0 && value.text.length <= 35000 &&
    !/xox[baprs]-[a-zA-Z0-9-]+|xapp-[a-zA-Z0-9-]+/.test(value.text), 'InvalidReply');
  requireThat(Array.isArray(value.readFiles) && value.readFiles.every(id => typeof id === 'string' && /^F[A-Z0-9]+$/.test(id)), 'InvalidReadEvidence');
  const body = { operation:value.operation, kind:value.kind, text:value.text, readFiles:[...value.readFiles] };
  if(value.kind === 'memory') {
    requireThat(/^[a-zA-Z0-9_-]{1,80}$/.test(value.memoryKey ?? '') &&
      !['__proto__','constructor','prototype'].includes(value.memoryKey) && value.text.length <= 4000, 'InvalidMemory');
    body.memoryKey = value.memoryKey;
  }
  return body;
}

// Select only signed body fields from an internal record, then apply the same
// validation again immediately before any effect or receipt completion.
export function storedBody(op) {
  return operationBody(Object.fromEntries([...BODY_FIELDS,'memoryKey'].filter(k=>Object.hasOwn(op,k)).map(k=>[k,op[k]])));
}
