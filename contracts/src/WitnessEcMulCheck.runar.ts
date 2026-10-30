import { SmartContract, assert, bin2num, ecOnCurve, ecPointX, ecPointY, len, substr } from 'runar-lang';
import type { ByteString, Point } from 'runar-lang';

/**
 * Proves `out = k·base` like EcMulCheck, but checks the multiplication instead of computing it.
 *
 * Right-to-left double-and-add over affine coordinates, branch-free. The accumulator starts at a fixed
 * offset point H instead of infinity, so every one of the 256 iterations has the same shape:
 *   add (only counted when the bit is 1):  λa·(xQ − xR) ≡ yQ − yR, then x' = λa² − xR − xQ, y' = λa(xR − x') − yR
 *   double Q:                              λd·2yQ ≡ 3xQ²,          then x' = λd² − 2xQ,     y' = λd(xQ − x') − yQ
 * The spender supplies λa and λd for each iteration in `w` (66 bytes per iteration: two 33-byte
 * little-endian script numbers; λa is ignored when the bit is 0). A λ that passes is the true slope
 * mod p, so every computed point is the true one. The final step subtracts H: it checks
 * out + H = R with one more checked addition (λf, the last 33 bytes of `w`).
 *
 * xR ≠ xQ is asserted wherever an addition is counted, because that is the one case where any λ passes;
 * an honest spender meets it only if it knows a discrete-log relation between H and the base.
 * secp256k1 has no point with y = 0, so a doubling's check always pins λd.
 */
export class WitnessEcMulCheck extends SmartContract {
  readonly base: Point;
  readonly k: bigint;
  readonly out: Point;

  constructor(base: Point, k: bigint, out: Point) {
    super(base, k, out);
    this.base = base;
    this.k = k;
    this.out = out;
  }

  public verify(w: ByteString) {
    // Read each constructor value once, first, so the script is push(base) ‖ push(k) ‖ push(out) ‖ body
    // and a referee can build it from one fixed body.
    const base: Point = this.base;
    const k: bigint = this.k;
    const out: Point = this.out;
    const p: bigint = 0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2fn;
    const n: bigint = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
    assert(ecOnCurve(base));
    assert(k > 0n);
    assert(k < n);
    assert(len(w) === 16929n); // 256 iterations x 66 bytes, plus λf

    let qx: bigint = ecPointX(base);
    let qy: bigint = ecPointY(base);
    // H: the secp256k1 x = 2 point (even y). Nobody knows its discrete log to any base.
    let rx: bigint = 2n;
    let ry: bigint = 0x66fbe727b2ba09e09f5a98d70a5efce8424c5fa425bbda1c511f860657b8535en;
    let rest: bigint = k;

    for (let i = 0n; i < 256n; i++) {
      const bit: bigint = rest % 2n;
      rest = rest / 2n;

      const la: bigint = bin2num(substr(w, i * 66n, 33n));
      assert((qx - rx) * bit + (1n - bit) !== 0n);
      assert(((la * (qx - rx) - (qy - ry)) * bit) % p === 0n);
      const ax: bigint = (((la * la - rx - qx) % p) + p) % p;
      const ay: bigint = (((la * (rx - ax) - ry) % p) + p) % p;
      rx = rx + bit * (ax - rx);
      ry = ry + bit * (ay - ry);

      const ld: bigint = bin2num(substr(w, i * 66n + 33n, 33n));
      assert((ld * 2n * qy - 3n * qx * qx) % p === 0n);
      const dx: bigint = (((ld * ld - 2n * qx) % p) + p) % p;
      qy = (((ld * (qx - dx) - qy) % p) + p) % p;
      qx = dx;
    }

    // R = H + k·base. Check out + H = R: one more checked addition, with out ≠ ±H.
    const ox: bigint = ecPointX(out);
    const oy: bigint = ecPointY(out);
    assert(ecOnCurve(out));
    assert(ox !== 2n);
    const lf: bigint = bin2num(substr(w, 16896n, 33n));
    const hy: bigint = 0x66fbe727b2ba09e09f5a98d70a5efce8424c5fa425bbda1c511f860657b8535en;
    assert((lf * (2n - ox) - (hy - oy)) % p === 0n);
    const fx: bigint = (((lf * lf - ox - 2n) % p) + p) % p;
    const fy: bigint = (((lf * (ox - fx) - oy) % p) + p) % p;
    assert(fx === rx);
    assert(fy === ry);
  }
}
