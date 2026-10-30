import { SmartContract, assert } from 'runar-lang';

/** Toolchain smoke test: the only spend that works is x = 3. */
export class Trivial extends SmartContract {
  constructor() {
    super();
  }

  public check(x: bigint) {
    assert(x === 3n);
  }
}
