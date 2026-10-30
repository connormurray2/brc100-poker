import { describe, expect, it } from 'vitest';
import { BigNumber, Curve, type Point } from '@bsv/sdk';
import { EC_MUL_WITNESS_BYTES, cardPoint, ecMulWitness } from '../src/index.js';

const curve = new Curve();
const G = curve.g as Point;
const N = BigInt('0x' + curve.n.toHex());

describe('ecMulWitness', () => {
  it('is the checker\'s fixed length for every k shape', () => {
    for (const k of [1n, 2n, 0x8000n, N - 1n]) expect(ecMulWitness(cardPoint(3), k).length / 2).toBe(EC_MUL_WITNESS_BYTES);
    expect(EC_MUL_WITNESS_BYTES).toBe(16_929);
  });

  it('refuses k = 0 and k = n', () => {
    expect(() => ecMulWitness(G, 0n)).toThrow(RangeError);
    expect(() => ecMulWitness(G, N)).toThrow(RangeError);
  });

  it('pins the final slope to the true product: a different k gives a different final slope', () => {
    const w1 = ecMulWitness(G, 12345n);
    const w2 = ecMulWitness(G, 12346n);
    expect(w1.slice(-66)).not.toBe(w2.slice(-66));
    expect(G.mul(new BigNumber(12345)).eq(G.mul(new BigNumber(12346)))).toBe(false);
  });
});
