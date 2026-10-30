# Gate c: deal cost and MessageBox

Status: **PASS** (2026-10-30). Pass = median round trip <= 2 s and every quoted fee 0. Measured: median 121 ms, all fees 0.

Seats exchange every deal message through MessageBox, so this number sets the pace of the game. Gate c has two
halves: the cost of the deal arithmetic, and the transport.

## MessageBox latency and fees

Harness: `npm run gate:c -w scripts/gates` (`scripts/gates/src/messagebox-latency.ts`; bookkeeping in
`latency-stats.ts`, unit-tested in `test/latency-stats.test.ts`).

- Host: the client's default, `https://message-box-us-1.bsvb.tech` (printed by the run).
- Client: `@bsv/message-box-client` 2.7.0, with a single `@bsv/sdk` 2.8.11 in the tree (nothing needed deduping).
- Identities: two `ProtoWallet`s from random `PrivateKey`s, created per run and never saved. A `ProtoWallet` cannot
  fund a payment, so a nonzero fee would have failed the sends; none did.
- Boxes: `brc100-poker-gate-c` (live) and `brc100-poker-gate-c-stored` (stored).
- Mode: **live** (authenticated WebSocket). Zero sends fell back to HTTP (the script counts them, because
  `sendLiveMessage` falls back silently).

### Live round trip

A sends 50 numbered pings over `sendLiveMessage`, one at a time; B echoes each; RTT is timed at A.

| | n | min | median | mean | p95 | max |
|---|---|---|---|---|---|---|
| Round trip at A (ms) | 50 | 115 | **121** | 123 | 146 | 163 |
| One way A to B (ms, same-process clock) | 50 | 57 | 60 | 62 | 81 | 90 |

Every sequence number arrived exactly once each way: 50/50 pings, 50/50 echoes, no loss, duplicates or strays.
An earlier 3-message smoke run gave a median of 127 ms.

### Stored messages

5 sent with `sendMessage`, listed with `listMessages`, acknowledged with `acknowledgeMessage`: 5/5 listed, none
lost or duplicated, 0 remain after the acknowledgement. Each HTTP call costs about 0.34 s (send median 341 ms,
list 341 ms, acknowledge 341 ms), against about 0.06 s one way over the live socket.

### Fees

`getMessageBoxQuote` for A to B, on both boxes: `deliveryFee` 0, `recipientFee` 0. All fees are zero, so no
reserve is needed for MessageBox.

### Caveats

- One machine, one run of 50, sequential (one message in flight). It shows the floor, not behaviour under load or
  from a distant seat. The margin is 16x, so the verdict is not close.
- Live sends are also stored on the server: after the run, 50 messages sat unacknowledged in each inbox. The game
  client must acknowledge what it handles, or the box grows. (The README also says polling `listMessages` is
  required for correctness, since a live push only reaches sockets held by the server process that took the send.)
- Quotes are for a fresh pair of identities with default permissions. A real table could set per-sender fees; the
  gate does not cover that.
- The client's default host is the mainnet deployment (`DEFAULT_TESTNET_HOST` is the same URL). The messages carry
  no money.

## Deal cost, already measured

From `spikes/deal-cost/bench.mjs` (not committed): one point multiplication in JavaScript is about 0.63 ms, and a
heads-up hand costs about **1.6 s per seat** at C = 16 cut-and-choose copies. The network adds little on top of
that: a hand's deal messages at about 0.06 s each way are small next to 1.6 s of arithmetic per seat.
