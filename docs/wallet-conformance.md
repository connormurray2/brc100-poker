# Wallet conformance: what a BRC-100 wallet must do to sit at the table

**Audience.** Anyone implementing a wallet that plays this game — a `go-arcade-toolbox` build (the
reference), a fork of it, or an unrelated BRC-100 wallet in any language.

**Reference implementation.** `cmd/agent` in this repository. Every requirement below is satisfied
by that binary, and `internal/agent/` is the code to read when this document is ambiguous.

---

## 1. The shape of the system

A player runs a **wallet process on their own machine.** It holds their private key and serves an
HTTP endpoint the table can call. The table sequences the game; it cannot read cards and cannot
spend the pot.

```
   browser (UI only)          player's machine              table service
   ┌──────────────┐          ┌──────────────────┐          ┌──────────────┐
   │ renders      │          │ wallet process   │◀─────────│ coordinator  │
   │ cards, bets  │─────────▶│  - BRC-100 core  │  signed  │  - sequences │
   └──────────────┘  join    │  - deal methods  │  HTTP    │    the deal  │
                     +url    │  - key NEVER     │─────────▶│  - proposes  │
                             │    leaves        │  results │    the pot   │
                             └──────────────────┘          └──────────────┘
```

Two properties define conformance:

1. **The key never leaves the wallet process.** Not to the browser, not to the table.
2. **The wallet decides.** Every request that produces a signature over money is refusable, and a
   conforming wallet verifies the request against its own record of the hand *before* asking the
   player.

### Why a local process rather than a browser wallet

A dealerless deal needs each player to strip their own mask from a card, which is multiplication by
the modular inverse of a secret scalar. No BRC-100 method exposes that operation, so a browser page
talking to a stock wallet over a substrate cannot complete a deal — see
[wallet-native-deal.md](wallet-native-deal.md). A local wallet process holds its own scalars and
performs the operation internally, which is why this architecture works today with no changes to any
published wallet.

A browser-only player can still **fund, sign, and settle** with a stock BRC-100 wallet. They cannot
participate in a dealerless deal, and the table marks such hands `server-dealt` rather than pretend
otherwise.

---

## 2. Transport and framing

- **HTTP POST** to a single endpoint. The reference serves `POST /` on `127.0.0.1:8091`.
- **`Content-Type: application/json`**, one request per body, one response per body.
- The wallet publishes an unauthenticated **`GET /identity`** returning `{ identityKey, audience }`
  so a client can discover which player it speaks for before authenticating anything. Everything it
  returns is public. It MUST still be origin-checked so an unlisted page cannot enumerate wallets.
- Requests over plaintext SHOULD be refusable via configuration (`-require-tls` in the reference).
  A wallet reachable off-host MUST require TLS.

### Request

```json
{
  "version": "brc100-substrate/1",
  "method": "signPot",
  "originator": "poker.example.com",
  "params": { },
  "identityKey": "<caller pubkey, 33-byte compressed DER hex>",
  "nonce": "<unique per request>",
  "timestampUnix": 1755648000,
  "audience": "<this wallet's identity key>",
  "signature": "<DER hex over the request digest>"
}
```

### Response

```json
{
  "version": "brc100-substrate/1",
  "requestNonce": "<the request's nonce>",
  "result": { },
  "error": null,
  "identityKey": "<this wallet's identity key>",
  "signature": "<DER hex over the response digest>"
}
```

A wallet **MUST sign its responses.** A caller has to be able to detect a substituted endpoint
rather than merely trust the channel.

---

## 3. Authentication (normative)

### 3.1 The digest

Both digests are SHA-256 over **length-prefixed** fields. Length-prefixing is not decorative:
concatenating raw fields lets two different inputs produce the same byte stream, so a signature over
one request could validate for another.

Each field is written as an 8-byte big-endian length followed by the raw bytes.

**Request digest**, in this exact order:

```
"brc100-substrate-request"   (domain separator)
version
method
originator
params            (the raw JSON bytes, byte-for-byte as transmitted)
identityKey
nonce
timestampUnix     (8-byte big-endian uint64, NOT length-prefixed)
audience
```

**Response digest:**

```
"brc100-substrate-response"
version
requestNonce
result            (raw JSON bytes; empty string when absent)
error.code        (empty string when there is no error)
error.message     (empty string when there is no error)
identityKey
```

