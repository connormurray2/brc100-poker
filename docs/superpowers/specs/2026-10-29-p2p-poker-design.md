# Peer-to-peer No Limit Hold'em on BRC-100 wallets: design

Status: draft 6 for Connor's approval, 2026-10-30.

History, so the reasoning survives:

- **Draft 1** let an opponent choose your cards and let a loser refuse to pay.
- **Draft 2**'s wallet-held masks (ECPM) were checked by private challenges, which leaked hole cards.
- **Draft 3**'s late posts could catch an honest seat. Its on-chain opening checks were unbuildable, and its remask proof was unprovable.
- **Draft 4**'s pre-signed answers could be voided by twinning the post transaction, and a late post still caught the seat owing the next move. Its uncaught shuffle cheat could read a card, and its fee model ignored mainnet rates.
- **Draft 5** had four more flaws:
  - its kick roll call let a loser void a hand;
  - its referee could not continue a deal on chain past one committed move;
  - committing a show in the same state as the river call leaked the hand before the call was bound;
  - escrow-funded `prepare` and pinned-fee exits could drain the reserve or become unmineable.

Draft 6 answers each of those, and records Connor's decisions of 2026-10-30.

Evidence: the throwaway spikes in `spikes/referee/` and `spikes/deal-cost/` (section 16).

## 1. Intent

Regular BSV users sit down at a poker table from their own BRC-100 wallet and play No Limit Texas Hold'em against
each other for testnet coins. There is no dealer, no house and no game server. The players' clients shuffle and
deal with keys derived from their wallets. The money on the table sits in an on-chain contract that only the
seated players can move.

The promises, in the terms the README must use:

1. **Nobody can read your cards through the deal, with one exception.**
   - Every step carries a proof anyone can check.
   - A player who cheats the shuffle is caught 15 times in 16, and pays at least a fine. A cheater who opens the bad copy loses its whole balance; a rational one stalls instead and pays the fine.
   - The exception: the 1 time in 16 a cheat is not caught, the cheater can learn your hole cards in a hand that someone folds. Folded hands stay private, so that is never discovered.
   - If such a hand reaches showdown, the duplicate shows, the hand is void and the cheater is identified.
   - Cheating loses money on average, and the README says exactly this.
2. **A hand you win is paid even if your opponent stops cooperating, provided the newest agreed state is posted on time.**
   - The project's watcher, or your own client, posts it when it falls due.
   - The next agreed move travels inside the post itself, so it lands on chain with it.
   - If the state is posted late while you are offline, only the move owed after it is exposed:
     - a betting decision you owed counts as a fold;
     - an owed shuffle opening costs a fixed fine;
     - any other deterministic move you owed counts as a fold.
3. **Money on the table can only go to the players at the table, and you can never lose more than you put on it.**
   - The buy-in is the only money you expose, approved in your wallet.
   - Adding more needs another approval.
   - Defending a dispute is paid from reserves inside the escrow, which every seat funded at buy-in.
4. **You can always get your balance back.**
   - If everything stalls, before or during a dispute, the contract pays every seat its balance after a timeout, without anyone's signature.

The limits, stated as prominently as the promises (section 13):

- The code your browser runs can see your own cards and sign for your seat.
- A stalled hand can freeze the table's money on mainnet:
  - up to about three days if the dispute is played out;
  - up to about five if everyone vanishes.
- An opponent who refuses to co-sign but answers every move on chain can push any hand on chain and freeze the table for days, while paying half the fees. Nothing on chain identifies the refuser.
- Colluding players can share information.
- Dispute costs depend on network fees.

The game concept comes from `prof-faustus/bsv-poker`. Its deal is not private: card *i* is `(i+1)·G`, so ratios
survive the shuffle and any peer recovers the deck (reproduced 2026-10-29). None of its cryptography is reused.

Guide: `bsv-blockchain/bsv-ai-toolkit` (`bsv-dev` skill, `@bsv/mcp`). Section 14 maps its guardrails.

## 2. Success: the first release is done when

Connor and a second person, **on a different internet connection**, each using released BSV Desktop on
**testnet**:

1. One creates a room and shares its link, or the other finds it in the lobby.
2. Both sit down and approve the room's testnet buy-in in their wallets.
3. A hand is dealt with no dealer, every proof verifies, and it is played to the end and settled on chain.
4. In a second hand, one player closes the tab. The watcher posts the hand and the other wins the pot on chain.
5. In a third hand, the loser refuses to sign after showdown. The winner is paid on chain through the showdown and the on-chain evaluator.

The run uses testnet constants (short windows, section 10.1) so it completes in a day.

## 3. Goals, non-goals, anti-goals

**Goals:**

- anyone with a BRC-100 wallet can join;
- rooms by link, plus a lobby;
- No Limit Hold'em, 6-max, with fixed buy-in and blinds;
- standard online conduct, with every serverless adaptation documented;
- enforceable outcomes;
- testnet first, designed and reviewed for mainnet.

**Non-goals for now:**

- a mainnet launch (phase 3 review first);
- other variants, tournaments or rake;
- a zero-knowledge shuffle proof (cut-and-choose instead);
- a post-hand reveal of folded cards (section 7.5);
- mobile wallets.

**Anti-goals:**

