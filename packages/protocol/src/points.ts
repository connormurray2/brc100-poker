import { BigNumber, Curve, Point, Utils } from '@bsv/sdk';

/** Thrown for any received point that is not a canonical, finite, on-curve secp256k1 point. */
export class PointError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PointError';
  }
}

const P: BigNumber = new Curve().p;

const toHex = (b: Uint8Array): string => Utils.toHex(Array.from(b));

/**
 * `@bsv/sdk` reduces x mod p instead of rejecting x >= p, so an on-curve check
 * after parsing is not enough: range-check the raw big-endian bytes first.
 */
function assertCanonical(raw: Uint8Array, name: 'x' | 'y'): BigNumber {
  const n = new BigNumber(toHex(raw), 16);
  if (n.cmp(P) >= 0) throw new PointError(`${name} is not a canonical field element`);
  return n;
}

function assertFinite(point: Point): Point {
  if (point.isInfinity()) throw new PointError('point at infinity');
  return point;
}

/** Parse a 33-byte compressed point (prefix 02/03), strictly. */
export function parsePoint(bytes: Uint8Array): Point {
  if (bytes.length !== 33) throw new PointError(`expected 33 bytes, got ${bytes.length}`);
  if (bytes[0] !== 0x02 && bytes[0] !== 0x03) {
    throw new PointError(`bad prefix 0x${bytes[0]!.toString(16).padStart(2, '0')}`);
  }
  assertCanonical(bytes.subarray(1), 'x');
  let point: Point;
  try {
    point = Point.fromString(toHex(bytes));
  } catch {
    throw new PointError('point is not on the curve');
  }
  if (!point.validate()) throw new PointError('point is not on the curve');
  return assertFinite(point);
}

/** 64-byte affine encoding, `x ‖ y` big-endian. This is what every hashed proof statement uses. */
export function toAffine64(p: Point): Uint8Array {
  assertFinite(p);
  return Uint8Array.from(Utils.toArray(p.getX().toHex(32) + p.getY().toHex(32), 'hex'));
}

/** Parse a 64-byte affine `x ‖ y`, checking x < p, y < p and on-curve. */
export function fromAffine64(b: Uint8Array): Point {
  if (b.length !== 64) throw new PointError(`expected 64 bytes, got ${b.length}`);
  const x = assertCanonical(b.subarray(0, 32), 'x');
  const y = assertCanonical(b.subarray(32), 'y');
  const point = new Point(x, y);
  if (!point.validate()) throw new PointError('point is not on the curve');
  return assertFinite(point);
}

/** 33-byte compressed encoding. */
export function toCompressed(p: Point): Uint8Array {
  assertFinite(p);
  return Uint8Array.from(Utils.toArray(p.encode(true, 'hex') as string, 'hex'));
}
