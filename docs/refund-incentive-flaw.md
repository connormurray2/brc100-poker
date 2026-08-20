# The refund incentive flaw

**Status: fixed by the session pot.** This document is the analysis that led there; see
[session-pot-design.md](session-pot-design.md) for what replaced it. Found by a player asking the
obvious question during a test session on 2026-08-20.

---

## 1. When does a player actually sign?

Twice per hand, and knowing which is which is necessary to follow the rest.

| # | When | What is signed | Who signs |
| --- | --- | --- | --- |
| 1 | At buy-in, **before any cards** | Every seat's refund | every seat |
| 2 | After the showdown | The settlement paying the winner | every seat |

Signature 1 is why `signRefund` exists and why it deliberately requires no recorded stake: the
stake cannot be recorded until its refund exists. Signature 2 is the one gated on the player's own
expectation of the payout.

So committing a buy-in already collected signatures. The prompt that appears at the *end* of a hand
is signature 2.

## 2. What a refund is, and what it is not

A refund spends the pot and is **timelocked** — 144 blocks past the hand, roughly a day on
teratestnet. It exists for liveness: if a seat disappears mid-hand, everyone else's money must not
be trapped forever.

It is not an escape hatch a player can pull at will *during* a hand. It matures long after.

**Folding is a different thing entirely, and I previously conflated the two.** Folding is an action
inside a hand. The pot is already funded on chain with both buy-ins, so folding does not return your
stake — it gives up your claim on the pot, and the settlement then pays the whole pot to the other
seat. A folding player receives **0**, exactly like a player who loses at showdown.

## 3. The flaw

At showdown, a losing seat compares:

| Action | Outcome |
| --- | --- |
| Sign the settlement | **0**. The winner takes the pot. |
| Refuse | Nothing settles. At the refund locktime, the pot is a first-broadcast race — and both seats hold a refund paying the **whole pot**, so up to **9,700** of a 10,000 pot. |

Refusing is never worse than signing and usually much better, so a rational loser never signs. Real
value hands therefore cannot complete against an opponent who understands this.

### This is why `-max-pot` is not a fix

`-max-pot` bounds the largest pot a wallet will sign for unattended. It is irrelevant here. The
attack is not signing something too large — it is **declining something legitimate**. No care in the
approver addresses it, because the approver's job is to say no, and saying no is the attack.

## 4. Why the obvious fix does not work either

My first proposal was to make the refund return each seat its own stake instead of the whole pot:

```
now:    refund_0 -> 9700 to seat 0      (either seat can take everything)
        refund_1 -> 9700 to seat 1

stake:  refund   -> 4850 to seat 0
                    4850 to seat 1      (one transaction, no race)
```

That removes the race and the windfall, and it is a genuine improvement. **It does not make signing
rational.** A loser who signs still gets 0, and 4,850 beats 0. The incentive to stall survives, just
smaller.

The general statement: a losing seat receives nothing from the settlement, so **any** refund paying
them anything at all is preferable to signing. For signing to be rational the refund would have to
pay a loser less than the settlement does — that is, zero — at which point it is not a refund and
liveness is gone.

**A single-hand n-of-n pot with a timelocked refund cannot be incentive-compatible.** The loser
always holds a free option to stall. This is a structural property, not a parameter to tune.

## 4b. What the original implementation did

The project this game descends from ([prof-faustus/bsv-poker](https://github.com/prof-faustus/bsv-poker))
faced the same problem and is explicit about not solving it. From its `FORMAL_SECURITY.md`:

> **Liveness vs. punishment.** A withholding peer triggers an *accountable abort* → unilateral
> nLockTime / 2-of-2 escrow recovery, so **no honest player loses funds**. On-chain economic
> *penalty* for a griefer (stake slashing) is **not** claimed.

So it claims exactly what we can claim — nobody loses funds — and explicitly disclaims the part we
were missing, a penalty for stalling.

Its recovery is the stake-back design, confirmed in `Chain.BuildEscrowRecovery`:

```csharp
var outs = new List<TxOut> {
    new(stakeA, P2pkhLockForPub(pubA)),
    new(stakeB - fee, P2pkhLockForPub(pubB))
};
```

One transaction, each funder's own stake, co-signed **before** the escrow is funded. That is
strictly better than what this project does today — we build one refund *per seat* paying that seat
the *whole pot*, which adds a first-broadcast race and a windfall on top of the shared flaw.

Two conclusions worth separating:

1. **We are worse than the original and should fix that.** Stake-back removes the race and the
   windfall. It does not make signing rational, but it takes the profit out of stalling: a refusing
   loser recovers their own buy-in rather than winning the pot.
2. **Neither implementation makes stalling irrational**, and the original says so plainly rather
   than implying otherwise. A losing seat still prefers its stake back to zero. Treating that as
   solved would be the mistake.

## 5. What would actually work

**Session-spanning pots.** Buy in once for many hands, as an online poker site does. Refusing to
settle then forfeits the whole remaining stack and every future hand, not just the hand already
lost. The refund still guarantees liveness, but exercising it costs the refuser their session.

This is the direction with the best cost/benefit, and it fits the game: nobody expects to settle
on chain after every hand anyway. It requires the pot to be a running balance that settles when a
player leaves, rather than one pot per hand.

**Other directions, weaker:**

- *Escalating stakes*: the timelock delay costs the refuser interest. Real but far too small to
  matter at these amounts.
- *A bond outside the pot*: forfeitable on a stall. Needs a party to adjudicate, which reintroduces
  trust.
- *Reputation*: works for a known group, not for strangers, which is the case that matters.

**What cannot work:** punishing a refusal within the protocol. The stake is already in the pot and
any transaction moving it needs the refusing seat's signature. There is nothing to punish with.

## 6. Until it is fixed

Everything else works and is tested: the dealerless deal, hole-card privacy, the browser relay, the
n-of-n pot, per-seat settlement signing. The money layer's *incentives* are what is unfinished.

Play for chips, or with people you would lend money to.

---

## Appendix: a correction

An earlier version of this document said a refusing loser would get "4,850 instead of the 5,000
they'd have kept by folding". Both halves were wrong. Folding does not return a stake — the pot is
already funded, so a folder receives 0 like any loser. And 4,850 does not deter anything, because
the alternative is 0. Writing the arithmetic out is what surfaced that the stake-back refund is an
improvement rather than a fix.
