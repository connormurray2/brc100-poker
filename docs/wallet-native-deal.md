# Wallet-native deal: what is missing, and what to do about it

> Implementing a wallet? Start with **[wallet-conformance.md](wallet-conformance.md)**, which
> specifies what a wallet must do to play. This document is narrower: it explains the one capability
> BRC-100 is missing and what finishing it would take.

**Status as of 2026-08-20.** The money is non-custodial and proven. The *deal* is not yet
wallet-native, and this document records exactly why, exactly what would fix it, and exactly
where the code has to change. It exists so the gap is not rediscovered from scratch.

---

## 1. The one-paragraph version

A dealerless deal needs each player to **mask** every card and later **strip** their own mask.
Masking through a BRC-100 wallet is possible today. Stripping is not: it requires multiplying a
point by the *modular inverse* of a derived key, and no method on `WalletInterface` does that. Until
a wallet exposes it, masking scalars must live in the application. They are ephemeral per-hand
secrets rather than custody, so this is a purity gap, not a funds-at-risk gap — **BSV Desktop still
signs every satoshi.**

---

## 2. Why the deal needs two operations

Cards are curve points. Card `i` is encoded as `(i+1)·G`, the standard Barnett–Smart encoding.

Masking is scalar multiplication, and it commutes:

```
a·(b·P) == b·(a·P)
```

That commutativity is the whole trick. Every player masks the deck with a secret scalar; because
order does not matter, players can mask in any sequence and strip in any sequence. Nobody can read a
card unless every player who masked it agrees to strip.

So each player needs, per hand:

| Operation | Meaning | Available over BRC-100 today? |
| --- | --- | --- |
| **mask** | `a·C` for every card | **Yes** |
| **strip** | `a⁻¹·P`, removing only your own mask | **No** |

---

## 3. Masking already works. Do not re-propose it.

`revealCounterpartyKeyLinkage` returns the ECDH point, and passing a *card* as the counterparty
yields `a·C`. Verified against `@bsv/sdk`: masks produced this way **commute across independent
wallets**.

```ts
// Get a·P out of a BRC-100 wallet, no new methods required.
const r = await wallet.revealCounterpartyKeyLinkage({
  counterparty: pointHex,      // the card, as a compressed DER point
  verifier: 'anyone'           // narrow this in production; see the caveat below
})
const w2 = new ProtoWallet('anyone')
const { plaintext } = await w2.decrypt({
  ciphertext: r.encryptedLinkage,
  counterparty: r.prover,
  protocolID: [2, 'counterparty linkage revelation'],
  keyID: r.revelationTime
})
const masked = Utils.toHex(plaintext)   // === a·C
```

Credit where due: this recipe came from Darren Kellenschwiler (deggen), who correctly pointed out
that the primitive already existed and that an earlier proposal of ours was overbuilt.

**Caveat before using it in production.** This repurposes a *permissioned key-linkage disclosure*
method (protected by BRC-72) as a masking primitive, by passing cards as counterparties. It works,
but it is not what the method is for, and a wallet would be within its rights to prompt or refuse.
Treat it as a viable path, not an endorsed one.

---

## 4. Stripping is the actual gap

Feeding `a·C` back through the same recipe gives `a²·C`, not `C`. It masks **again**. Verified.

The maths for stripping is trivial *if you hold the scalar* — this is deggen's own snippet, and it
round-trips correctly:

```ts
const key = PrivateKey.fromRandom()
const somePoint = PrivateKey.fromRandom().toPublicKey()
const maskedPoint = new PublicKey(key.deriveSharedSecret(somePoint))

const c = new Curve()
const inverseKey = new PrivateKey(key.invm(c.n))          // <-- needs the raw scalar
const unmaskedPoint = inverseKey.deriveSharedSecret(maskedPoint)
// somePoint.toString() === unmaskedPoint.toString()      // true
```

**`key.invm(c.n)` is the blocker.** It needs the private key in application memory.

Two clarifications worth keeping, because both came up and cost time:

- `a⁻¹` is the **modular inverse of a scalar mod n** — extended Euclid, microseconds. It is *not*
  the discrete log. The wallet already holds `a`, so `a⁻¹` is free to compute there.
