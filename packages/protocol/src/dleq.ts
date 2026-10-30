import { BigNumber, Curve, Hash, Point, Utils } from '@bsv/sdk';
import { toAffine64 } from './points.js';

/**
 * Non-interactive Chaum-Pedersen proofs of equal discrete logarithms (spec 7.2).
 * A statement asserts log_B1 P1 = log_B2 P2. The byte encoding of the hashed
 * statement is shared with the on-chain checker, so it is fixed here:
 *
 *   statementBytes = UTF-8 domain || 0x00 || affine64(B1, P1, B2, P2, A1, A2)
 */
export type DleqStatement = { domain: string; B1: Point; P1: Point; B2: Point; P2: Point };
export type DleqProof = { A1: Point; A2: Point; z: BigNumber };

const N: BigNumber = new Curve().n;
const NONCE_TAG = Utils.toArray('nonce', 'utf8');

const concat = (...parts: ArrayLike<number>[]): Uint8Array => {
  const out = new Uint8Array(parts.reduce((len, p) => len + p.length, 0));
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
};

/** The part of the statement that exists before A1 and A2 do: domain || 0x00 || B1, P1, B2, P2. */
export function statementCore(st: DleqStatement): Uint8Array {
  return concat(
    Utils.toArray(st.domain, 'utf8'),
    [0x00],
    toAffine64(st.B1),
    toAffine64(st.P1),
    toAffine64(st.B2),
    toAffine64(st.P2),
  );
}

/** The full hashed statement: core || A1 || A2, every point 64-byte affine. */
export function statementBytes(st: DleqStatement, A1: Point, A2: Point): Uint8Array {
  return concat(statementCore(st), toAffine64(A1), toAffine64(A2));
}

/** Big-endian SHA-256 of `bytes`, reduced mod n. */
export function challenge(bytes: Uint8Array): BigNumber {
  return new BigNumber(Hash.sha256(Array.from(bytes))).umod(N);
}

/**
 * Deterministic nonce: HMAC-SHA256(secret, "nonce" || SHA-256(statementCore)) mod n.
 * It cannot cover A1 or A2, which are derived from it.
 */
function nonce(nonceSecret: Uint8Array, st: DleqStatement): BigNumber {
  const digest = Hash.sha256(Array.from(statementCore(st)));
  const mac = Hash.sha256hmac(Array.from(nonceSecret), [...NONCE_TAG, ...digest]);
  const w = new BigNumber(mac).umod(N);
  if (w.isZero()) throw new Error('derived a zero nonce');
  return w;
}

const sameAffine = (a: Point, b: Point): boolean => {
  if (a.isInfinity() || b.isInfinity()) return false; // an infinite result never matches a finite A
  return a.getX().cmp(b.getX()) === 0 && a.getY().cmp(b.getY()) === 0;
};

/**
 * Prove log_B1 P1 = log_B2 P2 = s.
 *
 * The nonce depends only on the statement, so two proofs of one statement made with
 * different scalars would publish c·(s - s') and expose s to anyone who knows s'. Hence
 * `s` must be the true witness, and `nonceSecret` must be a full 32 bytes (an empty or
 * short secret makes the nonce guessable and a single proof leaks the witness).
 */
export function prove(st: DleqStatement, s: BigNumber, nonceSecret: Uint8Array): DleqProof {
  if (!(nonceSecret instanceof Uint8Array) || nonceSecret.length !== 32) {
    throw new RangeError('nonceSecret must be exactly 32 bytes');
  }
  if (s.isNeg() || s.isZero() || s.cmp(N) >= 0) throw new RangeError('witness must be in [1, n)');
  if (!sameAffine(st.B1.mul(s), st.P1) || !sameAffine(st.B2.mul(s), st.P2)) {
    throw new Error('s is not the witness of this statement');
  }
  const w = nonce(nonceSecret, st);
  const A1 = st.B1.mul(w);
  const A2 = st.B2.mul(w);
  const c = challenge(statementBytes(st, A1, A2));
  return { A1, A2, z: w.add(c.mul(s)).umod(N) };
}

const finite = (p: Point): boolean => p instanceof Point && !p.isInfinity() && p.validate();

/** `k·p`, with the zero scalar giving the point at infinity instead of relying on the library. */
const mulOrInfinity = (p: Point, k: BigNumber): Point => (k.isZero() ? new Point(null, null) : p.mul(k));

/** Check z·B1 = A1 + c·P1 and z·B2 = A2 + c·P2. Never throws for a malformed proof. */
export function verify(st: DleqStatement, pr: DleqProof): boolean {
  try {
    if (![st.B1, st.P1, st.B2, st.P2, pr.A1, pr.A2].every(finite)) return false;
    if (!(pr.z instanceof BigNumber) || pr.z.isNeg() || pr.z.cmp(N) >= 0) return false;
    const c = challenge(statementBytes(st, pr.A1, pr.A2));
    const rhs1 = pr.A1.add(mulOrInfinity(st.P1, c));
    const rhs2 = pr.A2.add(mulOrInfinity(st.P2, c));
    return sameAffine(mulOrInfinity(st.B1, pr.z), rhs1) && sameAffine(mulOrInfinity(st.B2, pr.z), rhs2);
  } catch {
    return false;
  }
}
