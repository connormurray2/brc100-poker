// Gate (a)'s step machinery: takes a recorded (write-ahead) transaction to SEEN or MINED, resuming
// from whatever state the run file holds. Broadcaster answers are classified per G-11:
//   - only a 404 means "never received", and only then are the same bytes POSTed (again);
//   - a failed or uncertain POST leaves the step `unknown` and is resolved by GET, never re-POSTed blindly;
//   - network errors, timeouts and 5xx are transient and retried with backoff;
//   - REJECTED (including "parent rejected"), DOUBLE_SPEND_ATTEMPTED and an input-blaming rejection of a
//     transaction never accepted stop the step with a message. Nothing is ever rebuilt or re-spent.
// Timelocked transactions are POSTed only once the chain tip has reached their nLockTime; ARC 476 is
// kept as a safety net (the broadcaster's own tip can lag).
import { Transaction } from '@bsv/sdk';
import {
  TxRejectedError, classifyPostFailure, classifyStatusRead, type Broadcaster,
} from '../broadcaster.js';
import { STEP_IDS, knownTxs, nowIso, type RunFile, type StepId, type StepRecord } from './store.js';
import { efHexOf } from './txs.js';

export const TIMEOUTS = { seen: 15 * 60_000, mined: 6 * 3600_000, height: 12 * 3600_000, retry: 30 * 60_000 };
export const PACING = { heightPollMs: 60_000, backoffMs: 5_000, backoffMaxMs: 5 * 60_000 };

export interface TipSource { name: string; read: () => Promise<number | undefined> }

export interface StepCtx {
  run: RunFile;
  save: () => void;
  b: Broadcaster;
  sleep: (ms: number) => Promise<void>;
  now: () => number;
  log: (msg: string) => void;
  /** Chain-tip sources besides the broadcaster's `GET /health` (the funded run adds WhatsOnChain `/chain/info`). */
  tipSources?: TipSource[];
  stopAfter?: StepId;
  timeouts?: typeof TIMEOUTS;
  pacing?: typeof PACING;
}

/** --stop-after reached. */
export class Stop extends Error {}

/** The step cannot go on without a human: never rebuilt, never re-spent. */
export class StepTerminal extends Error {
  constructor(readonly step: StepId, readonly reason: string) {
    super(`${step}: STOPPED — ${reason}. The transaction is not rebuilt or re-spent; inspect it, then re-run.`);
    this.name = 'StepTerminal';
  }
}

const T = (ctx: StepCtx) => ctx.timeouts ?? TIMEOUTS;
const P = (ctx: StepCtx) => ctx.pacing ?? PACING;

export const isTimelocked = (tx: Transaction) =>
  tx.lockTime > 0 && tx.lockTime < 500_000_000 && tx.inputs.some((i) => (i.sequence ?? 0xffffffff) !== 0xffffffff);

/** A transaction the network once reported SEEN: an input-blaming rejection of it is no verdict (G-11). */
const wasAccepted = (rec: StepRecord) => rec.seenStatus !== undefined;

/**
 * The highest tip any source reports: the broadcaster's `GET /health` blockHeight, any extra source,
 * and, as a floor, the highest block holding one of this run's transactions.
 */
export async function tipNow(ctx: StepCtx): Promise<{ tip?: number; source: string }> {
  const reads: Array<[string, number | undefined]> = [['/health blockHeight', await ctx.b.tipHeight()]];
  for (const s of ctx.tipSources ?? []) reads.push([s.name, await s.read().catch(() => undefined)]);
  const mined = Math.max(-1, ...STEP_IDS.map((id) => ctx.run.steps[id]?.blockHeight ?? -1));
  if (mined >= 0) reads.push(['highest block holding a run transaction', mined]);
  const known = reads.filter((r): r is [string, number] => r[1] !== undefined);
  if (known.length === 0) return { source: 'unknown (no source answered)' };
  const tip = Math.max(...known.map((r) => r[1]));
  return { tip, source: known.filter((r) => r[1] === tip).map((r) => r[0]).join(', ') };
}

