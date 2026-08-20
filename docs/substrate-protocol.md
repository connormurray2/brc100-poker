# The BRC-100 substrate protocol

> For wallet implementers: **[wallet-conformance.md](wallet-conformance.md)** collects the
> normative requirements — digests, grants, the deal methods, the signing gates and the
> cryptographic rules — into one specification with a checklist. This document is the protocol
> reference behind it.

A wire protocol for driving a BRC-100 wallet over the network, so a wallet that holds a player's
key can serve requests from a caller it does not trust.

It exists because `go-arcade-toolbox` is a BRC-100 wallet **library**: a key is passed to
`wallet.New` and never leaves that process, and the old JSON-RPC transport was removed in the
rewrite. Upstream states the limitation plainly — BSV Desktop can *pay* an application but
"cannot *drive* a toolbox wallet." Without a transport, player-held keys can only fund; they
cannot sign a pot settlement, which a non-custodial pot requires.

This document is complete enough to implement a second client — a browser wallet, say — against
the same protocol.

## Design position

**Identity is proven, never asserted.** The toolbox's own storage REST API is explicitly not the
model: its default authenticator "performs NO cryptographic verification — any caller can claim
any identity by setting the header", and it serves no TLS. Copying that shape would make every
network boundary a custody boundary.

A signature makes a request unforgeable but perfectly replayable, so three mechanisms bound
reuse independently: a **nonce cache** makes a captured request single-use, a **timestamp window**
bounds how long capturing is worth anything, and an **audience** binds a request to one wallet so
it cannot be replayed against another that grants the same caller.

## Transport

`POST` to the wallet's substrate endpoint with `content-type: application/json`. One request, one
response. TLS is required outside local development; the identity proof is layered on top rather
than trusted to the channel.

Bodies are capped at **1 MiB**. Wallet calls are small; anything larger is a bug or an attempt to
exhaust memory.

## Version

    brc100-substrate/1

A version mismatch is **refused, not negotiated**. This carries signing authority, so silently
falling back to an older contract is not acceptable.

## Request

```json
{
  "version": "brc100-substrate/1",
  "method": "signPot",
  "originator": "table.poker.local",
  "params": { "handId": "hand-1", "rawTxHex": "0100...", "potInput": 0 },
  "identityKey": "02ff3c9c…",
  "nonce": "9f8c…",
  "timestampUnix": 1755640000,
  "audience": "03a8ec94…",
  "signature": "3045…"
}
```

| Field | Meaning |
|---|---|
| `version` | Must equal the version above. |
| `method` | One of the methods below. |
| `originator` | FQDN-shaped caller identifier. BRC-100 requires it; the toolbox validates it. |
| `params` | Method arguments, opaque to this layer. |
| `identityKey` | Caller's public key, DER hex. **A claim until the signature proves it.** |
| `nonce` | Unique per request. A repeat is refused. |
| `timestampUnix` | Seconds. Must be within **2 minutes** of the server's clock. |
| `audience` | The wallet's identity key, so a request cannot be replayed against a different wallet. |
| `signature` | DER hex over the request digest, proving control of `identityKey`. |

## Response

```json
{
  "version": "brc100-substrate/1",
  "requestNonce": "9f8c…",
  "result": { "seat": 0, "der": "3045…" },
  "identityKey": "03a8ec94…",
  "signature": "3044…"
}
```

The wallet signs its responses too. TLS proves the channel but not *which key* answered, and a
caller must be able to detect a substituted endpoint. **Errors are signed as well** — otherwise a
caller cannot distinguish a real refusal from an injected one.

`requestNonce` ties a response to exactly one request, so an answer cannot be shifted onto a
different call.

## The signature digest

Both digests are SHA-256 over **length-prefixed** fields. Length-prefixing is not cosmetic:
concatenating raw fields would let moving a character between two adjacent fields produce the same
byte stream and therefore the same valid signature.

Each field is written as an 8-byte big-endian length followed by the bytes.

**Request digest** — fields in this exact order:

```
"brc100-substrate-request"
version
method
originator
params            (the raw JSON bytes, as sent)
identityKey
nonce
timestampUnix     (8-byte big-endian, not length-prefixed text)
audience
```

**Response digest**:

```
"brc100-substrate-response"
version
requestNonce
result            (raw JSON bytes; empty string if absent)
error.code        (empty string if absent)
error.message     (empty string if absent)
identityKey
```

Signatures are DER-encoded ECDSA over secp256k1, hex-encoded.

## Methods

