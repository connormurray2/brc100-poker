import { describe, expect, it } from 'vitest';
import { BigNumber, Curve, Point, Utils } from '@bsv/sdk';
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

  it('rejects a statement whose P2 is for a different exponent', () => {
    const st = statement();
    const pr = prove(st, s, secret);
    expect(verify({ ...st, P2: mul(X, s.add(bn(1))) }, pr)).toBe(false);
  });

  it('rejects a prover who uses the wrong witness', () => {
    const st = statement();
    expect(verify(st, prove(st, s.add(bn(1)).umod(n), secret))).toBe(false);
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
    expect(verify({ ...st, P1: inf }, pr)).toBe(false);
    expect(verify({ ...st, B2: inf }, pr)).toBe(false);
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
