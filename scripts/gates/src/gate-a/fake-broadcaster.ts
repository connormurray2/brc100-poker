// The dry run's network: an in-process BSV Broadcaster behind a fake `fetch`, so the harness drives
// exactly the code path of a funded run (EF hex over POST /tx, GET /tx/{txid}, GET /health) while
// Rúnar's MockProvider replays every input of every transaction through @bsv/sdk's Spend.
//
// On top of MockProvider it enforces what a broadcaster and node would, and MockProvider does not:
//   - the EF's claimed source outputs must match outputs this chain actually holds;
//   - no outpoint is spent twice;
//   - the fee covers the EF size at the /policy rate;
//   - size limits from /policy;
//   - nLockTime finality (ARC 476, nothing persisted), against a simulated tip.
// Blocks are mined only when the harness sleeps (each sleep is one block), so "mined" is only ever
// learned from a GET, as on testnet.
import { Transaction } from '@bsv/sdk';
import { MockProvider } from 'runar-sdk';
import { feeFor, type FeeRate } from '../broadcaster.js';

interface Held { rawHex: string; txStatus: 'SEEN_ON_NETWORK' | 'MINED' | 'REJECTED'; blockHeight?: number; blockHash?: string; extraInfo?: string; status?: number; timestamp: string }

export class FakeBroadcaster {
  readonly provider: MockProvider;
  private readonly held = new Map<string, Held>();
  private readonly outs = new Map<string, { satoshis: number; script: string }>();
  private readonly spent = new Set<string>();
  private mempool: string[] = [];
  readonly log: string[] = [];

  constructor(public tip: number, private readonly policyBody: unknown, private readonly rate: FeeRate, private readonly limits: { maxTxSize: number; maxScriptSize: number }) {
    this.provider = new MockProvider('testnet');
    this.provider.setFeeRate((rate.satoshis * 1000) / rate.bytes);
  }

  /** Coins that exist before the run (the funding transaction's outputs). */
  seed(tx: Transaction): void {
    const txid = tx.id('hex');
    tx.outputs.forEach((o, vout) => {
      const u = { satoshis: o.satoshis!, script: o.lockingScript.toHex() };
      this.outs.set(`${txid}:${vout}`, u);
      this.provider.addUtxo(`seed:${txid}`, { txid, outputIndex: vout, ...u });
    });
  }

  /** Replays a transaction a previous dry run already had accepted (resuming a dry run). */
  restore(rawHex: string, mined: { blockHeight: number } | undefined): void {
    const tx = Transaction.fromHex(rawHex);
    const txid = tx.id('hex');
    for (const i of tx.inputs) this.spent.add(`${i.sourceTXID}:${i.sourceOutputIndex}`);
    this.provider.addTransaction({
      txid, version: tx.version, locktime: tx.lockTime, raw: rawHex, inputs: [],
      outputs: tx.outputs.map((o) => ({ satoshis: o.satoshis!, script: o.lockingScript.toHex() })),
    });
    tx.outputs.forEach((o, vout) => this.outs.set(`${txid}:${vout}`, { satoshis: o.satoshis!, script: o.lockingScript.toHex() }));
    this.held.set(txid, mined
      ? { rawHex, txStatus: 'MINED', blockHeight: mined.blockHeight, blockHash: fakeHash(mined.blockHeight), timestamp: new Date().toISOString() }
      : { rawHex, txStatus: 'SEEN_ON_NETWORK', timestamp: new Date().toISOString() });
    if (!mined) this.mempool.push(txid);
  }

  /** One block: everything in the mempool is mined at tip + 1. */
  mine(): void {
    this.tip++;
    for (const txid of this.mempool) Object.assign(this.held.get(txid)!, { txStatus: 'MINED', blockHeight: this.tip, blockHash: fakeHash(this.tip) });
    if (this.mempool.length) this.log.push(`block ${this.tip}: mined ${this.mempool.length} tx`);
    this.mempool = [];
  }

