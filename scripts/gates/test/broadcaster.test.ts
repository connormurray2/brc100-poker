// The BSV Broadcaster client gate (a) drives, against a stubbed fetch (Review Focus 5): a 2xx POST
// is provisional, only a GET can say MINED, and 404 / UNKNOWN / failed requests mean "not yet".
import { describe, it, expect } from 'vitest';
import { LockingScript, Transaction, UnlockingScript } from '@bsv/sdk';
import { BroadcastError, createBroadcaster, feeFor, type Broadcaster } from '../src/broadcaster.js';

type Resp = { status: number; body?: unknown } | Error;
interface Call { method: string; path: string; body?: string; headers: Record<string, string>; signal?: AbortSignal }

/**
 * A fetch that answers from a queue per "METHOD /path". The last answer of each queue repeats, so a
 * poll loop can run as long as it likes; a route with no queue answers 404.
 */
function stubFetch(routes: Record<string, Resp[]>) {
  const calls: Call[] = [];
  const fetchFn = (async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    const method = init?.method ?? 'GET';
    const key = `${method} ${url.pathname}`;
    calls.push({
      method, path: url.pathname, body: typeof init?.body === 'string' ? init.body : undefined,
      headers: Object.fromEntries(new Headers(init?.headers).entries()), signal: init?.signal ?? undefined,
    });
    const queue = routes[key];
    const next = queue === undefined ? { status: 404, body: { error: 'not found' } } : queue.length > 1 ? queue.shift()! : queue[0]!;
    if (next instanceof Error) throw next;
    const text = next.body === undefined ? '' : typeof next.body === 'string' ? next.body : JSON.stringify(next.body);
    return new Response(text, { status: next.status, headers: { 'content-type': 'application/json' } });
  }) as typeof fetch;
  return { fetchFn, calls };
}

/** A fake clock: sleep advances it, so a timeout of minutes costs nothing. */
function clock() {
  let t = 0;
  return { now: () => t, sleep: async (ms: number) => { t += ms; } };
}

/** A small, valid EF transaction (every input carries its source transaction). */
function efTx(): { ef: string; txid: string } {
  const parent = new Transaction();
  parent.addOutput({ satoshis: 1000, lockingScript: LockingScript.fromHex('51') });
  const tx = new Transaction();
  tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
  tx.addOutput({ satoshis: 999, lockingScript: LockingScript.fromHex('51') });
  return { ef: tx.toHexEF(), txid: tx.id('hex') };
}

function make(routes: Record<string, Resp[]>): { b: Broadcaster; calls: Call[]; gets: (txid: string) => number } {
  const { fetchFn, calls } = stubFetch(routes);
  const c = clock();
  const b = createBroadcaster({ baseUrl: 'https://broadcaster.test', fetch: fetchFn, now: c.now, sleep: c.sleep, pollMs: { seen: 1000, mined: 1000 } });
  return { b, calls, gets: (txid) => calls.filter((x) => x.method === 'GET' && x.path === `/tx/${txid}`).length };
}

const status = (txid: string, txStatus: string, extra: Record<string, unknown> = {}) => ({ status: 200, body: { txid, txStatus, timestamp: '2026-10-30T00:00:00Z', ...extra } });

