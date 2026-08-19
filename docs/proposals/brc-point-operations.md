# Point operations for BRC-100 wallets

**Status:** draft proposal
**Author:** brc100-poker project
**Requires:** BRC-100 (wallet interface), BRC-42/43 (key derivation)

## Abstract

Two methods, `derivePoint` and `multiplyPoint`, letting an application ask a BRC-100 wallet to
perform scalar multiplication on a caller-supplied secp256k1 point using a derived key that the
wallet never discloses.

This is the missing primitive for protocols built on commutative encryption — mental poker,
verifiable shuffles, oblivious transfer, threshold schemes — where a participant must transform a
point without learning any other participant's secret, and without their own secret leaving the
wallet.

## Motivation

BRC-100 gives a web application a complete surface for **money**: `createAction`, `signAction`,
`createSignature`, `internalizeAction`. A browser page can build, sign and settle transactions with
the user's key never leaving the wallet.

It gives that application nothing for **protocols over points**. The existing surface —
`getPublicKey`, `createSignature`, `encrypt`, `decrypt`, `createHmac` — never accepts a caller
-supplied curve point as input, so an application that needs `s·P` has no way to ask for it.

The concrete case that motivated this proposal is a dealerless poker deal. Barnett–Smart mental
poker encodes card *i* as a fixed public point `Mᵢ = (i+1)·G`, and each player masks the deck by
multiplying every point by a secret scalar. Because scalar multiplication commutes —
`a·(b·P) == b·(a·P)` — masks applied by different players can be stripped in any order, which is
what lets a player be dealt a card nobody else can read.

Every step of that protocol is point multiplication over points other players produced. With no
wallet primitive for it, an application has exactly three options, and all three are worse:

1. **Generate the secrets in the browser.** They then live in JS memory, where a hostile script on
   the origin can read them, and a tab crash mid-hand loses the hand.
2. **Run a separate signing process per player.** Secure, and a requirement no ordinary user will
   accept: it defeats the purpose of having a wallet.
3. **Have a server deal the cards.** Which is the thing mental poker exists to avoid.

Applications are choosing (1) today because it is the only option a user will tolerate. This
proposal makes the secure option also the convenient one.

## Specification

### `derivePoint`

Returns the public point of a derived key, so a counterparty can be told which key will be used
before any protocol run begins.

```ts
derivePoint(args: {
  protocolID: [SecurityLevel, ProtocolString5To400Bytes]
  keyID: KeyIDStringUnder800Bytes
  counterparty?: PubKeyHex          // default 'self'
}): Promise<{ point: PubKeyHex }>   // compressed, 33 bytes
```

This is deliberately equivalent to `getPublicKey` for the same derivation. It exists as a separate
name so an application can be explicit that a value is a protocol point rather than an identity, and
so a wallet may present it differently in a permission prompt.

### `multiplyPoint`

Multiplies a caller-supplied point by the derived private key.

```ts
multiplyPoint(args: {
  point: PubKeyHex                  // compressed, 33 bytes
  protocolID: [SecurityLevel, ProtocolString5To400Bytes]
  keyID: KeyIDStringUnder800Bytes
  counterparty?: PubKeyHex
  invert?: BooleanDefaultFalse      // multiply by the modular inverse instead
  privileged?: BooleanDefaultFalse
  privilegedReason?: DescriptionString5to50Bytes
}): Promise<{ point: PubKeyHex }>
```

`invert: true` multiplies by `d⁻¹ mod n` rather than `d`, which is how a mask is removed. Without it
an application would have to ask for the scalar itself to unmask, defeating the point.

### Wallet obligations

A conforming wallet:

1. **MUST** derive the key from `protocolID` and `keyID` exactly as BRC-42/43 specifies for every
   other operation. It **MUST NOT** use an identity key, a spending key, or any key that signs
   transactions.
2. **MUST** validate that `point` decodes to a valid point on secp256k1 and is not the identity.
   An invalid point **MUST** be rejected, not coerced.
3. **MUST** reject a derived scalar of zero, and **MUST** reject `invert: true` when the scalar has
   no inverse. Both are unreachable for a correct derivation, and silently returning a wrong point
   would corrupt a protocol run undetectably.
