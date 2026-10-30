// Gate (a)'s resume and broadcast logic against a stubbed fetch: write-ahead resume, resume after a
// POST, an uncertain POST resolved by GET, transient errors with backoff, terminal verdicts that stop
// the step, and timelocked transactions POSTed only at their height (476 as the safety net).
import { describe, it, expect } from 'vitest';
import { LockingScript, Transaction, UnlockingScript } from '@bsv/sdk';
import { createBroadcaster } from '../src/broadcaster.js';
import { StepTerminal, advance, type StepCtx } from '../src/gate-a/steps.js';
import type { RunFile, StepRecord, StepState } from '../src/gate-a/store.js';

type Resp = { status: number; body?: unknown } | Error;

function stubFetch(routes: Record<string, Resp[]>) {
  const calls: Array<{ method: string; path: string; body?: string }> = [];
  const fetchFn = (async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    const method = init?.method ?? 'GET';
    calls.push({ method, path: url.pathname, body: typeof init?.body === 'string' ? init.body : undefined });
    const q = routes[`${method} ${url.pathname}`];
    const next = q === undefined ? { status: 404, body: {} } : q.length > 1 ? q.shift()! : q[0]!;
    if (next instanceof Error) throw next;
    return new Response(next.body === undefined ? '' : JSON.stringify(next.body), { status: next.status });
  }) as typeof fetch;
  return { fetchFn, calls };
}

const PACING = { heightPollMs: 60_000, backoffMs: 5_000, backoffMaxMs: 300_000 };
const SHORT = { seen: 10 * 60_000, mined: 60 * 60_000, height: 10 * 60_000, retry: 10 * 60_000 };

