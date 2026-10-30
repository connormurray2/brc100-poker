import { describe, expect, it } from 'vitest';
import { BigNumber, Curve, Point, Utils } from '@bsv/sdk';
import { PointError, fromAffine64, parsePoint, toAffine64, toCompressed } from '../src/points.js';

const G = new Curve().g as Point;
const hex = (b: Uint8Array) => Utils.toHex(Array.from(b));
const bytes = (h: string) => Uint8Array.from(Utils.toArray(h, 'hex'));

describe('parsePoint', () => {
  it('accepts the generator in both prefixes', () => {
    expect(parsePoint(toCompressed(G)).eq(G)).toBe(true);
    expect(parsePoint(toCompressed(G.neg())).eq(G.neg())).toBe(true);
  });

  it('rejects x >= p before the SDK can silently reduce it', () => {
    const b = bytes('02' + 'ff'.repeat(32));
    expect(() => parsePoint(b)).toThrow(PointError);
    expect(() => parsePoint(b)).toThrow(/x is not a canonical field element/);
  });

  it('rejects x == p exactly', () => {
    const p = new Curve().p.toString(16, 64);
    expect(() => parsePoint(bytes('02' + p))).toThrow(/x is not a canonical field element/);
  });

  it('rejects a wrong length', () => {
    expect(() => parsePoint(new Uint8Array(32))).toThrow(PointError);
    expect(() => parsePoint(new Uint8Array(65))).toThrow(PointError);
  });

  it('rejects an uncompressed or unknown prefix', () => {
    expect(() => parsePoint(bytes('04' + hex(toCompressed(G)).slice(2)))).toThrow(PointError);
    expect(() => parsePoint(bytes('00' + '00'.repeat(32)))).toThrow(PointError);
  });

  it('rejects an x with no curve point', () => {
    // x = 5 has no y on secp256k1 (5^3 + 7 = 132 is a non-residue)
    expect(() => parsePoint(bytes('02' + '05'.padStart(64, '0')))).toThrow(/not on the curve/);
  });

  it('rejects the all-zero encoding', () => {
    expect(() => parsePoint(new Uint8Array(33))).toThrow(PointError);
  });
});

describe('toCompressed', () => {
  it('encodes the generator as 33 bytes', () => {
    expect(hex(toCompressed(G))).toBe('0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798');
  });
});

describe('toAffine64 / fromAffine64', () => {
  it('round-trips G and 7*G', () => {
    for (const P of [G, G.mul(new BigNumber(7))]) {
      const raw = toAffine64(P);
      expect(raw.length).toBe(64);
      expect(fromAffine64(raw).eq(P)).toBe(true);
    }
  });

  it('is x || y big-endian', () => {
    const raw = hex(toAffine64(G));
    expect(raw.slice(0, 64)).toBe(G.getX().toHex(32));
    expect(raw.slice(64)).toBe(G.getY().toHex(32));
  });

  it('rejects a wrong length', () => {
    expect(() => fromAffine64(new Uint8Array(63))).toThrow(PointError);
    expect(() => fromAffine64(new Uint8Array(65))).toThrow(PointError);
  });

  it('rejects x >= p and y >= p', () => {
    const p = new Curve().p.toString(16, 64);
    const x = G.getX().toHex(32);
    const y = G.getY().toHex(32);
    expect(() => fromAffine64(bytes(p + y))).toThrow(/x is not a canonical field element/);
    expect(() => fromAffine64(bytes(x + p))).toThrow(/y is not a canonical field element/);
    expect(() => fromAffine64(bytes('ff'.repeat(32) + y))).toThrow(/x is not a canonical field element/);
    expect(() => fromAffine64(bytes(x + 'ff'.repeat(32)))).toThrow(/y is not a canonical field element/);
  });

  it('rejects a non-canonical x even when its reduction is a valid point', () => {
    // 2^256-1 reduces mod p to a real curve point; the SDK would accept it.
    const reduced = Point.fromString('02' + 'ff'.repeat(32));
    expect(reduced.validate()).toBe(true);
    expect(() => fromAffine64(bytes('ff'.repeat(32) + reduced.getY().toHex(32)))).toThrow(
      /x is not a canonical field element/,
    );
  });

  it('rejects an off-curve point', () => {
    const y = G.getY().add(new BigNumber(1)).toHex(32);
    expect(() => fromAffine64(bytes(G.getX().toHex(32) + y))).toThrow(/not on the curve/);
  });

  it('rejects the all-zero encoding (infinity)', () => {
    expect(() => fromAffine64(new Uint8Array(64))).toThrow(PointError);
  });
});
