import { describe, it, expect, beforeAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { PrivateKey, BigNumber, Transaction } from '@bsv/sdk';
import type { Point } from '@bsv/sdk';
import { RunarContract, MockProvider, LocalSigner, buildP2PKHScript } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { contractsRoot, loadArtifact } from './helpers';

const N = new BigNumber('FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141', 16);
const G = new PrivateKey(1).toPublicKey() as unknown as Point;
const X = G.mul(new BigNumber(0x5eed)); // an arbitrary non-generator base

/** 64-byte affine encoding, x || y big-endian: how a Rúnar `Point` is passed and pushed. */
const raw = (p: Point) => p.getX().toHex(32) + p.getY().toHex(32);
const mul = (p: Point, k: bigint) => p.mul(new BigNumber(k.toString(16), 16));

const K = 0x1234567890abcdef1234567890abcdefn;

async function lock(artifact: RunarArtifact, args: unknown[]) {
  const provider = new MockProvider('testnet'); // validateBroadcasts: every spend runs through @bsv/sdk Spend
  const signer = new LocalSigner('99'.repeat(32));
  provider.addUtxo(await signer.getAddress(), {
    txid: '99'.repeat(32), outputIndex: 0, satoshis: 50_000_000,
    script: buildP2PKHScript(await signer.getPublicKey()),
  });
  const contract = new RunarContract(artifact, args);
  await contract.deploy(provider, signer, { satoshis: 1000 });
  return { provider, signer, contract };
}

async function spend(artifact: RunarArtifact, args: unknown[]) {
  const { provider, signer, contract } = await lock(artifact, args);
  await contract.call('verify', [], provider, signer, {});
  return provider;
}

const REJECTED = /input 0: Script evaluation error/;

describe('EcMulCheck and EcMulGenCheck (one multiplication each)', () => {
  let mulArt: RunarArtifact;
  let genArt: RunarArtifact;

  beforeAll(() => {
    // One 429 KB compile per variant per file.
    execFileSync('node', [join(contractsRoot, 'compile.mjs'), 'EcMulCheck', 'EcMulGenCheck'], { stdio: 'inherit' });
    mulArt = loadArtifact('EcMulCheck');
    genArt = loadArtifact('EcMulGenCheck');
  }, 600_000);

  it('prints both script sizes', () => {
    console.log(`EcMulCheck script template: ${mulArt.script.length / 2} bytes`);
    console.log(`EcMulGenCheck script template: ${genArt.script.length / 2} bytes`);
    for (const a of [mulArt, genArt]) expect(a.script.length / 2).toBeGreaterThan(100_000);
  });

  it('EcMulCheck: an honest statement (X, k, k*X) is spendable', async () => {
    const provider = await spend(mulArt, [raw(X), K, raw(mul(X, K))]);
    expect(provider.getBroadcastedTxs()).toHaveLength(2); // the lock and the spend
  }, 600_000);

  it('EcMulCheck: (X, k, (k+1)*X) is rejected', async () => {
    await expect(spend(mulArt, [raw(X), K, raw(mul(X, K + 1n))])).rejects.toThrow(REJECTED);
  }, 600_000);

  it('EcMulCheck: k = 0 is rejected', async () => {
    await expect(spend(mulArt, [raw(X), 0n, raw(X)])).rejects.toThrow(REJECTED);
  }, 600_000);

  it('EcMulCheck: a base that is not on the curve is rejected', async () => {
    const bad = X.getX().toHex(32) + X.getY().add(new BigNumber(1)).toHex(32); // (x, y+1)
    await expect(spend(mulArt, [bad, K, raw(mul(X, K))])).rejects.toThrow(REJECTED);
  }, 600_000);

  it('EcMulCheck: k = n (a multiple of the order) is rejected', async () => {
    await expect(spend(mulArt, [raw(X), BigInt('0x' + N.toHex()), raw(X)])).rejects.toThrow(REJECTED);
  }, 600_000);

  it('EcMulGenCheck: an honest statement (k, k*G) is spendable', async () => {
    const provider = await spend(genArt, [K, raw(mul(G, K))]);
    expect(provider.getBroadcastedTxs()).toHaveLength(2);
  }, 600_000);

  it('EcMulGenCheck: (k, (k+1)*G) is rejected', async () => {
    await expect(spend(genArt, [K, raw(mul(G, K + 1n))])).rejects.toThrow(REJECTED);
  }, 600_000);

  it('EcMulGenCheck: k = 0 is rejected', async () => {
    await expect(spend(genArt, [0n, raw(G)])).rejects.toThrow(REJECTED);
  }, 600_000);

  it('EcMulGenCheck: k = n is rejected', async () => {
    await expect(spend(genArt, [BigInt('0x' + N.toHex()), raw(G)])).rejects.toThrow(REJECTED);
  }, 600_000);

  // ---- locking-script layout, for Task 5 to rebuild inside a covenant ----

  /** A Point push: one direct-push opcode 0x40 (64) then x || y. Not the 33-byte compressed form. */
  const pushPoint = (hex: string) => '40' + hex;

  /** Minimal Script number push, written independently of the SDK's encodeArg. */
  function pushScriptNum(n: bigint): string {
    if (n === 0n) return '00';
    if (n >= 1n && n <= 16n) return (0x50 + Number(n)).toString(16);
    const bytes: number[] = [];
    for (let a = n; a > 0n; a >>= 8n) bytes.push(Number(a & 0xffn));
    if (bytes[bytes.length - 1]! & 0x80) bytes.push(0x00); // keep the value positive
    return bytes.length.toString(16).padStart(2, '0') + Buffer.from(bytes).toString('hex');
  }

  /**
   * template with each constructor slot's single OP_0 placeholder byte (at slot.byteOffset, in
   * template coordinates) replaced by the slot's push, in one left-to-right pass.
   */
  function rebuild(artifact: RunarArtifact, pushes: string[]): string {
    const slots = [...artifact.constructorSlots!].sort((a, b) => a.byteOffset - b.byteOffset);
    let out = '';
    let pos = 0; // byte position in the template
    for (const slot of slots) {
      expect(artifact.script.slice(slot.byteOffset * 2, slot.byteOffset * 2 + 2)).toBe('00'); // OP_0 placeholder
      out += artifact.script.slice(pos * 2, slot.byteOffset * 2) + pushes[slot.paramIndex]!;
      pos = slot.byteOffset + 1;
    }
    return out + artifact.script.slice(pos * 2);
  }

  it('layout: the slot table is the one Task 5 documents', () => {
    const table = (a: RunarArtifact) =>
      a.constructorSlots!.map((s) => `${s.name}@${s.byteOffset}`).join(' ');
    expect(table(mulArt)).toBe('base@0 k@842 base@848 k@849 out@427040');
    expect(table(genArt)).toBe('k@34 k@40 out@426297');
    expect(mulArt.script.length / 2).toBe(427042);
    expect(genArt.script.length / 2).toBe(426299);
    expect(mulArt.stateFields ?? []).toEqual([]); // stateless: no OP_RETURN, no state suffix
    expect(genArt.stateFields ?? []).toEqual([]);
    expect(mulArt.codeSepIndexSlots ?? []).toEqual([]);
    expect(genArt.codeSepIndexSlots ?? []).toEqual([]);
  });

  it('layout: a hand-built locking script equals what the SDK deploys', async () => {
    // EcMulCheck: slots are base@0 k@842 base@848 k@849 out@427040 (template bytes), so the script is
    //   push(base) || T[1..842) || push(k) || T[843..848) || push(base) || push(k)
    //   || T[850..427040) || push(out) || T[427041..427042)
    const outM = raw(mul(X, K));
    const handMul = rebuild(mulArt, [pushPoint(raw(X)), pushScriptNum(K), pushPoint(outM)]);
    const locked = await lock(mulArt, [raw(X), K, outM]);
    const lockTx = Transaction.fromHex(locked.provider.getBroadcastedTxs()[0]!);
    expect(lockTx.outputs[0]!.lockingScript.toHex()).toBe(handMul);
    expect(locked.contract.getLockingScript()).toBe(handMul);
    expect(handMul.length / 2).toBe(427042 - 5 + 3 * 65 + 2 * (pushScriptNum(K).length / 2)); // 5 one-byte placeholders replaced

    // EcMulGenCheck: slots are k@34 k@40 out@426297.
    const outG = raw(mul(G, K));
    const handGen = rebuild(genArt, [pushScriptNum(K), pushPoint(outG)]);
    const lockedGen = await lock(genArt, [K, outG]);
    const lockGenTx = Transaction.fromHex(lockedGen.provider.getBroadcastedTxs()[0]!);
    expect(lockGenTx.outputs[0]!.lockingScript.toHex()).toBe(handGen);
    expect(lockedGen.contract.getLockingScript()).toBe(handGen);
  }, 600_000);

  it('layout: the hand-built script matches the SDK for every shape of k', () => {
    const nMinus1 = BigInt('0x' + N.toHex()) - 1n; // top bit set: a 33-byte push with a 0x00 pad
    const ks = [0n, 1n, 7n, 16n, 17n, 127n, 128n, 255n, 256n, 0x7fffn, 0x8000n, K, nMinus1];
    for (const k of ks) {
      const o = raw(mul(G, k === 0n ? 1n : k));
      const hand = rebuild(genArt, [pushScriptNum(k), pushPoint(o)]);
      expect(new RunarContract(genArt, [k, o]).getLockingScript()).toBe(hand);
      const o2 = raw(mul(X, k === 0n ? 1n : k));
      const hand2 = rebuild(mulArt, [pushPoint(raw(X)), pushScriptNum(k), pushPoint(o2)]);
      expect(new RunarContract(mulArt, [raw(X), k, o2]).getLockingScript()).toBe(hand2);
    }
  });
});
