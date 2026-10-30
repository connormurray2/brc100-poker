import { SmartContract, assert, ecMulGen, ecOnCurve } from 'runar-lang';
import type { Point } from 'runar-lang';

/**
 * Proves `out = k·G` on chain, with exactly one generator multiplication.
 *
 * The EcMulCheck variant for a base equal to EC_G. It has no `base` property: the base is G.
 * Stateless; the statement (k, out) is baked into the locking script and `verify()` takes no
 * arguments.
 */
export class EcMulGenCheck extends SmartContract {
  readonly k: bigint;
  readonly out: Point;

  constructor(k: bigint, out: Point) {
    super(k, out);
    this.k = k;
    this.out = out;
  }

  public verify() {
    // secp256k1 group order, written out: the compiler type-checks EC_N but cannot lower it
    // ("method parameter 'EC_N' is not on the stack"), so the constant cannot be used here.
    const n: bigint = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
    assert(this.k % n !== 0n);
    const r: Point = ecMulGen(this.k);
    assert(ecOnCurve(r));
    assert(r === this.out);
  }
}