`params` and `result` are hashed as **transmitted bytes**, not as re-serialized JSON. A wallet that
re-encodes before hashing will produce a different digest and reject valid requests.

### 3.2 Verification, in order

A conforming wallet MUST perform all of these, and MUST NOT proceed if any fails:

1. `version` equals `brc100-substrate/1` exactly. **Refuse a mismatch rather than negotiate** — this
   channel carries signing authority, and silently accepting an older contract is not acceptable.
2. `originator` is FQDN-shaped: non-empty, ≤255 characters, contains a `.`, contains none of
   `space`, `tab`, `CR`, `LF`, `/`, `\`, `:`.
3. `audience` equals this wallet's own identity key. This is what stops a request captured from one
   wallet being replayed against another.
4. `timestampUnix` is within **±2 minutes** of local time.
5. `nonce` has not been seen before. The nonce cache is what actually prevents replay; the timestamp
   window only bounds how long the cache must remember.
6. `signature` verifies as ECDSA over the request digest against `identityKey`.
7. `identityKey` is **granted** the named method (§4).

Identity is *proven*, never asserted. A wallet that trusts a header naming the caller has made every
network boundary a custody boundary.

### 3.3 Other required protections

- **Body size cap.** Reject oversized bodies with `too_large` rather than buffering them.
- **Rate limiting** per caller identity, returning `rate_limited`.
- **CORS allowlist** if a browser may call directly. Never `*`.

---

## 4. Grants: least privilege

A wallet MUST maintain a per-caller-identity set of permitted methods and refuse anything outside it
with `forbidden`. Two profiles are meaningful:

| Profile | May call | Notably may NOT call |
| --- | --- | --- |
| **Table** | `getPublicKey`, `getNetwork`, `signPot`, `internalizeAction`, all six deal methods | `listOutputs`, `listActions`, `createAction`, `signAction` |
| **Owner** (the player's own client) | everything served | — |

A table has no business enumerating a player's coins or transaction history, and no business making
the wallet spend on its own. Granting the deal methods is safe because **the secrets never leave the
wallet** — the table can sequence a deal it cannot read.

---

## 5. Methods

### 5.1 BRC-100 core

| Method | Purpose |
| --- | --- |
| `getPublicKey` | Identity key, or a BRC-42/43 derived key |
| `getNetwork` | `mainnet` / `testnet` |
| `internalizeAction` | Record an incoming payment (BRC-29 payout, faucet claim) |
| `createAction`, `signAction`, `listOutputs`, `listActions` | Owner profile only |

`internalizeAction` params, as the reference expects them:

```json
{ "beefHex": "...", "outputIndex": 0,
  "derivationPrefix": "<base64>", "derivationSuffix": "<base64>",
  "senderIdentityKey": "<hex>", "description": "..." }
