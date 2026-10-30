// Gate (a) transaction builders. Nothing here touches the network.
//
// GateReferee's `prepare` and `fraud` are assembled by hand, ported from contracts/test/gate-d.test.ts
// (the SDK cannot put a stateful covenant, two stateless checkers and a P2PKH fee coin in one call).
// HeadsUpReferee's post, act and forfeit are built by the Rúnar SDK (prepareCall / finalizeCall) on a
// CaptureProvider whose `broadcast` only hands the transaction back; the harness submits it. Deploys,
// the refund pot and the refund are plain hand-built transactions.
//
// Every fee is sized from the serialized transaction at the /policy rate, on the Extended Format size
// (the larger of the two, and what is POSTed), with a floor of 1 sat.
import {
  BigNumber, Curve, Hash, LockingScript, Point, PrivateKey, ProtoWallet, Script, Transaction,
  TransactionSignature, UnlockingScript, Utils,
} from '@bsv/sdk';
import {
  RunarContract, LocalSigner, encodeArg, encodePushData, encodeScriptNumber,
} from 'runar-sdk';
import type { Provider, TransactionData, UTXO } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { cardPoint, challenge, prove, statementBytes, toAffine64 } from '@brc100-poker/protocol';
import type { DleqStatement } from '@brc100-poker/protocol';
import { feeFor, type FeeRate } from '../broadcaster.js';
import { p2pkhScript, pkhOf } from './store.js';

// ---- sizes, EF and fees --------------------------------------------------------------------------

export type SourceLookup = (txid: string, vout: number) => { satoshis: number; script: string } | undefined;

export const lookupIn = (txs: Map<string, Transaction>): SourceLookup => (txid, vout) => {
  const o = txs.get(txid)?.outputs[vout];
  return o ? { satoshis: o.satoshis!, script: o.lockingScript.toHex() } : undefined;
};

const varIntLen = (n: number) => (n < 0xfd ? 1 : n <= 0xffff ? 3 : n <= 0xffffffff ? 5 : 9);

/**
 * EF size (BRC-30): the raw size, plus the 6-byte marker `0000000000EF` after the version, plus per
 * input the source output's satoshis and locking script.
 */
export function efSizeOf(tx: Transaction, src: SourceLookup): number {
  let n = tx.toBinary().length + 6;
  for (const i of tx.inputs) {
    const o = src(i.sourceTXID!, i.sourceOutputIndex);
    if (!o) throw new Error(`no source output for ${i.sourceTXID}:${i.sourceOutputIndex}`);
    const len = o.script.length / 2;
    n += 8 + varIntLen(len) + len;
  }
  return n;
}

/** The fee actually paid: inputs (from their sources) minus outputs. */
export function paidFee(tx: Transaction, src: SourceLookup): number {
  const inSats = tx.inputs.reduce((s, i) => s + src(i.sourceTXID!, i.sourceOutputIndex)!.satoshis, 0);
  return inSats - tx.outputs.reduce((s, o) => s + o.satoshis!, 0);
}

/** EF hex, with every input's source transaction attached. */
export function efHexOf(tx: Transaction, txs: Map<string, Transaction>): string {
  for (const i of tx.inputs) {
    const src = txs.get(i.sourceTXID!);
    if (!src) throw new Error(`EF: source transaction ${i.sourceTXID} is not in the run file`);
    i.sourceTransaction = src;
  }
  return tx.toHexEF();
}

export const STOCK_MAX_SCRIPT = 500_000; // a stock node's maxscriptsizepolicy (toolkit consensus facts)
export const STOCK_MAX_TX = 10_000_000; // a stock node's maxtxsizepolicy

export interface Measured {
  size: number; efSize: number; fee: number; required: number;
  maxUnlockingScript: number; maxLockingScript: number; flags: string[];
}