- any component other than a player's wallet holding long-term keys;
- any output payable to a non-player;
- a creator, relay, host or watcher with powers a seated player lacks;
- promises stronger than the code guarantees.

## 4. Decisions Connor made

- **No game server.** P2P throughout. MessageBox, an overlay and a watcher are fine if they cannot see cards or touch money.
- **Rooms.** Shared by link, with a lobby of rooms that have a seated player. Empty rooms disappear, and the creator has no powers.
- **Game.** 6-max, fixed blinds, No Limit Hold'em only.
- **Inactivity.** Standard rules: timeouts fold, sitting out still pays blinds, and inactive players are eventually removed.
- **Enforceable money.** A Rúnar covenant escrow. Rúnar is built from upstream `main` and vendored.
- **Masks.** Derived from the wallet and held by the page, with non-interactive proofs (chosen over ECPM after review).
- **Freezes.** Long freezes are accepted and documented (section 10.4).
- **Phase 1 enforces everything on chain,** behind go/no-go gates.
- **The buy-in is the limit.** One wallet-approved buy-in per table, and wallet-approved top-ups.
- **Folded hands stay private.** The README states the consequence (promise 1), and a post-hand audit is listed as a later improvement.
- **A missed shuffle opening costs a fixed fine,** not the whole balance (section 10.3).
- **The project runs an always-on watcher by default** (section 5.3).
- **Rúnar bug icellan/runar#178 will be fixed upstream** to unblock the evaluator (gate e).
- **Repository.** Lives in this repository (`connormurray2/brc100-poker`), a personal project.

## 5. Architecture

```
  Player's browser (static app)                     Shared infrastructure
  ┌─────────────────────────────────┐               ┌──────────────────────────────┐
  │ UI (React)                      │◀── signed ──▶│ MessageBox (live + stored     │
  │ Table engine (deterministic):   │   messages    │  messages between identities)│
  │  - NLHE rules and side pots     │               └──────────────────────────────┘
  │  - deal: masks, proofs, checks  │               ┌──────────────────────────────┐
  │  - co-signed state machine      │── adverts ──▶│ Lobby overlay service (ours)  │
  │  - escrow and dispute builder   │               └──────────────────────────────┘
  │  - local watcher                │── states ───▶┌──────────────────────────────┐
  │ Transcript store (IndexedDB)    │               │ Watcher service (ours)        │
  └────────────┬────────────────────┘               └──────────────┬───────────────┘
               │ BRC-100 (WalletClient 'auto')                     │ txs
  ┌────────────▼────────────────────┐               ┌──────────────▼───────────────┐
  │ BSV Desktop                     │               │ BSV Broadcaster               │
  │  root key, signatures, HMACs,   │               └──────────────────────────────┘
  │  coins                          │               On chain: table escrow, dispute
  └─────────────────────────────────┘               referee, checkers (Rúnar)
```

- **Client.** A static SPA: TypeScript, React and Vite, using `@bsv/sdk`, `@bsv/message-box-client` and the Rúnar SDK (browser-shimmed, slim artifacts; G-25, G-32). It talks to the wallet with `WalletClient('auto', originator)` (G-01).
- **Every seat runs the full game** over the same co-signed state sequence (section 8).
- **MessageBox** carries messages: live delivery for play, stored delivery for reconnect.
- **Lobby overlay.** A SHIP/SLAP topic manager and lookup service in this repository, carrying public adverts only. It can hide a room from the lobby, never from its link.
- **BSV Broadcaster** receives every table transaction (G-09, G-13). Money counts at `SEEN_ON_NETWORK` (G-10).

### 5.1 Wallet permissions

`public/manifest.json`, with identical `metanet.groupPermissions` and `babbage.groupPermissions` (G-03):

| Purpose | Protocol / permission | Counterparty | Notes |
| --- | --- | --- | --- |
| Deal secrets (masks, permutations, nonces) | `[1, "brc100 poker deal"]` via `createHmac` | `self` | deterministic, reload-safe (5.2) |
| Transcript signatures | `[1, "brc100 poker transcript"]` | `anyone` | keyID = room ID |
| Escrow signatures | `[1, "brc100 poker escrow"]` | `self` | keyID = room ID + seat epoch |
| MessageBox IDs and encryption | `[1, "messagebox"]` | per peer | |
| MessageBox server auth (BRC-103) | `[2, "auth message signature"]` | MessageBox host | prompts once per host |
| Payouts (BRC-29) | `[2, "3241645161d8"]` | `self` | self-derived payout keys (10.5) |
| Lobby advert basket | basket `brc100 poker rooms` | | PushDrop advert (6) |
| Spending allowance | small monthly | | lobby adverts only |

- Buy-ins and top-ups are explicit wallet spend prompts (10.5).
- Refuse to play unless `getNetwork()` returns `testnet` (G-02).
- Raise the grouped prompt at connect without awaiting it (G-05).

### 5.2 Deriving deal secrets

- **Scalars.** `HMAC(wallet key, domain ‖ room ‖ hand ‖ role ‖ index) mod n`, from `createHmac` with counterparty `self`, rejecting zero.
  - No other party can compute it (G-06), and a reloaded page recovers it.
  - The hand counter is monotonic across void hands.