```

> **BRC-29 trap, learned the expensive way.** The `keyID` for a BRC-29 payment is
> `base64(prefix)` / `base64(suffix)`, while the remittance carries the **raw** bytes. Get this
> wrong and you pay to a key nobody can derive: our first real-money payout put 4,800 satoshis in a
> permanently unspendable output. Senders must use `LockForCounterparty`.

### 5.2 `signPot` — the only method that signs money

```json
// params
{ "handId": "...", "rawTxHex": "...", "potInput": 0 }
// result
{ "seat": 2, "der": "<DER signature hex>" }
```

A conforming wallet MUST apply **two independent gates, in this order:**

1. **Verify against its own record first.** Parse the transaction and check it against what this
   wallet knows about the hand: the pot outpoint it funded, the stake it committed, the payouts it
   expects. Reject a mismatch *without troubling the player.* A wallet that asks the player about a
   transaction it could have known was wrong has outsourced its job.
2. **Then ask the player**, showing the material terms: hand ID, purpose, pot outpoint and amount,
   **every output with amount and description**, and the fee.

Rules that follow from this:

- A prompt that does not say what is being signed is a rubber stamp, and an **undeclared output is
  exactly how a pot gets skimmed.** Show them all.
- **Anything other than an explicit yes declines.** A mistyped answer must not move money.
- **Unreadable input declines.** If stdin is closed or the UI is gone, fail closed. Failing open
  here would give away the entire protection the wallet exists to provide.
- Auto-approval, if offered at all, MUST be opt-in, non-default, and loud. It is a custodial posture
  wearing a non-custodial costume.

### 5.3 The deal methods

Six methods, called in this order. All points are **33-byte compressed DER hex**; all scalars are
**32-byte big-endian hex**. Card `i` is the point `(i+1)·G`.

#### `dealCommit`

```json
// params → result
{ "handId": "...", "deckSize": 52, "seats": 6 }
{ "shuffleCommitment": "<sha256 hex>", "remaskCommitment": "<sha256 hex>" }
```

Generate this hand's secrets and publish commitments to them **before the deal begins**. This binds
the wallet to the transformation it will apply before it can see anyone else's contribution — which
is what makes a biased shuffle detectable and attributable rather than merely suspected.

Commitments are SHA-256 over length-prefixed fields with **distinct domain separators**, so a
commitment made for one purpose cannot be replayed as a commitment for another:

- shuffle: `"brc100-poker/proof/shuffle/v1"`, the global scalar, the permutation (each entry as
  4-byte big-endian)
- remask: `"brc100-poker/proof/remask/v1"`, the global scalar, then every per-position scalar

#### `dealShuffle` and `dealRemask`

```json
// params → result   (both methods)
{ "handId": "...", "deck": ["<point>", ...] }
{ "deck": ["<point>", ...] }
```

`dealShuffle` applies the committed global scalar and permutation. `dealRemask` removes the global
scalar and replaces it with the committed per-position scalars — this is what makes each position
independently revealable.

#### `dealFinal`

```json
{ "handId": "...", "deck": ["<point>", ...] }
```

Records the completed deck. **This method exists because of a real bug**: a wallet only ever sees the
deck as it was during its own pass, so reading a card requires knowing the *final* deck. A
conforming wallet MUST re-validate the deck it is handed rather than trusting it.

#### `dealReveal`

```json
{ "handId": "...", "positions": [0, 1] }
{ "positions": [0, 1], "scalars": ["<scalar>", ...] }
```

Discloses this wallet's per-position scalars **for the named positions only.** This is the method
that deals a card, and the reason the deal is dealerless: a wallet reveals a scalar for one position
without revealing anything about the others.

A conforming wallet MUST refuse to reveal a position outside what the protocol permits for the
requesting seat, and MUST NOT reveal a position twice in a way that lets a caller accumulate the
whole deck.

#### `dealCard`

```json
{ "handId": "...", "position": 0, "disclosures": { "<seat identity key>": "<scalar>" } }
{ "card": "As" }
```

Given every other seat's scalar for a position, plus its own, the wallet strips all masks and names
the card. Card strings are rank+suit, e.g. `As`, `Td`, `2c`.

**Hole-card privacy is enforced by construction, not by policy**: only the seat entitled to a card
ever receives the full set of disclosures for it.

---

## 6. Errors

Codes are stable; messages are not. A wallet MUST use these rather than inventing its own.

| Code | Meaning |
| --- | --- |
| `bad_request` | Malformed or unparseable |
| `unauthenticated` | Identity not proven |
| `forbidden` | Authenticated but not granted this method |
| `unknown_method` | Not served |
| `replayed` | Nonce seen before |
| `expired` | Timestamp outside the window |
| `declined` | The player refused |
| `rate_limited` | Caller exceeded its allowance |
| `too_large` | Body over the maximum |
| `internal` | Fault in the wallet |

`declined` is a first-class outcome, not a failure. A player refusing to sign is the system working.

---

## 7. Cryptographic requirements

These are non-negotiable, and each one is here because the obvious implementation gets it wrong.

**Validate scalars.** Reject zero and anything ≥ the curve order. `ec.PrivateKeyFromBytes` in go-sdk
**accepts a zero key** and silently reduces out-of-range values, so a wallet that trusts the
constructor will happily use a key that is not a key.

**Validate points before using them.** In order:

1. 33 bytes, leading byte `0x02` or `0x03`
2. **Both coordinates are canonical field elements — in `[0, p)`**
3. On the curve
4. Not the identity

Step 2 is **not** implied by step 3, and skipping it is the likeliest way to build a broken wallet.
`PublicKey.fromString('02' + 'ff'×32)` — an x-coordinate greater than the field prime — is accepted
by both `@bsv/sdk` and `go-sdk`, silently reduced (to `0x1000003d0`), and then reported **on-curve**.
The range check MUST run *before* the parser, because the parser performs the reduction. This is the
classic invalid-curve opening.

**Use constant-time comparison** for commitments and any secret-dependent equality, so a mismatch
cannot be located byte by byte.

**Use a library's curve implementation.** The upstream project this game descends from hand-rolled a
Montgomery ladder over `big.Int` and documented honestly that it was not constant-time.

**Never reuse a masking scalar** across hands, and never derive one from a key the wallet also uses
for signing or encryption. For a counterparty point `Q`, `d·Q` **is** the ECDH shared secret with
`Q` — so a masking key that doubles as a spending or identity key hands away that secret.

---

## 8. Money-side obligations

A wallet that plays for real value MUST also:

- **Hold a fully-signed refund of its own stake before funding.** This is the backstop for the whole
  design: an absent or uncooperative seat then costs everyone a wait, never their money. Refunds use
  `nLockTime` with a non-final sequence.
- **Abort abandoned actions.** An action built but never broadcast leaves change against an all-zero
  txid and can block the wallet entirely — we hit a 500-satoshi failure against a 95,936-satoshi
  balance. Call `abortAction` on any path that abandons a built transaction.
- **Broadcast binary Extended Format**, not hex-as-bytes.
- **Set the network explicitly.** Do not rely on defaults: in `go-arcade-toolbox`, storage defaults
  to mainnet while the performance provider defaults to testnet. A wallet that inherits both is
  incoherent.
- **Treat a 4xx broadcast rejection as a rejection.** The toolbox's classifier returns
  `err == nil` with `Rejected: true`; code that only checks `err` will read a rejection as success.
- **Back up the wallet database.** There is no UTXO discovery and no restore-from-seed. The database
  *is* the wallet.

---

## 9. Conformance checklist

A wallet can sit at the table when all of these hold.

**Transport**
- [ ] `POST /` accepts the request envelope; `GET /identity` serves `{identityKey, audience}`
- [ ] Response bodies are signed by the wallet
- [ ] Body size cap, rate limiting, CORS allowlist (never `*`)
- [ ] TLS required when reachable off-host

**Authentication**
- [ ] Length-prefixed digests with the exact field order in §3.1
- [ ] `params`/`result` hashed as transmitted bytes, not re-serialized
- [ ] Version mismatch refused, not negotiated
- [ ] Originator FQDN-shaped; audience equals own identity key
- [ ] ±2 minute clock window; nonce cache rejecting replays
- [ ] ECDSA signature verified against the claimed identity key
- [ ] Per-identity grants enforced; table profile cannot list outputs or actions

**Methods**
- [ ] `getPublicKey`, `getNetwork`, `internalizeAction`
- [ ] `signPot` verifies against the wallet's own record **first**, then asks the player
- [ ] The prompt shows every output with amount and description
- [ ] Non-explicit answers and unreadable input both decline
- [ ] All six deal methods, with `dealFinal` re-validating the deck it receives
- [ ] `dealReveal` discloses only the named positions
- [ ] Stable error codes from §6

**Cryptography**
- [ ] Scalars rejected if zero or ≥ curve order
- [ ] Points range-checked **before** parsing, then curve-checked, then identity-checked
- [ ] Constant-time comparison for commitments
- [ ] Distinct domain separators per commitment type
- [ ] Masking scalars never reused and never shared with a signing or encryption key

**Money**
- [ ] Signed refund held before funding
- [ ] `abortAction` on every abandoned-action path
- [ ] Broadcasts binary EF; treats 4xx as rejection
- [ ] Network set explicitly
- [ ] Database backed up

---

## 10. Reading list

| Topic | File |
| --- | --- |
| Wire protocol in detail | [substrate-protocol.md](substrate-protocol.md) |
| Why the deal is not yet wallet-native | [wallet-native-deal.md](wallet-native-deal.md) |
| Funding and BRC-29 | [funding.md](funding.md) |
| Co-signing findings | [spike-cosign-findings.md](spike-cosign-findings.md) |
| Running a seat | [playing.md](playing.md) |
| Design decisions | [decisions.md](decisions.md) |
| Reference implementation | `cmd/agent/`, `internal/agent/`, `internal/protocol/substrate/` |
