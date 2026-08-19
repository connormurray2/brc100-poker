# Co-signing spike findings (task 3.10)

**Result: PASS.** Two wallets, each holding only its own key, funded a 2-of-2 output that
neither controls alone and co-signed a settlement spending it, with real coins on
teratestnet. The non-custodial pot design in `design.md` D5 holds. Reproduce with:

```sh
go run ./cmd/spike-cosign -alice secrets/a.key -bob secrets/b.key \
  -alice-db secrets/a.db -bob-db secrets/b.db
```

Every constraint D5 lists was exercised: randomisation disabled, change output required,
pot coins in a dedicated basket, 125 sat/kB fees, over-declared unlocking script length,
local script verification before broadcast, and client-side locktime finality.

## Confirmed working

- **The two-step path.** `CreateAction(SignAndProcess=false)` returns
  `SignableTransaction{Tx, Reference}`; `SignAction(Reference, Spends)` completes it with a
  caller-supplied unlocking script. This is the load-bearing mechanism for the whole design.
- **Independent signatures assemble.** Two separately-held keys each produced a signature
  over the same transaction; `OP_0 <sigA> <sigB>` satisfied the 2-of-2 and passed the real
  script interpreter locally before broadcast.
- **Declared vs actual script length.** Declared 155 bytes for an assembled 146 — the
  over-declaration rule works and leaves fee margin.
- **Finality gating.** A locktime 100 blocks ahead is correctly judged not-yet-spendable
  while one at the tip is spendable.

## Findings that cost real debugging time

### 1. BRC-29 keyID components are BASE64, the remittance is raw bytes

The single most expensive mistake. The sender derives with:

```go
keyID := brc29.KeyID{
    DerivationPrefix: base64.StdEncoding.EncodeToString(rawPrefix),
    DerivationSuffix: base64.StdEncoding.EncodeToString(rawSuffix),
}
lock, _ := brc29.LockForCounterparty(senderPriv, keyID, recipientPubKey)
```

but `InternalizeAction` receives the **raw** bytes and base64-encodes them internally
before validating. Deriving from the raw strings produces a script the recipient's wallet
does not recognise as its own: the payment lands on-chain and is silently unspendable.

We produced exactly that outcome on the first run — 4,800 sat paid to a plain
identity-key P2PKH, visible on-chain, invisible to the wallet. `examples/internalize` is
ground truth here, and its own comment states it: "The KeyID carries the base64 form; the
InternalizeAction remittance carries the raw bytes."

Also: the sender uses `LockForCounterparty`, not `LockForSelf` (which needs the
recipient's private key). `WithTestNet()` affects only the base58 address string, never the
resulting P2PKH script.

### 2. Internalizing needs BEEF that carries a merkle proof

The BEEF returned at broadcast time has no proof in it — no block existed yet. Passing it
to `InternalizeAction` fails with `beef verification failed (bad merkle proof)`. The proof
must be fetched from the oracle after mining and re-merged:

```go
rec, _ := oracle.GetTx(ctx, txid)          // MerklePath + RawTx
tx, _ := transaction.NewTransactionFromBytes(rec.RawTx)
bump, _ := transaction.NewMerklePathFromHex(hex.EncodeToString(rec.MerklePath))
tx.AddMerkleProof(bump)
beef := transaction.NewBeefV2(); beef.MergeTransaction(tx)
provenBEEF, _ := beef.AtomicBytes(tx.TxID())
```

This is "acceptance is not settlement" made concrete: broadcast succeeded, and the winner
still could not receive the coin.

### 3. Custom outputs are not tracked by the wallet

Confirmed empirically. The pot output appears in its basket with `spendable=false` and is
absent from the balance, because only change is minted into the UTXO store. The application
owns pot-UTXO bookkeeping, exactly as `design.md` D7 assumes.

### 4. A wallet with fragmented, unconfirmed coins stops being able to fund

After several faucet claims and spike runs, a wallet holding 190,202 sat across 9 spendable
outputs could not fund a **1,000 sat** payment: `funder: not enough funds`. Fresh wallets
funded immediately. The cause is not a shortfall — it is coin selection against fragmented
and partly-unconfirmed state, plausibly interacting with `WithRequiredChangeOutput`.

Practical consequence for the table service: **do not assume a positive balance means a
fundable wallet.** Surface funding failures as a distinct operational state, and prefer a
few consolidated coins over many small ones.

### 5. Teratestnet block production is intermittent

Chain height sat at 29600 across 90 seconds of polling, and a settlement stayed `unproven`
for a full 8 minutes before a later run saw `completed`. Anything that waits on a proof
needs a generous timeout and must degrade gracefully rather than hang.

## Not yet proven

- **The payout internalize end-to-end.** The derivation is now correct — the locking-script
  mismatch is gone — but confirming a credited, spendable payout needs a mined settlement,
  which needs the network to produce a block within the wait window. The spike reports this
  as SKIPPED rather than failing, because the co-signing result is already established by
  that point.
- **n-of-n beyond 2-of-2.** The mechanism generalises, but only 2-of-2 has been run.
- **A refusing seat and the refund broadcast.** Task 8.17 covers this; the finality gate is
  proven but no refund has been broadcast after its locktime matured.