- **Permutations.** One `createHmac` call seeds a local SHA-256 counter stream, so there is no wallet call per card. An unbiased Fisher–Yates shuffle reads from that stream using rejection sampling, never `mod`.
- **Proof nonces.** `HMAC(wallet key, "nonce" ‖ H(full statement))`, RFC 6979 style, so only a byte-identical re-send reuses one.

### 5.3 The watcher

A small Node service, deployed with the lobby overlay. Each client also runs the same logic locally.

**What it holds:** per table, the newest co-signed state's post transaction, and the move data committed in it (10.2). Every seat sends both as soon as a state is co-signed.

**What it does:**

- posts the newest co-signed state at its rung whenever the segment is unsettled (G-35, G-38);
- submits each committed move on chain when it is owed;
- calls `refund` or `forfeit` when they fall due.

**What it cannot do.** It holds only bytes every seat already agreed, and every outcome is pinned by the contract. So it can delay nothing it does not hold and cannot change any outcome.

**If it goes down,** each player's own client is the fallback watcher, and the README says to keep the table open until the hand settles.

### 5.4 Wallet self-test (before sitting)

- `createHmac` is deterministic for the same input and differs across inputs.
- A transcript signature verifies.
- An escrow `createSignature` over a known digest verifies.

Failure names the missing call.

## 6. Rooms and the lobby

- **Room.**
  - Identified by a random 128-bit room ID.
  - Its terms are fixed when the room is created:
    - buy-in and blinds;
    - 6 seats;
    - the minimum balance to be dealt in (10.6);
    - the network fee rate read from BSV Broadcaster `GET /policy`, with the reserves sized from it (10.6);
    - the creator identity key, creation time and protocol version.
  - The terms are signed by the creator and immutable.
  - Buy-ins are capped (testnet 1,000,000 sats; the mainnet cap is set at the phase 3 review).
- **Seats.**
  - A player asks to sit with a signed message.
  - The button seat proposes the next hand's first state, including requests in arrival order.
  - A seat that disagrees refuses to co-sign, and the request waits.
  - Only seats whose clients are online can sit out (10.5).
- **Lobby advert.**
  - A PushDrop output carrying the signed terms and the seat count.
  - Refreshed by the button seat every 10 minutes and on seat changes.
  - Adverts older than 15 minutes are dropped.

## 7. The deal

### 7.1 Cards and points

Card *i* (0..51) is the first `02 ‖ SHA-256("brc100-poker/card/v1/" ‖ i ‖ "/" ‖ t)`, `t = 0, 1, …`, that decodes to a canonical point. No discrete-log relation between cards is known (DDH).

Every received point must be:

- 33 bytes, compressed, with prefix 02 or 03;
- canonical: x below the field prime, checked **before** parsing;
- on curve and not infinity.

Every proof statement has one canonical byte encoding, defined in the protocol package and used identically off chain and on chain.

That encoding uses **64-byte affine points (`x ‖ y`)**, because Rúnar has no modular square root and cannot decompress a 33-byte point in script. Points travel as 33 bytes on the wire, and every client and the referee record and hash the 64-byte form.

### 7.2 Proofs

Proofs are non-interactive Chaum–Pedersen proofs of equal discrete logarithms over two bases. For `Y = s·X ∧ S = s·G`:

- The prover sends `A₁ = w·G, A₂ = w·X, z = w + c·s`, with `c = H(statement, A₁, A₂)`.
- The verifier checks `z·G = A₁ + c·S` and `z·X = A₂ + c·Y`.

Inputs to every step are points recorded in co-signed state and covered by earlier proofs.

### 7.3 Per hand

Dealt-in seats are `s₁..sₙ`, starting left of the button. Heads-up, `n = 2`, with 9 used positions: 2 + 2 hole and 5 board.

1. **Shuffle chain with cut-and-choose (C = 16).**
   - Each other seat commits to a random value (hash).
   - Seat *sᵢ* computes 16 candidates `Oᶜ = πᶜ(kᶜ·D)` from its input deck `D`, where `Kᶜ = kᶜ·G`. It checks each candidate has 52 distinct canonical points.
   - It commits to `H(D) ‖ H(H(O⁰) ‖ K⁰ ‖ … ‖ H(O¹⁵) ‖ K¹⁵)`.
   - The other seats reveal their values. The used copy is `u = H(values) mod 16`, and `K_used = Kᵘ` and `H(Oᵘ)` are recorded in state.
   - Seat *sᵢ* opens the 15 unused copies (`kᶜ, πᶜ`).
   - Every other seat checks `Oᶜ[πᶜ(i)] = kᶜ·D[i]` for every *i*, batched with a random linear combination.
2. **Remask chain, one move per seat.**
   - Each seat transforms all used positions *p* in one move, with `s_p = r_p · k_used⁻¹`.
   - For each position it publishes `S_p = s_p·G` and `P_p = r_p·G`, with two proofs:
     - (i) `Y_p = s_p·X_p ∧ S_p = s_p·G`;
     - (ii) `P_p = k_used·S_p ∧ K_used = k_used·G`.
   - Publishing `k⁻¹·X_p` and then applying `r_p` separately is forbidden: the next seat could strip it with its own `k`.
