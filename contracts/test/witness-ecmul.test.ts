import { describe, it, expect, beforeAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { PrivateKey, BigNumber, Point } from '@bsv/sdk';
import { ecMulWitness } from '@brc100-poker/protocol';

import { RunarContract, MockProvider, LocalSigner, buildP2PKHScript } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { contractsRoot, loadArtifact } from './helpers';

const N = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
const P = 2n ** 256n - 2n ** 32n - 977n;
const md = (a: bigint) => ((a % P) + P) % P;
type Aff = [bigint, bigint];

const G = new PrivateKey(1).toPublicKey() as unknown as Point;
const ptOf = ([x, y]: Aff) => new Point(x.toString(16), y.toString(16));
const aff = (p: Point): Aff => [BigInt('0x' + p.getX().toHex(32)), BigInt('0x' + p.getY().toHex(32))];
const hex64 = ([x, y]: Aff) => x.toString(16).padStart(64, '0') + y.toString(16).padStart(64, '0');
/** The witness from @brc100-poker/protocol, and the true k·base from @bsv/sdk. */
const witness = (b: Aff, k: bigint) => ({ w: ecMulWitness(ptOf(b), k), out: aff(ptOf(b).mul(new BigNumber(k.toString(16), 16))) });
const X = aff(G.mul(new BigNumber(0x5eed)));

async function spend(artifact: RunarArtifact, base: Aff, k: bigint, out: Aff, w: string) {
  const provider = new MockProvider('testnet'); // validateBroadcasts: every spend runs through @bsv/sdk Spend
  const signer = new LocalSigner('99'.repeat(32));
  provider.addUtxo(await signer.getAddress(), {
    txid: '99'.repeat(32), outputIndex: 0, satoshis: 50_000_000, script: buildP2PKHScript(await signer.getPublicKey()),
  });
  const c = new RunarContract(artifact, [hex64(base), k, hex64(out)]);
  await c.deploy(provider, signer, { satoshis: 1000 });
  await c.call('verify', [w], provider, signer, {});
  return provider;
}

const REJECTED = /input 0: Script evaluation error/;
const K = 0x1234567890abcdef1234567890abcdefn;
const flip = (w: string, byte: number) => w.slice(0, 2 * byte) + (parseInt(w.slice(2 * byte, 2 * byte + 2), 16) ^ 1).toString(16).padStart(2, '0') + w.slice(2 * byte + 2);

describe('WitnessEcMulCheck (slopes supplied, checked mod p)', () => {
  let art: RunarArtifact;
  beforeAll(() => {
    execFileSync('node', [join(contractsRoot, 'compile.mjs'), 'WitnessEcMulCheck'], { stdio: 'inherit' });
    art = loadArtifact('WitnessEcMulCheck');
  }, 600_000);

  it('is far smaller than EcMulCheck (427,042 B)', () => {
    console.log(`WitnessEcMulCheck script template: ${art.script.length / 2} bytes; witness ${witness(X, K).w.length / 2} bytes`);
    expect(art.script.length / 2).toBeLessThan(100_000);
  });

  it('accepts honest statements: arbitrary base, the generator, k = 1, k = n − 1', async () => {
    for (const [b, k] of [[X, K], [aff(G), K], [X, 1n], [X, N - 1n]] as Array<[Aff, bigint]>) {
      const { w, out } = witness(b, k);
      expect((await spend(art, b, k, out, w)).getBroadcastedTxs()).toHaveLength(2);
    }
  }, 600_000);

  it('accepts 8 random statements', async () => {
    for (let i = 0; i < 8; i++) {
      const k = BigInt('0x' + PrivateKey.fromRandom().toHex()) % N || 1n;
      const b = aff(G.mul(new BigNumber(PrivateKey.fromRandom().toHex(), 16)));
      const { w, out } = witness(b, k);
      await spend(art, b, k, out, w);
    }
  }, 600_000);

  it('rejects a wrong product: (k+1)·base and −(k·base)', async () => {
    const { w } = witness(X, K);
    const wrong = witness(X, K + 1n).out;
    await expect(spend(art, X, K, wrong, w)).rejects.toThrow(REJECTED);
    await expect(spend(art, X, K, wrong, witness(X, K + 1n).w)).rejects.toThrow(REJECTED);
    const { out } = witness(X, K);
    await expect(spend(art, X, K, [out[0], md(-out[1])], w)).rejects.toThrow(REJECTED);
  }, 600_000);

  it('rejects any single tampered slope: an add, a double, the final one', async () => {
    const { w, out } = witness(X, K); // K's bit 0 is 1, so iteration 0's add slope counts
    for (const byte of [0, 33, 66 * 100 + 33, 16896]) await expect(spend(art, X, K, out, flip(w, byte))).rejects.toThrow(REJECTED);
  }, 600_000);

  it('rejects a short or long witness, k = 0, k = n, and an off-curve base', async () => {
    const { w, out } = witness(X, K);
    await expect(spend(art, X, K, out, w.slice(0, -2))).rejects.toThrow(REJECTED);
    await expect(spend(art, X, K, out, w + '00')).rejects.toThrow(REJECTED);
    await expect(spend(art, X, 0n, out, w)).rejects.toThrow(REJECTED);
    await expect(spend(art, X, N, out, w)).rejects.toThrow(REJECTED);
    await expect(spend(art, [X[0], md(X[1] + 1n)], K, out, w)).rejects.toThrow(REJECTED);
  }, 600_000);
});
