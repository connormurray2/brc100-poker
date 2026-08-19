# Playing a hand

You keep your own key. The table coordinates play and pays its own fees, but it never holds
your key and never holds the pot alone — so it can stall a hand and it cannot take your money.

## 1. Generate a key

```sh
go run ./cmd/keygen -out secrets/player.key
```

Prints your identity key and a teratestnet address. The private key is written to the file with
mode 0600 and never to stdout, so it cannot end up in a terminal transcript.

**Back up both the key and the wallet database.** The database *is* the wallet: the toolbox has
no UTXO discovery and no restore-from-seed, so a lost database means unspendable coins even with
the key in hand.

## 2. Fund it

```sh
go run ./cmd/fund -key secrets/player.key -db secrets/player.db
```

Claims 100,000 sat from the teratestnet faucet. The faucet pays BRC-29, so this does two things:
it asks the faucet for a payment derived from your identity key, then internalizes the result as
a *wallet payment* with its derivation material. Without the second half the coin exists
on-chain and your wallet cannot spend it. See `docs/funding.md` for the detail.

Alternatively, send yourself a payment from [BSV Desktop](https://bsvblockchain.org) on
teratestnet.

## 3. Run your agent

```sh
go run ./cmd/agent \
  -key secrets/player.key \
  -db secrets/player.db \
  -listen 127.0.0.1:8091 \
  -table <the table's identity key>
```

It prints your identity key, your balance, and the substrate audience. Give the table operator
your **identity key** and the address your agent listens on.

The agent binds to `127.0.0.1` by default. Exposing it beyond your own machine means exposing a
signing endpoint, so if you do, use `-require-tls` and understand what you are publishing.

## 4. Approve signing requests

When the hand settles, your agent shows you what it is being asked to sign:

```
─── signing request ────────────────────────────────────
  hand:    e2e-hand-1
  purpose: pot settlement
  pot:     2c64b4de…:0 (4000 sat)
  outputs:
        3600 sat  an expected recipient     76a914675614ecd1924e05…
         365 sat  another seat              76a914d945141fd58cae1a…
  fee:     35 sat
────────────────────────────────────────────────────────
  sign this? [y/N]
```

Two things happen before you are ever asked:

1. **The agent checks the transaction against its own record of the hand.** A settlement that
   pays the wrong seat, alters an amount, spends a different pot, or carries an output you never
   agreed to is refused outright — you are not asked to rubber-stamp something the agent could
   already tell was wrong.
2. **You are shown the material terms**, not just "sign this?". Every output is listed, because
   an undeclared extra output is exactly how a pot gets skimmed.

Anything other than an explicit `y` declines. A mistyped answer does not move money.

`-auto-approve` exists for development and gives away the protection the agent exists to
provide. It warns loudly, and it is never the default.

## What protects you

- **No stake before a refund.** Your agent will not record a stake until a signed refund of it
  exists, and the table will not accept your buy-in until then either. If the hand stalls, you
  broadcast the refund and recover your stake once its locktime matures.
- **The pot needs every seat.** It is an n-of-n output: no single party — including the table —
  can move it.
- **Your hole cards are yours.** The deal is dealerless: no one, including the table, learns your
  cards. Every other seat holds a secret for your card positions and discloses it only to you.
- **A stall is attributable.** If a seat stops cooperating, the others can say which one.

## What does not protect you

- **Every seat must be online to settle.** That is inherent to a non-custodial n-of-n pot. A seat
  that disappears costs everyone a wait until the refund matures, not their money.
- **The table can stall you.** It cannot steal from you.
- **Teratestnet coins are worthless.** This is a test network.
