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

**Status as of 2026-08-19: the API claim path is failing server-side.** Both
`POST /api/claim` (legacy address) and `POST /api/claim/wallet` (identity key) return:

```json
{"error":"Faucet error: merged Beef failed validation.","code":"faucet_error"}
```

This was reproduced against three independently generated addresses, on both endpoints,
with well-formed requests — the faucet validates the address *before* the captcha, and a
malformed address returns a distinguishable `"Malformed address"` error, so the request
shape is not the problem. `GET /api/stats` shows the faucet has served claims previously
(236 payouts, 23.6M sats), so this is a fault in its treasury/BEEF-merging step rather than
a rate limit. Retry later, or use the web UI, which may take a different code path.

### 2. BSV Desktop

Install [BSV Desktop](https://bsvblockchain.org), create a wallet on **teratestnet**, and
send a payment to the address from `keygen`. This is the upstream-intended loop and does
not depend on the faucet API.

## Crediting the payment

A payment does not enter the wallet by arriving on-chain. The wallet learns about a coin
only from a transaction it created or from an explicit internalize call, and the coin must
be internalized as a **wallet payment** — a basket insertion records no derivation material,
so the coin would be visible but unspendable.