describe('broadcaster: waitFor (Review Focus 5)', () => {
  it('a POST answered 200 followed by REJECTED on the first GET makes waitFor reject with REJECTED', async () => {
    const { ef, txid } = efTx();
    const { b } = make({
      'POST /tx': [{ status: 200, body: { txid, status: 200, txStatus: 'RECEIVED' } }],
      [`GET /tx/${txid}`]: [status(txid, 'REJECTED', { extraInfo: 'script evaluation failed', status: 461 })],
    });
    await expect(b.submit(ef)).resolves.toEqual({ txid, status: 'RECEIVED' });
    await expect(b.waitFor(txid, 'SEEN_ON_NETWORK', 60_000)).rejects.toThrow(/REJECTED.*script evaluation failed/);
  });

  it('404 then SEEN_ON_NETWORK resolves', async () => {
    const { txid } = efTx();
    const { b, gets } = make({ [`GET /tx/${txid}`]: [{ status: 404, body: { error: 'not found' } }, status(txid, 'SEEN_ON_NETWORK')] });
    await expect(b.waitFor(txid, 'SEEN_ON_NETWORK', 60_000)).resolves.toBe('SEEN_ON_NETWORK');
    expect(gets(txid)).toBe(2);
  });

  it('never resolves MINED without a GET saying so, even when the POST echoes MINED', async () => {
    const { ef, txid } = efTx();
    const stuck = make({
      'POST /tx': [{ status: 202, body: { txid, status: 202, txStatus: 'MINED' } }], // an idempotent re-submit echo
      [`GET /tx/${txid}`]: [status(txid, 'SEEN_ON_NETWORK')],
    });
    await expect(stuck.b.submit(ef)).resolves.toEqual({ txid, status: 'MINED' });
    await expect(stuck.b.waitFor(txid, 'MINED', 10 * 60_000)).rejects.toThrow(/timed out.*MINED.*SEEN_ON_NETWORK/);
    expect(stuck.gets(txid)).toBeGreaterThan(5);

    const later = make({
      [`GET /tx/${txid}`]: [{ status: 404 }, status(txid, 'SEEN_ON_NETWORK'), status(txid, 'MINED', { blockHeight: 1_700_123, blockHash: 'ab'.repeat(32) })],
    });
    const st = await later.b.waitForStatus(txid, 'MINED', 10 * 60_000);
    expect(st).toMatchObject({ txStatus: 'MINED', blockHeight: 1_700_123 });
    expect(later.gets(txid)).toBe(3);
  });

  it('SEEN with a competing transaction does not count', async () => {
    const { txid } = efTx();
    const { b } = make({ [`GET /tx/${txid}`]: [status(txid, 'SEEN_ON_NETWORK', { competingTxs: ['cd'.repeat(32)] })] });
    await expect(b.waitFor(txid, 'SEEN_ON_NETWORK', 30_000)).rejects.toThrow(/timed out.*competing/);
  });

  it('DOUBLE_SPEND_ATTEMPTED rejects', async () => {
    const { txid } = efTx();
    const { b } = make({ [`GET /tx/${txid}`]: [status(txid, 'DOUBLE_SPEND_ATTEMPTED', { competingTxs: ['cd'.repeat(32)] })] });
    await expect(b.waitFor(txid, 'SEEN_ON_NETWORK', 30_000)).rejects.toThrow(/DOUBLE_SPEND_ATTEMPTED/);
  });

  it('UNKNOWN, a failed request and a 5xx all mean "not yet", and every request carries a timeout signal', async () => {
    const { txid } = efTx();
    const { b, calls } = make({
      [`GET /tx/${txid}`]: [status(txid, 'UNKNOWN'), new TypeError('fetch failed'), { status: 502, body: 'bad gateway' }, status(txid, 'SEEN_MULTIPLE_NODES')],
    });
    await expect(b.waitFor(txid, 'SEEN_ON_NETWORK', 60_000)).resolves.toBe('SEEN_MULTIPLE_NODES');
    expect(calls).toHaveLength(4);
    for (const c of calls) expect(c.signal).toBeInstanceOf(AbortSignal);
  });

  it('a status for a different txid is not taken as this one', async () => {
    const { txid } = efTx();
    const { b } = make({ [`GET /tx/${txid}`]: [status('ee'.repeat(32), 'MINED')] });
    await expect(b.waitFor(txid, 'MINED', 10_000)).rejects.toThrow(/timed out/);
  });
});

