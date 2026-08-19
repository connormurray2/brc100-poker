# Port decisions

Decisions made while porting `prof-faustus/bsv-poker` (C#/WPF) to Go. Recorded here because
several of them delete real, working upstream code and the reasoning should outlive the commit.

## The two game stacks (task 6.1)

Upstream contains two parallel, non-communicating implementations of "a poker game":

| Stack | Wired into play? | Evaluator | Hi-lo / lowball | Betting engine |
|---|---|---|---|---|
| `Variant` enum (`Variant.cs`) | **Yes** — live engine, networked play | `HandEval.cs` | no | **yes** (`HoldemEngine`) |
| `PokerGame` / `GameDef` (`Games/PokerGames.cs`) | No — reachable only from test-only on-chain helpers | `Games/PokerEval.cs` (better) | yes (`LowEval.cs`) | **no** |

Neither matches the variant list the upstream docs and `REBUILD_SPEC.md` advertise.

**Decision: carry forward the live stack's engine, and the dormant stack's evaluator.**

- Keep `HoldemEngine`'s betting, side pots and showdown: it is the only betting engine that
  exists, and it is genuinely good (real layered side pots by distinct commitment level).
- Keep `PokerEval` as the evaluator rather than `HandEval`. `PokerEval` returns an explicit
  category, guards short input, and does not carry `HandEval`'s bugs:
  - `HandEval.Best` returns `Score = -1` instead of failing on fewer than five cards, because
    its loop never executes.
  - `HandEval.BestForVariant` is **wrong pre-river for Omaha**: its hardcoded three-deep board
    loop still runs against a three- or four-card board. `Showdown.BestConstrained` is the
    generic, correct version.
- Drop the `PokerGame`/`GameDef` table itself. Its value was the evaluator and the hi-lo split;
  the vertical slice is Texas Hold'em only, so the game table is scope we are not shipping.
- Keep `LowEval` unported for now. It is correct (verified upstream against all 2,598,960
  five-card hands, including the 8-or-better flag) but nothing in a Hold'em-only slice uses it.
  Port it when a lowball variant is actually in scope.

## Variants in this slice

Texas Hold'em only. The card model keeps the generic hole-card count and the
"must use exactly two hole cards" rule as explicit parameters, because those are what make the
other community-card variants a configuration change rather than a rewrite — but no other
variant is wired up or tested here.

`Pineapple` is deliberately not carried forward even as a name: upstream ships it without its
defining post-flop discard rule, and its own help text quietly redefines it as "3 private cards,
use any." Reintroducing it would mean implementing it, not porting it.

## Deliberate non-ports

- **The whole chain layer.** Transaction serialisation, FORKID sighash, P2PKH and multisig
  signing, script engine, Base58Check, RIPEMD-160, and the 18-file BSV P2P node with header sync
  and bloom-filter SPV. BRC-100 supersedes all of it. Retiring `Chain.SighashForkId` also retires
  a real risk: it has no known-answer vector anywhere in the 9,915-LOC upstream test suite, so a
  wrong-but-self-consistent sighash would have passed everything and been rejected by every node.
- **`PeerDiscovery.cs`** — sweeps a /24 with ~762 concurrent TCP connects every four seconds plus
  a shared `/tmp` rendezvous file. On a hosted droplet this is port-scanning.
- **`Profile.cs`** — a process-wide static file lock (so a server would get exactly one profile),
  a hard 64-instance cap, desktop-only paths, and the raw 32-byte identity seed written to disk in
  plaintext.
- **The 120 ms re-broadcast-everything loop** in `NetGame`/`NetBlackjack`. It is a polling
  protocol dressed as an event-driven one, tuned empirically until multi-seat deals stopped
  stalling. Replaced by explicit acknowledgement and targeted catch-up.
- **`BotPolicy`** — 30 lines that never read the bot's own cards. Treat as greenfield if wanted.
- **The WPF app and installer.**

## Ported with a fix, not as-is

- Evaluator short-input handling: fail, do not return a sentinel score.
- Omaha pre-river board handling: use the generic constrained search.
- `ShuffleProof` is wired **into live play** rather than left as a post-hoc audit. Upstream builds
  and tests it but never calls it from the live game, which leaves a malicious shuffler
  unconstrained — unacceptable once real value is on the table.
