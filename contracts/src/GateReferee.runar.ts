import {
  StatefulSmartContract,
  assert,
  bin2num,
  cat,
  ecAdd,
  extractAmount,
  extractHashPrevouts,
  extractOutpoint,
  hash256,
  left,
  len,
  num2bin,
  reverseBytes,
  right,
  sha256,
  toByteString,
} from 'runar-lang';
import type { ByteString, Point, Ripemd160 } from 'runar-lang';

/**
 * Gate (d): convicts a recorded Chaum–Pedersen proof that is false, with the EC multiplications in
 * separate checker outputs. The real dispute referee reuses this pattern.
 *
 * Recorded (readonly, baked at deploy): the statement B1 = G (implied), P1, B2, P2, the proof
 * A1, A2, z, and the domain. The proof claims
 *   eq 1: z·G  = A1 + c·P1
 *   eq 2: z·B2 = A2 + c·P2
 * with c = sha256(domain ‖ 0x00 ‖ G ‖ P1 ‖ B2 ‖ P2 ‖ A1 ‖ A2) read big-endian, mod n (every point
 * 64-byte affine), exactly as `challenge(statementBytes(...))` in @brc100-poker/protocol.
 *
 * `prepare(eq, L, M, …)` (phase 0 → 1): stores the claimed products L = z·B and M = c·P and
 * creates two stateless WitnessEcMulCheck outputs whose locking scripts bake those claims in:
 *   eq 1: (base = G,  k = z, out = L)  and  (base = P1, k = c, out = M)
 *   eq 2: (base = B2, k = z, out = L)  and  (base = P2, k = c, out = M)
 * A WitnessEcMulCheck script is push(base) ‖ push(k) ‖ push(out) ‖ body, one fixed body for every
 * statement. The caller hands the body in, and its length and SHA-256 are bound to constants baked at
 * deploy. The caller pays a bond into the continuation.
 *
 * `fraud(feeOutpoint)` (phase 1 → 2): requires the transaction's inputs to be exactly
 * (this referee, (prepTxid, 1), (prepTxid, 2), feeOutpoint), where prepTxid is the txid in the
 * referee's own outpoint. Spending the two checkers (each with its slope witness) is what proves
 * L = z·B and M = c·P. It then
 * asserts L !== A + M, and returns the bond to the prepare caller.
 *
 * Every method is ALL|FORKID: the continuation pins the referee's full value, and the caller's
 * fee coin pays the miner, with change.
 */
export class GateReferee extends StatefulSmartContract {
  readonly domain: ByteString;
  readonly B2: Point;
  readonly P1: Point;
  readonly P2: Point;
  readonly A1: Point;
  readonly A2: Point;
  readonly z: bigint;
  readonly bondSats: bigint;

  // The WitnessEcMulCheck body (everything after its three constructor pushes), from the caller.
  readonly chkLen: bigint;
  readonly chkHash: ByteString;

  /** affine(G), the implied B1, and eq 1's L base. */
  readonly gAffine: ByteString = toByteString(
    '79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8',
  );

  /** 0 = recorded, 1 = prepared, 2 = fraud proven. */
  phase: bigint = 0n;
  /** The disputed equation, set by prepare. */
  eq: bigint = 0n;
  /** The claimed products, set by prepare and baked into the checkers. */
  L: Point;
  M: Point;
  /** Where fraud returns the bond: the prepare caller's choice. */
  bondPKH: Ripemd160;

  constructor(
    domain: ByteString,
    B2: Point,
    P1: Point,
    P2: Point,
    A1: Point,
    A2: Point,
    z: bigint,
    bondSats: bigint,
    chkLen: bigint,
    chkHash: ByteString,
    L: Point,
    M: Point,
    bondPKH: Ripemd160,
  ) {
    super(domain, B2, P1, P2, A1, A2, z, bondSats, chkLen, chkHash, L, M, bondPKH);
    this.domain = domain;
    this.B2 = B2;
    this.P1 = P1;
    this.P2 = P2;
    this.A1 = A1;
    this.A2 = A2;
    this.z = z;
    this.bondSats = bondSats;
    this.chkLen = chkLen;
    this.chkHash = chkHash;
    this.L = L;
    this.M = M;
    this.bondPKH = bondPKH;
  }