Only what this game needs. Generalising to the full BRC-100 surface is a separate concern — a
smaller grant set is a smaller blast radius.

| Method | Purpose |
|---|---|
| `getPublicKey` | The wallet's identity key. |
| `getNetwork` | The network, as a **valid BRC-100 value**. |
| `createAction` | Build a transaction. |
| `signAction` | Complete a previously created transaction. |
| `internalizeAction` | Record an incoming payment so it becomes spendable. |
| `listOutputs` | Enumerate outputs. **Sensitive.** |
| `listActions` | Enumerate history. **Sensitive.** |
| `signPot` | Sign one input of a pot transaction. Not BRC-100 — see below. |

### `signPot` is ours, not BRC-100's

BRC-100 has no partial-signature concept. `signPot` is this application's co-signing primitive
and the only method that produces a signature over money the *caller* proposed. It is namespaced
alongside the real methods deliberately, so it goes through the same authentication, grant and
consent path rather than getting a side door.

Params:

```json
{ "handId": "hand-1", "rawTxHex": "0100...", "potInput": 0 }
```

Result:

```json
{ "seat": 0, "der": "3045…41" }
```

The `der` is the signature with its sighash-type byte appended (`0x41`, `SIGHASH_ALL | FORKID`).

### `getNetwork` must be translated

The wallet's own `GetNetwork` returns internal names and emits the outright-invalid `"ttn"` on
teratestnet — a value go-sdk's own `NetworkFromString` rejects. A conforming implementation
returns `"mainnet"` or `"testnet"` and never leaks the library's internal name.

## Grants

Least privilege, deny by default. An identity with no grant is refused every method.

**Table grants** — what a table service may do to a player's wallet:

    getPublicKey, getNetwork, signPot, internalizeAction

It can ask for the seat's identity, propose a signature, and hand over a received payment. It
**cannot** enumerate the player's outputs or history, and cannot make the wallet spend on its own.

**Owner grants** — the player's own client: everything served.

An ungranted caller is told only that it is not permitted. Naming the available methods would be
free reconnaissance.

## Signing consent

A server built without an approver is **refused outright** rather than defaulting to
approve-everything: that default would be a silent downgrade from non-custodial to custodial.

The approver receives the material terms — the pot, **every output in order**, and the fee —
because a prompt that only says "sign this?" is a rubber stamp, and an undeclared extra output is
exactly how a pot gets skimmed.

One approval authorises exactly **one** signature. A replayed signing call is refused by the nonce
cache before it reaches the approver again.

A client should also expect the wallet to refuse a signature that does not match its own record of
the hand — wrong winner, altered amount, different pot, undeclared output — *before* any human is
asked. A prompt is not a safety mechanism if it fires on requests the software should have
rejected.

## Errors

```json
{ "code": "forbidden", "message": "caller is not permitted to call \"listOutputs\"" }
```

| Code | HTTP | Meaning |
|---|---|---|
| `bad_request` | 400 | Malformed request, bad version, or invalid originator. |
| `unauthenticated` | 401 | Identity not proven, signature invalid, or wrong audience. |
| `expired` | 401 | Timestamp outside the window. |
| `forbidden` | 403 | Authenticated but not granted this method. |
| `declined` | 403 | The player refused, or the request failed the wallet's own checks. |
| `unknown_method` | 501 | Not served, or no handler registered. |
| `replayed` | 409 | The nonce has been seen. |
| `rate_limited` | 429 | Caller exceeded its allowance. |
| `too_large` | 413 | Body over 1 MiB. |
| `internal` | 500 | A fault in the wallet or substrate. |

Codes are stable; messages are not. Branch on the code.

## Rate limiting

Per caller, applied **after** authentication so an unauthenticated flood cannot consume a
legitimate caller's allowance. One flooding caller cannot deny service to another. Default 120
requests per minute.

## Implementing a client

1. Fetch the wallet's identity key out of band — it is your `audience`.
2. Per request: build the envelope, generate a fresh nonce, stamp the time, compute the digest,
   sign it, POST it.
3. On the response: check `requestNonce` matches, check `identityKey` is the wallet you intended,
   and **verify the signature**. Skipping that last step means you cannot tell the real wallet
   from a substituted endpoint.
4. Never retry a `declined` or `forbidden` as though it were transient. Do retry `rate_limited`
   after a pause.

Reference implementation: `internal/protocol/substrate` (server), `internal/agent` (handlers).
Both have tests covering asserted identity, impersonation, per-field tampering, cross-wallet
replay, nonce replay, and grant enforcement.
