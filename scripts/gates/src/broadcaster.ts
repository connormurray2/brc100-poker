// A small BSV Broadcaster (formerly Arcade) client: POST /tx as EF, GET /tx/{txid}, GET /policy and
// GET /health. Shapes follow the toolkit's bsv-broadcaster facts (verified 2026-10-22) and the
// repository's openapi/bsv-broadcaster.openapi.yaml; GET /policy was also read live on 2026-10-30.
//
// Rules this client enforces (G-09, G-10, G-15; Review Focus 5):
//   - A 2xx POST is provisional. Its txStatus is reported, never trusted: only waitFor's GETs decide.
//   - 404, UNKNOWN, a failed or timed-out request and a 5xx all mean "not yet".
//   - REJECTED and DOUBLE_SPEND_ATTEMPTED are terminal and reject.
//   - SEEN_ON_NETWORK (or later) counts only with no competing transaction.
//   - MINED is reported only from a GET whose txStatus says so.
//   - Every request has its own timeout, and every poll loop has a deadline.
import { Transaction } from '@bsv/sdk';

export const TESTNET_BROADCASTER = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech';

/** `GET /tx/{txid}` body (TransactionStatus). Only `txid`, `txStatus` and `timestamp` are always present. */
export interface TxStatus {
  txid: string;
  txStatus: string;
  timestamp?: string;
  /** ARC code recorded with a REJECTED verdict, when classifiable. */
  status?: number;
  extraInfo?: string;
  competingTxs?: string[];
  blockHash?: string;
  blockHeight?: number;
  [k: string]: unknown;
}

export interface Policy {
  satPerKb: number;
  maxTxSize: number;
  maxScriptSize: number;
  miningFee: FeeRate;
  /** The body exactly as returned, in case the documented field names drift. */
  raw: unknown;
}

export interface FeeRate { satoshis: number; bytes: number }

export type Until = 'SEEN_ON_NETWORK' | 'MINED';