/** A run with a funding transaction and one recorded step spending it (signatures are not checked here). */
function fixture(o: { state: StepState; lockTime?: number; seen?: boolean; routes: Record<string, Resp[]> }) {
  const funding = new Transaction();
  funding.addInput({ sourceTXID: 'ee'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51'), sequence: 0xffffffff });
  funding.addOutput({ satoshis: 10_000, lockingScript: LockingScript.fromHex('51') });
  const tx = new Transaction();
  tx.lockTime = o.lockTime ?? 0;
  tx.addInput({ sourceTXID: funding.id('hex'), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51'), sequence: o.lockTime ? 0xfffffffe : 0xffffffff });
  tx.addOutput({ satoshis: 9_999, lockingScript: LockingScript.fromHex('51') });
  const txid = tx.id('hex');
  const rec = {
    what: 'test', txid, rawHex: tx.toHex(), lockTime: tx.lockTime, state: o.state, builtAt: '', flags: [],
    ...(o.seen ? { seenStatus: { txid, txStatus: 'SEEN_ON_NETWORK' } } : {}),
  } as unknown as StepRecord;
  const run: RunFile = {
    version: 1, dryRun: false, network: 'testnet', broadcaster: '', address: '', createdAt: '', updatedAt: '',
    funding: { txid: funding.id('hex'), rawHex: funding.toHex(), source: 'test' }, steps: { forfeit: rec },
  };
  const routes = Object.fromEntries(Object.entries(o.routes).map(([k, v]) => [k.replace('{txid}', txid), v]));
  const { fetchFn, calls } = stubFetch(routes);
  let t = 0;
  const sleeps: number[] = [];
  const states: string[] = [];
  const logs: string[] = [];
  const sleep = async (ms: number) => { sleeps.push(ms); t += ms; };
  const ctx: StepCtx = {
    run, save: () => { states.push(rec.state); }, sleep, now: () => t, log: (m) => logs.push(m),
    b: createBroadcaster({ baseUrl: 'https://broadcaster.test', fetch: fetchFn, now: () => t, sleep, pollMs: { seen: 1000, mined: 1000 } }),
    pacing: PACING, timeouts: SHORT,
  };
  const ef = (() => { const c = Transaction.fromHex(tx.toHex()); c.inputs[0]!.sourceTransaction = funding; return c.toHexEF(); })();
  return { ctx, rec, txid, ef, calls, sleeps, states, logs, posts: () => calls.filter((c) => c.method === 'POST') };
}

const st = (txStatus: string, extra: Record<string, unknown> = {}) => ({ status: 200, body: { txid: '{txid}', txStatus, ...extra } });
/** Builds the fixture, then rewrites '{txid}' in every queued body to the real txid. */
function setup(o: Parameters<typeof fixture>[0]) {
  const f = fixture(o);
  for (const q of Object.values(o.routes)) for (const r of q) {
    if (!(r instanceof Error) && r.body && typeof r.body === 'object' && (r.body as { txid?: string }).txid === '{txid}') (r.body as { txid: string }).txid = f.txid;
  }
  return f;
}

describe('gate (a) steps: resume', () => {
  it('after a write-ahead with no POST: POSTs the recorded bytes once, then waits for SEEN', async () => {
    const f = setup({ state: 'built', routes: { 'POST /tx': [{ status: 202, body: { txid: '{txid}', status: 202, txStatus: 'RECEIVED' } }], 'GET /tx/{txid}': [st('SEEN_ON_NETWORK')] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts()).toHaveLength(1);
    expect(f.posts()[0]!.body).toBe(f.ef);
    expect(f.rec.state).toBe('seen');
  });

  it('after a POST: reads GET first and does not POST again', async () => {
    const f = setup({ state: 'submitted', routes: { 'GET /tx/{txid}': [st('SEEN_ON_NETWORK')] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts()).toHaveLength(0);
    expect(f.rec.state).toBe('seen');
  });

  it('after a POST the broadcaster never received (404): POSTs the identical bytes', async () => {
    const f = setup({ state: 'submitted', routes: { 'GET /tx/{txid}': [{ status: 404 }, st('SEEN_ON_NETWORK')], 'POST /tx': [{ status: 202, body: { txid: '{txid}', txStatus: 'RECEIVED' } }] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts().map((p) => p.body)).toEqual([f.ef]);
  });

  it('transient GET failures (fetch error, 502, 503) retry with backoff and never POST', async () => {
    const f = setup({ state: 'seen', seen: true, routes: { 'GET /tx/{txid}': [new TypeError('fetch failed'), { status: 502 }, { status: 503 }, st('MINED', { blockHeight: 7 })] } });
    await advance(f.ctx, 'forfeit', 'mined');
    expect(f.posts()).toHaveLength(0);
    expect(f.sleeps.slice(0, 3)).toEqual([5_000, 10_000, 20_000]);
    expect(f.rec).toMatchObject({ state: 'mined', blockHeight: 7 });
  });
});

describe('gate (a) steps: an uncertain POST', () => {
  it('a POST whose fetch fails leaves the step "unknown" and is resolved by GET, not re-POSTed', async () => {
    const f = setup({ state: 'built', routes: { 'POST /tx': [new TypeError('socket hang up')], 'GET /tx/{txid}': [st('SEEN_ON_NETWORK')] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts()).toHaveLength(1);
    expect(f.states).toContain('unknown');
    expect(f.rec.state).toBe('seen');
    expect(f.rec.postUncertain?.reason).toMatch(/socket hang up/);
  });

  it('an "unknown" step found absent (404) is POSTed again, after a backoff', async () => {
    const f = setup({ state: 'built', routes: { 'POST /tx': [new TypeError('socket hang up'), { status: 202, body: { txid: '{txid}', txStatus: 'RECEIVED' } }], 'GET /tx/{txid}': [{ status: 404 }, st('SEEN_ON_NETWORK')] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts().map((p) => p.body)).toEqual([f.ef, f.ef]);
    expect(f.sleeps).toContain(5_000);
  });

  it('resuming a step left "unknown" asks GET before anything else', async () => {
    const f = setup({ state: 'unknown', routes: { 'GET /tx/{txid}': [st('RECEIVED'), st('SEEN_ON_NETWORK')] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts()).toHaveLength(0);
    expect(f.rec.state).toBe('seen');
  });

  it('UNREACHABLE is transient (G-11): resolved by GET, then POSTed again only on a 404', async () => {
    const f = setup({ state: 'built', routes: { 'POST /tx': [{ status: 400, body: { error: 'UNREACHABLE' } }, { status: 202, body: { txid: '{txid}', txStatus: 'RECEIVED' } }], 'GET /tx/{txid}': [{ status: 404 }, st('SEEN_ON_NETWORK')] } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts()).toHaveLength(2);
  });
});

describe('gate (a) steps: terminal verdicts stop the step, nothing is rebuilt', () => {
  it('a REJECTED "parent rejected" on resume', async () => {
    const f = setup({ state: 'submitted', routes: { 'GET /tx/{txid}': [st('REJECTED', { extraInfo: 'parent rejected: 1234', status: 461 })] } });
    await expect(advance(f.ctx, 'forfeit', 'seen')).rejects.toThrow(StepTerminal);
    await expect(advance(f.ctx, 'forfeit', 'seen')).rejects.toThrow(/parent rejected.*not rebuilt/);
    expect(f.posts()).toHaveLength(0);
  });

  it('an input-blaming 466 on a first POST that the broadcaster never recorded', async () => {
    const f = setup({ state: 'built', routes: { 'POST /tx': [{ status: 400, body: { error: 'transaction failed validation', reason: 'UTXO_SPENT', status: 466 } }], 'GET /tx/{txid}': [{ status: 404 }] } });
    await expect(advance(f.ctx, 'forfeit', 'seen')).rejects.toThrow(/466.*another spent its inputs/);
    expect(f.posts()).toHaveLength(1);
  });

  it('an input-blaming REJECTED for a transaction POSTed but never SEEN is a real conflict: it stops at once', async () => {
    // BSV Broadcaster can answer 2xx and mark the transaction REJECTED (466) about 20 ms later.
    const f = setup({ state: 'submitted', routes: { 'GET /tx/{txid}': [st('REJECTED', { extraInfo: 'UTXO_SPENT', status: 466 })] } });
    await expect(advance(f.ctx, 'forfeit', 'seen')).rejects.toThrow(/another transaction spent its inputs/);
    expect(f.sleeps).toEqual([]);
    expect(f.posts()).toHaveLength(0);
  });

  it('a validator rejection on POST (465) stops at once, without a GET', async () => {
    const f = setup({ state: 'built', routes: { 'POST /tx': [{ status: 400, body: { error: 'transaction failed validation', reason: 'fee too low', status: 465 } }] } });
    await expect(advance(f.ctx, 'forfeit', 'seen')).rejects.toThrow(/STOPPED.*465/);
    expect(f.calls.filter((c) => c.method === 'GET' && c.path.startsWith('/tx/'))).toHaveLength(0);
  });

  it('an input-blaming rejection of a transaction already SEEN is no verdict (G-11): it reads again', async () => {
    const f = setup({ state: 'seen', seen: true, routes: { 'GET /tx/{txid}': [st('REJECTED', { extraInfo: 'missingorspent', status: 466 }), st('MINED', { blockHeight: 9 })] } });
    await advance(f.ctx, 'forfeit', 'mined');
    expect(f.rec).toMatchObject({ state: 'mined', blockHeight: 9 });
    expect(f.posts()).toHaveLength(0);
  });

  it('...but a rejection that persists past the deadline stops the step, pointing at the explorer', async () => {
    const f = setup({ state: 'seen', seen: true, routes: { 'GET /tx/{txid}': [st('REJECTED', { extraInfo: 'UTXO_SPENT', status: 466 })] } });
    await expect(advance(f.ctx, 'forfeit', 'mined')).rejects.toThrow(/no verdict.*whatsonchain/);
    expect(f.posts()).toHaveLength(0);
  });
});

describe('gate (a) steps: timelocked transactions', () => {
  it('are not POSTed before the tip reaches nLockTime; the wait is paced at one poll a minute', async () => {
    const f = setup({ state: 'built', lockTime: 1005, routes: {
      'GET /health': [{ status: 200, body: { blockHeight: 1003 } }, { status: 200, body: { blockHeight: 1004 } }, { status: 200, body: { blockHeight: 1005 } }],
      'POST /tx': [{ status: 202, body: { txid: '{txid}', txStatus: 'RECEIVED' } }], 'GET /tx/{txid}': [st('SEEN_ON_NETWORK')],
    } });
    await advance(f.ctx, 'forfeit', 'seen');
    const order = f.calls.map((c) => `${c.method} ${c.path.startsWith('/tx/') ? '/tx/:id' : c.path}`);
    expect(order.slice(0, 4)).toEqual(['GET /health', 'GET /health', 'GET /health', 'POST /tx']);
    expect(f.sleeps.slice(0, 2)).toEqual([60_000, 60_000]);
    expect(f.rec).toMatchObject({ state: 'seen', tipAtSubmit: 1005 });
    expect(f.rec.notFinal).toBeUndefined();
  });

  it('a 476 at the height (the broadcaster lags) is marked not-final, paced, and retried with the same bytes', async () => {
    const f = setup({ state: 'built', lockTime: 1005, routes: {
      'GET /health': [{ status: 200, body: { blockHeight: 1005 } }],
      'POST /tx': [{ status: 400, body: { error: 'transaction failed validation', reason: 'non-final', status: 476 } }, { status: 202, body: { txid: '{txid}', txStatus: 'RECEIVED' } }],
      'GET /tx/{txid}': [st('SEEN_ON_NETWORK')],
    } });
    await advance(f.ctx, 'forfeit', 'seen');
    expect(f.posts().map((p) => p.body)).toEqual([f.ef, f.ef]);
    expect(f.rec.notFinal).toHaveLength(1);
    expect(f.rec.notFinal![0]).toMatchObject({ tip: 1005, message: /476/ });
    expect(f.sleeps).toContain(60_000);
    expect(f.rec.state).toBe('seen');
  });

  it('with no tip source answering, never POSTs: it times out asking for a re-run', async () => {
    const f = setup({ state: 'built', lockTime: 1005, routes: { 'GET /health': [{ status: 503 }] } });
    await expect(advance(f.ctx, 'forfeit', 'seen')).rejects.toThrow(/height 1005 not reached.*re-run/);
    expect(f.posts()).toHaveLength(0);
  });
});
