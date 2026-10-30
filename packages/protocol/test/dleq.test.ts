import { describe, expect, it } from 'vitest';
import { BigNumber, Curve, Hash, Point, Utils } from '@bsv/sdk';
import { cardPoint } from '../src/cards.js';
import { challenge, prove, statementBytes, statementCore, verify, type DleqStatement } from '../src/dleq.js';
import { toAffine64, toCompressed } from '../src/points.js';

const curve = new Curve();
const G = curve.g as Point;
const n: BigNumber = curve.n;
const secret = new Uint8Array(32).fill(7);
const hex = (b: Uint8Array) => Utils.toHex(Array.from(b));
const bn = (v: number) => new BigNumber(v);
const mul = (p: Point, k: BigNumber) => p.mul(k);

const s = new BigNumber('1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef', 16).umod(n);
const X = cardPoint(5);

const statement = (domain = 'test/v1'): DleqStatement => ({ domain, B1: G, P1: mul(G, s), B2: X, P2: mul(X, s) });

describe('dleq', () => {
  it('verifies an honest proof', () => {
    const st = statement();
    expect(verify(st, prove(st, s, secret))).toBe(true);
  });

  it('rejects an honest proof checked against a statement with another P2', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    expect(verify({ ...st, P2: mul(X, s.add(bn(1))) }, pr)).toBe(false);
  });

  it('prove throws when s is not the witness of the statement', () => {
    const st = statement();
    expect(() => prove(st, s.add(bn(1)).umod(n), secret)).toThrow();
    expect(() => prove({ ...st, P2: mul(X, s.add(bn(1))) }, s, secret)).toThrow(); // P2 off
    expect(() => prove({ ...st, P1: mul(G, s.add(bn(1))) }, s, secret)).toThrow(); // P1 off
    expect(() => prove(st, bn(0), secret)).toThrow();
    expect(() => prove(st, s.add(n), secret)).toThrow();
  });

  it('prove throws unless the nonce secret is exactly 32 bytes', () => {
    const st = statement();
    expect(() => prove(st, s, new Uint8Array(0))).toThrow();
    expect(() => prove(st, s, new Uint8Array(31).fill(7))).toThrow();
    expect(() => prove(st, s, new Uint8Array(33).fill(7))).toThrow();
    expect(verify(st, prove(st, s, new Uint8Array(32).fill(1)))).toBe(true);
  });

  it('rejects z + 1', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    expect(verify(st, { ...pr, z: pr.z.add(bn(1)).umod(n) })).toBe(false);
  });

  it('returns false, not a throw, for z outside [0, n)', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    expect(verify(st, { ...pr, z: pr.z.add(n) })).toBe(false);
    expect(verify(st, { ...pr, z: new BigNumber(-1) })).toBe(false);
  });

  it('returns false, not a throw, for points at infinity', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    const inf = new Point(null, null);
    expect(verify(st, { ...pr, A1: inf })).toBe(false);
    expect(verify(st, { ...pr, A2: inf })).toBe(false);
    for (const key of ['B1', 'P1', 'B2', 'P2'] as const) {
      expect(verify({ ...st, [key]: inf }, pr)).toBe(false);
    }
  });

  it('returns false, not a throw, for points that are not on the curve', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    const off = new Point(G.getX(), G.getY().add(bn(1)).umod(curve.p));
    expect(off.validate()).toBe(false);
    expect(verify(st, { ...pr, A1: off })).toBe(false);
    expect(verify(st, { ...pr, A2: off })).toBe(false);
    for (const key of ['B1', 'P1', 'B2', 'P2'] as const) {
      expect(verify({ ...st, [key]: off }, pr)).toBe(false);
    }
  });

  it('is deterministic', () => {
    const st = statement();
    const a = prove(st, s, secret);
    const b = prove(st, s, secret);
    expect(hex(toCompressed(a.A1))).toBe(hex(toCompressed(b.A1)));
    expect(hex(toCompressed(a.A2))).toBe(hex(toCompressed(b.A2)));
    expect(a.z.toHex(32)).toBe(b.z.toHex(32));
  });

  it('derives the nonce from the statement: a different domain gives a different A1', () => {
    const a = prove(statement('domain-a'), s, secret);
    const b = prove(statement('domain-b'), s, secret);
    expect(hex(toCompressed(a.A1))).not.toBe(hex(toCompressed(b.A1)));
  });

  it('a different nonce secret gives a different A1', () => {
    const st = statement();
    const a = prove(st, s, secret);
    const b = prove(st, s, new Uint8Array(32).fill(8));
    expect(hex(toCompressed(a.A1))).not.toBe(hex(toCompressed(b.A1)));
  });

  it('a proof does not verify under another domain', () => {
    const pr = prove(statement('domain-a'), s, secret);
    expect(verify(statement('domain-b'), pr)).toBe(false);
  });

  it('encodes the statement as domain || 0x00 || 64-byte affine points', () => {
    const st = statement('abc');
    const pr = prove(st, s, secret);
    const bytes = statementBytes(st, pr.A1, pr.A2);
    const core = statementCore(st);
    expect(bytes.length).toBe(3 + 1 + 6 * 64);
    expect(core.length).toBe(3 + 1 + 4 * 64);
    expect(hex(bytes.subarray(0, 4))).toBe('61626300');
    expect(hex(bytes.subarray(0, core.length))).toBe(hex(core));
    const expected = [st.B1, st.P1, st.B2, st.P2, pr.A1, pr.A2].map((p) => hex(toAffine64(p))).join('');
    expect(hex(bytes.subarray(4))).toBe(expected);
  });

  it('challenge differs when a point is encoded 33-byte instead of 64-byte', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    const wide = statementBytes(st, pr.A1, pr.A2);
    const dom = Utils.toArray(st.domain, 'utf8').length + 1;
    const narrow = Uint8Array.from([
      ...wide.subarray(0, dom),
      ...[st.B1, st.P1, st.B2, st.P2, pr.A1, pr.A2].flatMap((p) => Array.from(toCompressed(p))),
    ]);
    expect(narrow.length).not.toBe(wide.length);
    expect(challenge(narrow).cmp(challenge(wide))).not.toBe(0);
  });

  it('challenge is SHA-256 mod n, big-endian', () => {
    const c = challenge(Uint8Array.from([1, 2, 3]));
    expect(c.toHex(32)).toBe('039058c6f2c0cb492c533b0a4d14ef77cc0f78abccced5287d84a1a2011cfb81');
    expect(c.cmp(n)).toBeLessThan(0);
  });

  it('matches the pinned vector Task 5 recomputes on chain', () => {
    const st: DleqStatement = {
      domain: 'brc100-poker/test/v1',
      B1: G,
      P1: mul(G, bn(5)),
      B2: cardPoint(0),
      P2: mul(cardPoint(0), bn(5)),
    };
    const pr = prove(st, bn(5), secret);
    const bytes = statementBytes(st, pr.A1, pr.A2);

    // The nonce, computed independently of dleq.ts.
    const w = new BigNumber(
      Hash.sha256hmac(Array.from(secret), [
        ...Utils.toArray('nonce', 'utf8'),
        ...Hash.sha256(Array.from(statementCore(st))),
      ]),
    ).umod(n);
    expect(hex(toAffine64(pr.A1))).toBe(hex(toAffine64(mul(G, w))));
    expect(hex(toAffine64(pr.A2))).toBe(hex(toAffine64(mul(cardPoint(0), w))));

    expect(hex(bytes)).toBe(
      '6272633130302d706f6b65722f746573742f763100' +
        '79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8' +
        '2f8bde4d1a07209355b4a7250a5c5128e88b84bddc619ab7cba8d569b240efe4d8ac222636e5e3d6d4dba9dda6c9c426f788271bab0d6840dca87d3aa6ac62d6' +
        '1738215e26042202a099ecc26fe754243449ad80eb1b4fc83831fc77a5ebcc4c4a2973dec482348c75d81e6552f02aa8e9fa525805fdf55e72c6677f957feb4e' +
        'c4eba27f0ed4419711e984b4b575ba7f5ea85204a9a527865191c3927913bb1bda4a70f00d54137fadcb72a879f72408d9da53869c523c475b0a48c0c8f27492' +
        '3ca0f7cbfd36fbd5c3727d8117793e120865a1ed78d749b4bf2bbe1272fef1b99e3deae0024674ba0ccf5f0229eab3881ccb1ff2edba7d8c45432c4058856836' +
        '8747b1b3b9aaacbdff3acbc4da76ae59474e69073fa297fff9cc04390fb55d9bab4a8ae0556933bb083d32ede82a854a9069199a2420619c134938a9517a9bfc',
    );
    expect(challenge(bytes).toHex(32)).toBe('be78eefbc4e16758becd5923cfbfac786a332434dc7cb0da2f7cbbfc7f156a0e');
    expect(hex(toAffine64(pr.A1))).toBe(
      '3ca0f7cbfd36fbd5c3727d8117793e120865a1ed78d749b4bf2bbe1272fef1b99e3deae0024674ba0ccf5f0229eab3881ccb1ff2edba7d8c45432c4058856836',
    );
    expect(hex(toAffine64(pr.A2))).toBe(
      '8747b1b3b9aaacbdff3acbc4da76ae59474e69073fa297fff9cc04390fb55d9bab4a8ae0556933bb083d32ede82a854a9069199a2420619c134938a9517a9bfc',
    );
    expect(pr.z.toHex(32)).toBe('17cc927f92e031b8b915e369aad24affdc870b93ac6f7d6a948d4b37e37343c3');
    expect(verify(st, pr)).toBe(true);
  });

  it('verifies the spec 7.3 step 2 remask pair', () => {
    const k = new BigNumber('00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee', 16).umod(n);
    const r = new BigNumber('0badc0de0badc0de0badc0de0badc0de0badc0de0badc0de0badc0de0badc0de', 16).umod(n);
    const Xp = cardPoint(17);
    const sp = r.mul(k.invm(n)).umod(n);
    const Sp = mul(G, sp);
    const Pp = mul(G, r);
    const Yp = mul(Xp, sp);
    const Kused = mul(G, k);

    expect(hex(toCompressed(Pp))).toBe(hex(toCompressed(mul(Sp, k))));

    const stI: DleqStatement = { domain: 'brc100-poker/remask-i/v1', B1: G, P1: Sp, B2: Xp, P2: Yp };
    const stII: DleqStatement = { domain: 'brc100-poker/remask-ii/v1', B1: G, P1: Kused, B2: Sp, P2: Pp };
    const prI = prove(stI, sp, secret);
    const prII = prove(stII, k, secret);
    expect(verify(stI, prI)).toBe(true);
    expect(verify(stII, prII)).toBe(true);
    expect(verify(stI, prII)).toBe(false);
    expect(verify(stII, prI)).toBe(false);
  });
});