/** Blocks until the tip reaches `height`. Never POSTs. */
async function waitForHeight(ctx: StepCtx, id: StepId, height: number) {
  const deadline = ctx.now() + T(ctx).height;
  for (;;) {
    const { tip, source } = await tipNow(ctx);
    if (tip !== undefined && tip >= height) return { tip, source };
    if (ctx.now() + P(ctx).heightPollMs > deadline) throw new Error(`${id}: height ${height} not reached (tip ${tip ?? '?'}); re-run to resume`);
    ctx.log(`${id}: waiting for height ${height} before POSTing (tip ${tip ?? '?'}, ${source})`);
    await ctx.sleep(P(ctx).heightPollMs);
  }
}

/**
 * Reads GET /tx/{txid} until it says something definite. 'absent' is a 404 (never received);
 * 'found' is any status this instance holds that does not end the step.
 */
export async function resolveByGet(ctx: StepCtx, id: StepId, rec: StepRecord): Promise<'absent' | 'found'> {
  const deadline = ctx.now() + T(ctx).retry;
  let delay = P(ctx).backoffMs;
  for (;;) {
    const v = classifyStatusRead(rec.txid, await ctx.b.readStatus(rec.txid));
    let note: string;
    switch (v.kind) {
      case 'absent': return 'absent';
      case 'pending': case 'seen': case 'mined': return 'found';
      case 'terminal': throw new StepTerminal(id, v.reason);
      case 'input-blamed':
        if (!wasAccepted(rec)) throw new StepTerminal(id, `${v.reason} — another transaction spent its inputs`);
        note = `${v.reason} on a transaction the network had accepted: no verdict (G-11), reading again`;
        break;
      case 'transient': note = v.reason; break;
    }
    if (ctx.now() + delay > deadline) {
      if (v.kind === 'input-blamed') throw new StepTerminal(id, `${note}; it is probably mined or spent by itself — check https://test.whatsonchain.com/tx/${rec.txid}`);
      throw new Error(`${id}: no definite answer from GET /tx/${rec.txid} (${note}); re-run to resume`);
    }
    ctx.log(`${id}: GET /tx: ${note}; retrying in ${delay / 1000} s`);
    await ctx.sleep(delay);
    delay = Math.min(delay * 2, P(ctx).backoffMaxMs);
  }
}

export async function submitStep(ctx: StepCtx, id: StepId, rec: StepRecord) {
  const tx = Transaction.fromHex(rec.rawHex);
  const ef = efHexOf(tx, knownTxs(ctx.run));
  const timelocked = isTimelocked(tx);
  const deadline = ctx.now() + (timelocked ? T(ctx).height : T(ctx).retry);
  let delay = P(ctx).backoffMs;
  for (;;) {
    const { tip, source } = timelocked ? await waitForHeight(ctx, id, rec.lockTime) : await tipNow(ctx);
    try {
      ctx.log(`${id}: POST /tx ${rec.txid} (${ef.length / 2} EF bytes)${timelocked ? `, nLockTime ${rec.lockTime}, tip ${tip} (${source})` : ''}`);
      const resp = await ctx.b.submit(ef);
      Object.assign(rec, { state: 'submitted', submitResponse: resp, submittedAt: nowIso(), tipAtSubmit: tip, tipSource: source });
      ctx.save();
      ctx.log(`${id}: POST answered ${resp.status} (provisional; waiting for GET /tx)`);
      return;
    } catch (e) {
      const v = classifyPostFailure(e);
      if (v.kind === 'terminal') throw new StepTerminal(id, v.reason);
      if (v.kind === 'not-final') {
        // Safety net: our tip said final, the broadcaster's view disagrees. Nothing was persisted.
        (rec.notFinal ??= []).push({ at: nowIso(), tip, message: v.reason });
        ctx.save();
        if (ctx.now() + P(ctx).heightPollMs > deadline) throw new Error(`${id}: still not final (${v.reason}); re-run to resume`);
        ctx.log(`${id}: not final for the broadcaster yet (ARC 476); retrying in ${P(ctx).heightPollMs / 1000} s`);
        await ctx.sleep(P(ctx).heightPollMs);
        continue;
      }
      // unknown or input-blamed: the bytes may have arrived. Record that, then ask, never re-POST blindly.
      Object.assign(rec, { state: 'unknown', postUncertain: { at: nowIso(), reason: v.reason }, tipAtSubmit: tip, tipSource: source });
      ctx.save();
      ctx.log(`${id}: POST outcome unknown (${v.reason}); resolving by GET`);
      if ((await resolveByGet(ctx, id, rec)) === 'found') {
        Object.assign(rec, { state: 'submitted', submittedAt: nowIso() });
        ctx.save();
        return;
      }
      if (v.kind === 'input-blamed') throw new StepTerminal(id, `${v.reason}; this broadcaster never received the transaction, so another spent its inputs`);
      if (ctx.now() + delay > deadline) throw new Error(`${id}: not accepted (${v.reason}); re-run to resume`);
      ctx.log(`${id}: never received (404); POSTing the identical bytes again in ${delay / 1000} s`);
      await ctx.sleep(delay);
      delay = Math.min(delay * 2, P(ctx).backoffMaxMs);
    }
  }
}

