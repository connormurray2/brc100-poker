# Playing a hand

You keep your own key. The table coordinates play and pays its own fees, but it never holds your key
and never holds the pot alone — so it can stall a hand and it cannot take your money.

There are **two ways to sit down**, and the difference is worth understanding before you start:

| | Runs your own wallet process | Browser wallet only |
| --- | --- | --- |
| Fund, bet, settle | Yes | Yes |
| Your key stays yours | Yes | Yes |
| **Cards dealt dealerlessly** | **Yes** | No — the server shuffles and can see them |
| Setup | One command in a terminal | Nothing |

Both are non-custodial with respect to *money*. Only the first gives you card privacy, because a
dealerless deal needs your wallet to strip its own mask from a card — an operation no published
BRC-100 wallet exposes yet ([why](wallet-native-deal.md)). A local wallet process does it
internally.

**If you care about your hole cards, run the wallet process.** It is one command.

---

## 1. Generate a key

```sh
go run ./cmd/keygen -out secrets/player.key
```

Prints your identity key and a teratestnet address. The private key is written with mode `0600` and
never to stdout, so it cannot end up in a terminal transcript.

**Back up both the key and the wallet database.** The database *is* the wallet: the toolbox has no
UTXO discovery and no restore-from-seed, so a lost database means unspendable coins even with the key
in hand.

## 2. Fund it

You can do this from the page once your wallet is running (step 4) — click **Claim from the
faucet**. That route claims in-process, so nothing has to be stopped.

To fund from a terminal instead, with the wallet **not** running:

```sh
go run ./cmd/fund -key secrets/player.key -db secrets/player.db
```

Either way this claims 100,000 satoshis from the teratestnet faucet. The faucet pays BRC-29, so this does two
things: it asks for a payment derived from your identity key, then internalizes the result as a
*wallet payment* with its derivation material. Without the second half the coin exists on-chain and
your wallet cannot spend it. See [funding.md](funding.md).

Check it landed:

```sh
go run ./cmd/agent -key secrets/player.key -db secrets/player.db
# prints: balance: 100000 sat
```

---

## 3. Run your wallet process

This is the part that gives you card privacy.

**Open the table page and copy the command it shows you.** It arrives complete — the table's
identity key and origin are already filled in, because this deployment serves a single table and the
page knows both. Click **Copy command**, paste into a terminal, run it.

It looks like this:

```sh
go run ./cmd/agent \
  -key    secrets/player.key \
  -db     secrets/player.db \
  -table  <this table's identity key, prefilled> \
  -origin <this page's origin, prefilled> \
  -listen 127.0.0.1:8091
```

It prints your identity key, your balance, and the substrate address. **Leave it running** — it is
your seat's wallet for the whole session.

What the flags mean, and why they matter:

| Flag | Why |
| --- | --- |
| `-table` | Least privilege. Only this table may call your wallet, and only the methods a table needs — it cannot enumerate your coins or your history. Omit it and your wallet warns that no table is authorised. |
| `-origin` | Lets the browser page call your wallet. Without it, an unlisted page cannot even discover which wallet is running here. |
| `-listen` | Bound to `127.0.0.1` on purpose. Nothing outside your machine can reach it. |
| `-require-tls` | Add this if you ever expose the wallet beyond localhost. Refuses plaintext. |
| `-auto-approve` | **Don't.** It approves every signing request without asking, which is a custodial posture in disguise. It exists for tests and says so loudly when used. |

### What you will see

When the table asks you to sign a settlement, your terminal shows the material terms and waits:

```
─── signing request ────────────────────────────────────
  hand:    h-4f2a
  purpose: settle
  pot:     9b3c…:0 (10000 sat)
  outputs:
        6200 sat  payout seat 0            76a914c0ffee…
        3700 sat  payout seat 1            76a9141dea11…
  fee:     100 sat
────────────────────────────────────────────────────────
  sign this? [y/N]
```

Read the outputs. Your wallet has already checked this transaction against its own record of the
hand and refused anything that did not match, so what you are being asked is the last gate rather
than the only one. **Anything other than `y` declines** — a mistyped answer must not move money, and
a closed terminal declines too.

---

## 4. Sit down

Open **https://poker.siftbitcoin.com**.

1. **Connect wallet.** The page reads your identity key. Your private key never leaves your machine.
1. **Connect.** The page hands you one complete command to run, and the wallet address defaults to
   `http://127.0.0.1:8091`.
2. **Fund**, if you have not already — the page claims from the faucet through your running wallet
   and shows the balance.
3. **Join the table** and commit your buy-in.
4. **Act on your turn.** The page offers exactly the actions the engine says are legal.
5. **Approve signing** in the terminal running your wallet when the pot settles.

The page registers your wallet on every join, so a hand is always dealerless. If registration
fails it says so and does not seat you into a hand you would have assumed was private.

Make sure your wallet and the table are on the **same network**. The page checks and warns.

---

## 5. Playing without a terminal

A browser-only player connects a BRC-100 wallet — BSV Desktop or anything implementing the standard
— and plays normally: funding, betting and settling all work, and the wallet still signs every
satoshi. `WalletClient('auto')` from `@bsv/sdk` finds it with nothing to configure.

The only thing you give up is card privacy: with no wallet process to hold your masking secrets, the
server shuffles. The table says so, every time, on the table itself.

---

## What protects you

- **No stake before a refund.** Your wallet will not record a stake until a signed refund of it
  exists, and the table will not accept your buy-in until then. If the hand stalls, broadcast the
  refund and recover your stake once its locktime matures.
- **The pot needs every seat.** It is an n-of-n output: no single party — including the table — can
  move it.
- **Two gates before any signature.** Your wallet verifies the transaction against its own record of
  the hand *first*, then asks you. A proposal that does not match is refused without troubling you.
- **Your hole cards are yours** — when you run the wallet process. Every other seat holds a secret
  for your card positions and discloses it only to you.
- **A stall is attributable.** If a seat stops cooperating, the others can say which one.

## What does not protect you

- **Every seat must be online to settle.** Inherent to a non-custodial n-of-n pot. A seat that
  disappears costs everyone a wait until the refund matures, not their money.
- **The table can stall you.** It cannot steal from you.
- **A browser-only seat has no card privacy.** See above; the table is explicit about it.
- **Teratestnet coins are worthless.** This is a test network.

---

## Troubleshooting

**"No BRC-100 wallet answered."** Nothing is serving a wallet the page can reach. If you are running
`cmd/agent`, check it is still up and that you passed `-origin` with the exact page origin.

**"The table could not register your wallet."** The address is wrong or unreachable. It must be
reachable *from your browser*, so `http://127.0.0.1:8091` — not a container-internal address.

**"no table authorised".** You started the wallet without `-table`. Restart it with the identity key
from `/api/info`.

**Signing request never appears.** The table can only call methods it was granted. Confirm the
`-table` key matches the table you actually joined.

**Balance is zero after funding.** The faucet payment was not internalized. Re-run `cmd/fund` and see
[funding.md](funding.md) — a coin can exist on-chain and still be unspendable by your wallet.

---

Implementing a wallet rather than playing? See [wallet-conformance.md](wallet-conformance.md).