3. **Hole cards.**
   - For position *p* owned by seat *O*, the other seats strip in turn, each removing its `r_p` from the previous partial with a proof against `P_p`.
   - Seat *O* removes its own last and reads the card.
4. **Board.** When a street is due, every seat strips each board position in turn, with proofs.
5. **Showdown.** Every live hand is shown. The shown player strips its own mask with a proof.
6. **Card checks.** Every revealed point must be one of the 52 cards, and all revealed cards must be distinct. If not:
   - the hand is **void**: every seat's hand-start balance is restored, across any checkpoints taken during the hand (10.1);
   - every shuffler must then open its used copy (`kᵘ, πᵘ`). The hand is dead, so nothing live is revealed.
   - An opening that fails, or is missed, identifies the cheater (10.3). A void is therefore always attributable.

**Strip rules each client enforces:**

- strip a hole position only for its rightful owner (repeatable after a reload);
- strip a board position only once its street is due;
- strip a shown position only at showdown;
- keep stripping after folding.

**Measured cost** (gate c, `spikes/deal-cost/`). At C = 16:

- building a seat's copies takes about 0.5 s of JavaScript;
- checking an opponent's 15 opened copies takes about 1.05 s;
- proofs take about 30 ms.

That is roughly 1.6 s per seat per heads-up hand. Six-handed it is about 6 s per seat, sequential across seats.

### 7.4 Why masks are not in the wallet

Checking a wallet-held mask (ECPM) without the scalar needs the verifier to send points for the wallet to multiply, and two such paths compose into stripping an opponent's mask. Proofs need the scalar, and the page already sees every unmasked card. So wallet-derived scalars held in the page for one hand change no trust boundary, and they run on released BSV Desktop.

A later ECPM `prove` operation, a Chaum–Pedersen proof computed inside the wallet, would let masks move back.

### 7.5 Folded hands stay private: the consequence (Connor's decision)

The used copy is never opened in a normal hand.

- **The last shuffler** can make positions decode to the same cards: both of your hole cards can be copied into its own hand. It compensates in its remask, so every proof still passes.
- **The first shuffler** can skew the deck's composition, but cannot target positions through the honest permutation that follows.

- **If either player folds,** nothing is ever revealed, and the cheater has learned your hole cards.
- **If the hand reaches showdown,** the duplicate is visible and voids the hand. The forced opening then identifies the cheater.

The deterrent is economic:

- every attempt is caught 15 times in 16 at the opening. A rational cheater then stalls and pays the fine (10.3), rather than opening the bad copy and losing its balance;
- an attempt that reaches showdown is caught there.

The README states this in these words.

**Later improvement:** a post-hand audit that opens every used position after each hand, folded hands included. That would catch every attempt, at the cost of making folded cards public.

## 8. Agreement: co-signed states and committed moves

- **Messages.** Every message is canonical JSON `{v, room, hand, seq, prev, sender, kind, data}`, signed with the sender's transcript key, so any seat can forward it. A re-send after a reload is byte-identical.
- **States.**
  - A state is the dispute referee's state fields plus a transcript hash.
  - A step is agreed only when every seat has co-signed its post transaction (10.2).
  - A client never signs two different states at one `seq`.
- **Committed moves.**
  - When the next owed move is deterministic, the owing seat gives every seat the move's data before anyone co-signs.
  - The data is **inlined in the state's post transaction**, so posting the state also makes that move.
  - Deterministic moves are: commitments, selection values, openings, remasks, strips, shows and acknowledgements.
- **No reveal in the state that records a decision.** After any betting action that ends the betting, the next state is an acknowledgement owed by the next revealer. Its show or strip comes one state later, so the decision is bound on chain before any card is revealed.
  - Board strips are already safe: the first stripper's output reveals nothing.
  - Hole strips, remasks, selection reveals and openings are safe for the reasons in 7.3.
- **Granularity.** A state is taken wherever exactly one seat owes the next input. Heads-up, the deal is about 14 states (remasks batched per seat), plus one per betting action and one acknowledgement before each revealing move.
- **On the clock.** The state machine defines exactly who is on the clock at every state.
- **Gone.** "Gone" is decided by the chain.
- **Reconnect.** All secrets are wallet-derived, and the transcript lives in IndexedDB, on MessageBox and with the watcher, so a returning client resumes from the last co-signed state.

## 9. Gated unknowns

Each is a go/no-go gate in phase 1 (section 15). If one fails, the build stops and Connor decides.

- **The on-chain evaluator.**
  - Its size is fine: 11 KB of script (spike).
  - icellan/runar#175 is fixed by Connor's PR icellan/runar#177, pending merge.
  - **icellan/runar#178 still breaks the evaluator's kicker logic.** We fix it upstream in all seven tiers (gate e).
- **The checker wiring** (10.7) and the real size of a full prepare-plus-fraud pair.
- **Mining multi-megabyte dispute transactions.** Policy was read on 2026-10-29/30 (BSV Broadcaster, testnet and mainnet: 1 sat/kB, 100 MB scripts), but nothing that size has been mined.
- **BSV Desktop's foreign-input buy-in flow** (10.5).
- **The fee rate.** On 2026-10-30, mainnet BSV Broadcaster `/policy` reported 1 sat/kB. The toolkit's facts (verified 2026-10-22/23) record 100 sat/kB on every mainnet datahub, and 100 sat/kB as BSV Desktop's default.
  - The design reads `/policy` at table creation and sizes reserves from it with a margin (10.6).
  - The conflict is flagged for the toolkit to re-verify.