/** Waits for `until`, turning a terminal status into StepTerminal. */
async function waitClassified(ctx: StepCtx, id: StepId, rec: StepRecord, until: 'SEEN_ON_NETWORK' | 'MINED', timeoutMs: number) {
  try {
    return await ctx.b.waitForStatus(rec.txid, until, timeoutMs);
  } catch (e) {
    if (!(e instanceof TxRejectedError)) throw e;
    const v = classifyStatusRead(rec.txid, { httpStatus: 200, body: e.status });
    if (v.kind === 'input-blamed' && wasAccepted(rec)) {
      throw new StepTerminal(id, `${v.reason} after the network had accepted it: no verdict (G-11); it is probably mined — check https://test.whatsonchain.com/tx/${rec.txid}`);
    }
    throw new StepTerminal(id, 'reason' in v ? v.reason : e.message);
  }
}

/** Takes a recorded step to SEEN (no competing transaction) or MINED, resuming from its recorded state. */
export async function advance(ctx: StepCtx, id: StepId, until: 'seen' | 'mined') {
  const rec = ctx.run.steps[id];
  if (!rec) throw new Error(`step ${id} has no transaction yet`);
  if (rec.state === 'mined' || (until === 'seen' && rec.state === 'seen')) return;
  if (rec.state !== 'built') {
    // Resuming after a POST (or an uncertain one). Only a 404 sends the same bytes again.
    if ((await resolveByGet(ctx, id, rec)) === 'absent') {
      ctx.log(`${id}: the broadcaster has no record (404); POSTing the identical bytes`);
      rec.state = 'built';
    } else if (rec.state === 'unknown') {
      Object.assign(rec, { state: 'submitted', submittedAt: rec.submittedAt ?? nowIso() });
    }
    ctx.save();
  }
  if (rec.state === 'built') await submitStep(ctx, id, rec);
  if (rec.state === 'submitted') {
    const st = await waitClassified(ctx, id, rec, 'SEEN_ON_NETWORK', T(ctx).seen);
    Object.assign(rec, { state: 'seen', seenAt: nowIso(), seenStatus: st });
    ctx.save();
    ctx.log(`${id}: ${st.txStatus}, no competing transaction`);
  }
  if (until === 'mined' && rec.state === 'seen') {
    const st = await waitClassified(ctx, id, rec, 'MINED', T(ctx).mined);
    const { merklePath: _m, ...kept } = st;
    Object.assign(rec, {
      state: 'mined', minedAt: nowIso(), minedStatus: kept, blockHeight: st.blockHeight, blockHash: st.blockHash,
      blocksToMine: st.blockHeight !== undefined && rec.tipAtSubmit !== undefined ? st.blockHeight - rec.tipAtSubmit : undefined,
    });
    ctx.save();
    ctx.log(`${id}: MINED in block ${st.blockHeight} (${rec.blocksToMine ?? '?'} block(s) after submit)`);
  }
  if (ctx.stopAfter === id) throw new Stop(`stopped after ${id} (--stop-after); re-run to resume`);
}
