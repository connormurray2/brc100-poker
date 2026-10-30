import {
  StatefulSmartContract,
  assert,
  checkSig,
  extractLocktime,
  extractSequence,
  extractAmount,
  extractOutputHash,
  hash160,
  hash256,
  num2bin,
  cat,
} from 'runar-lang';
import type { PubKey, Sig, ByteString } from 'runar-lang';

/**
 * SPIKE CONTRACT, copied unchanged from spikes/referee/contracts/ (2026-10-29) so gate (a) can mine
 * its post, act and forfeit shapes on testnet. Not product code: the real escrow and referee get
 * their own plan. A heads-up escrow that can referee a stalled hand.
 *
 * Off chain, both seats co-sign every state of the hand as a `post` transaction spending this
 * escrow, signed SINGLE|ANYONECANPAY|FORKID so anyone can add fee inputs later. Each post carries
 * nLockTime on a ladder (newest state matures first), so a stale state can never be posted before
 * the newest one.
 *
 * On chain, once a state is posted, the seat on the clock must `act` before `deadline` or anyone
 * can call `forfeit`, which treats that seat as having folded. Each on-chain move pushes the
 * deadline out by one window, on a fixed schedule nobody can backdate.
 *
 * Out of scope for the spike: street progression, showdown and the deal (those need card points;
 * see MaskCheck for the on-chain cost of checking a wallet's masking).
 */
export class HeadsUpReferee extends StatefulSmartContract {
  readonly pk0: PubKey;
  readonly pk1: PubKey;
  /** Height at which the newest possible state (seq === maxSeq) matures. */
  readonly anchor: bigint;
  readonly maxSeq: bigint;
  /** Blocks per ladder rung and per on-chain response. */
  readonly window: bigint;
  readonly bigBlind: bigint;
  readonly p2pkhPrefix: ByteString = '1976a914' as ByteString;
  readonly p2pkhSuffix: ByteString = '88ac' as ByteString;

  /** Stacks at the start of the hand. */
  bal0: bigint;
  bal1: bigint;
  /** Chips each seat has put in this hand. */
  com0: bigint = 0n;
  com1: bigint = 0n;
  /** 0 = the hand is being played off chain; 1 = a state has been posted. */
  phase: bigint = 0n;
  seq: bigint = 0n;
  toAct: bigint = 0n;
  deadline: bigint = 0n;

  constructor(
    pk0: PubKey,
    pk1: PubKey,
    anchor: bigint,
    maxSeq: bigint,
    window: bigint,
    bigBlind: bigint,
    bal0: bigint,
    bal1: bigint,
  ) {
    super(pk0, pk1, anchor, maxSeq, window, bigBlind, bal0, bal1);
    this.pk0 = pk0;
    this.pk1 = pk1;
    this.anchor = anchor;
    this.maxSeq = maxSeq;
    this.window = window;
    this.bigBlind = bigBlind;
    this.bal0 = bal0;
    this.bal1 = bal1;
  }

  /**
   * Post a state both seats signed off chain.
   *
   * @sighash SINGLE|ANYONECANPAY|FORKID
   */
  public post(newSeq: bigint, newToAct: bigint, newCom0: bigint, newCom1: bigint, sig0: Sig, sig1: Sig) {
    assert(this.phase === 0n);
    assert(checkSig(sig0, this.pk0));
    assert(checkSig(sig1, this.pk1));
    assert(newSeq >= 1n);
    assert(newSeq <= this.maxSeq);
    assert(newToAct === 0n || newToAct === 1n);
    assert(newCom0 >= 0n && newCom0 <= this.bal0);
    assert(newCom1 >= 0n && newCom1 <= this.bal1);

    // The ladder: state n matures at anchor + (maxSeq - n) * window. A newer state therefore
    // becomes postable strictly before any older one.
    const lt: bigint = extractLocktime(this.txPreimage);
    assert(lt === this.anchor + (this.maxSeq - newSeq) * this.window);
    assert(lt < 500000000n);
    assert(extractSequence(this.txPreimage) !== 0xffffffffn);

    // The continuation carries the escrow's whole value, so whoever adds the fee input cannot
    // skim it into their change. (A mutate-only continuation is sized by the spender.)
    this.addOutput(extractAmount(this.txPreimage), this.bal0, this.bal1, newCom0, newCom1, 1n, newSeq,
      newToAct, lt + this.window);
  }

