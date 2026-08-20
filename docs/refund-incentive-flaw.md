# The refund incentive flaw

**Status: known, unfixed, and it makes real-value play unsafe.** Found by a player asking the
obvious question during a test session on 2026-08-20.

---

## The question

> What if I don't approve after I've already lost the hand?

---

## The answer, and why it is worse than it sounds

A losing seat is strictly better off refusing to sign the settlement.

Each seat holds a fully-signed refund that returns **the whole pot** to that seat, timelocked. The
refunds all spend the same outpoint, so only one can ever be mined. The design assumed the
settlement always wins that race because it has no timelock, which is true — *if the settlement is
ever broadcast.*

It is only broadcast if every seat signs. So:

| | Signs the settlement | Refuses |
| --- | --- | --- |
| Loser's outcome | receives 0 | pot becomes a race they can win |

Concretely, with a 5,000 satoshi buy-in each and a 10,000 satoshi pot:

- Loser signs: they get **0**.
- Loser refuses: nothing settles. At the refund locktime, whoever broadcasts first takes
  **9,700** — and both seats hold an equivalent refund, so it is a first-broadcast race.

**Refusing is never worse than signing, and is usually much better.** A rational player never
signs a losing settlement, which means real-value hands cannot complete.

This also disposes of `-max-pot` as a safety mechanism, which is what the player was actually
pointing out: bounding the pot a wallet will sign for does nothing, because the attack is not
signing something too large — it is **declining something legitimate**.

---

## Why the refund design is wrong

The refund exists to answer a real problem: a seat that disappears must not trap everyone's money.
Paying the whole pot to a single seat solves liveness and creates this incentive at the same time.

A refund should restore the **status quo before the hand**, not hand one player the pot. Each seat
put in its buy-in; a refund should return each seat exactly that.

---

## The fix

**One refund transaction paying every seat its own stake back**, rather than one refund per seat
paying that seat everything.

```
pot 10000  =  seat 0 buy-in 5000  +  seat 1 buy-in 5000

current: refund_0 -> 9700 to seat 0        (either seat can take everything)
         refund_1 -> 9700 to seat 1

fixed:   refund   -> 4850 to seat 0
                     4850 to seat 1        (nobody gains by stalling)
```

Then refusing to settle costs a losing seat its stake rather than winning it the pot. The refund
becomes what it was meant to be: a way out that nobody prefers.

Properties this restores:

- **Liveness.** A vanished seat still cannot trap funds; the refund matures and pays everyone.
- **Incentive compatibility.** A loser gains nothing by refusing, so settlement is the rational
  move. A winner still prefers settling, because the refund pays them only their stake rather than
  their winnings.
- **No race.** One refund, one outcome, so it does not matter who broadcasts it.

### Work involved

- `cosign.BuildRefund` takes a set of recipients and amounts, not one recipient
- `PotManager.OpenPot` builds and collects signatures for one refund rather than N
- The agent's `signRefund` check becomes: every seat is paid its own stake, not one named
  beneficiary is paid the pot
- `RefundFor`/`StakeInfo` return the single shared refund
- Tests: a losing seat gains nothing by refusing; the refund pays every seat its stake; the
  settlement still beats the refund when everyone cooperates

### Not fixed by

Punishing a refusal. There is nothing to punish with: the stake is already in the pot and the
protocol has no way to burn it or award it, since any such transaction would itself need the
refusing seat's signature.

---

## Until it is fixed

Real-value play is unsafe with untrusted opponents. The mental poker, the relay, the deal privacy
and the settlement machinery all work; the money layer's incentives do not. Play for chips, or
with people you would lend money to.