/** Statuses at or past the gate: final (0-conf) when no competing transaction is listed. */
export const SEEN_OR_LATER = ['SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'MINED', 'IMMUTABLE'];
export const MINED_OR_LATER = ['MINED', 'IMMUTABLE'];
export const TERMINAL_FAILURES = ['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'];

/** A POST that was not accepted. `notFinal` is ARC 476: resubmit the identical bytes later. */
export class BroadcastError extends Error {
  constructor(
    message: string,
    readonly httpStatus: number,
    readonly arcStatus: number | undefined,
    readonly body: unknown,
    readonly retryable: boolean,
    readonly notFinal: boolean,
  ) {
    super(message);
    this.name = 'BroadcastError';
  }
}

/** A GET that returned a terminal failure. */
export class TxRejectedError extends Error {
  constructor(readonly status: TxStatus) {
    super(`${status.txid} ${status.txStatus}` +
      (status.status !== undefined ? ` (ARC ${status.status})` : '') +
      (status.extraInfo ? `: ${status.extraInfo}` : '') +
      (status.competingTxs?.length ? `; competing: ${status.competingTxs.join(', ')}` : ''));
    this.name = 'TxRejectedError';
  }
}

export interface BroadcasterOptions {
  baseUrl?: string;
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
  /** Per-request timeout for GETs. */
  requestTimeoutMs?: number;
  /** Per-request timeout for POST /tx (a 1.8 MB transaction is ~3.7 MB of EF hex). */
  postTimeoutMs?: number;
  pollMs?: { seen: number; mined: number };
  /** Called after every poll, for progress output. */
  onPoll?: (txid: string, status: TxStatus | undefined, note: string) => void;
}

export interface Broadcaster {
  readonly baseUrl: string;
  submit(efHex: string): Promise<{ txid: string; status: string }>;
  /** One GET: the status, or undefined on 404. Throws on any other failure. */
  getStatus(txid: string): Promise<TxStatus | undefined>;
  waitForStatus(txid: string, until: Until, timeoutMs: number): Promise<TxStatus>;
  waitFor(txid: string, until: Until, timeoutMs: number): Promise<string>;
  readPolicy(): Promise<Policy>;
  /** The instance's processed tip (`GET /health` blockHeight), or undefined if unavailable. */
  tipHeight(): Promise<number | undefined>;
}

/** Fee for `bytes` of serialized transaction at `rate`, rounded up, never below 1 sat. */
export function feeFor(bytes: number, rate: FeeRate): number {
  return Math.max(1, Math.ceil((bytes * rate.satoshis) / rate.bytes));
}

/** Whether a status satisfies `until`. SEEN-level needs an empty competingTxs. */
export function reached(st: TxStatus, until: Until): boolean {
  if (until === 'MINED') return MINED_OR_LATER.includes(st.txStatus);
  return SEEN_OR_LATER.includes(st.txStatus) && (st.competingTxs ?? []).length === 0;
}

async function readBody(res: Response): Promise<unknown> {
  const text = await res.text();
  try { return JSON.parse(text); } catch { return text; }
}

const describe = (body: unknown) => (typeof body === 'string' ? body : JSON.stringify(body)).slice(0, 500);

export function createBroadcaster(opts: BroadcasterOptions = {}): Broadcaster {
  const baseUrl = (opts.baseUrl ?? TESTNET_BROADCASTER).replace(/\/+$/, '');
  const doFetch = opts.fetch ?? fetch;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const now = opts.now ?? Date.now;
  const requestTimeoutMs = opts.requestTimeoutMs ?? 15_000;
  const postTimeoutMs = opts.postTimeoutMs ?? 180_000;
  const pollMs = opts.pollMs ?? { seen: 5_000, mined: 30_000 };

  async function getJson(path: string): Promise<{ status: number; body: unknown }> {
    const res = await doFetch(`${baseUrl}${path}`, { signal: AbortSignal.timeout(requestTimeoutMs) });
    return { status: res.status, body: await readBody(res) };
  }

  async function submit(efHex: string): Promise<{ txid: string; status: string }> {
    let txid: string;
    try {
      txid = Transaction.fromHexEF(efHex).id('hex');
    } catch (e) {
      throw new Error(`submit: not an Extended Format transaction (attach every input's source): ${(e as Error).message}`);
    }
    const res = await doFetch(`${baseUrl}/tx`, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain' },
      body: efHex,
      signal: AbortSignal.timeout(postTimeoutMs),
    });
    const body = await readBody(res);
    if (res.ok) {
      const b = body as { txid?: string; txStatus?: string };
      if (b?.txid !== txid) throw new Error(`submit: broadcaster answered for txid ${b?.txid}, expected ${txid}`);
      return { txid, status: String(b.txStatus) };
    }
    const b = (typeof body === 'object' && body !== null ? body : {}) as { status?: number; reason?: string; error?: string; detail?: string };
    const arc = typeof b.status === 'number' ? b.status : undefined;
    const notFinal = arc === 476;
    const retryable = notFinal || res.status === 503 || res.status >= 500;
    throw new BroadcastError(
      `POST /tx ${txid}: HTTP ${res.status}${arc !== undefined ? ` ARC ${arc}` : ''}: ${b.reason ?? b.detail ?? b.error ?? describe(body)}`,
      res.status, arc, body, retryable, notFinal,
    );
  }

  async function getStatus(txid: string): Promise<TxStatus | undefined> {
    const { status, body } = await getJson(`/tx/${txid}`);
    if (status === 404) return undefined;
    if (status < 200 || status >= 300) throw new Error(`GET /tx/${txid}: HTTP ${status}: ${describe(body)}`);
    const st = body as TxStatus;
    if (typeof st?.txStatus !== 'string') throw new Error(`GET /tx/${txid}: no txStatus in ${describe(body)}`);
    if (st.txid !== txid) throw new Error(`GET /tx/${txid}: answered for txid ${st.txid}`);
    return st;
  }

  async function waitForStatus(txid: string, until: Until, timeoutMs: number): Promise<TxStatus> {
    const deadline = now() + timeoutMs;
    const interval = until === 'MINED' ? pollMs.mined : pollMs.seen;
    let note = 'no response yet';
    for (;;) {
      let st: TxStatus | undefined;
      try {
        st = await getStatus(txid);
        note = st === undefined ? 'HTTP 404 (not yet)' : st.txStatus +
          (st.competingTxs?.length ? ` with competing transactions ${st.competingTxs.join(', ')}` : '');
      } catch (e) {
        note = `request failed (${(e as Error).message})`;
      }
      opts.onPoll?.(txid, st, note);
      if (st) {
        if (TERMINAL_FAILURES.includes(st.txStatus)) throw new TxRejectedError(st);
        if (reached(st, until)) return st;
      }
      if (now() + interval > deadline) {
        throw new Error(`waitFor ${txid}: timed out after ${timeoutMs} ms waiting for ${until}; last: ${note}`);
      }
      await sleep(interval);
    }
  }

  async function waitFor(txid: string, until: Until, timeoutMs: number): Promise<string> {
    return (await waitForStatus(txid, until, timeoutMs)).txStatus;
  }

  async function readPolicy(): Promise<Policy> {
    const { status, body } = await getJson('/policy');
    if (status !== 200) throw new Error(`GET /policy: HTTP ${status}: ${describe(body)}`);
    const p = (body as { policy?: Record<string, unknown> })?.policy;
    const fee = p?.miningFee as Partial<FeeRate> | undefined;
    const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);
    const satoshis = num(fee?.satoshis);
    const bytes = num(fee?.bytes);
    const maxTxSize = num(p?.maxtxsizepolicy);
    const maxScriptSize = num(p?.maxscriptsizepolicy);
    if (satoshis === undefined || bytes === undefined || !(bytes > 0) || maxTxSize === undefined || maxScriptSize === undefined) {
      throw new Error(`GET /policy: expected policy.miningFee.{satoshis,bytes}, policy.maxtxsizepolicy and ` +
        `policy.maxscriptsizepolicy; got ${describe(body)}`);
    }
    return { satPerKb: (satoshis * 1000) / bytes, maxTxSize, maxScriptSize, miningFee: { satoshis, bytes }, raw: body };
  }

  async function tipHeight(): Promise<number | undefined> {
    try {
      const { status, body } = await getJson('/health');
      const h = (body as { blockHeight?: unknown })?.blockHeight;
      return status === 200 && typeof h === 'number' && Number.isInteger(h) && h > 0 ? h : undefined;
    } catch {
      return undefined;
    }
  }

  return { baseUrl, submit, getStatus, waitForStatus, waitFor, readPolicy, tipHeight };
}

// The brief's interface, bound to the testnet broadcaster and the global fetch.
let defaultClient: Broadcaster | undefined;
const client = () => (defaultClient ??= createBroadcaster());
export const submit = (efHex: string) => client().submit(efHex);
export const waitFor = (txid: string, until: Until, timeoutMs: number) => client().waitFor(txid, until, timeoutMs);
export const readPolicy = () => client().readPolicy();