  /**
   * Create the two checkers for equation `eqSel`, claiming L = z·B and M = c·P.
   *
   * The products' values are not checked here: a false claim just makes its checker unspendable,
   * so fraud cannot follow.
   *
   * Their WIDTHS are checked, and must be: Rúnar does not enforce a Point's 64 bytes at runtime.
   * An 81-byte lPt would put 17 caller bytes of script after the checker's `0x40` push (so the
   * checker passes for any L), and would stretch the stored state, which Rúnar parses from the END
   * of the scriptCode (checking only its length and leading 0x6a), letting fraud read a forged
   * phase, eq and L. Every fixed-width argument is length-checked for the same reason, and so are
   * the recorded Points spliced into the checkers (fail closed on a malformed record).
   */
  public prepare(
    eqSel: bigint,
    lPt: Point,
    mPt: Point,
    body: ByteString,
    refundPKH: Ripemd160,
  ) {
    assert(this.phase === 0n);
    assert(eqSel === 1n || eqSel === 2n);
    assert(len(lPt) === 64n);
    assert(len(mPt) === 64n);
    assert(len(refundPKH) === 20n);
    assert(len(this.B2) === 64n);
    assert(len(this.P1) === 64n);
    assert(len(this.P2) === 64n);
    assert(len(this.A1) === 64n);
    assert(len(this.A2) === 64n);
    assert(len(body) === this.chkLen);
    assert(sha256(body) === this.chkHash);

    const c: bigint = this.challengeScalar();
    const pushZ: ByteString = this.pushNum(this.z);
    const pushC: ByteString = this.pushNum(c);

    let lBase: ByteString = this.gAffine;
    let mBase: Point = this.P1;
    if (eqSel === 2n) {
      lBase = this.B2;
      mBase = this.P2;
    }
    const lScript: ByteString = this.chkScript(lBase, pushZ, lPt, body);
    const mScript: ByteString = this.chkScript(mBase, pushC, mPt, body);

    // Output 0: the continuation, carrying the referee's full value plus the bond.
    this.addOutput(extractAmount(this.txPreimage) + this.bondSats, 1n, eqSel, lPt, mPt, refundPKH);
    // Outputs 1 and 2: the checkers. Output 3, if any, is the caller's change.
    this.addDataOutput(1n, lScript);
    this.addDataOutput(1n, mScript);
  }

  /**
   * Convict the recorded proof on the prepared equation. `feeOutpoint` (36 bytes) is the caller's
   * fee coin, input 3.
   */
  public fraud(feeOutpoint: ByteString) {
    assert(this.phase === 1n);
    assert(len(feeOutpoint) === 36n);

    // The referee is output 0 of the prepare transaction, so the checkers it created are that
    // transaction's outputs 1 and 2. Bind the inputs to exactly those.
    const own: ByteString = extractOutpoint(this.txPreimage);
    assert(right(own, 4n) === toByteString('00000000'));
    const prepTxid: ByteString = left(own, 32n);
    const checkers: ByteString = cat(cat(prepTxid, toByteString('01000000')), cat(prepTxid, toByteString('02000000')));
    assert(hash256(cat(cat(own, checkers), feeOutpoint)) === extractHashPrevouts(this.txPreimage));

    let a: Point = this.A1;
    if (this.eq === 2n) {
      a = this.A2;
    }
    assert(this.L !== ecAdd(a, this.M));

    this.addOutput(extractAmount(this.txPreimage) - this.bondSats, 2n, this.eq, this.L, this.M, this.bondPKH);
    this.addDataOutput(this.bondSats, cat(cat(toByteString('76a914'), this.bondPKH), toByteString('88ac')));
  }

  /** sha256(statement) as a big-endian unsigned integer, mod n. */
  private challengeScalar(): bigint {
    const st: ByteString = cat(
      cat(cat(cat(this.domain, toByteString('00')), this.gAffine), cat(this.P1, this.B2)),
      cat(this.P2, cat(this.A1, this.A2)),
    );
    // bin2num is little-endian and signed: reverse, then append 0x00 to keep it non-negative.
    const h: bigint = bin2num(cat(reverseBytes(sha256(st)), toByteString('00')));
    // secp256k1 group order, written out: the compiler type-checks EC_N but cannot lower it.
    const order: bigint = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
    return h % order;
  }