/** Sizes, the fee paid against the fee required, and every size over /policy or a stock default. */
export function measure(tx: Transaction, src: SourceLookup, rate: FeeRate, limits: { maxTxSize: number; maxScriptSize: number }): Measured {
  const size = tx.toBinary().length;
  const efSize = efSizeOf(tx, src);
  const unlocks = tx.inputs.map((i) => i.unlockingScript!.toBinary().length);
  const locks = tx.outputs.map((o) => o.lockingScript.toBinary().length);
  const flags: string[] = [];
  if (size > limits.maxTxSize) flags.push(`POLICY: tx ${size} B > /policy maxtxsizepolicy ${limits.maxTxSize}`);
  if (size > STOCK_MAX_TX) flags.push(`STOCK: tx ${size} B > stock maxtxsizepolicy ${STOCK_MAX_TX}`);
  unlocks.forEach((n, i) => {
    if (n > limits.maxScriptSize) flags.push(`POLICY: input ${i} unlocking script ${n} B > /policy maxscriptsizepolicy ${limits.maxScriptSize}`);
    if (n > STOCK_MAX_SCRIPT) flags.push(`STOCK: input ${i} unlocking script ${n} B > stock maxscriptsizepolicy ${STOCK_MAX_SCRIPT}`);
  });
  locks.forEach((n, i) => {
    if (n > limits.maxScriptSize) flags.push(`POLICY: output ${i} locking script ${n} B > /policy maxscriptsizepolicy ${limits.maxScriptSize}`);
    if (n > STOCK_MAX_SCRIPT) flags.push(`STOCK: output ${i} locking script ${n} B > stock maxscriptsizepolicy ${STOCK_MAX_SCRIPT}`);
  });
  return {
    size, efSize, fee: paidFee(tx, src), required: feeFor(efSize, rate),
    maxUnlockingScript: Math.max(0, ...unlocks), maxLockingScript: Math.max(0, ...locks), flags,
  };
}

// ---- the gate wallet -----------------------------------------------------------------------------

export interface Wallet {
  signer: LocalSigner;
  pub: string;
  pkh: string;
  script: string;
}

export async function walletOf(key: PrivateKey): Promise<Wallet> {
  const signer = new LocalSigner(key.toHex().padStart(64, '0'));
  const pub = await signer.getPublicKey();
  const pkh = pkhOf(pub);
  return { signer, pub, pkh, script: p2pkhScript(pkh) };
}

async function signP2PKH(w: Wallet, tx: Transaction, idx: number, coin: UTXO) {
  const sig = await w.signer.sign(tx.toHex(), idx, coin.script, coin.satoshis);
  tx.inputs[idx]!.unlockingScript = UnlockingScript.fromHex(encodePushData(sig) + encodePushData(w.pub));
}

/**
 * Spend one wallet coin into `outputs` plus change back to the wallet, the fee sized in a fixed-point
 * loop on the EF size.
 */
export async function buildFunded(w: Wallet, coin: UTXO, outputs: Array<{ script: string; satoshis: number }>, rate: FeeRate, src: SourceLookup): Promise<Transaction> {
  const out = outputs.reduce((s, o) => s + o.satoshis, 0);
  let fee = 0;
  for (let round = 0; round < 10; round++) {
    const change = coin.satoshis - out - fee;
    if (change < 1) throw new Error(`coin ${coin.txid}:${coin.outputIndex} (${coin.satoshis} sats) cannot cover ${out} + fee ${fee}`);
    const tx = new Transaction();
    tx.addInput({ sourceTXID: coin.txid, sourceOutputIndex: coin.outputIndex, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    for (const o of outputs) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.script) });
    tx.addOutput({ satoshis: change, lockingScript: LockingScript.fromHex(w.script) });
    await signP2PKH(w, tx, 0, coin);
    const need = feeFor(efSizeOf(tx, withCoin(src, coin)), rate);
    if (need <= fee) return tx;
    fee = need;
  }
  throw new Error('buildFunded: the fee did not converge');
}

const withCoin = (src: SourceLookup, ...coins: UTXO[]): SourceLookup => (txid, vout) =>
  coins.find((c) => c.txid === txid && c.outputIndex === vout) ?? src(txid, vout);

// ---- GateReferee: the recorded invalid proof and the checker splice (from gate-d.test.ts) --------

const curve = new Curve();
const G = curve.g as Point;
const N = BigInt('0x' + curve.n.toHex());
const bn = (k: bigint) => new BigNumber(k.toString(16), 16);
const big = (b: BigNumber) => BigInt('0x' + (b.toHex() || '0'));
const mul = (p: Point, k: bigint) => p.mul(bn(k));
const aff = (p: Point) => Utils.toHex(Array.from(toAffine64(p)));
const sha256Hex = (hex: string) => Utils.toHex(Hash.sha256(Utils.toArray(hex, 'hex')));
const rev = (hex: string) => hex.match(/../g)!.reverse().join('');
const le32 = (n: number) => rev(n.toString(16).padStart(8, '0'));