- `invert` leaks nothing new. Recovering `a` from `(C, a·C)` is exactly as hard as from `(G, a·G)`.
- `getPublicKey({ identityKey: true })` returns `a·G`, **not** the card. Undoing `a·C` needs the
  scalar, and `a·G → a` is ECDLP. So that route cannot strip a mask.

### The interface has no route to a scalar

`WalletInterface` exposes **29 methods** and `keyDeriver` appears **zero times** in
`Wallet.interfaces.ts`. `keyDeriver` is a property of the in-process `ProtoWallet` class, not part of
the interface — so over a substrate (which is every real wallet, BSV Desktop included) there is no
scalar, no inverse, and no private key. An application whose keys live in a wallet can perform step
one and never step two.

---

## 5. What we shipped instead (the current design)

Masking scalars are generated **per hand, in the application**, and discarded when the hand ends.

Why this is acceptable:

- The scalars mask cards. They **cannot move money.** The pot is n-of-n multisig and every
  settlement input is signed by the seat's own BRC-100 wallet.
- They are ephemeral. A leaked hand-scalar exposes one hand's cards, not funds, and not future hands.
- Refund safety is unaffected: pre-signed nLockTime refunds mean a stalled hand always returns
  stakes.

What it costs: the application *could* read cards it holds scalars for. That is a weaker
trust story than a wallet-native deal, and it is the only reason this document exists.

**Everything is written so the upgrade is a swap, not a rewrite** — see §7.

---

## 6. Upstream state