  /** WitnessEcMulCheck(base, k, out)'s locking script, with `pushK` the push of k. */
  private chkScript(base: ByteString, pushK: ByteString, out: Point, body: ByteString): ByteString {
    return cat(cat(cat(toByteString('40'), base), pushK), cat(cat(toByteString('40'), out), body));
  }

  /**
   * The minimal Script push of k in [0, n), as the SDK splices a bigint constructor arg:
   * 0 → OP_0, 1..16 → OP_1..OP_16, else a direct push of the minimal little-endian signed
   * encoding (with a 0x00 pad when the top bit of the last byte is set).
   */
  private pushNum(k: bigint): ByteString {
    // The encoding's length: the smallest sz with k < 2^(8·sz − 1). Flat ifs, each overwriting a
    // larger bound with a smaller one (no nesting: icellan/runar#178).
    let sz: bigint = 33n;
    if (k < 0x8000000000000000000000000000000000000000000000000000000000000000n) {
      sz = 32n;
    }
    if (k < 0x80000000000000000000000000000000000000000000000000000000000000n) {
      sz = 31n;
    }
    if (k < 0x800000000000000000000000000000000000000000000000000000000000n) {
      sz = 30n;
    }
    if (k < 0x8000000000000000000000000000000000000000000000000000000000n) {
      sz = 29n;
    }
    if (k < 0x80000000000000000000000000000000000000000000000000000000n) {
      sz = 28n;
    }
    if (k < 0x800000000000000000000000000000000000000000000000000000n) {
      sz = 27n;
    }
    if (k < 0x8000000000000000000000000000000000000000000000000000n) {
      sz = 26n;
    }
    if (k < 0x80000000000000000000000000000000000000000000000000n) {
      sz = 25n;
    }
    if (k < 0x800000000000000000000000000000000000000000000000n) {
      sz = 24n;
    }
    if (k < 0x8000000000000000000000000000000000000000000000n) {
      sz = 23n;
    }
    if (k < 0x80000000000000000000000000000000000000000000n) {
      sz = 22n;
    }
    if (k < 0x800000000000000000000000000000000000000000n) {
      sz = 21n;
    }
    if (k < 0x8000000000000000000000000000000000000000n) {
      sz = 20n;
    }
    if (k < 0x80000000000000000000000000000000000000n) {
      sz = 19n;
    }
    if (k < 0x800000000000000000000000000000000000n) {
      sz = 18n;
    }
    if (k < 0x8000000000000000000000000000000000n) {
      sz = 17n;
    }
    if (k < 0x80000000000000000000000000000000n) {
      sz = 16n;
    }
    if (k < 0x800000000000000000000000000000n) {
      sz = 15n;
    }
    if (k < 0x8000000000000000000000000000n) {
      sz = 14n;
    }
    if (k < 0x80000000000000000000000000n) {
      sz = 13n;
    }
    if (k < 0x800000000000000000000000n) {
      sz = 12n;
    }
    if (k < 0x8000000000000000000000n) {
      sz = 11n;
    }
    if (k < 0x80000000000000000000n) {
      sz = 10n;
    }
    if (k < 0x800000000000000000n) {
      sz = 9n;
    }
    if (k < 0x8000000000000000n) {
      sz = 8n;
    }
    if (k < 0x80000000000000n) {
      sz = 7n;
    }
    if (k < 0x800000000000n) {
      sz = 6n;
    }
    if (k < 0x8000000000n) {
      sz = 5n;
    }
    if (k < 0x80000000n) {
      sz = 4n;
    }
    if (k < 0x800000n) {
      sz = 3n;
    }
    if (k < 0x8000n) {
      sz = 2n;
    }
    if (k < 0x80n) {
      sz = 1n;
    }
    let out: ByteString = cat(num2bin(sz, 1n), num2bin(k, sz));
    if (k <= 16n) {
      out = num2bin(k + 80n, 1n);
    }
    if (k === 0n) {
      out = toByteString('00');
    }
    return out;
  }
}