export const GATE_DOMAIN = 'brc100-poker/gate-a/v1';
export const REF_SATS = 2_000;
export const BOND = 1_000;
export const CHECKER_SATS = 1;
/** The equation convicted: eq 1 carries both checker templates, the largest prepare (~1.8 MB). */
export const FRAUD_EQ = 1 as const;

type Recorded = { st: DleqStatement; A1: Point; A2: Point; z: bigint };

/** gate-d's pinned honest vector (s = 5, X = cardPoint(0)) under this gate's domain, with z + 1 recorded: both equations are false. */
export function invalidProof(): Recorded {
  const s = 5n;
  const X = cardPoint(0);
  const st: DleqStatement = { domain: GATE_DOMAIN, B1: G, P1: mul(G, s), B2: X, P2: mul(X, s) };
  const pr = prove(st, bn(s), new Uint8Array(32).fill(7));
  return { st, A1: pr.A1, A2: pr.A2, z: (big(pr.z) + 1n) % N };
}

const cOf = (r: Recorded) => big(challenge(statementBytes(r.st, r.A1, r.A2)));

function products(r: Recorded, eq: 1 | 2): { L: Point; M: Point } {
  const c = cOf(r);
  return eq === 1 ? { L: mul(G, r.z), M: mul(r.st.P1, c) } : { L: mul(r.st.B2, r.z), M: mul(r.st.P2, c) };
}

export type Tmpl = { mulA: string; mulMid: string; mulB: string; mulTail: string; genHead: string; genMid: string; genBody: string; genTail: string };

function segmentsOf(art: RunarArtifact): { names: string[]; segs: string[] } {
  const slots = [...art.constructorSlots!].sort((a, b) => a.byteOffset - b.byteOffset);
  const segs: string[] = [];
  let pos = 0;
  for (const s of slots) {
    if (art.script.slice(s.byteOffset * 2, s.byteOffset * 2 + 2) !== '00') throw new Error(`${art.contractName}: slot ${s.name} is not an OP_0 placeholder`);
    segs.push(art.script.slice(pos * 2, s.byteOffset * 2));
    pos = s.byteOffset + 1;
  }
  segs.push(art.script.slice(pos * 2));
  return { names: slots.map((s) => String(s.name)), segs };
}

/** The checker template segments, with the slot shape GateReferee hard-codes asserted. */
export function templatesOf(mulArt: RunarArtifact, genArt: RunarArtifact): Tmpl {
  const m = segmentsOf(mulArt);
  const g = segmentsOf(genArt);
  const ok = m.names.join() === 'base,k,base,k,out' && m.segs[0] === '' && m.segs[3] === '' && g.names.join() === 'k,k,out';
  if (!ok) throw new Error(`checker slot shapes changed (${m.names}; ${g.names}): GateReferee's splice no longer applies`);
  return { mulA: m.segs[1]!, mulMid: m.segs[2]!, mulB: m.segs[4]!, mulTail: m.segs[5]!, genHead: g.segs[0]!, genMid: g.segs[1]!, genBody: g.segs[2]!, genTail: g.segs[3]! };
}

const pushPoint = (p: Point) => '40' + aff(p);

function pushNum(n: bigint): string {
  if (n === 0n) return '00';
  if (n >= 1n && n <= 16n) return (0x50 + Number(n)).toString(16);
  const bytes: number[] = [];
  for (let a = n; a > 0n; a >>= 8n) bytes.push(Number(a & 0xffn));
  if (bytes[bytes.length - 1]! & 0x80) bytes.push(0x00);
  return bytes.length.toString(16).padStart(2, '0') + Buffer.from(bytes).toString('hex');
}

const mulScript = (t: Tmpl, base: Point, k: bigint, out: Point) =>
  pushPoint(base) + t.mulA + pushNum(k) + t.mulMid + pushPoint(base) + pushNum(k) + t.mulB + pushPoint(out) + t.mulTail;
const genScript = (t: Tmpl, k: bigint, out: Point) =>
  t.genHead + pushNum(k) + t.genMid + pushNum(k) + t.genBody + pushPoint(out) + t.genTail;