| Item | Where | Status |
| --- | --- | --- |
| SDK method `multiplyPoint` (optional, with `invert`) | [bsv-blockchain/ts-stack#488](https://github.com/bsv-blockchain/ts-stack/pull/488) | Open |
| BRC proposal (BRC-229) | [bsv-blockchain/BRCs#230](https://github.com/bsv-blockchain/BRCs/pull/230) | Open; overstates the case, needs narrowing to the inverse only |
| Superseded, overbuilt SDK PR | [ts-stack#487](https://github.com/bsv-blockchain/ts-stack/pull/487) | Closed — do not revive |

Maintainer position (deggen): adding to the wallet interface is plausible but needs **coordination
and an interface-version bump**, to be settled on a tech call. He is right to be cautious, and we
have the evidence for why:

- Declaring the method **required** on `WalletInterface` broke **23 call sites** — every substrate
  (`WalletClient`, `HTTPWalletJSON`, `WalletWireTransceiver`, `window.CWI`, `XDM`,
  `ReactNativeWebView`) plus the KV store, registry and identity clients.
- Declaring it **required** on `ProtoWallet` broke `@bsv/wallet-toolbox`, where `Wallet`,
  `PrivilegedKeyManager` and the wallet managers satisfy the class *structurally* without extending
  it.

**Optional + feature detection is the only shape that compiles.** Any future attempt must keep that.

---

## 7. The upgrade path, concretely

### 7.1 When the SDK ships `multiplyPoint`

Feature-detect and swap. Do not branch the protocol; the algebra is identical.

```js
const canWalletMask = typeof wallet.multiplyPoint === 'function'

async function mask (pointHex, keyID) {
  if (canWalletMask) {
    const { point } = await wallet.multiplyPoint({
      point: pointHex, protocolID: [2, 'mental poker deal'], keyID
    })
    return point
  }
  return appMask(pointHex, keyID)          // current fallback
}

async function strip (pointHex, keyID) {
  if (canWalletMask) {
    const { point } = await wallet.multiplyPoint({
      point: pointHex, protocolID: [2, 'mental poker deal'], keyID, invert: true
    })
    return point
  }
  return appStrip(pointHex, keyID)
}
```

`WalletClient` in the PR also exposes `supportsMultiplyPoint()` for the same purpose.

Use a **stable `protocolID`** and a `keyID` that is deterministic per (hand, deck position), or masks
will not reproduce across a reconnect.

### 7.2 Wire substrate support is a separate step

`multiplyPoint` in the SDK is not enough for BSV Desktop, because a browser reaches the wallet over
a substrate. That needs a **call code**, which is an interface-version decision, deliberately left
out of #488. When it happens:

- `packages/sdk/src/wallet/substrates/WalletWireCalls.ts` — add the enum entry (the table currently
  ends at `getVersion = 28`)
- `WalletWireTransceiver.ts` — serialize args; send the point as 33 raw bytes, matching how
  `getPublicKey` returns a key
- `WalletWireProcessor.ts` — decode and dispatch; **check the method exists before calling it**,
  since it is optional, and return a legible error otherwise
- `InvokableWalletBase.ts` (covers `window.CWI`, `XDM`, `ReactNativeWebView`), `HTTPWalletJSON.ts`,
  `WalletClient.ts` — forward it

**Encode `invert` as an optional boolean, never a bare flag.** A wallet reading an older frame must
not read absence as `true`: silently inverting a mask would deal wrong cards rather than fail, which
is far worse than an error.

### 7.3 BSV Desktop needs a change too — and it is small

This is the part that surprises people. Two changes, not one:

1. **The Electron main process is already generic.** `electron/httpServer.ts` uses `app.all('*')`
   with no method allowlist, so it forwards anything.
2. **The renderer is not.** `src/onWalletReady.ts` dispatches on a `switch (req.path)` with a
   `404 'Unknown wallet path'` default. A page calling `/multiplyPoint` gets a 404 **regardless of
   which SDK version is installed.**

So adding support is roughly fifteen lines, copied from the `/getPublicKey` case:

```ts
case '/multiplyPoint': {
  try {
    const args = JSON.parse(req.body) as MultiplyPointArgs
    const result = await wallet.multiplyPoint(args, origin)
    response = { request_id: req.request_id, status: 200, body: JSON.stringify(result) }
  } catch (error) {
    response = { request_id: req.request_id, status: 400,
      body: JSON.stringify({ message: error instanceof Error ? error.message : String(error) }) }
  }
  break
}
```

### 7.4 Running a local wallet-native demo before anything merges

Enough for a demo on a dev machine, not for users:

1. Build the SDK fork: `packages/sdk` on branch `brc-229-invert`, `pnpm --filter @bsv/sdk build`
2. `pnpm link` it into `bsv-desktop` (which already depends on `@bsv/sdk ^2.2.0`)
3. Add the `/multiplyPoint` case from §7.3
4. Run BSV Desktop in dev mode; the poker page reaches it on `localhost:2121` as usual

Note `bsv-desktop` moves fast on pinned SDK versions, so a long-lived fork will drift. Keep it to a
single rebaseable commit and treat it as demo scaffolding.

---

## 8. Traps already paid for

Do not rediscover these.

- **`"files": []` tsconfigs.** Both `ts-sdk` and `ts-stack` use project-reference roots, so
  `tsc --noEmit -p tsconfig.json` reports **0 errors vacuously**. The real check is `tsc -b`.
- **`ts-stack` is pnpm + oxlint + prettier.** npm's flat install breaks `ts-jest` outright.
  `pnpm install --config.engine-strict=false` if a transitive dep rejects the local Node version.
- **`ts-sdk` is archived.** All SDK work belongs in `bsv-blockchain/ts-stack`, `packages/sdk`.
- **Non-canonical points parse silently.** `PublicKey.fromString('02' + 'ff'.repeat(32))` accepts an
  x-coordinate greater than the field prime, reduces it to `0x1000003d0`, and `validate()` then
  returns `true`. **A canonical-encoding check is not implied by an on-curve check**, and it must run
  *before* the parser, because the parser performs the reduction. `go-sdk` has the same behaviour
  independently — this is an interoperability hazard, not one library's quirk.
- **Always baseline before blaming your diff.** `wallet-toolbox` carries 4 pre-existing `TS2307`
  errors for missing express middleware. Establish the baseline with
  `git checkout main -- packages/sdk/src && rebuild`, because a stale built `.d.ts` will make your
  change look innocent or guilty incorrectly.
- **Sonar's zero-new-findings gate is strict.** `typeof x !== 'function'` guards must throw
  `TypeError`, not `Error` (rule `typescript:S7786`).
- **Never make an added interface member required.** See §6.

---

## 9. Definition of done

The deal is wallet-native when all of these hold:

- [ ] `multiplyPoint` (or equivalent) is in a released `@bsv/sdk`
- [ ] A call code exists so it works over a substrate
- [ ] BSV Desktop routes `/multiplyPoint` in `onWalletReady.ts`
- [ ] The poker deal path calls it, with the app fallback retained for older wallets
- [ ] The UI stops showing the `server-dealt` warning pill for wallet-capable seats
- [ ] No masking scalar is ever generated in, or held by, the application for a wallet-capable seat
