// The witness WitnessEcMulCheck's verify(w) checks: every slope of a right-to-left double-and-add of
// k·base, started from the fixed offset point H (x = 2), plus the slope of the final out + H = R.
import type { Point } from '@bsv/sdk';

const P = 2n ** 256n - 2n ** 32n - 977n;
const N = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
const H: Aff = [2n, 0x66fbe727b2ba09e09f5a98d70a5efce8424c5fa425bbda1c511f860657b8535en];

/** Witness length: 256 iterations of (add slope, double slope), 33 bytes each, then the final slope. */
export const EC_MUL_WITNESS_BYTES = 256 * 66 + 33;

type Aff = [bigint, bigint];

const md = (a: bigint) => ((a % P) + P) % P;
function inv(a: bigint): bigint {
  let r = 1n;
  let b = md(a);
  for (let e = P - 2n; e > 0n; e >>= 1n) {
    if (e & 1n) r = (r * b) % P;
    b = (b * b) % P;
  }
  return r;
}
const addSlope = (a: Aff, b: Aff) => md((b[1] - a[1]) * inv(b[0] - a[0]));
const dblSlope = (a: Aff) => md(3n * a[0] * a[0] * inv(2n * a[1]));
function step(a: Aff, l: bigint, bx: bigint): Aff {
  const x = md(l * l - a[0] - bx);
  return [x, md(l * (a[0] - x) - a[1])];
}

/** 33-byte little-endian script number (non-negative, below 2^256). */
function le33(v: bigint): string {
  let h = '';
  for (let i = 0n; i < 33n; i++) h += Number((v >> (8n * i)) & 0xffn).toString(16).padStart(2, '0');
  return h;
}

/**
 * The hex witness for k·base, 0 < k < n. Throws on the inputs the checker refuses or for which no
 * witness exists (an accumulator meeting ±Q, which needs a discrete-log relation between H and base).
 */
export function ecMulWitness(base: Point, k: bigint): string {
  if (k <= 0n || k >= N) throw new RangeError('k must be in [1, n)');
  let q: Aff = [BigInt('0x' + base.getX().toHex(32)), BigInt('0x' + base.getY().toHex(32))];
  let r: Aff = H;
  let rest = k;
  let w = '';
  for (let i = 0; i < 256; i++) {
    const bit = rest % 2n;
    rest /= 2n;
    if (bit === 1n) {
      if (r[0] === q[0]) throw new Error('accumulator met ±Q: no witness exists');
      const l = addSlope(r, q);
      w += le33(l);
      r = step(r, l, q[0]);
    } else w += le33(0n);
    const d = dblSlope(q);
    w += le33(d);
    q = step(q, d, q[0]);
  }
  // out = R − H, and the checker verifies out + H = R with this slope.
  const out = step(r, addSlope(r, [H[0], md(-H[1])]), H[0]);
  if (out[0] === H[0]) throw new Error('k·base = ±H: no witness exists');
  return w + le33(addSlope(out, H));
}