  readonly fetch = (async (input: string | URL, init?: RequestInit): Promise<Response> => {
    const url = new URL(String(input));
    const method = init?.method ?? 'GET';
    if (method === 'POST' && url.pathname === '/tx') return this.post(String(init?.body ?? ''));
    if (method === 'GET' && url.pathname.startsWith('/tx/')) {
      const txid = url.pathname.slice(4);
      const h = this.held.get(txid);
      if (!h) return json(404, { error: 'not found' });
      const { rawHex: _r, ...st } = h;
      return json(200, { txid, ...st });
    }
    if (method === 'GET' && url.pathname === '/health') return json(200, { healthy: true, version: 'dry-run', blockHeight: this.tip });
    if (method === 'GET' && url.pathname === '/policy') return json(200, this.policyBody);
    return json(404, { error: 'not found' });
  }) as typeof fetch;

  private async post(efHex: string): Promise<Response> {
    let tx: Transaction;
    try { tx = Transaction.fromHexEF(efHex.trim()); } catch (e) { return json(400, { error: 'decode failed', reason: (e as Error).message, status: 460 }); }
    const txid = tx.id('hex');
    const known = this.held.get(txid);
    if (known && known.txStatus !== 'REJECTED') return json(202, { txid, status: 202, txStatus: known.txStatus });
    const reject = (status: number, reason: string) => {
      this.held.set(txid, { rawHex: tx.toHex(), txStatus: 'REJECTED', status, extraInfo: reason, timestamp: new Date().toISOString() });
      this.log.push(`REJECTED ${txid} (ARC ${status}): ${reason}`);
      return json(400, { error: 'transaction failed validation', reason, txid, status });
    };

    // Finality first, and persist nothing (ARC 476 is a condition of the chain view, not a verdict).
    const nonFinalInput = tx.inputs.some((i) => (i.sequence ?? 0xffffffff) !== 0xffffffff);
    if (tx.lockTime !== 0 && nonFinalInput && tx.lockTime < 500_000_000 && tx.lockTime > this.tip) {
      return json(400, { error: 'transaction failed validation', reason: `non-final: nLockTime ${tx.lockTime} > tip ${this.tip}`, txid, status: 476 });
    }
    const size = tx.toBinary().length;
    if (size > this.limits.maxTxSize) return reject(461, `tx ${size} B over maxtxsizepolicy`);
    for (const s of [...tx.inputs.map((i) => i.unlockingScript!), ...tx.outputs.map((o) => o.lockingScript)]) {
      if (s.toBinary().length > this.limits.maxScriptSize) return reject(461, `script ${s.toBinary().length} B over maxscriptsizepolicy`);
    }
    let inSats = 0;
    for (const i of tx.inputs) {
      const op = `${i.sourceTXID}:${i.sourceOutputIndex}`;
      const have = this.outs.get(op);
      const claimed = i.sourceTransaction?.outputs[i.sourceOutputIndex];
      if (!have) return reject(461, `input spends unknown outpoint ${op}`);
      if (!claimed || claimed.satoshis !== have.satoshis || claimed.lockingScript.toHex() !== have.script) return reject(461, `EF source output for ${op} does not match the chain`);
      if (this.spent.has(op)) return reject(466, `UTXO_SPENT ${op}`);
      inSats += have.satoshis;
    }
    const fee = inSats - tx.outputs.reduce((s, o) => s + o.satoshis!, 0);
    const need = feeFor(efHex.trim().length / 2, this.rate);
    if (fee < need) return reject(465, `fee ${fee} < ${need} for ${efHex.trim().length / 2} EF bytes`);
    try {
      await this.provider.broadcast(tx); // every input through @bsv/sdk Spend
    } catch (e) {
      return reject(461, (e as Error).message);
    }
    for (const i of tx.inputs) this.spent.add(`${i.sourceTXID}:${i.sourceOutputIndex}`);
    tx.outputs.forEach((o, vout) => this.outs.set(`${txid}:${vout}`, { satoshis: o.satoshis!, script: o.lockingScript.toHex() }));
    this.held.set(txid, { rawHex: tx.toHex(), txStatus: 'SEEN_ON_NETWORK', timestamp: new Date().toISOString() });
    this.mempool.push(txid);
    this.log.push(`accepted ${txid}: ${size} B, ${tx.inputs.length} inputs all through Spend`);
    return json(202, { txid, status: 202, txStatus: 'RECEIVED' });
  }

  stats() { return this.provider.getValidationStats(); }
}

const fakeHash = (h: number) => h.toString(16).padStart(64, '0');
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