4. **MUST** apply the same origin and permission policy it applies to `createSignature`. This is a
   key-using operation and belongs behind the same gate.
5. **SHOULD** treat a high rate of `multiplyPoint` calls under one `keyID` as ordinary protocol
   traffic rather than suspicious: a 52-card deal is 52 calls per pass.

### Errors

Reuses BRC-100's existing error taxonomy: an invalid point is a request error, a refused permission
is a permission error, and an unsupported method is the standard unknown-method error so an
application can detect absence and fall back.

## Security considerations

### This is not a signing oracle

The obvious objection is that `multiplyPoint` is a signing oracle: an attacker supplies a point and
learns something linear in the private key.

It is not, for two independent reasons.

**Recovering the scalar is ECDLP.** Given `d·P` for chosen `P`, recovering `d` is the discrete
logarithm problem. `d·G` is already public for any key with a public point, so the structure the
method exposes is not new.

**The derived key never signs anything.** ECDSA is dangerous under an oracle because the same `d`
appears in `s = k⁻¹(z + r·d)`. Here the key derived from a protocol `keyID` is not a spending key and
produces no transaction signatures, so there is no signature equation for an oracle to attack. This
is the same separation BRC-100 already relies on for `encrypt` and `createHmac`, which are likewise
key-using operations over caller-supplied data.

### The real risk is key reuse, and derivation addresses it

The genuine danger would be a wallet implementing this over an identity or spending key. Then
`multiplyPoint(Q)` for a counterparty's public key `Q` yields the static ECDH shared secret `d·Q`,
which would let a caller decrypt messages encrypted to that counterparty.

Mandatory BRC-42/43 derivation is what prevents this, which is why obligation 1 is a MUST rather
than a SHOULD. A wallet that ignores it has built a genuine oracle.

### An application still cannot read another player's cards

Worth stating because it is the property the motivating protocol depends on. `multiplyPoint` applies
*this* wallet's secret. Reading a masked card requires every participant's secret, and no
participant's wallet will disclose one — it only ever returns a transformed point. The primitive lets
a player participate; it does not let anyone reconstruct someone else's view.

### Invalid-point rejection is load-bearing

An off-curve or identity point must be refused rather than coerced onto the curve. Accepting one
would let a participant inject a value whose transformation reveals structure about the scalar, and
is the standard invalid-curve attack.

**A canonical-encoding check is required, and is not implied by an on-curve check.** Writing the
reference implementation surfaced this concretely: go-sdk's `PublicKeyFromString` accepts the
compressed point `02` followed by thirty-two `ff` bytes — an x-coordinate numerically **greater than
the field prime** — reduces it silently, and `IsOnCurve` then reports `true`. An implementation that
validates only with `IsOnCurve` therefore accepts a point that was never validly encoded.

A conforming wallet **MUST** check that both coordinates are in `[0, p)` before the on-curve test.
The reference implementation does, and a test asserts that this exact value is rejected.

## Rationale for the chosen shape

**Why not expose the scalar under a derived key?** An application could then do the arithmetic
itself. But a scalar that leaves the wallet is a scalar in the browser, which is precisely the
weakness this proposal removes.

**Why `invert` as a flag rather than a separate method?** Masking and unmasking are the same
operation with a different scalar, and a wallet that offered only the forward direction would force
an application to obtain the scalar to unmask.

**Why reuse `protocolID`/`keyID` rather than a new namespace?** Because every existing BRC-100
key-using method uses them, so a wallet's derivation, permission and audit paths already handle them.
A new namespace would be new code in the security-critical path for no benefit.

## Backwards compatibility

Purely additive. A wallet that does not implement these methods returns the standard unknown-method
error, which an application can detect and fall back from. No existing method changes.

## Reference implementation

`internal/brc/points` in the brc100-poker project implements both methods against secp256k1, with
tests covering the properties a protocol depends on:

- commutativity across independent keys — `a·(b·P) == b·(a·P)`
- `invert` recovering the original point
- rejection of off-curve, identity, non-canonically-encoded, and malformed points
- rejection of a zero scalar
- key separation: the same point under different `keyID`s produces unrelated results

The same project contains a working dealerless poker deal built on the primitive, which is the
end-to-end demonstration that the shape is sufficient.
