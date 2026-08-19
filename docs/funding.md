# Funding a wallet on teratestnet

Teratestnet coins are worthless, but the wallet database that tracks them is not
replaceable: the toolbox has no UTXO discovery and no restore-from-seed, so a lost database
means unspendable coins even with the key in hand. Back up `*.db` alongside the key.

## Generate a key

```sh
go run ./cmd/keygen -out secrets/table.key
```

Prints the identity key and the teratestnet address; writes the private key to the given
path with mode 0600 and never to stdout. `secrets/` is gitignored. Teratestnet is
testnet-based, so addresses use the testnet version byte.

## Funding options

### 1. Faucet — https://faucet-ttn.bsvblockchain.tech/

100,000 sats per claim, spendable immediately (the claim returns Atomic BEEF carrying the
funded ancestors' proofs, so there are no confirmations to wait for).

- Web UI: detects a BRC-100 wallet automatically, or takes a legacy address manually.
- API: `POST /api/claim` with `{"address": "..."}`. A Turnstile captcha token is normally
  required; an `Authorization: Bearer <key>` header raises limits and skips the captcha.

**Note the host.** The page's own example shows `faucet.teratestnet.org`, which does not
resolve. The live host is `faucet-ttn.bsvblockchain.tech`.

**The faucet pays BRC-29, not a plain address.** This is the part that is easy to get
wrong. `POST /api/claim/wallet` takes `{identityKey, captchaToken}` and returns:

```json
{"txid":"…","amount":100000,"atomicBEEF":"…","outputIndex":0,
 "derivationPrefix":"…","derivationSuffix":"…","senderIdentityKey":"…"}
```

The faucet derives a key from the claiming wallet's identity key and pays *that*. The
client must then internalize the returned transaction as a **wallet payment**, supplying
the derivation material — a basket insertion records none, so the coin would be visible
and permanently unspendable. `cmd/fund` does both halves:

```sh
go run ./cmd/fund -key secrets/table.key -db secrets/table.db -captcha "<turnstile>"
```

The captcha token comes from the faucet page; `-bearer` with an API key skips it.

**Status as of 2026-08-19: the faucet is failing server-side.** Both claim endpoints return
HTTP **503**:

```json
{"error":"Faucet error: merged Beef failed validation.","code":"faucet_error"}
```

Evidence that this is the faucet and not our request:

- The request now reaches the faucet's transaction-building step: a malformed address gets
  a distinguishable `"Malformed address"` error, and an unparseable body gets
  `"Invalid request body"`.
- `GET /api/stats` has been frozen at 236 payouts / 23,600,000 sats, so nobody is claiming
  successfully.
- `GET /api/balance` shows a treasury of 4,975,994,756 sat (~49.76 BSV), so it is not out
  of funds, and arcade itself answers 200.
- The same failure occurs for three independently generated keys, on both endpoints.

Our half of the flow is verified independently: the toolbox's own `examples/internalize`
performs the identical BRC-29 wallet-payment internalize and yields a **spendable** coin,
so only the faucet's own BEEF merge is broken. Retry later.

### 2. BSV Desktop

Install [BSV Desktop](https://bsvblockchain.org), create a wallet on **teratestnet**, and
send a payment to the address from `keygen`. This is the upstream-intended loop and does
not depend on the faucet API.

## Crediting the payment

A payment does not enter the wallet by arriving on-chain. The wallet learns about a coin
only from a transaction it created or from an explicit internalize call, and the coin must
be internalized as a **wallet payment** — a basket insertion records no derivation material,
so the coin would be visible but unspendable.
