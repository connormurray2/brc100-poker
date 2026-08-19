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

### 4. An abandoned action leaves a phantom coin that poisons coin selection

Repeatedly, a wallet with a healthy balance became unable to fund even a 500 sat payment:

```
basket default: 10 outputs, 196301 sat total, 95936 sat spendable across 8
Balance() = 95936, claimableInDefault = 8
   500 sat probe: FAILED — funder: not enough funds
```

**Root cause: an output with an all-zero txid.** Listing the outputs shows

```
0000000000000000000000000000000000000000000000000000000000000000.0   365 sat  spendable=false
```

That is the change output of a settlement that was built but never broadcast — the run failed
at verification, so the action was abandoned with its change already recorded against a txid
that does not exist. Its presence blocks the funder entirely: every probe fails regardless of
amount, even though eight genuinely spendable coins remain and `BasketClaimableCount` reports
them as claimable.

Two hypotheses tested and **disproved**: it is not `WithRequiredChangeOutput` (the same
failure occurs with that option removed) and it is not a real shortfall (500 sat fails against
95,936 sat).

Consequences for the table service, in order of importance:

1. **Abandon actions explicitly.** A `CreateAction` that will not be completed must be aborted
   so its provisional change is released, rather than left for the funder to trip over.
2. **A positive balance does not mean a fundable wallet.** Surface funding failure as its own
   operational state, and check for zero-txid outputs when diagnosing it.
3. Prefer a few consolidated coins over many small ones.

### 5. Teratestnet block production is intermittent

Chain height sat at 29600 across 90 seconds of polling, and a settlement stayed `unproven`
for a full 8 minutes before a later run saw `completed`. Anything that waits on a proof
needs a generous timeout and must degrade gracefully rather than hang.

## Proven since

**The payout receive path works end to end.** `receive2_integration_test.go` pays a BRC-29
output to a wallet that did **not** build the transaction — the real winner case — waits for a
block, internalizes it, and asserts the coin is spendable. Confirmed on teratestnet:

```
winner balance before: 0 sat
paid 4800 sat in 9e0e53f4…; mined at height 29650, payout at vout 0
internalize accepted: true
winner balance after: 4800 sat (delta +4800)
output 9e0e53f4…0  4800 sat  spendable=true
```

Two notes for anyone re-running it. Internalizing into the wallet that *built* the transaction
fails on a UNIQUE constraint, because that wallet already holds the output row — proving
receipt requires a separate wallet. And blocks arrive roughly every ten minutes, so a
six-minute wait is too tight; the test allows twenty.

**n-of-n for 2..6 seats.** `internal/protocol/cosign` verifies every table size through the
real script interpreter, not just the 2-of-2 the spike ran.

**The refund recovery path works on-chain.** `refund_integration_test.go` funds a pot, lets the
hand stall with no settlement, and recovers the stake with the pre-signed nLockTime refund:

```
pot funded: 1cbba739…:0 for 5000 sat
refund co-signed and verified locally against the pot script
locktime matured at height 29655
refund accepted for broadcast: status=RECEIVED
refund mined at height 29656 — the stalled stake is recovered
```

One encoding trap worth recording: `Oracle.Broadcast` takes the **binary Extended Format**
blob from `tx.EF()`, not hex and not plain raw bytes. Passing hex-as-bytes is rejected with
`failed to parse transaction`, which reads like a malformed transaction rather than a
malformed encoding. EF carries each input's source satoshis and locking script, which is what
lets the validator check the script without fetching ancestors.

**A complete hand for real value.** `internal/e2e/hand_integration_test.go` joins the game and
money layers: two seats deal with mental poker over the transport, play the hand, and settle
the pot to the winner with both signatures. On teratestnet:

```
pot funded: 2c64b4de…:0 for 4000 sat
refund co-signed and verified, spendable from height 29714
seat 0 holds [3d Ts], seat 1 holds [Jd 9c], board [Ah 5d Jh Jc 4d]
hand complete: seat 1 wins
both seats verified the settlement against their own expectation
settlement broadcast 158fdb59…, mined at height 29666
winner balance 100000 -> 103600 sat (delta +3600)
```

The first run of this test **failed at verification**, and that was the design working: the
wallet added a 365 sat change output to pay the settlement's fee, and the skim check refused an
output the expectation did not account for. A legitimate change output has to be *declared*,
not tolerated — which is exactly the property that makes an illegitimate one refusable.

## Not yet proven

Every money mechanism in the design is now demonstrated on-chain. What remains is breadth
rather than risk: more seats than two in a real-value hand, and the deferred features listed in
the proposal.
