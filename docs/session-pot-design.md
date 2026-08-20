# Session-spanning pot: design

**Status: implemented.** Steps 1-5 of section 8 are done; see the tests named there. Supersedes the per-hand pot, which is incentive-incompatible —
see [refund-incentive-flaw.md](refund-incentive-flaw.md).

The goal in one sentence: **a player must never gain by refusing to sign.**

---

## 1. Why the per-hand pot cannot be fixed

A losing seat receives nothing from a settlement. So any refund paying them anything beats signing,
and a refund that pays nothing is not a refund. The per-hand pot has no parameter that fixes this.

## 2. The structure

One pot per **session**, not per hand. Players buy in once, play many hands, and settle when
someone leaves.

```
buy-in:   both seats fund a single n-of-n pot          40,000
hand 1:   seat 0 +300      balances  20,300 / 19,700
hand 2:   seat 1 +500      balances  19,800 / 20,200
hand 3:   seat 0 +1,200    balances  21,000 / 19,000
leave:    settle on chain  21,000 to seat 0, 19,000 to seat 1
```

Nothing touches the chain between the buy-in and the exit. Hands move a **running balance**, which
is what makes the incentive work.

## 3. The refund must track the balance, not the buy-in

This is the part that is easy to get wrong, and I did at first.

A refund returning each seat its **buy-in** still rewards refusing: a seat down 1,000 recovers
20,000 instead of 19,000. It erases their losses. The refund has to pay the **current balance**:

| Refund pays | Seat down 1,000 gains by refusing |
| --- | --- |
| whole pot to one seat (today) | up to the entire pot |
| each seat's buy-in | +1,000 — their losses are erased |
| **each seat's current balance** | **nothing** |

So after every hand, all seats co-sign a fresh refund reflecting the new balances. Refusing to
settle then yields exactly what settling yields, and the only difference is the wait.

## 4. Stale refunds, and the ladder

A seat holds a refund per state. A loser could refuse to sign the new one and broadcast the
**previous** one, which reflects balances from before they lost. This is the payment-channel
revocation problem.

**Solution: decreasing locktimes.** Refund *n* has an **earlier** locktime than refund *n-1*.

```
refund_0 (buy-in)      locktime H + 144
refund_1 (after hand 1) locktime H + 140
refund_2 (after hand 2) locktime H + 136
...
```

The newest refund is always spendable first, so a stale one loses the race. This needs no penalty
mechanism and no revocation keys.

Consequences to accept:

- **The ladder is finite.** With a 4-block step and a 144-block start, a session is at most 36
  hands. Reaching the floor forces a settle-and-reopen, which is fine and should be automatic.
- **A seat that refuses to sign refund *n*** leaves the table holding refund *n-1*. It matures
  later, so an honest seat broadcasting the newer one still wins. The refuser gains nothing and
  waits longer.

## 5. What a seat verifies

Unchanged in shape, different in content. For a refund, a seat requires that it pays **every** seat
the balance the hand history implies, and that its locktime is earlier than the refund it replaces.
For a settlement, that it pays every seat its final balance.

Both are checkable from a seat's own record of the hands it played, which is the property that
matters: a seat never has to trust the table's arithmetic.

## 6. What this changes

| Component | Change |
| --- | --- |
| `cosign.BuildRefund` | multiple recipients with per-seat amounts, and a caller-supplied locktime |
| `PotManager` | one pot per session; re-sign a refund after each hand; ladder the locktimes |
| `LiveTable` | balances are the source of truth; settle on exit, not on showdown |
| agent `signRefund` | verify every seat is paid its balance, and the locktime decreases |
| agent `recordStake` | records a session and its running balance, not a hand |
| UI | show session balance and "cash out", not per-hand settlement |

## 7. What it still does not solve

**A seat can stall the exit.** If a player refuses to sign the final settlement, everyone waits for
the refund's locktime instead. They gain nothing by it — the refund pays the same balances — but
they can impose a delay out of spite.

That is the residual, and it is the same one the upstream project accepts: no honest player loses
funds, and a griefer is not punished. The difference is that here a griefer also **gains nothing**,
which is what makes the game playable against strangers.

## 8. Order of work

1. `BuildRefund` takes recipients and amounts — mechanical, well covered by tests
2. Ladder the locktimes and verify the decrease in `signRefund`
3. `PotManager`: session pot, re-sign after each hand
4. `LiveTable`: balances as source of truth, settle on exit
5. UI: session balance, cash out
6. Tests: refusing gains nothing at any point; a stale refund loses to a fresh one; the ladder floor
   forces a clean reopen
