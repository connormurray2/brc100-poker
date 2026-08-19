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

## 3. Play in the browser with BSV Desktop

Open **https://poker.siftbitcoin.com** and click **Connect wallet**.

The page finds your BRC-100 wallet automatically. `WalletClient('auto')` from `@bsv/sdk` races
every substrate it knows — the injected `window.CWI` provider and the local HTTP ports BSV Desktop
serves — and uses whichever answers. There is nothing to configure and no ports to open.

1. **Connect wallet.** The page asks your wallet for its identity key. Your private key never
   leaves the wallet.
2. **Take a seat**, then commit your buy-in.
3. **Act on your turn.** The page offers exactly the actions the engine says are legal.
4. **Approve signing in your wallet.** When the pot settles, BSV Desktop prompts you. The page
   cannot sign for you, and does not see your key.

Make sure BSV Desktop is on **teratestnet** — the page checks and warns if your wallet is on
mainnet while the table is not.

### The headless alternative

`cmd/agent` exists for a seat with no browser — a bot, a test, or a server-side player. It holds a
key on its own machine and serves the same BRC-100 substrate. A human playing in a browser does not
need it.

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
