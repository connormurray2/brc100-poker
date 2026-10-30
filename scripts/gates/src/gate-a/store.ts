// Gate (a) persistence: the key, the run file, and the wallet's UTXO set derived from it.
//
// There is no indexer, so the run file IS the wallet: the funding transaction plus every transaction
// this harness built. A step's raw bytes are written before they are submitted (write-ahead), and a
// re-run always resubmits those exact bytes rather than rebuilding, so a crash at any point resumes
// without ever creating a second, conflicting spend.
import { chmodSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { Hash, PrivateKey, Transaction, Utils } from '@bsv/sdk';
import type { UTXO } from 'runar-sdk';
import type { FeeRate, TxStatus } from '../broadcaster.js';

export const STEP_IDS = ['referee-deploy', 'prepare', 'fraud', 'hur-deploy', 'post', 'act', 'forfeit', 'refund'] as const;
export type StepId = (typeof STEP_IDS)[number];
export type StepState = 'built' | 'submitted' | 'seen' | 'mined';

export interface StepRecord {
  what: string;
  txid: string;
  rawHex: string;
  /** Serialized (raw) size, and Extended Format size (what is POSTed). */
  size: number;
  efSize: number;
  fee: number;
  feeRate: FeeRate;
  /** The fee was sized on this many bytes (the EF size, which is the larger). */
  feeSizedOn: number;
  inputs: number;
  outputs: number;
  lockTime: number;
  maxUnlockingScript: number;
  maxLockingScript: number;
  /** Sizes that exceed the policy read from /policy, or the stock node defaults. */
  flags: string[];
  notes?: Record<string, unknown>;
  state: StepState;
  builtAt: string;
  submittedAt?: string;
  seenAt?: string;
  minedAt?: string;
  submitResponse?: { txid: string; status: string };
  /** ARC 476 answers (not final yet), with the tip we believed at the time. */
  notFinal?: Array<{ at: string; tip?: number; message: string }>;
  tipAtSubmit?: number;
  tipSource?: string;
  blockHeight?: number;
  blockHash?: string;
  blocksToMine?: number;
  seenStatus?: TxStatus;
  minedStatus?: TxStatus;
}

export interface PolicyRecord {
  satPerKb: number;
  maxTxSize: number;
  maxScriptSize: number;
  miningFee: FeeRate;
  raw: unknown;
  readAt: string;
  source: string;
}

export interface HurParams {
  /** Height already reached when the escrow was deployed: the fraud transaction's block. */
  h0: number;
  anchor: number;
  maxSeq: number;
  window: number;
  seq: number;
  bigBlind: number;
  bal: number;
  postLockTime: number;
  forfeitLockTime: number;
  refundLockTime: number;
  refundBal: number;
  /** Fee rate (sat/kB) handed to the Rúnar SDK so its estimate covers the EF size. */
  sdkRate: number;
  postCoinSats: number;
}

export interface RunFile {
  version: 1;
  dryRun: boolean;
  network: 'testnet';
  broadcaster: string;
  address: string;
  createdAt: string;
  updatedAt: string;
  /** sha256 of each compiled artifact's script: a resume refuses if the contracts changed. */
  artifacts?: Record<string, string>;
  funding?: { txid: string; rawHex: string; source: string };
  policy?: PolicyRecord;
  hur?: HurParams;
  steps: Partial<Record<StepId, StepRecord>>;
  /** Dry run only: the simulated chain tip, so a dry run can resume too. */
  fakeChain?: { tip: number };
}

export const nowIso = () => new Date().toISOString();

// ---- the key -------------------------------------------------------------------------------------

/** Loads the 32-byte hex key at `path`, creating it (mode 0600) if absent. Never logged. */
export function loadOrCreateKey(path: string): { key: PrivateKey; created: boolean } {
  if (existsSync(path)) {
    const hex = readFileSync(path, 'utf8').trim();
    if (!/^[0-9a-f]{64}$/i.test(hex)) throw new Error(`${path} is not a 32-byte hex key`);
    return { key: PrivateKey.fromHex(hex), created: false };
  }
  mkdirSync(dirname(path), { recursive: true });
  const key = PrivateKey.fromRandom();
  writeFileSync(path, key.toHex().padStart(64, '0') + '\n', { mode: 0o600 });
  chmodSync(path, 0o600);
  return { key, created: true };
}

export const keyHex = (k: PrivateKey) => k.toHex().padStart(64, '0');
export const pkhOf = (pubHex: string) => Utils.toHex(Hash.hash160(Utils.toArray(pubHex, 'hex')));
export const p2pkhScript = (pkh: string) => `76a914${pkh}88ac`;

// ---- the run file --------------------------------------------------------------------------------

export function loadRun(path: string): RunFile | undefined {
  if (!existsSync(path)) return undefined;
  return JSON.parse(readFileSync(path, 'utf8')) as RunFile;
}

/** Atomic write: a crash mid-write never leaves a torn run file. */
export function saveRun(path: string, run: RunFile): void {
  run.updatedAt = nowIso();
  mkdirSync(dirname(path), { recursive: true });
  const tmp = `${path}.tmp`;
  writeFileSync(tmp, JSON.stringify(run, null, 1) + '\n', { mode: 0o600 });
  renameSync(tmp, path);
}

// ---- the wallet ----------------------------------------------------------------------------------

/** Every transaction the run knows: the funding transaction and every recorded step. */
export function knownTxs(run: RunFile): Map<string, Transaction> {
  const m = new Map<string, Transaction>();
  if (run.funding) m.set(run.funding.txid, Transaction.fromHex(run.funding.rawHex));
  for (const id of STEP_IDS) {
    const s = run.steps[id];
    if (s) m.set(s.txid, Transaction.fromHex(s.rawHex));
  }
  return m;
}

const outpoint = (txid: string, vout: number) => `${txid}:${vout}`;

/**
 * The wallet's spendable coins: outputs paying `script` in the funding transaction or in a step the
 * network has SEEN, minus every outpoint any recorded step spends (built or not: recorded bytes are
 * committed and will be resubmitted). Largest first.
 */
export function walletUtxos(run: RunFile, script: string): UTXO[] {
  const spent = new Set<string>();
  const sources: Array<{ txid: string; tx: Transaction }> = [];
  if (run.funding) sources.push({ txid: run.funding.txid, tx: Transaction.fromHex(run.funding.rawHex) });
  for (const id of STEP_IDS) {
    const s = run.steps[id];
    if (!s) continue;
    const tx = Transaction.fromHex(s.rawHex);
    for (const i of tx.inputs) spent.add(outpoint(i.sourceTXID!, i.sourceOutputIndex));
    if (s.state === 'seen' || s.state === 'mined') sources.push({ txid: s.txid, tx });
  }
  const out: UTXO[] = [];
  for (const { txid, tx } of sources) {
    tx.outputs.forEach((o, vout) => {
      if (o.lockingScript.toHex() === script && !spent.has(outpoint(txid, vout))) {
        out.push({ txid, outputIndex: vout, satoshis: o.satoshis!, script });
      }
    });
  }
  return out.sort((a, b) => b.satoshis - a.satoshis);
}

/** The UTXO at `vout` of a recorded transaction. */
export function utxoOf(tx: Transaction, vout: number): UTXO {
  const o = tx.outputs[vout]!;
  return { txid: tx.id('hex'), outputIndex: vout, satoshis: o.satoshis!, script: o.lockingScript.toHex() };
}