## 10. Money: the table escrow and the dispute referee

Shape A throughout. Two Rúnar artifacts per seat count (2..6; heads-up first):

- **The table escrow** (small, phase 0): `settle`, `post`, `refund`.
- **The dispute referee** (phase 1): `act`, `fold`, `move`, `advance`, `prepare`, `fraud`, `forfeit`, `payout`, `cancel`, `kick`, `timeoutRefund`.

Each embeds the others' code hashes in excised constructor slots (`computeTemplateHash`).

Rules for every contract:

- **Full value pinned.** Every continuation carries the escrow's full value, minus pinned fees: `addOutput(extractAmount(preimage), …)`.
- **Explicit continuations.** Continuations are always passed explicitly, and state is re-read from the broadcast output.
- **`ALL|FORKID` only.** No `SINGLE|ANYONECANPAY`, so icellan/runar#176 does not apply.
- **Hand-written output checks.** The settle and buy-in output checks walk the supplied outputs by hand (CompactSize). Rúnar's auto-injected continuation hash covers only continuation-plus-change.

### 10.1 Segments, the ladder, the refunds

- **Segments.** A segment runs between two on-chain settlements: the deal, or a street.
  - The escrow state records the segment-start balances.
  - It also records the **hand-start balances**, carried across checkpoints, so a void returns the whole hand's contributions.