export function refereeArgs(r: Recorded, t: Tmpl): unknown[] {
  return [
    Utils.toHex(Utils.toArray(r.st.domain, 'utf8')),
    aff(r.st.B2), aff(r.st.P1), aff(r.st.P2), aff(r.A1), aff(r.A2), r.z,
    BigInt(BOND),
    BigInt(t.mulA.length / 2), BigInt(t.mulB.length / 2), sha256Hex(t.mulA + t.mulB),
    t.mulMid, t.mulTail,
    t.genHead, t.genMid, BigInt(t.genBody.length / 2), sha256Hex(t.genBody), t.genTail,
    '00'.repeat(64), '00'.repeat(64), '00'.repeat(20),
  ];
}

function refereeUnlock(c: RunarContract, method: string, args: unknown[], changePKH: string, change: bigint, preimage: string) {
  const pub = c.artifact.abi.methods.filter((m) => m.isPublic);
  const idx = pub.findIndex((m) => m.name === method);
  const m = pub[idx]!;
  const tail = m.params.slice(args.length).map((p) => p.name).join();
  if (tail !== '_changePKH,_changeAmount,txPreimage') throw new Error(`GateReferee.${method}: unexpected trailing params ${tail}`);
  let hex = c.buildStatefulPrefix('', m.usesCodePart ?? true);
  for (const a of args) hex += encodeArg(a);
  hex += encodePushData(changePKH) + encodeScriptNumber(change) + encodePushData(preimage);
  if (pub.length > 1) hex += encodeScriptNumber(BigInt(idx));
  return hex;
}

/**
 * A referee spend: input 0 the referee, then `extra` (empty unlocks: the checkers' verify() takes no
 * arguments), then the wallet coin; outputs the continuation, then `outputs`, then change.
 */