describe('broadcaster: submit', () => {
  it('posts the EF hex as text/plain to /tx, with a timeout, and checks the returned txid', async () => {
    const { ef, txid } = efTx();
    const ok = make({ 'POST /tx': [{ status: 202, body: { txid, status: 202, txStatus: 'RECEIVED' } }] });
    await ok.b.submit(ef);
    expect(ok.calls[0]).toMatchObject({ method: 'POST', path: '/tx', body: ef });
    expect(ok.calls[0]!.headers['content-type']).toBe('text/plain');
    expect(ok.calls[0]!.signal).toBeInstanceOf(AbortSignal);

    const wrong = make({ 'POST /tx': [{ status: 202, body: { txid: 'ee'.repeat(32), status: 202, txStatus: 'RECEIVED' } }] });
    await expect(wrong.b.submit(ef)).rejects.toThrow(/txid/);
  });

  it('refuses a plain (non-EF) transaction before sending it', async () => {
    const tx = Transaction.fromHexEF(efTx().ef);
    const { b, calls } = make({});
    await expect(b.submit(tx.toHex())).rejects.toThrow(/Extended Format/);
    expect(calls).toHaveLength(0);
  });

  it('classifies 476 and 503 as retryable, and a validator failure as terminal', async () => {
    const { ef } = efTx();
    const cases: Array<[Resp, { notFinal: boolean; retryable: boolean; message: RegExp }]> = [
      [{ status: 400, body: { error: 'transaction failed validation', reason: 'non-final', status: 476 } }, { notFinal: true, retryable: true, message: /476/ }],
      [{ status: 503, body: 'busy' }, { notFinal: false, retryable: true, message: /503/ }],
      [{ status: 400, body: { error: 'transaction failed validation', reason: 'fee too low', status: 465 } }, { notFinal: false, retryable: false, message: /465.*fee too low/ }],
    ];
    for (const [resp, want] of cases) {
      const { b } = make({ 'POST /tx': [resp] });
      const err = await b.submit(ef).then(() => undefined, (e: unknown) => e);
      expect(err).toBeInstanceOf(BroadcastError);
      expect(err).toMatchObject({ notFinal: want.notFinal, retryable: want.retryable });
      expect(String((err as Error).message)).toMatch(want.message);
    }
  });
});

describe('broadcaster: policy, fees and height', () => {
  // Read from https://arcade-v2-testnet-us-1.bsvblockchain.tech/policy on 2026-10-30.
  const OBSERVED = {
    policy: { miningFee: { satoshis: 1, bytes: 1000 }, maxtxsizepolicy: 100000000, maxscriptsizepolicy: 100000000, maxtxsigopscountspolicy: 4294967295, standardFormatSupported: true },
    timestamp: '2026-10-30T12:06:36.626896277Z',
  };

  it('readPolicy reads the observed testnet shape and keeps the raw body', async () => {
    const { b, calls } = make({ 'GET /policy': [{ status: 200, body: OBSERVED }] });
    const p = await b.readPolicy();
    expect(p).toMatchObject({ satPerKb: 1, maxTxSize: 100_000_000, maxScriptSize: 100_000_000, miningFee: { satoshis: 1, bytes: 1000 } });
    expect(p.raw).toEqual(OBSERVED);
    expect(calls[0]!.signal).toBeInstanceOf(AbortSignal);
  });

  it('readPolicy refuses a body without the fields it names', async () => {
    const { b } = make({ 'GET /policy': [{ status: 200, body: { policy: { fee: 1 } } }] });
    await expect(b.readPolicy()).rejects.toThrow(/miningFee/);
  });

  it('feeFor prices the serialized size at the policy rate, with a floor of 1 sat', () => {
    const rate = { satoshis: 1, bytes: 1000 };
    expect(feeFor(0, rate)).toBe(1);
    expect(feeFor(999, rate)).toBe(1);
    expect(feeFor(1001, rate)).toBe(2);
    expect(feeFor(1_814_363, rate)).toBe(1815);
    expect(feeFor(250, { satoshis: 100, bytes: 1000 })).toBe(25);
  });

  it('tipHeight reads /health blockHeight, and is undefined when it is missing or the call fails', async () => {
    expect(await make({ 'GET /health': [{ status: 200, body: { healthy: true, blockHeight: 1_700_000 } }] }).b.tipHeight()).toBe(1_700_000);
    expect(await make({ 'GET /health': [{ status: 200, body: { healthy: true } }] }).b.tipHeight()).toBeUndefined();
    expect(await make({ 'GET /health': [new TypeError('fetch failed')] }).b.tipHeight()).toBeUndefined();
  });
});
