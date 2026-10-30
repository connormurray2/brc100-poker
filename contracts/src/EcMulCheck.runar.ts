import { SmartContract, assert, ecMul, ecOnCurve } from 'runar-lang';
import type { Point } from 'runar-lang';

/**
 * Proves `out = k·base` on chain, with exactly one elliptic-curve multiplication.
 *
 * Stateless. The statement (base, k, out) is baked into the locking script at creation, and
 * `verify()` takes no arguments, so anyone can spend it, and it spends only if the statement is
 * true. One `ecMul` is about 429 KB of script, so the referee creates one checker per equation
 * (L = z·B and M = c·P) rather than packing two multiplications into a single script.
 * For a base of G use EcMulGenCheck, which is cheaper.
 */
export class EcMulCheck extends SmartContract {
  readonly base: Point;
  readonly k: bigint;
  readonly out: Point;

  constructor(base: Point, k: bigint, out: Point) {
    super(base, k, out);
    this.base = base;
    this.k = k;
    this.out = out;
  }

  public verify() {
    assert(ecOnCurve(this.base));
    // secp256k1 group order, written out: the compiler type-checks EC_N but cannot lower it
    // ("method parameter 'EC_N' is not on the stack"), so the constant cannot be used here.
    const n: bigint = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
    assert(this.k % n !== 0n);
    const r: Point = ecMul(this.base, this.k);
    assert(ecOnCurve(r));
    assert(r === this.out);
  }
}