async function callReferee(refArt: RunarArtifact, w: Wallet, src: SourceLookup, rate: FeeRate, o: {
  ref: UTXO; method: 'prepare' | 'fraud'; args: (feeOutpoint: string) => unknown[]; extra: UTXO[]; coin: UTXO;
  outputs: Array<{ script: string; satoshis: number }>; next: { satoshis: number; state: Record<string, unknown> };
}): Promise<Transaction> {
  const ref = RunarContract.fromUtxo(refArt, o.ref);
  const nextC = RunarContract.fromUtxo(refArt, o.ref);
  nextC.setState(o.next.state);
  const contScript = nextC.getLockingScript();
  const methodIndex = refArt.abi.methods.filter((m) => m.isPublic).findIndex((m) => m.name === o.method);
  const args = o.args(rev(o.coin.txid) + le32(o.coin.outputIndex));
  const totalIn = o.ref.satoshis + o.extra.reduce((s, u) => s + u.satoshis, 0) + o.coin.satoshis;
  const totalOut = o.next.satoshis + o.outputs.reduce((s, x) => s + x.satoshis, 0);
  const all = withCoin(src, o.ref, ...o.extra, o.coin);
  let fee = 0;
  for (let round = 0; round < 10; round++) {
    const change = totalIn - totalOut - fee;
    if (change < 1) throw new Error(`${o.method}: coin cannot cover outputs and fee ${fee}`);
    const tx = new Transaction();
    tx.addInput({ sourceTXID: o.ref.txid, sourceOutputIndex: o.ref.outputIndex, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    for (const e of o.extra) tx.addInput({ sourceTXID: e.txid, sourceOutputIndex: e.outputIndex, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    tx.addInput({ sourceTXID: o.coin.txid, sourceOutputIndex: o.coin.outputIndex, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    tx.addOutput({ satoshis: o.next.satoshis, lockingScript: LockingScript.fromHex(contScript) });
    for (const x of o.outputs) tx.addOutput({ satoshis: x.satoshis, lockingScript: LockingScript.fromHex(x.script) });
    tx.addOutput({ satoshis: change, lockingScript: LockingScript.fromHex(w.script) });
    const { preimageHex } = ref.computeOpPushTxWithCodeSep(tx, 0, o.ref.script, o.ref.satoshis, methodIndex);
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(refereeUnlock(ref, o.method, args, w.pkh, BigInt(change), preimageHex));
    await signP2PKH(w, tx, tx.inputs.length - 1, o.coin);
    const need = feeFor(efSizeOf(tx, all), rate);
    if (need <= fee) return tx;
    fee = need;
  }
  throw new Error(`${o.method}: the fee did not converge`);
}

export interface RefereeKit { refArt: RunarArtifact; tmpl: Tmpl; rec: Recorded }

/** The deployed referee: the invalid proof recorded at phase 0. */
export const refereeLockingScript = (k: RefereeKit) => new RunarContract(k.refArt, refereeArgs(k.rec, k.tmpl)).getLockingScript();

export async function buildPrepare(k: RefereeKit, w: Wallet, src: SourceLookup, rate: FeeRate, ref: UTXO, coin: UTXO) {
  const { L, M } = products(k.rec, FRAUD_EQ);
  const r = k.rec;
  const lScript = genScript(k.tmpl, r.z, L);
  const mScript = mulScript(k.tmpl, r.st.P1, cOf(r), M);
  return callReferee(k.refArt, w, src, rate, {
    ref, coin, method: 'prepare', extra: [],
    args: () => [BigInt(FRAUD_EQ), aff(L), aff(M), k.tmpl.mulA, k.tmpl.mulB, k.tmpl.genBody, w.pkh],
    outputs: [{ script: lScript, satoshis: CHECKER_SATS }, { script: mScript, satoshis: CHECKER_SATS }],
    next: { satoshis: ref.satoshis + BOND, state: { phase: 1n, eq: BigInt(FRAUD_EQ), L: aff(L), M: aff(M), bondPKH: w.pkh } },
  });
}

export async function buildFraud(k: RefereeKit, w: Wallet, src: SourceLookup, rate: FeeRate, ref: UTXO, checkers: [UTXO, UTXO], coin: UTXO) {
  const st = RunarContract.fromUtxo(k.refArt, ref).state;
  return callReferee(k.refArt, w, src, rate, {
    ref, coin, method: 'fraud', extra: checkers,
    args: (feeOutpoint) => [feeOutpoint],
    outputs: [{ script: w.script, satoshis: BOND }], // the bond returns to bondPKH, the wallet
    next: { satoshis: ref.satoshis - BOND, state: { ...st, phase: 2n } },
  });
}

// ---- what every output must pay ------------------------------------------------------------------

/** The continuation a covenant spend must create: the deployed code with `state`. */
export function continuationOf(art: RunarArtifact, utxo: UTXO, state: Record<string, unknown>): string {
  const c = RunarContract.fromUtxo(art, utxo);
  c.setState(state);
  return c.getLockingScript();
}

/** prepare's outputs: the phase-1 continuation, the two checkers, change to the gate key. */
export function prepareExpected(k: RefereeKit, w: Wallet, ref: UTXO): string[] {
  const { L, M } = products(k.rec, FRAUD_EQ);
  const r = k.rec;
  return [
    continuationOf(k.refArt, ref, { phase: 1n, eq: BigInt(FRAUD_EQ), L: aff(L), M: aff(M), bondPKH: w.pkh }),
    genScript(k.tmpl, r.z, L), mulScript(k.tmpl, r.st.P1, cOf(r), M), w.script,
  ];
}

/** fraud's outputs: the phase-2 continuation, the bond to the gate key, change to the gate key. */
export function fraudExpected(k: RefereeKit, w: Wallet, ref: UTXO): string[] {
  const st = RunarContract.fromUtxo(k.refArt, ref).state;
  return [continuationOf(k.refArt, ref, { ...st, phase: 2n }), w.script, w.script];
}

/** Refuses a transaction unless its outputs pay exactly `expected`, in order. */
export function assertOutputs(what: string, tx: Transaction, expected: string[]): void {
  const got = tx.outputs.map((o) => o.lockingScript.toHex());
  const bad = got.length !== expected.length ? [`${got.length} outputs, expected ${expected.length}`]
    : got.flatMap((g, i) => (g === expected[i] ? [] : [`output ${i} pays an unexpected script (${g.length / 2} B, starts ${g.slice(0, 16)})`]));
  if (bad.length) throw new Error(`${what}: refusing to record a transaction whose outputs are not the gate key's or the contracts': ${bad.join('; ')}`);
}

// ---- HeadsUpReferee seats, the refund pot ---------------------------------------------------------

const ESCROW_PROTOCOL: [1, string] = [1, 'brc100 poker escrow'];
const SEAT_KEY_ID = 'gate-a';

/** A seat signs the way a BRC-100 wallet does: createSignature over a digest the caller built. */
export class Seat {
  private readonly wallet: ProtoWallet;
  constructor(priv: PrivateKey) { this.wallet = new ProtoWallet(priv); }
  async pub(): Promise<string> {
    return (await this.wallet.getPublicKey({ protocolID: ESCROW_PROTOCOL, keyID: SEAT_KEY_ID, counterparty: 'self' })).publicKey;
  }
  async sign(digestHex: string, flag: number): Promise<string> {
    const { signature } = await this.wallet.createSignature({
      hashToDirectlySign: Utils.toArray(digestHex, 'hex'), protocolID: ESCROW_PROTOCOL, keyID: SEAT_KEY_ID, counterparty: 'self',
    });
    return Utils.toHex(signature) + flag.toString(16).padStart(2, '0');
  }
}

/** The two seats' keys, derived from the gate key (never stored or printed). */
export function seatsOf(gate: PrivateKey): [Seat, Seat] {
  const seat = (i: number) => new Seat(PrivateKey.fromHex(Utils.toHex(Hash.sha256([
    ...Utils.toArray(gate.toHex().padStart(64, '0'), 'hex'), ...Utils.toArray(`gate-a/seat/${i}`, 'utf8'),
  ]))));
  return [seat(0), seat(1)];
}

/** A bare 2-of-2 between the seats: the pot a pre-signed nLockTime refund spends (G-38). */
export const potScript = (pk0: string, pk1: string) => `5221${pk0}21${pk1}52ae`;

/**
 * The refund-shaped transaction: the pot back to the seats' hand-start balances, both seats signing
 * ALL|FORKID in advance, non-final until `lockTime` (sequence 0xfffffffe). Its fee comes from the pot.
 */
export async function buildRefund(seats: [Seat, Seat], pot: UTXO, lockTime: number, refundBal: number, dummy = false): Promise<Transaction> {
  const [pk0, pk1] = [await seats[0].pub(), await seats[1].pub()];
  const tx = new Transaction();
  tx.lockTime = lockTime;
  tx.addInput({ sourceTXID: pot.txid, sourceOutputIndex: pot.outputIndex, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xfffffffe });
  tx.addOutput({ satoshis: refundBal, lockingScript: LockingScript.fromHex(p2pkhScript(pkhOf(pk0))) });
  tx.addOutput({ satoshis: refundBal, lockingScript: LockingScript.fromHex(p2pkhScript(pkhOf(pk1))) });
  let sigs: string[];
  if (dummy) {
    sigs = ['30'.padEnd(146, '0'), '30'.padEnd(146, '0')]; // 73-byte placeholders, the longest DER + flag
  } else {
    const preimage = TransactionSignature.format({
      sourceTXID: pot.txid, sourceOutputIndex: pot.outputIndex, sourceSatoshis: pot.satoshis, transactionVersion: tx.version,
      otherInputs: [], outputs: tx.outputs, inputIndex: 0, subscript: Script.fromHex(pot.script),
      inputSequence: 0xfffffffe, lockTime, scope: 0x41,
    });
    const digest = Utils.toHex(Hash.hash256(preimage));
    sigs = [await seats[0].sign(digest, 0x41), await seats[1].sign(digest, 0x41)];
  }
  tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex('00' + encodePushData(sigs[0]!) + encodePushData(sigs[1]!));
  return tx;
}

// ---- HeadsUpReferee calls through the SDK ----------------------------------------------------------

/**
 * A Rúnar Provider that never touches a network: it offers the coins it is given, prices at
 * `feeRate`, and "broadcasts" by keeping the transaction for the harness to submit.
 */
export class CaptureProvider implements Provider {
  captured: Transaction | undefined;
  constructor(public utxos: UTXO[], public feeRate: number, private readonly txs: Map<string, Transaction>) {}
  async getTransaction(txid: string): Promise<TransactionData> {
    const tx = this.captured?.id('hex') === txid ? this.captured : this.txs.get(txid);
    if (!tx) throw new Error(`CaptureProvider: ${txid} unknown`);
    return {
      txid, version: tx.version, locktime: tx.lockTime, raw: tx.toHex(),
      inputs: tx.inputs.map((i) => ({ txid: i.sourceTXID!, outputIndex: i.sourceOutputIndex, script: i.unlockingScript?.toHex() ?? '', sequence: i.sequence ?? 0xffffffff })),
      outputs: tx.outputs.map((o) => ({ satoshis: o.satoshis!, script: o.lockingScript.toHex() })),
    };
  }
  async broadcast(tx: Transaction): Promise<string> { this.captured = tx; return tx.id('hex'); }
  async getUtxos(): Promise<UTXO[]> { return this.utxos; }
  async getContractUtxo(): Promise<UTXO | null> { return null; }
  getNetwork(): 'testnet' { return 'testnet'; }
  async getFeeRate(): Promise<number> { return this.feeRate; }
  async getRawTransaction(txid: string): Promise<string> { return (await this.getTransaction(txid)).raw!; }
}

const sdkWarnings: string[] = [];

/** Runs `fn` with the SDK's advisory console warnings collected (read them with takeSdkWarnings). */
export async function quietly<T>(fn: () => Promise<T>): Promise<T> {
  const warn = console.warn;
  console.warn = (...a: unknown[]) => { sdkWarnings.push(a.map(String).join(' ')); };
  try { return await fn(); } finally { console.warn = warn; }
}

/** The SDK warnings collected since the last call, deduplicated. */
export function takeSdkWarnings(): string[] {
  const out = [...new Set(sdkWarnings)];
  sdkWarnings.length = 0;
  return out;
}

export interface HurKit { art: RunarArtifact; seats: [Seat, Seat]; pk0: string; pk1: string }

export const hurState = (p: { bal: number }, s: { com0: number; com1: number; seq: number; toAct: number; deadline: number }) => ({
  bal0: BigInt(p.bal), bal1: BigInt(p.bal), com0: BigInt(s.com0), com1: BigInt(s.com1),
  phase: 1n, seq: BigInt(s.seq), toAct: BigInt(s.toAct), deadline: BigInt(s.deadline),
});

export const hurArgs = (k: HurKit, p: { anchor: number; maxSeq: number; window: number; bigBlind: number; bal: number }) =>
  [k.pk0, k.pk1, BigInt(p.anchor), BigInt(p.maxSeq), BigInt(p.window), BigInt(p.bigBlind), BigInt(p.bal), BigInt(p.bal)];

/** The co-signed state the post carries: seat 0 has bet 200 over seat 1's 100, and seat 1 is on the clock. */
export const POSTED = { com0: 200, com1: 100, toAct: 1 };
/** Seat 1's on-chain raise. */
export const RAISE_TO = 600;

export interface PostPlan { seq: number; lockTime: number; deadline: number; bal: number; window: number }

export async function preparePost(k: HurKit, w: Wallet, escrow: UTXO, coin: UTXO, p: PostPlan, sdkRate: number, txs: Map<string, Transaction>) {
  const c = RunarContract.fromUtxo(k.art, escrow);
  const cap = new CaptureProvider([coin], sdkRate, txs);
  c.connect(cap, w.signer);
  const prepared = await quietly(() => c.prepareCall('post', [BigInt(p.seq), BigInt(POSTED.toAct), BigInt(POSTED.com0), BigInt(POSTED.com1), null, null], {
    locktime: p.lockTime, sequence: 0xfffffffe,
    outputs: [{ satoshis: 2 * p.bal, state: hurState(p, { ...POSTED, seq: p.seq, deadline: p.deadline }) }],
  } as never));
  return { c, cap, prepared };
}

/**
 * The exact-cover coin for the post (icellan/runar#176: a SINGLE post with change never validates).
 * Sized on a probe escrow and probe coin: the post's size does not depend on their txids, and its
 * SINGLE|ANYONECANPAY signatures do not cover the fee input. Raises the SDK's rate until the coin
 * covers the post's EF size at the policy rate.
 */
export async function sizePostCoin(k: HurKit, w: Wallet, escrowScript: string, p: PostPlan, rate: FeeRate, satPerKb: number): Promise<{ coinSats: number; sdkRate: number }> {
  const escrow: UTXO = { txid: 'e5'.repeat(32), outputIndex: 0, satoshis: 2 * p.bal, script: escrowScript };
  const probe = (sats: number): UTXO => ({ txid: 'c0'.repeat(32), outputIndex: 1, satoshis: sats, script: w.script });
  let sdkRate = satPerKb * 1.5;
  for (let round = 0; round < 20; round++, sdkRate *= 1.25) {
    const big = await preparePost(k, w, escrow, probe(1_000_000), p, sdkRate, new Map());
    const est = 1_000_000 - big.prepared.tx.outputs[1]!.satoshis!;
    const exact = await preparePost(k, w, escrow, probe(est), p, sdkRate, new Map());
    if (exact.prepared.tx.outputs.length !== 1) continue;
    const src = withCoin(() => undefined, escrow, probe(est));
    const need = feeFor(efSizeOf(exact.prepared.tx, src) + 8, rate); // +8: DER signatures vary by a byte or two each
    if (est >= need) return { coinSats: est, sdkRate };
  }
  throw new Error('sizePostCoin: no SDK fee rate produced an exact-cover post that meets the policy fee');
}

export async function buildPost(k: HurKit, w: Wallet, escrow: UTXO, coin: UTXO, p: PostPlan, sdkRate: number, txs: Map<string, Transaction>): Promise<Transaction> {
  const { c, cap, prepared } = await preparePost(k, w, escrow, coin, p, sdkRate, txs);
  if (prepared.tx.outputs.length !== 1) throw new Error(`post: expected exact cover (no change), got ${prepared.tx.outputs.length} outputs`);
  const sigs = { 4: await k.seats[0].sign(prepared.sighash, 0xc3), 5: await k.seats[1].sign(prepared.sighash, 0xc3) };
  await quietly(() => c.finalizeCall(prepared, sigs));
  return cap.captured!;
}

export async function buildAct(k: HurKit, w: Wallet, escrow: UTXO, coin: UTXO, p: { bal: number; deadline: number; seq: number }, rate: FeeRate, satPerKb: number, txs: Map<string, Transaction>): Promise<{ tx: Transaction; sdkRate: number }> {
  let sdkRate = satPerKb * 1.5;
  const src = withCoin(lookupIn(txs), escrow, coin);
  for (let round = 0; round < 20; round++, sdkRate *= 1.25) {
    const c = RunarContract.fromUtxo(k.art, escrow);
    const cap = new CaptureProvider([coin], sdkRate, txs);
    c.connect(cap, w.signer);
    const prepared = await quietly(() => c.prepareCall('act', [2n, BigInt(RAISE_TO), null], {
      changeAddress: w.pkh, // change to the gate key, explicitly (not the SDK's signer default)
      outputs: [{ satoshis: 2 * p.bal, state: hurState(p, { com0: POSTED.com0, com1: RAISE_TO, seq: p.seq + 1, toAct: 0, deadline: p.deadline }) }],
    } as never));
    if (paidFee(prepared.tx, src) < feeFor(efSizeOf(prepared.tx, src) + 8, rate)) continue;
    const sig = await k.seats[1].sign(prepared.sighash, 0x41);
    await quietly(() => c.finalizeCall(prepared, { 2: sig }));
    return { tx: cap.captured!, sdkRate };
  }
  throw new Error('act: no SDK fee rate met the policy fee');
}

/**
 * Anyone forfeits the seat on the clock (seat 0) once nLockTime >= deadline. The payout outputs are
 * pinned by the contract; the caller's fee coin returns change as the contract's optional trailing
 * output, sized in a fixed-point loop.
 */
export async function buildForfeit(k: HurKit, w: Wallet, escrow: UTXO, coin: UTXO, p: { bal: number; lockTime: number }, rate: FeeRate, txs: Map<string, Transaction>): Promise<Transaction> {
  const src = withCoin(lookupIn(txs), escrow, coin);
  const pay0 = { scriptHex: p2pkhScript(pkhOf(k.pk0)), satoshis: p.bal - POSTED.com0 }; // the folder keeps what it did not put in
  const pay1 = { scriptHex: p2pkhScript(pkhOf(k.pk1)), satoshis: p.bal + POSTED.com0 };
  let fee = 1;
  for (let round = 0; round < 10; round++) {
    const change = coin.satoshis - fee;
    const c = RunarContract.fromUtxo(k.art, escrow);
    const cap = new CaptureProvider([], 1, txs);
    c.connect(cap, w.signer);
    const prepared = await quietly(() => c.prepareCall('forfeit', [w.pkh, BigInt(change)], {
      locktime: p.lockTime, sequence: 0xfffffffe, feeUtxo: coin,
      terminalOutputs: [pay0, pay1, { scriptHex: w.script, satoshis: change }],
    } as never));
    await quietly(() => c.finalizeCall(prepared, {}));
    const tx = cap.captured!;
    const need = feeFor(efSizeOf(tx, src), rate);
    if (need <= fee) return tx;
    fee = need;
  }
  throw new Error('forfeit: the fee did not converge');
}