- **The ladder.** The state at `seq = k` may be posted only at `nLockTime = anchor + (K − k)·W`, so the newest state becomes postable first (G-35).
- **Per-network constants:**

  | Network | `W` | `K` per segment | `anchor` |
  | --- | --- | --- | --- |
  | Mainnet | 6 blocks (the toolkit's measured defence window) | 16 for the deal, 24 per betting segment, with a forced checkpoint at `K − 4` | co-signed segment start height + 6 |
  | Testnet (success run) | 2 blocks | 8 for the deal, 12 per betting segment | |

  Testnet blocks are irregular, so the testnet windows are for acceptance only.
- **`refund()`** (phase 0):
  - Anyone can call it, with no signature, at `nLockTime ≥ refundHeight`, where `refundHeight = last rung + 2W`.
  - It pays **hand-start** balances and voids the hand, with outputs pinned. A void, `refund` and `timeoutRefund` all return the same balances.
  - It accepts an external fee input and trailing change, so it survives any fee rate.
  - An idle table rolls its escrow before `refundHeight − W`.
- **`timeoutRefund()`** (phase 1):
  - Anyone can call it, with no signature, at a far height fixed when the post is made: post height plus `4K·W`.
  - It returns hand-start balances, minus fees charged to whichever seat is on the clock.
  - Like every anyone-can-call terminal path (`forfeit`, `payout`, `cancel`), it accepts an external fee input and trailing change, so it survives any fee rate.
- **"Segment balance" in penalties** means the seat's hand-start balance.
- **Anchor drift.** Clients refuse to sign states over a settlement that is not yet `SEEN_ON_NETWORK`, so the anchor cannot drift.
  - This is the exit promise 4 relies on once a dispute has started (G-20, G-38).

### 10.2 Posting, and what a late post can still do

Script cannot read the height, so nothing stops a post being broadcast late, and every deadline after it is computed from its lock time.

**The defence: post the newest state at its rung.** The watcher and each client's local watcher do this automatically. A state posted at its rung gives the seat on the clock `W` blocks.

**After a post, the committed move is already on chain.** Its data is inlined in the post transaction, so a late poster cannot separate the post from the move and race it with a `forfeit`.

**What a late post can still catch is the one move owed after the newest co-signed state.** If its seat is offline:

| Owed move | Penalty |
| --- | --- |
| Betting decision | fold |
| Shuffle opening or selection reveal | the fixed fine (10.3) |
| Any other deterministic move | fold, plus fees |

That is why promise 2 is conditional on posting on time, and why the watcher runs by default.

**Posts are funded from the escrow.**

- The covenant enforces `fee ≤ cap`, a fixed satoshi cap per method, computed at four times the table's `/policy` rate for that method's maximum size.
- Within the cap, the co-signers choose the actual fee at signing from the current `/policy`.
- If fees rise above the cap, every exit still works with an external fee input (10.1).

### 10.3 The dispute referee

Posting migrates the escrow into the dispute referee. The deadline is `nLockTime + W`, and each on-chain move pushes it out by `W` on a fixed schedule. `prepare` and `fraud` are callable by anyone at any time and do not move the schedule.

| Owed move | On chain | Checked by the contract |
| --- | --- | --- |
| Betting action | `act(kind, amount)` signed by the mover | turn; call, raise and all-in amounts; minimum raise; street completion |
| Fold | `fold()` | terminal, payout pinned |
| Committed deterministic move | inlined in the post | format checks (below) |
| Uncommitted deterministic move | `move(data, sig)` by the owing seat | the owing seat's signature over the transaction; format checks; the fraud window applies |
| Street due | `advance()` by anyone | every seat's strip answers for the street on record |
| Hand complete | `payout()` by anyone | after a fraud window of `W`: the evaluator, card membership and distinctness; payout pinned |
| Card check fails | `cancel()` by anyone | membership or distinctness provably fails; void; owed used-copy openings follow |
| Missed deadline | `forfeit()` by anyone | betting: fold. Opening or selection reveal: the fine. Other: fold plus fees |
| Proven fraud | `prepare` + `fraud` by anyone | the cheater forfeits its hand-start balance to the others; the hand is void |

**Format checks at record time.** All recorded proof data must be well formed, or the referee refuses it, so checkers only ever run on valid input:

- every point 33 bytes, compressed, with prefix 02 or 03, x below p, on curve;
- every scalar in `[1, n)`;
- every permutation a permutation.

**The fixed fine:** `ceil(hand-start balance of the stalling seat / 15)` plus the dispute fees.

- Heads-up, a cheat can win at most the cheater's own stack, so stalling instead of opening costs more than the cheat is expected to gain. The margin is only the fees.
- Phase 2 scales the fine with the number of seats (`ceil((n − 1) · balance / 15)`), because at 6-max a cheat can win up to five stacks.
- An honest offline seat loses the fine, not everything.
- An on-chain forfeit counts as a timeout for the sit-out and removal rules (11), so a griefer cannot stall hand after hand.

**`prepare` is caller-funded and bonded.**

- The caller supplies an external fee input and posts a bond.
- A successful `fraud` returns the bond and reimburses the caller from the cheater's balance.
- A checker that lapses unspent forfeits the bond.
- `prepare` may only target the most recently recorded move, once per move.

**Referee state size.** Each recorded move's proof data stays in state until its fraud window closes, so the state grows to about 15–20 KB heads-up by showdown. That is priced in 10.6.

### 10.4 Freeze bound

Heads-up deal segment, with remasks batched (about 14 states):

- 2 shuffle commitments;
- 2 selection commits and 2 reveals;
- 2 openings;
- 2 remasks;
- 4 hole strip rounds.

**Worst case on mainnet:**

- a stall at the first state of a 24-state betting segment waits `6 + 23 × 6 = 144` blocks before the post;
- then the adversary can take every move it owes to the last block of its window: up to about 24 moves × 6 = 144 blocks;
- then a fraud window.

That is about 300 blocks: **up to about three days if the dispute is played out**, usually far less.

If everyone vanishes, `timeoutRefund` fires at post plus `4K·W`, which comes to about 720 blocks: **about five days**.

Six-max is longer (phase 2). Testnet constants shrink all of this to hours.

**Minimum balance to be dealt in:** 20 big blinds, and at least the largest dispute cost at the table's fee rate (10.6). This stops a near-empty seat being a disposable cheater.

### 10.5 Buy-ins, settlements, payouts, removal

- **Buy-in and top-up.**
  1. The client calls `createAction` with the escrow as a foreign input, `signAndProcess: false` and `randomizeOutputs: false`, adding the player's coins and the continuation.
  2. The wallet shows the spend, and the player approves.
  3. The client collects every seat's escrow signature and completes with `signAction` (G-22).

  Wallet-toolbox keeps caller inputs and outputs first; gate (b) checks BSV Desktop does too. The covenant checks the payer's balance against the continuation value minus the old value plus the fee, and every other seat's balance unchanged.
- **`settle`.** Every seat signs `ALL|FORKID`.
  - A pinned prefix of outputs must carry the continuation, then payouts to leaving seats' registered payout scripts.
  - The check is a prefix compare (`left(serialisedOutputs, len(prefix)) === prefix`). No CompactSize walk is needed, because every pinned output has a known length.
  - Any trailing outputs (wallet change, a wallet commission) are ignored. That defends a seat only against its own client, which 13.1 already concedes.
  - It runs at every hand end and checkpoint.
- **Payouts.**
  - Paid to P2PKH keys each player derives for itself under BRC-29, registered at buy-in.
  - At least 546 sats, or the balance goes to the fee.
  - Each client checks its own payout (`forSelf: true`) before signing (G-21).
- **Removing an absent seat.** The escrow's `kick` path:
  - Any seat can post a roll call naming a seat, **but only after the whole ladder**: at `nLockTime ≥ anchor + K·W`, with a non-final sequence. Every real state is postable strictly earlier, so a roll call can never displace the newest state of a hand in progress.
  - The roll-call fee is charged to the caller's balance.
  - The named seat must answer with a signed acknowledgement within `W`.
  - If it does not, it is paid out at its hand-start balance and removed, and the table continues.
  - Sitting out is only for online clients. An offline seat is kicked, not charged blinds it cannot co-sign.

### 10.6 Fees and reserves

- **Rate.** The table reads `/policy` when the room is created and fixes it in the terms. Every escrow-funded fee is pinned at four times that rate.
- **Reserve.** Each seat's buy-in includes a dispute reserve inside the escrow, sized to cover the largest dispute at that rate: about 3.5 MB of transactions for one fraud proof, plus a full on-chain hand. At 1 sat/kB that is about 10,000 sats; at 100 sat/kB about 1,000,000.
- **Who pays.** At resolution, a seat that forfeits or is proven to cheat pays every dispute fee from its balance. A dispute that ends with every move made splits its fees equally, which also covers fee griefing.
- **Checker size.** Checkers are split so no single script exceeds what gate (a) shows is mined.
  - A one-`ecMul` step is at least 428,676 B before checks and hashing. Whether it stays under the stock 500 KB policy must be measured.
- **Template bytes.** `prepare` must produce each checker's exact locking script, so the caller supplies each template in the unlocking script and the referee splices the statement in.
  - A four-step `prepare` therefore carries about 1.75 MB of templates plus about 1.75 MB of outputs: about 3.5 MB.
  - Likewise a `post` carries the referee's full script, with its evaluator, as its continuation: about 200 KB, not the spike's 13 KB.
  - Gate (a) measures all of these, and the reserve is re-sized from the measurements.

### 10.7 Checker wiring

- **The checkers.** Constant-script, stateless Rúnar contracts, each created by the dispute referee as output 1 of a `prepare` transaction, with its statement baked into the locking script.
- **The binding.** The referee's `fraud` requires input 1 to be `(own previous txid, 1)`, via `extractOutpoint` and `extractHashPrevouts`. That binding is verified feasible against Rúnar's companion-verifier pattern.
- **Broadcast as a pair.** `prepare` and `fraud` are always broadcast together, so the checker output cannot be raced. A `prepare` whose checker is never spent leaves the referee unchanged.

| Checker | Script | Cost |
| --- | --- | --- |
| Chaum–Pedersen inequality | split into one-`ecMul` steps, intermediates checked with `ecOnCurve` and `z ≢ 0` | about 3.5 MB in total |
| Opening element `kᶜ·D[πᶜ⁻¹(j)] ≠ Oᶜ[j]` | one `ecMul` | about 0.9 MB with its template, split if gate (a) requires |

## 11. Conduct rules

- **Clock.** 30 s per decision (proposed). A live client folds an idle player (checks if free) and co-signs.
- **Sitting out.** After 3 consecutive timeouts, for online clients only. An on-chain forfeit counts as a timeout. Blinds are still deducted each orbit, and the seat is removed after 2 orbits.
- **Gone.** The hand goes on chain (10.2, 10.3), and the table's money is frozen until it resolves (10.4). A seat absent between hands is kicked (10.5).

## 12. What is enforceable, for players

| Situation | Outcome |
| --- | --- |
| Opponent stops answering mid-hand | The watcher posts on time; committed moves are submitted by anyone; a betting stall folds them; you win by forfeit |
| Opponent loses and refuses to sign | You post the hand; their show is committed; `payout` pays you |
| Opponent cheats an unused shuffle copy | Caught 15 in 16. A rational cheater stalls and pays the fine; opening the bad copy would cost their whole balance |
| Opponent cheats the used copy (1 in 16) | May learn your hole cards in a hand someone folds; at showdown the duplicate voids the hand and identifies them |
| A player vanishes between hands | Kicked after one full ladder plus a window, and paid their balance |
| Opponent refuses to co-sign but answers on chain | The hand is played on chain and the table frozen for days; fees are split |
| Everyone vanishes | A refund pays every balance after its timeout, before or during a dispute |
| You are offline and a state is posted late | A betting decision you owed folds; an owed opening costs the fine |
| Two or more opponents collude | They can share hole cards and dump chips |

## 13. Trust model (README, `docs/trust-model.md`, `docs/what-happens-when.md`; G-34)

Written for mainnet.

1. **The code you run.** It can see your own cards and sign anything for your seat: fold your hands, play badly, or co-sign a settlement moving your balance to another seat it controls. The covenant and the buy-in cap bound the loss at your table balance. Pinned releases let a player run a known build.
2. **Freezes.** Up to about three days on mainnet if a dispute is played out, and about five if everyone vanishes (10.4). An opponent can force this on any hand at half the fees.
3. **Liveness.** If the watcher is down and you are offline when a state falls due, you can lose the one move owed after it: a betting decision or another deterministic move (a fold), or an opening (the fine).
4. **Folded hands stay private** (7.5). An uncaught cheat can learn your hole cards in a folded hand.
5. **Collusion and Sybils.** Identities are free, so penalties are economic.
6. **The relay, lobby and watcher** can delay messages, hide rooms or post late. They cannot change outcomes or see cards.
7. **New code on chain.** The escrow, referee, checkers and evaluator need cold review before mainnet (G-30). Rúnar rc.1 has had silent miscompiles (icellan/runar#175, #178), so every contract is tested differentially through real `Spend`.
8. **Fees.** Disputes cost about 10,000 sats at 1 sat/kB and about 1,000,000 at 100 sat/kB. Reserves are sized at table creation. A fee rise beyond the 4× margin leaves the timeout refund as the exit.
9. **Script policy.** Checker steps are about 0.45 MB each, accepted by BSV Broadcaster today. Gate (a) must see the full set mined.
10. **0-conf.** Money counts at `SEEN_ON_NETWORK`; miner collusion is out of scope (G-14).

## 14. Guardrails map

| Guardrail | Where it is met |
| --- | --- |
| G-01/02/03/04/05 | 5.1 |
| G-06 | `createHmac` with counterparty `self` (5.2) |
| G-07/08 | sign only digests rebuilt from checked state |
| G-09..16 | BSV Broadcaster for every transaction, provisional 2xx, the two-read dead rule, timeouts |
| G-17 | write-ahead storage of states, committed moves and transactions |
| G-20/38 | `refund` and `timeoutRefund` in script, the watcher |
| G-21 | 10.5 |
| G-22 | 10.5 |
| G-25 | Rúnar from `main`, vendored and pinned (with #177 and the #178 fix) |
| G-26/36 | height gates assert `< 500,000,000` with non-final sequences |
| G-27 | no script ECDSA |
| G-28 | real `Spend`, differentially |
| G-29 | size and fee gate |
| G-30 | cold reviews |
| G-31/32 | 5 (versions and browser build) |
| G-33 | gate script (15) |
| G-34 | section 13 |
| G-35/37 | ladder, full value pinned, `ALL|FORKID` |

## 15. Phases and gates

**Phase 1: heads-up, end to end (the success bar).** Gates first, each go/no-go:

| Gate | What it must show |
| --- | --- |
| (a) | Mine on testnet, from a funded key, every transaction kind at full size: a late post carrying its inlined committed move, an uncommitted signed `move`, a full `prepare` + `fraud` pair built by the real referee, the refunds. Also read `maxtxsizepolicy`. |
| (b) | BSV Desktop's foreign-input buy-in with `randomizeOutputs: false`, with output ordering checked. |
| (c) | Deal cost measured (done: about 1.6 s per seat per hand). MessageBox latency and fees. |
| (d) | The checker wiring accepts a real fraud proof and refuses a "fraud" claim against an honest answer. |
| (e) | Fix icellan/runar#178 upstream, then the on-chain evaluator agrees with the reference over a large random corpus through `Spend`. |

Then build the protocol package, the contracts, the client, the lobby and the watcher. A testnet gate script (G-33) exercises every transaction kind with headless `@bsv/wallet-toolbox` wallets before UI work.

**Phase 2: up to 6 seats.** It covers:

- n-way contracts, side pots on chain, and seat-count migration;
- sequential 6-max strips and their freeze bound;
- a review of multi-seat `C` and collusion;
- **the fine scaled with seats** (10.3);
- **a real penalty for a folded seat that stops stripping.** Today a folded colluder has nothing left to forfeit, and its stall ends in `timeoutRefund`, rescuing its partner.

**Phase 3: mainnet readiness.** Cold reviews, a policy and fee re-check, the buy-in cap, a trust-model review, and reconsidering the post-hand audit.

## 16. Spike evidence (throwaway)

**`spikes/referee/`:**

- A heads-up referee (3,849-byte script), with every path tested through real `Spend` with BRC-100-style signatures.
  - Post or act is about 13.4 KB; forfeit or close about 4.8 KB.
- A four-`ecMul` check is about 1.75 MB and validates in about 0.8 s.
- A 7-card evaluator is 11 KB, blocked by Rúnar miscompiles: #175 is fixed in #177, and #178 is open.
- The spike's `SINGLE|ANYONECANPAY` post hit #176, which draft 5 avoids.

**`spikes/deal-cost/`:** the deal cost measured above.

**Not exercised:**

- a broadcast;
- BSV Desktop signatures;
- streets and showdown on chain;
- checker wiring;
- more seats.

## 17. Code layout and reuse

- **Layout:**
  - `app/`: the client;
  - `packages/protocol/`: cards, deal, proofs, states, NLHE engine and evaluator, transaction builders;
  - `contracts/`: Rúnar sources, artifacts, and the vendored Rúnar with provenance;
  - `services/lobby-overlay/`;
  - `services/watcher/`;
  - `scripts/testnet-gate/`.
- **Ported from the Go code:** the NLHE engine and the hand evaluator (checked over all 2,598,960 five-card hands). Vectors from Go come first, and the TypeScript port and the on-chain evaluator are tested against them.
- **Retired:** tag the current head `go-table-final`, then remove the Go table, agent and old docs from `main`. Delete the spikes once phase 1's contracts replace them.

## 18. Calls I made (Connor to check)

- Committed move data inlined in each state's post, replacing draft 4's pre-signed answer transactions (twinnable), plus a signed `move` method for uncommitted moves.
- An acknowledgement state before any revealing move that follows a decision.
- A roll call only after the whole ladder, with the fee charged to the caller.
- A caller-funded, bonded `prepare`; external fee inputs on every exit; and post fees capped rather than fixed.
- 64-byte affine points in every hashed statement.
- Opening the used copy after a void, so a void always identifies its cheater. This reveals only a dead hand's cards and is consistent with keeping folds private.
- The fine formula `ceil(segment balance / 15)` plus fees, and a longer window for openings than for betting.
- Testnet (not teratestnet), with the per-network constants in 10.1.
- Fees pinned at four times the table's `/policy` rate, with reserves and minimum balance sized from the largest dispute.
- The phase-1 `timeoutRefund`, the `kick` roll call, and sit-out only for online clients.
- The watcher deployed with the lobby overlay, holding only co-signed bytes and committed moves.

Proposed constants are Connor's to change: clock, timeouts, `C`, `K`, `W`, the fine divisor, the fee margin, the reserve, the minimum balance and the buy-in cap.
