// Gate (d): a referee creates the checkers (WitnessEcMulCheck) for a recorded Chaum–Pedersen proof in
// `prepare`, and `fraud` spends them, each with its slope witness, alongside the referee. Every transaction below is assembled by hand (the SDK
// cannot mix a stateful covenant, two stateless checkers and a P2PKH fee coin in one call) and
// broadcast to MockProvider, which replays EVERY input through @bsv/sdk's real Spend interpreter.
import { describe, it, expect, beforeAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { BigNumber, Curve, Hash, Point, Transaction, UnlockingScript, LockingScript, Utils } from '@bsv/sdk';
import {
  RunarContract, MockProvider, LocalSigner, buildP2PKHScript, encodeArg, encodePushData,
  encodeScriptNumber, dryRunMultiContractInput, serializeState,
} from 'runar-sdk';
import type { UTXO } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { cardPoint, challenge, ecMulWitness, fromAffine64, prove, statementBytes, toAffine64 } from '@brc100-poker/protocol';
import type { DleqStatement } from '@brc100-poker/protocol';
import { contractsRoot, loadArtifact } from './helpers';

const curve = new Curve();
const G = curve.g as Point;
const N = BigInt('0x' + curve.n.toHex());
const bn = (k: bigint) => new BigNumber(k.toString(16), 16);
const big = (b: BigNumber) => BigInt('0x' + (b.toHex() || '0'));
const mul = (p: Point, k: bigint) => p.mul(bn(k));
/** 64-byte affine x || y, big-endian: how a Rúnar Point is passed, stored and pushed. */
const aff = (p: Point) => Utils.toHex(Array.from(toAffine64(p)));
const sha256Hex = (hex: string) => Utils.toHex(Hash.sha256(Utils.toArray(hex, 'hex')));
const rev = (hex: string) => hex.match(/../g)!.reverse().join('');

const DOMAIN = 'brc100-poker/test/v1';
const SECRET = new Uint8Array(32).fill(7);
const REF_SATS = 50_000;
const BOND = 20_000;
const CHECKER_SATS = 1;
const FEE_COIN = 2_000_000;
const REFUND_PKH = Utils.toHex(Hash.hash160(Utils.toArray('33'.repeat(33), 'hex')));
const ZERO_POINT = '00'.repeat(64);
const p2pkh = (pkh: string) => `76a914${pkh}88ac`;

const REJECTED = (i: number) => new RegExp(`input ${i}: Script evaluation error`);

// ---- the recorded statement and proof ---------------------------------------------------------

/** B1 = G is implied. A recorded proof: the statement points, A1, A2 and z. */
type Recorded = { st: DleqStatement; A1: Point; A2: Point; z: bigint };

/** The pinned vector from packages/protocol/test/dleq.test.ts: s = 5, X = cardPoint(0). */
function honest(): Recorded {
  const s = 5n;
  const X = cardPoint(0);
  const st: DleqStatement = { domain: DOMAIN, B1: G, P1: mul(G, s), B2: X, P2: mul(X, s) };
  const pr = prove(st, bn(s), SECRET);
  return { st, A1: pr.A1, A2: pr.A2, z: big(pr.z) };
}

/** An honest proof with z + 1 recorded: both equations are false. */
const tamperedZ = (r: Recorded): Recorded => ({ ...r, z: (r.z + 1n) % N });

/**
 * The realistic cheat: the prover claims P2 = (s+1)·B2 (a different card) and proves with s. Eq 1
 * (z·G = A1 + c·P1) is true; eq 2 (z·B2 = A2 + c·P2) is false. `prove` refuses false statements,
 * so it is built by hand.
 */
function wrongP2(): Recorded {
  const s = 5n;
  const w = 0x1d2c3b4a59687786950a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6n;
  const X = cardPoint(0);
  const st: DleqStatement = { domain: DOMAIN, B1: G, P1: mul(G, s), B2: X, P2: mul(X, s + 1n) };
  const A1 = mul(G, w);
  const A2 = mul(X, w);
  const c = big(challenge(statementBytes(st, A1, A2)));
  return { st, A1, A2, z: (w + c * s) % N };
}

const cOf = (r: Recorded) => big(challenge(statementBytes(r.st, r.A1, r.A2)));

/** The true products for an equation: L = z·B, M = c·P. */
function products(r: Recorded, eq: 1 | 2): { L: Point; M: Point } {
  const c = cOf(r);
  return eq === 1
    ? { L: mul(G, r.z), M: mul(r.st.P1, c) }
    : { L: mul(r.st.B2, r.z), M: mul(r.st.P2, c) };
}

// ---- the checker body and the off-chain splice ------------------------------------------------

/** WitnessEcMulCheck's body: its script after the three constructor-slot placeholders at bytes 0, 1, 2. */
function bodyOf(art: RunarArtifact): string {
  const slots = [...art.constructorSlots!].sort((a, b) => a.byteOffset - b.byteOffset);
  // The shape GateReferee splices. A compiler change that moves a slot fails here first.
  expect(slots.map((x) => [x.name, x.byteOffset])).toEqual([['base', 0], ['k', 1], ['out', 2]]);
  expect(art.script.slice(0, 6)).toBe('000000'); // three OP_0 placeholders
  return art.script.slice(6);
}

const pushPoint = (p: Point | string) => '40' + (typeof p === 'string' ? p : aff(p));

/** Minimal Script-number push, written independently of the SDK (as in ecmulcheck.test.ts). */
function pushNum(n: bigint): string {
  if (n === 0n) return '00';
  if (n >= 1n && n <= 16n) return (0x50 + Number(n)).toString(16);
  const bytes: number[] = [];
  for (let a = n; a > 0n; a >>= 8n) bytes.push(Number(a & 0xffn));
  if (bytes[bytes.length - 1]! & 0x80) bytes.push(0x00);
  return bytes.length.toString(16).padStart(2, '0') + Buffer.from(bytes).toString('hex');
}

/** WitnessEcMulCheck(base, k, out): push(base) push(k) push(out) body. */
const chkScript = (base: Point, k: bigint, out: Point | string, b = body) => pushPoint(base) + pushNum(k) + pushPoint(out) + b;

/**
 * A checker's unlocking script: the slope witness for the (base, k) its script pushes, read back from
 * the script itself so it follows whatever checker is spent. No witness exists for k outside [1, n).
 */
function checkerUnlock(script: string): string {
  const base = fromAffine64(Uint8Array.from(Utils.toArray(script.slice(2, 130), 'hex')));
  const op = Number.parseInt(script.slice(130, 132), 16);
  let k: bigint;
  if (op >= 0x51 && op <= 0x60) k = BigInt(op - 0x50);
  else if (op >= 0x01 && op <= 0x4b) k = BigInt('0x' + (rev(script.slice(132, 132 + op * 2)) || '0'));
  else k = 0n;
  try {
    return encodePushData(ecMulWitness(base, k));
  } catch {
    return encodePushData('00');
  }
}

// ---- transactions -----------------------------------------------------------------------------

let chkArt: RunarArtifact;
let refArt: RunarArtifact;
let body: string;

/** Constructor args. `L0`, `M0` and `pkh0` are the initial state (normally zeros). */
function refereeArgs(r: Recorded, L0 = ZERO_POINT, M0 = ZERO_POINT, pkh0 = '00'.repeat(20)): unknown[] {
  return [
    Utils.toHex(Utils.toArray(r.st.domain, 'utf8')),
    aff(r.st.B2), aff(r.st.P1), aff(r.st.P2), aff(r.A1), aff(r.A2), r.z,
    BigInt(BOND),
    BigInt(body.length / 2), sha256Hex(body),
    L0, M0, pkh0,
  ];
}

type Env = { provider: MockProvider; rec: Recorded; ref: UTXO };

const utxoAt = (tx: Transaction, i: number): UTXO => ({
  txid: tx.id('hex'), outputIndex: i, satoshis: tx.outputs[i]!.satoshis!, script: tx.outputs[i]!.lockingScript.toHex(),
});
const lastTx = (p: MockProvider) => Transaction.fromHex(p.getBroadcastedTxs().at(-1)!);
const stateOf = (u: UTXO) => RunarContract.fromUtxo(refArt, u).state;

async function setup(rec: Recorded): Promise<Env> {
  const provider = new MockProvider('testnet');
  const deployer = new LocalSigner('11'.repeat(32));
  provider.addUtxo(await deployer.getAddress(), {
    txid: '11'.repeat(32), outputIndex: 0, satoshis: 10_000_000, script: buildP2PKHScript(await deployer.getPublicKey()),
  });
  const contract = new RunarContract(refArt, refereeArgs(rec));
  await contract.deploy(provider, deployer, { satoshis: REF_SATS });
  return { provider, rec, ref: utxoAt(lastTx(provider), 0) };
}

let coinSeq = 0;
/** A fresh P2PKH coin the caller pays fees (and the bond) from. */
async function feeCoin(provider: MockProvider, satoshis: number) {
  const signer = new LocalSigner('22'.repeat(32));
  const pub = await signer.getPublicKey();
  const utxo: UTXO = {
    txid: (++coinSeq).toString(16).padStart(64, 'c'), outputIndex: 0, satoshis, script: buildP2PKHScript(pub),
  };
  provider.addUtxo(await signer.getAddress(), utxo);
  const pkh = Utils.toHex(Hash.hash160(Utils.toArray(pub, 'hex')));
  return { signer, pub, utxo, pkh };
}

/** Unlocking script for a referee method: user args, the change pair, the preimage, the selector. */
function refereeUnlock(
  c: RunarContract, method: string, args: unknown[], changePKH: string, change: bigint, preimage: string, codePart?: string,
) {
  const pub = c.artifact.abi.methods.filter((m) => m.isPublic);
  const idx = pub.findIndex((m) => m.name === method);
  const m = pub[idx]!;
  const tail = m.params.slice(args.length).map((p) => p.name);
  expect(tail).toEqual(['_changePKH', '_changeAmount', 'txPreimage']); // no _newAmount: addOutput sizes the continuation
  let hex = codePart === undefined ? c.buildStatefulPrefix('', m.usesCodePart ?? true) : encodePushData(codePart);
  for (const a of args) hex += encodeArg(a);
  hex += encodePushData(changePKH) + encodeScriptNumber(change) + encodePushData(preimage);
  if (pub.length > 1) hex += encodeScriptNumber(BigInt(idx));
  return hex;
}

/**
 * Build and sign a referee spend: input 0 the referee, then `extraInputs`, then the caller's fee
 * coin; outputs the continuation, then `outputs`, then the caller's change. Not broadcast.
 */
async function callReferee(env: Env, o: {
  method: 'prepare' | 'fraud';
  args: (feeOutpoint: string) => unknown[];
  extraInputs?: Array<{ utxo: UTXO; unlock: string }>;
  outputs: Array<{ script: string; satoshis: number }>;
  next: { satoshis: number; state: Record<string, unknown> };
  /** Attack hooks: the contract used for the preimage, a claimed `_codePart`, a raw continuation. */
  contract?: RunarContract;
  codePart?: string;
  contScript?: string;
}): Promise<Transaction> {
  const extra = o.extraInputs ?? [];
  const ref = o.contract ?? RunarContract.fromUtxo(refArt, env.ref);
  const methodIndex = refArt.abi.methods.filter((m) => m.isPublic).findIndex((m) => m.name === o.method);
  let contScript = o.contScript;
  if (contScript === undefined) {
    const nextC = RunarContract.fromUtxo(refArt, env.ref);
    nextC.setState(o.next.state);
    contScript = nextC.getLockingScript();
  }
  const coin = await feeCoin(env.provider, FEE_COIN);
  const feeOutpoint = rev(coin.utxo.txid) + '00000000';
  const args = o.args(feeOutpoint);
  const totalIn = env.ref.satoshis + extra.reduce((s, e) => s + e.utxo.satoshis, 0) + coin.utxo.satoshis;
  const totalOut = o.next.satoshis + o.outputs.reduce((s, e) => s + e.satoshis, 0);

  let fee = 0;
  for (;;) {
    const change = totalIn - totalOut - fee;
    expect(change).toBeGreaterThan(0);
    const tx = new Transaction();
    tx.addInput({ sourceTXID: env.ref.txid, sourceOutputIndex: env.ref.outputIndex, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    for (const e of extra) {
      tx.addInput({ sourceTXID: e.utxo.txid, sourceOutputIndex: e.utxo.outputIndex, unlockingScript: UnlockingScript.fromHex(e.unlock), sequence: 0xffffffff });
    }
    tx.addInput({ sourceTXID: coin.utxo.txid, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    tx.addOutput({ satoshis: o.next.satoshis, lockingScript: LockingScript.fromHex(contScript) });
    for (const out of o.outputs) tx.addOutput({ satoshis: out.satoshis, lockingScript: LockingScript.fromHex(out.script) });
    tx.addOutput({ satoshis: change, lockingScript: LockingScript.fromHex(p2pkh(coin.pkh)) });

    const { preimageHex } = ref.computeOpPushTxWithCodeSep(tx, 0, env.ref.script, env.ref.satoshis, methodIndex);
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(refereeUnlock(ref, o.method, args, coin.pkh, BigInt(change), preimageHex, o.codePart));
    const feeIdx = tx.inputs.length - 1;
    const sig = await coin.signer.sign(tx.toHex(), feeIdx, coin.utxo.script, coin.utxo.satoshis);
    tx.inputs[feeIdx]!.unlockingScript = UnlockingScript.fromHex(encodePushData(sig) + encodePushData(coin.pub));

    const need = Math.ceil((tx.toBinary().length * 100) / 1000) + 10; // MockProvider's 100 sat/kB floor
    if (need <= fee) {
      console.log(`  ${o.method} tx: ${tx.toBinary().length} B (${tx.inputs.length} in, ${tx.outputs.length} out; ` +
        `referee unlock ${tx.inputs[0]!.unlockingScript!.toBinary().length} B)`);
      return tx;
    }
    fee = need;
  }
}

async function prepareTx(env: Env, eq: 1 | 2, L: Point | string, M: Point | string, b: string = body) {
  const r = env.rec;
  const hexOf = (p: Point | string) => (typeof p === 'string' ? p : aff(p));
  const c = cOf(r);
  const lScript = chkScript(eq === 1 ? G : r.st.B2, r.z, L, b);
  const mScript = chkScript(eq === 1 ? r.st.P1 : r.st.P2, c, M, b);
  return callReferee(env, {
    method: 'prepare',
    args: () => [BigInt(eq), hexOf(L), hexOf(M), b, REFUND_PKH],
    outputs: [{ script: lScript, satoshis: CHECKER_SATS }, { script: mScript, satoshis: CHECKER_SATS }],
    next: {
      satoshis: env.ref.satoshis + BOND,
      state: { phase: 1n, eq: BigInt(eq), L: hexOf(L), M: hexOf(M), bondPKH: REFUND_PKH },
    },
  });
}

async function fraudTx(env: Env, checkers: [UTXO, UTXO]) {
  const st = stateOf(env.ref);
  return callReferee(env, {
    method: 'fraud',
    args: (feeOutpoint) => [feeOutpoint],
    extraInputs: checkers.map((u) => ({ utxo: u, unlock: checkerUnlock(u.script) })),
    outputs: [{ script: p2pkh(REFUND_PKH), satoshis: BOND }],
    next: { satoshis: env.ref.satoshis - BOND, state: { ...st, phase: 2n } },
  });
}

/** Broadcast (every input through Spend) and follow the referee to output 0. */
async function send(env: Env, tx: Transaction) {
  await env.provider.broadcast(tx);
  env.ref = utxoAt(tx, 0);
}

/** A transaction from a fresh fee coin carrying exactly `outs` then change, broadcast. */
async function handTx(env: Env, outs: Array<{ script: string; satoshis: number }>) {
  const coin = await feeCoin(env.provider, FEE_COIN);
  let fee = 0;
  for (;;) {
    const tx = new Transaction();
    tx.addInput({ sourceTXID: coin.utxo.txid, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(''), sequence: 0xffffffff });
    for (const o of outs) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.script) });
    tx.addOutput({ satoshis: FEE_COIN - outs.reduce((a, o) => a + o.satoshis, 0) - fee, lockingScript: LockingScript.fromHex(p2pkh(coin.pkh)) });
    const sig = await coin.signer.sign(tx.toHex(), 0, coin.utxo.script, coin.utxo.satoshis);
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(encodePushData(sig) + encodePushData(coin.pub));
    const need = Math.ceil((tx.toBinary().length * 100) / 1000) + 10;
    if (need <= fee) {
      await env.provider.broadcast(tx);
      return tx;
    }
    fee = need;
  }
}

const size = (tx: Transaction) => tx.toBinary().length;
const unlockSize = (tx: Transaction) => tx.inputs[0]!.unlockingScript!.toBinary().length;

/** prepare then fraud, broadcasting both; returns the two transactions. */
async function prepareAndFraud(env: Env, eq: 1 | 2, L: Point, M: Point) {
  const prep = await prepareTx(env, eq, L, M);
  await send(env, prep);
  const fr = await fraudTx(env, [utxoAt(prep, 1), utxoAt(prep, 2)]);
  return { prep, fr };
}

// ---- tests ------------------------------------------------------------------------------------

describe('gate d: referee-created checkers and fraud proofs', () => {
  beforeAll(() => {
    execFileSync('node', [join(contractsRoot, 'compile.mjs'), 'WitnessEcMulCheck', 'GateReferee'], { stdio: 'inherit' });
    chkArt = loadArtifact('WitnessEcMulCheck');
    refArt = loadArtifact('GateReferee');
    body = bodyOf(chkArt);
    console.log(`GateReferee script: ${refArt.script.length / 2} bytes (template); checker body ${body.length / 2} bytes`);
  }, 600_000);

  it('the off-chain splice equals the SDK-built checker script for every k shape the referee pushes', () => {
    const X = cardPoint(3);
    for (const k of [1n, 16n, 17n, 127n, 128n, 0x8000n, 0x1234567890abcdefn, N - 1n]) {
      expect(chkScript(X, k, mul(X, k))).toBe(new RunarContract(chkArt, [aff(X), k, aff(mul(X, k))]).getLockingScript());
      expect(chkScript(G, k, mul(G, k))).toBe(new RunarContract(chkArt, [aff(G), k, aff(mul(G, k))]).getLockingScript());
    }
  });

  it('the on-chain challenge equals challenge() for the pinned vector', async () => {
    const r = honest();
    // The pinned vector of packages/protocol/test/dleq.test.ts.
    expect(cOf(r).toString(16).padStart(64, '0')).toBe('be78eefbc4e16758becd5923cfbfac786a332434dc7cb0da2f7cbbfc7f156a0e');
    expect(r.z.toString(16).padStart(64, '0')).toBe('17cc927f92e031b8b915e369aad24affdc870b93ac6f7d6a948d4b37e37343c3');
    const env = await setup(r);
    const { L, M } = products(r, 1);
    const prep = await prepareTx(env, 1, L, M);
    await send(env, prep); // the referee built output 2 with ITS c; hashOutputs only matches if it equals challenge()
    expect(prep.outputs[2]!.lockingScript.toHex())
      .toBe(new RunarContract(chkArt, [aff(r.st.P1), cOf(r), aff(M)]).getLockingScript());
    expect(prep.outputs[1]!.lockingScript.toHex())
      .toBe(new RunarContract(chkArt, [aff(G), r.z, aff(L)]).getLockingScript());
    const st = stateOf(env.ref);
    expect(st.phase).toBe(1n);
    expect(st.eq).toBe(1n);
    expect(st.L).toBe(aff(L));
    expect(st.M).toBe(aff(M));
    expect(env.ref.satoshis).toBe(REF_SATS + BOND);
    console.log(`checker scripts as deployed: L ${prep.outputs[1]!.lockingScript.toBinary().length} B, ` +
      `M ${prep.outputs[2]!.lockingScript.toBinary().length} B`);
  }, 600_000);

  it('proves fraud against a recorded invalid proof (eq 1, tampered z) and returns the bond', async () => {
    const env = await setup(tamperedZ(honest()));
    const { L, M } = products(env.rec, 1);
    const { prep, fr } = await prepareAndFraud(env, 1, L, M);
    await send(env, fr);
    expect(fr.outputs[0]!.satoshis).toBe(REF_SATS);
    expect(fr.outputs[1]!.satoshis).toBe(BOND);
    expect(fr.outputs[1]!.lockingScript.toHex()).toBe(p2pkh(REFUND_PKH));
    expect(stateOf(env.ref).phase).toBe(2n);
    console.log(`eq 1: prepare ${size(prep)} B (${prep.inputs.length} in, ${prep.outputs.length} out; ` +
      `referee unlock ${unlockSize(prep)} B), fraud ${size(fr)} B (${fr.inputs.length} in, ${fr.outputs.length} out; ` +
      `referee unlock ${unlockSize(fr)} B), total ${size(prep) + size(fr)} B`);
  }, 600_000);

  it('proves fraud against a recorded invalid proof (eq 2, tampered z) and returns the bond', async () => {
    const env = await setup(tamperedZ(honest()));
    const { L, M } = products(env.rec, 2);
    const { prep, fr } = await prepareAndFraud(env, 2, L, M);
    await send(env, fr);
    expect(fr.outputs[0]!.satoshis).toBe(REF_SATS);
    expect(fr.outputs[1]!.satoshis).toBe(BOND);
    expect(stateOf(env.ref).phase).toBe(2n);
    console.log(`eq 2: prepare ${size(prep)} B (referee unlock ${unlockSize(prep)} B), fraud ${size(fr)} B ` +
      `(referee unlock ${unlockSize(fr)} B), total ${size(prep) + size(fr)} B; ` +
      `referee locking script ${env.ref.script.length / 2} B`);
  }, 600_000);

  it('a wrong P2 is convicted on eq 2, and eq 1 (which is true) cannot convict it', async () => {
    const bad = wrongP2();
    {
      const env = await setup(bad);
      const { L, M } = products(bad, 2);
      const { fr } = await prepareAndFraud(env, 2, L, M);
      await send(env, fr);
      expect(stateOf(env.ref).phase).toBe(2n);
    }
    {
      const env = await setup(bad);
      const { L, M } = products(bad, 1);
      const { fr } = await prepareAndFraud(env, 1, L, M);
      await expect(env.provider.broadcast(fr)).rejects.toThrow(REJECTED(0));
    }
  }, 600_000);

  it('the on-chain push of z matches the SDK for every encoding shape, and each such fraud convicts', async () => {
    // A cheater chooses z, so every Script-number shape must splice exactly as the SDK does, or a
    // well-chosen z would make the checker unbuildable and the lie unprovable.
    const shapes = [1n, 16n, 17n, 127n, 128n, 255n, 0x7fffn, 0x8000n, 1n << 255n, N - 1n];
    for (const z of shapes) {
      const env = await setup({ ...honest(), z });
      const { L, M } = products(env.rec, 1);
      const { prep, fr } = await prepareAndFraud(env, 1, L, M); // prepare fails unless the splice matches
      expect(prep.outputs[1]!.lockingScript.toHex()).toBe(new RunarContract(chkArt, [aff(G), z, aff(L)]).getLockingScript());
      await send(env, fr);
      expect(stateOf(env.ref).phase).toBe(2n);
    }
  }, 600_000);

  for (const eq of [1, 2] as const) {
    it(`refuses fraud against an honest proof (eq ${eq}): prepare succeeds, fraud does not`, async () => {
      const env = await setup(honest());
      const { L, M } = products(env.rec, eq);
      const { fr } = await prepareAndFraud(env, eq, L, M); // prepare broadcast: the statements are true
      expect(stateOf(env.ref).phase).toBe(1n);
      await expect(env.provider.broadcast(fr)).rejects.toThrow(REJECTED(0));
    }, 600_000);
  }

  it('an honest proof cannot be convicted with a false product: the checker cannot be spent', async () => {
    // L' != z·G and M' != c·P1, each chosen so that L != A1 + M and the referee input alone passes.
    const r = honest();
    const { L, M } = products(r, 1);
    const lies: Array<[Point, Point, number]> = [
      [mul(G, r.z + 1n), M, 1], // a false L: checker 1 refuses the true product's witness
      [L, mul(r.st.P1, cOf(r) + 1n), 2], // a false M: checker 2 refuses likewise
    ];
    for (const [lieL, lieM, bad] of lies) {
      const env = await setup(r);
      const { prep, fr } = await prepareAndFraud(env, 1, lieL, lieM); // prepare does not check products
      // The referee's own input accepts (L != A + M), so only the checker stands in the way.
      expect(dryRunMultiContractInput(fr.toHex(), 0, env.ref.script, env.ref.satoshis)).toEqual({ valid: true });
      const other = bad === 1 ? 2 : 1;
      expect(dryRunMultiContractInput(fr.toHex(), other, prep.outputs[other]!.lockingScript.toHex(), CHECKER_SATS))
        .toEqual({ valid: true });
      await expect(env.provider.broadcast(fr)).rejects.toThrow(REJECTED(bad));
    }
  }, 600_000);

  it('a Point argument stretched past 64 bytes can forge neither a checker nor the stored state', async () => {
    // The attack (review, fix round 1): an 81-byte lPt. The checker's `0x40` pushes the first 64
    // bytes as `out`, then RUNS the last 17 (OP_2DROP OP_1 OP_1 OP_NOP x14) before its final
    // OP_EQUAL, so it passes for any L. The same 17 bytes stretch the stored state: Rúnar reads a
    // fixed-width state section from the END of the scriptCode and checks only that it is 165
    // bytes and starts 0x6a. So the fraud caller claims a _codePart 17 bytes longer, and the
    // referee reads phase 1, the eq, and a garbage L out of lPt's own bytes.
    const le8 = (n: number) => n.toString(16).padStart(2, '0') + '00'.repeat(7);
    const tailOps = '6d5151' + '61'.repeat(14);
    for (const eq of [1, 2] as const) {
      const r = honest();
      const env = await setup(r);
      const deployed = new RunarContract(refArt, refereeArgs(r));
      const { L, M } = products(r, eq);
      const lHex = '6a' + le8(1) + le8(eq) + '11'.repeat(47) + tailOps;
      expect(lHex.length / 2).toBe(81);
      const prep = await prepareTx(env, eq, lHex, M);
      const accepted = await env.provider.broadcast(prep).then(
        () => true,
        (e: unknown) => {
          expect(String(e)).toMatch(REJECTED(0));
          return false;
        },
      );
      if (accepted) {
        // Carry the attack through, to show what an unchecked width buys.
        env.ref = utxoAt(prep, 0);
        const codePart = deployed.getCodePartHex() + '6a' + le8(1) + le8(eq);
        const garbageL = lHex.slice(17 * 2);
        const contScript = codePart + '6a' +
          serializeState(refArt.stateFields!, { phase: 2n, eq: BigInt(eq), L: garbageL, M: aff(M), bondPKH: REFUND_PKH });
        const fr = await callReferee(env, {
          method: 'fraud',
          args: (fo) => [fo],
          extraInputs: [utxoAt(prep, 1), utxoAt(prep, 2)].map((u) => ({ utxo: u, unlock: checkerUnlock(u.script) })),
          outputs: [{ script: p2pkh(REFUND_PKH), satoshis: BOND }],
          next: { satoshis: env.ref.satoshis - BOND, state: {} },
          contract: deployed, codePart, contScript,
        });
        await env.provider.broadcast(fr);
        throw new Error(`eq ${eq}: an HONEST proof was convicted and the bond refunded (fraud ${fr.id('hex')})`);
      }
      // A stretched M is refused the same way.
      const mHex = aff(M) + tailOps;
      await expect(env.provider.broadcast(await prepareTx(env, eq, L, mHex))).rejects.toThrow(REJECTED(0));
      // So is a short one.
      await expect(env.provider.broadcast(await prepareTx(env, eq, L, aff(M).slice(0, -2)))).rejects.toThrow(REJECTED(0));
    }
  }, 600_000);

  it('fraud requires the referee to be output 0: a phase-1 referee at output 3 is refused', async () => {
    // Only a deployer can make this (prepare always continues at output 0). Inputs are
    // (T:3, T:1, T:2, fee), so hashPrevouts, the phase, the checkers and L != A + M all hold:
    // only the vout-0 assert refuses it.
    const r = tamperedZ(honest());
    const env = await setup(r);
    const { L, M } = products(r, 1);
    const forged = new RunarContract(refArt, refereeArgs(r));
    forged.setState({ phase: 1n, eq: 1n, L: aff(L), M: aff(M), bondPKH: REFUND_PKH });
    const T = await handTx(env, [
      { script: p2pkh(REFUND_PKH), satoshis: 1 },
      { script: chkScript(G, r.z, L), satoshis: CHECKER_SATS },
      { script: chkScript(r.st.P1, cOf(r), M), satoshis: CHECKER_SATS },
      { script: forged.getLockingScript(), satoshis: REF_SATS + BOND },
    ]);
    env.ref = utxoAt(T, 3);
    const fr = await fraudTx(env, [utxoAt(T, 1), utxoAt(T, 2)]);
    for (const i of [1, 2]) expect(dryRunMultiContractInput(fr.toHex(), i, T.outputs[i]!.lockingScript.toHex(), CHECKER_SATS)).toEqual({ valid: true });
    await expect(env.provider.broadcast(fr)).rejects.toThrow(REJECTED(0));
  }, 600_000);

  it('refuses checkers not created by this referee', async () => {
    const env = await setup(tamperedZ(honest()));
    const { L, M } = products(env.rec, 1);
    const prep = await prepareTx(env, 1, L, M);
    await send(env, prep);

    // Byte-identical, spendable checkers, created by an unrelated transaction.
    const dup = await handTx(env, [
      { script: p2pkh(REFUND_PKH), satoshis: 1 },
      { script: prep.outputs[1]!.lockingScript.toHex(), satoshis: CHECKER_SATS },
      { script: prep.outputs[2]!.lockingScript.toHex(), satoshis: CHECKER_SATS },
    ]);

    for (const pair of [
      [utxoAt(dup, 1), utxoAt(dup, 2)],
      [utxoAt(dup, 1), utxoAt(prep, 2)],
      [utxoAt(prep, 2), utxoAt(prep, 1)], // the right checkers, swapped
    ] as Array<[UTXO, UTXO]>) {
      const fr = await fraudTx(env, pair);
      for (const i of [1, 2]) {
        expect(dryRunMultiContractInput(fr.toHex(), i, pair[i - 1]!.script, CHECKER_SATS)).toEqual({ valid: true });
      }
      await expect(env.provider.broadcast(fr)).rejects.toThrow(REJECTED(0));
    }
    // Control: the referee's own checkers convict.
    await send(env, await fraudTx(env, [utxoAt(prep, 1), utxoAt(prep, 2)]));
    expect(stateOf(env.ref).phase).toBe(2n);
  }, 600_000);

  it('refuses a prepare with a forged checker body: a flipped byte, one byte short, one byte long', async () => {
    const env = await setup(tamperedZ(honest()));
    const flip = (hex: string, at: number) =>
      hex.slice(0, at * 2) + (Number.parseInt(hex.slice(at * 2, at * 2 + 2), 16) ^ 0x01).toString(16).padStart(2, '0') + hex.slice(at * 2 + 2);
    const forged: Array<[string, 1 | 2, string]> = [
      ['a flipped byte', 1, flip(body, 1000)],
      ['a flipped byte (eq 2)', 2, flip(body, 30_000)],
      ['one byte short', 1, body.slice(0, -2)],
      ['one byte long', 2, body + '61'],
    ];
    for (const [why, eq, t] of forged) {
      const { L, M } = products(env.rec, eq);
      // The outputs are what the referee itself would splice from these segments, so only the
      // template binding can refuse.
      const tx = await prepareTx(env, eq, L, M, t);
      await expect(env.provider.broadcast(tx), why).rejects.toThrow(REJECTED(0));
    }
    // Control: the genuine template prepares.
    const { L, M } = products(env.rec, 1);
    await send(env, await prepareTx(env, 1, L, M));
    expect(stateOf(env.ref).phase).toBe(1n);
  }, 600_000);

  it('fraud in phase 0 is refused by the phase guard alone', async () => {
    // A phase-0 referee deployed as output 0 beside two genuine, spendable checkers, with the true
    // products and the refund PKH already in its state: hashPrevouts, vout 0, the checkers,
    // L != A + M and the bond output all hold, so only the phase guard refuses.
    const r = tamperedZ(honest());
    const env = await setup(r);
    const { L, M } = products(r, 1);
    const D = await handTx(env, [
      { script: new RunarContract(refArt, refereeArgs(r, aff(L), aff(M), REFUND_PKH)).getLockingScript(), satoshis: REF_SATS },
      { script: chkScript(G, r.z, L), satoshis: CHECKER_SATS },
      { script: chkScript(r.st.P1, cOf(r), M), satoshis: CHECKER_SATS },
    ]);
    env.ref = utxoAt(D, 0);
    expect(stateOf(env.ref)).toMatchObject({ phase: 0n, L: aff(L), M: aff(M), bondPKH: REFUND_PKH });
    const fr = await fraudTx(env, [utxoAt(D, 1), utxoAt(D, 2)]);
    for (const i of [1, 2]) expect(dryRunMultiContractInput(fr.toHex(), i, D.outputs[i]!.lockingScript.toHex(), CHECKER_SATS)).toEqual({ valid: true });
    await expect(env.provider.broadcast(fr)).rejects.toThrow(REJECTED(0));
  }, 600_000);

  it('requires each phase: no second prepare, no prepare after fraud', async () => {
    const env = await setup(tamperedZ(honest()));
    const { L, M } = products(env.rec, 1);
    const deployed = env.ref;

    // Phase 1: a second prepare is refused.
    const prep = await prepareTx(env, 1, L, M);
    await send(env, prep);
    await expect(env.provider.broadcast(await prepareTx(env, 1, L, M))).rejects.toThrow(REJECTED(0));

    // Phase 2: done.
    await send(env, await fraudTx(env, [utxoAt(prep, 1), utxoAt(prep, 2)]));
    expect(stateOf(env.ref).phase).toBe(2n);
    await expect(env.provider.broadcast(await prepareTx(env, 1, L, M))).rejects.toThrow(REJECTED(0));
    expect(deployed.txid).not.toBe(env.ref.txid);
  }, 600_000);
});
