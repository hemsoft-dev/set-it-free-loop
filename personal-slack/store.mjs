import { createHash, randomUUID } from 'node:crypto';
import { closeSync, existsSync, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export const hash = value => createHash('sha256').update(value).digest('hex');
export const requireThat = (condition, code) => { if (!condition) throw Object.assign(new Error(code), { code }); };
export function atomic(path, value) {
  const temporary = `${path}.${randomUUID()}.tmp`;
  const fd = openSync(temporary, 'wx', 0o600);
  try { writeFileSync(fd, value); fsyncSync(fd); } finally { closeSync(fd); }
  try { renameSync(temporary, path); } catch (error) { unlinkSync(temporary); throw error; }
}

// The receiver is the sole state writer. Owner helpers only create immutable inbox files.
export class Store {
  constructor(root) {
    this.root = root;
    mkdirSync(root, { recursive: true, mode: 0o700 });
    requireThat(!lstatSync(root).isSymbolicLink(), 'UnsafeStateDirectory');
    for (const name of ['inbox', 'files']) mkdirSync(join(root, name), { mode: 0o700, recursive: true });
    this.path = join(root, 'state.json');
    if (existsSync(this.path)) {
      const envelope = JSON.parse(readFileSync(this.path, 'utf8'));
      requireThat(envelope.hash === hash(JSON.stringify(envelope.data)), 'StateChecksumMismatch');
      this.state = envelope.data;
      requireThat(this.state.schema === 1 && this.state.requests && this.state.memory && this.state.operations, 'InvalidState');
    } else {
      this.state = { schema: 1, requests: {}, memory: {}, operations: {}, cursors: {}, incidents: {} };
      this.save();
    }
  }
  save() {
    requireThat(!this.failed, 'StateWritePreviouslyFailed');
    try { atomic(this.path, JSON.stringify({ hash: hash(JSON.stringify(this.state)), data: this.state }) + '\n'); }
    catch (error) { this.failed = true; throw error; } // No ACK or effects after a failed durable save.
  }
  lock() {
    const path = join(this.root, 'receiver.lock');
    // Never steal an existing lock. A crashed PID is reconciled by the operator.
    const fd = openSync(path, 'wx', 0o600);
    writeFileSync(fd, JSON.stringify({ pid: process.pid, started: new Date().toISOString() })); fsyncSync(fd); closeSync(fd);
    return () => unlinkSync(path);
  }
}

export function enqueue(root, operation) {
  const bytes = JSON.stringify(operation);
  requireThat(bytes.length <= 250000, 'OperationTooLarge');
  const id = hash(`${operation.source}\n${operation.operation}`);
  const path = join(root, 'inbox', `${id}.json`);
  // Same logical operation can only ever have one body. Temp + hard-link publishes
  // without overwriting a prior intent, even when two helpers race.
  return { id, path, bytes };
}