  /**
   * The seat on the clock checks, calls or raises on chain. kind 1 = check/call to match,
   * kind 2 = raise to `to`.
   */
  public act(kind: bigint, to: bigint, sig: Sig) {
    assert(this.phase === 1n);
    let mine: bigint = this.com0;
    let theirs: bigint = this.com1;
    let stack: bigint = this.bal0;
    let mover: PubKey = this.pk0;
    if (this.toAct === 1n) {
      mover = this.pk1;
      mine = this.com1;
      theirs = this.com0;
      stack = this.bal1;
    }
    // A Sig is single-use, so the mover's key is chosen first and checked once.
    assert(checkSig(sig, mover));

    if (kind === 1n) {
      // Matching, capped at the stack for an all-in call.
      if (theirs <= stack) {
        assert(to === theirs);
      } else {
        assert(to === stack);
      }
    } else {
      assert(kind === 2n);
      assert(to <= stack);
      assert(to > theirs);
      assert(to - theirs >= this.bigBlind || to === stack);
    }
    assert(to >= mine);

    // Same reason as post: pin the value, or the mover could skim the escrow into its change.
    const amount: bigint = extractAmount(this.txPreimage);
    if (this.toAct === 0n) {
      this.addOutput(amount, this.bal0, this.bal1, to, this.com1, 1n, this.seq + 1n, 1n,
        this.deadline + this.window);
    } else {
      this.addOutput(amount, this.bal0, this.bal1, this.com0, to, 1n, this.seq + 1n, 0n,
        this.deadline + this.window);
    }
  }

  /** The seat on the clock folds on chain. */
  public fold(sig: Sig, changePKH: ByteString, changeAmount: bigint) {
    assert(this.phase === 1n);
    let mover: PubKey = this.pk0;
    if (this.toAct === 1n) {
      mover = this.pk1;
    }
    assert(checkSig(sig, mover));
    assert(this.foldPayoutHash(changePKH, changeAmount) === extractOutputHash(this.txPreimage));
  }

  /** The seat on the clock missed its deadline: it is treated as having folded. Anyone may call. */
  public forfeit(changePKH: ByteString, changeAmount: bigint) {
    assert(this.phase === 1n);
    const lt: bigint = extractLocktime(this.txPreimage);
    assert(lt >= this.deadline);
    assert(lt < 500000000n);
    assert(extractSequence(this.txPreimage) !== 0xffffffffn);
    assert(this.foldPayoutHash(changePKH, changeAmount) === extractOutputHash(this.txPreimage));
  }

  /** Both seats agree a settlement off chain. Their ALL|FORKID signatures fix every output. */
  public close(sig0: Sig, sig1: Sig) {
    assert(checkSig(sig0, this.pk0));
    assert(checkSig(sig1, this.pk1));
  }

  /** The hash of the outputs a fold by the seat on the clock must create. */
  private foldPayoutHash(changePKH: ByteString, changeAmount: bigint): ByteString {
    // The folding seat keeps what it did not put in; the other seat takes the rest.
    let out0: bigint = this.bal0 - this.com0;
    let out1: bigint = this.bal1 + this.com0;
    if (this.toAct === 1n) {
      out0 = this.bal0 + this.com1;
      out1 = this.bal1 - this.com1;
    }
    const pay0 = cat(cat(num2bin(out0, 8n), this.p2pkhPrefix), cat(hash160(this.pk0), this.p2pkhSuffix));
    const pay1 = cat(cat(num2bin(out1, 8n), this.p2pkhPrefix), cat(hash160(this.pk1), this.p2pkhSuffix));
    let outs: ByteString = cat(pay0, pay1);
    if (changeAmount > 0n) {
      outs = cat(outs, cat(cat(num2bin(changeAmount, 8n), this.p2pkhPrefix), cat(changePKH, this.p2pkhSuffix)));
    }
    return hash256(outs);
  }
}
