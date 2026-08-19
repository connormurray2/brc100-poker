# Resolved parameters and deferred scope

The proposal left four parameters open and named features it deliberately did not build. This
records what was decided and what a follow-up would actually involve.

## Resolved parameters

### Refund locktime: 144 blocks

`POKER_REFUND_LOCK_BLOCKS=144`, roughly a day at mainnet block rates.

The tension is real in both directions. Too short and a refund races the settlement the seats just
agreed to — a seat could stall deliberately, wait for maturity, and reclaim its stake from a pot it
had already agreed to lose. Too long and a griefed player waits days to get their money back.

144 blocks is chosen because it is long enough that no honest settlement could still be in flight,
and short enough to describe to a player as "about a day". It does **not** scale with seat count:
the risk is a single uncooperative seat, and that risk does not grow with table size.

On teratestnet, where block production is intermittent and sometimes stalls entirely, 144 blocks
may be considerably longer than a day in wall-clock terms. That is a property of the test network,
not of the design, and it is why the number is configuration rather than a constant.

### Fees: the table underwrites them

The table service pays transaction fees from its own wallet, and the pot pays out in full.

The alternative — each seat contributing its share of the fee — was rejected because it makes the
settlement's output set depend on arithmetic every seat must reproduce identically. Every seat
already has to agree on the output set exactly, since signatures commit to it, and adding a
per-seat fee split multiplies the ways that agreement can fail for no benefit to the player.

The cost to the operator is a few hundred satoshis per hand. The benefit is that a seat's
expectation is simply "the winner receives the pot, less the declared fee", which is checkable
without reproducing a division.

**Consequence:** the table's wallet must stay funded, and a table that runs out of fee money cannot
settle. That is an availability problem, not a custody one — players' refunds are unaffected — but
it needs monitoring.

### Buy-in and blinds: 5,000 / 25 / 50 satoshis

`POKER_BUY_IN_SATS=5000`, `POKER_SMALL_BLIND=25`, `POKER_BIG_BLIND=50`.

A 100-big-blind stack, which is the conventional starting depth and makes the betting engine's
behaviour recognisable to anyone who plays poker. The absolute numbers are small enough that one
faucet claim (100,000 sat) funds twenty hands, so a demo does not spend its time claiming coins.

### Browser client: the agent's local port

A browser talks to the player's own agent over `127.0.0.1`, and the agent speaks the substrate.

The alternative — a browser extension implementing the substrate directly — is strictly better for
usability and was not chosen because it is a second protocol implementation before the first one
has been exercised. `docs/substrate-protocol.md` is written so that extension can be built against
a protocol a working client already proves.

The agent binds to localhost by default. Exposing it more widely means publishing a signing
endpoint, which is why `-require-tls` exists and why the default is not `0.0.0.0`.

## Deferred scope

The proposal scoped this to one vertical slice: a single Texas Hold'em table playing a complete
hand for real value. Everything below was named as out of scope, and this is what each would
actually take.

### The other five poker variants

Upstream advertises six variants. Two obstacles, in order:

1. **The two game stacks must be reconciled first.** Upstream has a `Variant` enum wired into live
   play and a separate `PokerGame`/`GameDef` table with the better evaluator and hi-lo support but
   no betting engine. We took the live engine plus the better evaluator; the variant *table* was
   not ported. See `docs/port-decisions.md`.
2. **The evaluator already handles the hard part.** `BestConstrained` implements the
   exactly-two-hole-cards rule the Omaha family needs, at every street. Adding Omaha is mostly
   configuration: hole-card count, the `UseHole` constraint, and deck size.

Lowball variants additionally need the A-5 evaluator, which upstream has and we did not port
because nothing in a Hold'em-only slice uses it.

**Not worth porting as-is:** upstream's Pineapple ships without its defining post-flop discard
rule, and its own help text quietly redefines the game. Reintroducing it means implementing it.

### Group blackjack

A different game with a different money shape: a communal dealer computed jointly, and a pot that
pays out hand after hand rather than once. The mental-poker layer transfers directly. The
settlement layer does not: a table that pays out repeatedly cannot use a single n-of-n pot with one
settlement, and would need either a chain of pot outputs or a channel-style running balance.

### Encrypted chat

Upstream's chat is not what its documentation claims. The docs describe a `ChatService` that does
not exist, and describe fresh ephemeral ECDH per message; the code uses a **static** long-term ECDH
between identity keys plus a counter, deliberately, so history stays recoverable. There is no
forward secrecy.

Building this means deciding which of those two designs is wanted, not porting. If forward secrecy
matters, it is new work.

### Card NFTs

Upstream binds card data with `OP_DROP` and never `OP_RETURN`, one satoshi per card. The design
ports cleanly to BRC-100 baskets, with one caveat that the co-signing spike already established:
a **basket-inserted coin records no derivation material and cannot be signed by the wallet**. Cards
held as baskets would be visible and unspendable, which may be exactly right for a collectible and
is exactly wrong if they are ever meant to move.

### On-chain hand tape and replay

Upstream writes a hand as a chain of transactions, roughly twenty per hand, and replays it move by
move. This is the feature most affected by our transport change: we replaced a
re-broadcast-everything loop with targeted catch-up, and the tape assumed the former.

The pot ledger already records the money side. A tape would additionally need the *game* messages
durably ordered, which the session layer tracks in memory but does not persist.

### Multi-table and tournaments

Out of scope and not attempted. One note from the vertical slice that matters here: a table's
funding wallet is a shared resource, and the phantom-change failure mode described in the runbook
gets more likely, not less, as more tables share one wallet. Coin consolidation would need solving
before running many tables from one key.

## Known limits of what was built

Stated plainly, because a demo that oversells is worse than one that does not exist.

- **Every seat must be online to settle.** Inherent to a non-custodial n-of-n pot. A seat that
  disappears costs everyone a wait, not their money. A threshold scheme would relax this and is
  materially more complex.
- **The table can stall a hand.** It cannot steal from one.
- **Two seats is what has been exercised for real value.** n-of-n is verified for 2..6 seats
  through the script interpreter, but only a heads-up hand has settled on-chain.
- **The toolbox is `v0.1.0`**, and its `createAction`/`signAction` path — which this design leans
  on hardest — has no executing conformance coverage upstream despite 98 vendored vectors. Our own
  integration tests are the real coverage.
- **Arcade is a single point of failure** for settlement. No fallback, no multi-arcade HA.
- **BRC-100 identity discovery is unusable on teratestnet**: it resolves to the public testnet
  overlay and certifier set. Player identity comes from the game protocol instead.
