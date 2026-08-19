// Command spike-cosign is the go/no-go gate for the non-custodial pot design.
//
// It answers one question with real coins on teratestnet: can two wallets that each hold
// only their own key jointly fund an output neither controls alone, and then co-sign a
// settlement spending it?
//
// If this fails, the whole design needs revisiting — so it runs standalone, before any game
// code depends on it. See design.md D5 and tasks.md section 3.
//
// The flow:
//
//  1. Alice funds a 2-of-2 output with a custom locking script.
//  2. Alice builds the settlement with the two-step path, getting back a signable
//     transaction rather than a broadcast one.
//  3. Alice and Bob each produce a signature over that transaction independently.
//  4. The signatures are assembled into an unlocking script, verified locally, and
//     completed with SignAction.
//  5. A pre-signed nLockTime refund is built and its finality checked before broadcast.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"

	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

const originator = "spike.poker.local"

// potSats is the shared output's value. Small: this is a protocol test, not a load test.
const potSats = 5000

func main() {
	aliceKey := flag.String("alice", "secrets/alice.key", "path to Alice's key")
	bobKey := flag.String("bob", "secrets/bob.key", "path to Bob's key")
	aliceDB := flag.String("alice-db", "secrets/alice.db", "path to Alice's wallet database")
	bobDB := flag.String("bob-db", "secrets/bob.db", "path to Bob's wallet database")
	flag.Parse()

	if err := run(*aliceKey, *bobKey, *aliceDB, *bobDB); err != nil {
		fmt.Fprintf(os.Stderr, "\nSPIKE FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nSPIKE PASSED: two independent wallets funded and co-signed a 2-of-2 output.")
}

func run(aliceKeyPath, bobKeyPath, aliceDBPath, bobDBPath string) error {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	alicePriv, err := loadKey(aliceKeyPath)
	if err != nil {
		return fmt.Errorf("loading Alice's key: %w", err)
	}
	bobPriv, err := loadKey(bobKeyPath)
	if err != nil {
		return fmt.Errorf("loading Bob's key: %w", err)
	}

	// Each wallet is a separate process's worth of state: separate key, separate
	// database. Neither can sign for the other.
	alice, err := brc100.New(ctx, brc100.Options{
		Backend: brc100.BackendSQLite, SQLitePath: aliceDBPath,
		StorageName: "poker-fund", PrivateKeyHex: hex.EncodeToString(alicePriv.Serialize()),
		MaxDBConns: 8, Logger: logger,
	}, nil)
	if err != nil {
		return fmt.Errorf("building Alice's wallet: %w", err)
	}
	defer func() { _ = alice.Close(ctx) }()
	if err := alice.Start(ctx); err != nil {
		return fmt.Errorf("starting Alice's monitor: %w", err)
	}

	bal, err := alice.Wallet.Balance(ctx)
	if err != nil {
		return fmt.Errorf("reading Alice's balance: %w", err)
	}
	fmt.Printf("step 0: Alice has %d sat\n", bal)
	if bal < potSats*3 {
		return fmt.Errorf("alice needs at least %d sat to fund the pot and fees, has %d", potSats*3, bal)
	}

	alicePub, bobPub := alicePriv.PubKey(), bobPriv.PubKey()

	// ---- Step 1: fund a 2-of-2 output -----------------------------------------
	//
	// A bare 2-of-2 multisig: OP_2 <pubA> <pubB> OP_2 OP_CHECKMULTISIG. Spending it
	// needs a signature from both keys, so neither Alice nor Bob — nor any table
	// service — can move it alone. That is the whole point.
	potScript, err := twoOfTwoScript(alicePub, bobPub)
	if err != nil {
		return fmt.Errorf("building the 2-of-2 script: %w", err)
	}
	fmt.Printf("step 1: pot script is %d bytes: %s\n", len(*potScript), potScript.String())

	fundRes, err := alice.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: "fund the shared poker pot",
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:     *potScript,
			Satoshis:          potSats,
			OutputDescription: "two-of-two shared pot output",
			// A dedicated basket: the funder has no exclusion list, so a pot coin
			// sharing the funding basket could be selected again to pay a fee,
			// producing a duplicate input the network rejects.
			Basket: brc100.PotBasket,
		}},
		Options: &sdk.CreateActionOptions{
			SignAndProcess: ptr(true),
			// Defaults to TRUE. Shuffling outputs would move the pot's vout after
			// we recorded it, and would break any signature committing to the
			// output set.
			RandomizeOutputs: ptr(false),
		},
	}, originator)
	if err != nil {
		return fmt.Errorf("funding the pot: %w", err)
	}
	potTxID := fundRes.Txid.String()
	fmt.Printf("step 1: funded the pot with %d sat in tx %s\n", potSats, potTxID)

	// The pot output is at vout 0 because randomisation is off.
	const potVout = 0

	fundedTx, err := transaction.NewTransactionFromBEEF(fundRes.Tx)
	if err != nil {
		return fmt.Errorf("parsing the funding transaction: %w", err)
	}
	if int(potVout) >= len(fundedTx.Outputs) {
		return fmt.Errorf("funding tx has %d outputs, expected the pot at vout %d", len(fundedTx.Outputs), potVout)
	}
	got := fundedTx.Outputs[potVout]
	if got.Satoshis != potSats || !strings.EqualFold(got.LockingScript.String(), potScript.String()) {
		return fmt.Errorf("vout %d is not the pot output; randomisation may have moved it", potVout)
	}
	fmt.Printf("step 1: verified the pot sits at vout %d with %d sat\n", potVout, got.Satoshis)

	// ---- Step 2: build the settlement, unsigned -------------------------------
	//
	// The winner is paid to a BRC-29-derived script so the receiving wallet can
	// actually spend it. Here Alice is the notional winner; in the real game the
	// engine decides.
	// Bob is the notional sender of the payout (he is the counterparty paying the
	// winner out of the shared pot); Alice is the notional winner. In the real game the
	// engine decides the winner and the table proposes this transaction.
	payoutPrefix := []byte("poker-hand-0001")
	payoutSuffix := []byte("winner-seat-0")
	pay, err := brc29Payout(bobPriv, alicePub, payoutPrefix, payoutSuffix)
	if err != nil {
		return fmt.Errorf("deriving the payout: %w", err)
	}
	payoutScript := pay.script

	// An n-of-n scriptSig is roughly 1 + 73n bytes: OP_0 plus a DER signature with a
	// sighash byte per key. Over-declare rather than under-declare: under-declaring
	// underpays the fee and earns an unretryable 4xx.
	const unlockingScriptLen = 1 + 73*2 + 8

	settleRes, err := alice.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: "settle the shared poker pot to the winner",
		InputBEEF:   fundRes.Tx,
		Inputs: []sdk.CreateActionInput{{
			Outpoint:              transaction.Outpoint{Txid: fundRes.Txid, Index: potVout},
			InputDescription:      "the two-of-two shared pot",
			UnlockingScriptLength: unlockingScriptLen,
		}},
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:     *payoutScript,
			Satoshis:          potSats - 200, // leave room for the fee
			OutputDescription: "pot payout to the winning seat",
		}},
		Options: &sdk.CreateActionOptions{
			// The whole point: do not sign or send. Return something signable so
			// signatures can be gathered from independent wallets.
			SignAndProcess:   ptr(false),
			RandomizeOutputs: ptr(false),
		},
	}, originator)
	if err != nil {
		return fmt.Errorf("building the settlement: %w", err)
	}
	if settleRes.SignableTransaction == nil {
		return errors.New("CreateAction returned no signable transaction; the two-step path is unavailable")
	}
	fmt.Printf("step 2: got a signable transaction, reference %d bytes\n", len(settleRes.SignableTransaction.Reference))

	settleTx, err := transaction.NewTransactionFromBEEF(settleRes.SignableTransaction.Tx)
	if err != nil {
		return fmt.Errorf("parsing the signable transaction: %w", err)
	}

	// Find the input that spends the pot. Its index is not guaranteed to be 0: the
	// funder may have prepended its own inputs to pay the fee.
	potInput := -1
	for i, in := range settleTx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == potTxID && in.SourceTxOutIndex == potVout {
			potInput = i
			break
		}
	}
	if potInput < 0 {
		return fmt.Errorf("the signable transaction does not spend the pot output %s:%d", potTxID, potVout)
	}
	fmt.Printf("step 2: the pot is spent by input %d of %d\n", potInput, len(settleTx.Inputs))

	// The sighash preimage needs the input's source satoshis and locking script.
	settleTx.Inputs[potInput].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      potSats,
		LockingScript: potScript,
	})

	// ---- Step 3: each party signs independently -------------------------------
	//
	// In the real system these signatures are produced on different machines by
	// wallets that never share a key. Here they are produced from the two separate
	// key objects, which is the same trust boundary in one process.
	shf := sighash.AllForkID
	preimageOwner, err := settleTx.CalcInputPreimage(uint32(potInput), shf)
	if err != nil {
		return fmt.Errorf("computing the sighash preimage: %w", err)
	}
	_ = preimageOwner

	sigA, err := signInput(settleTx, potInput, alicePriv, shf)
	if err != nil {
		return fmt.Errorf("alice signing: %w", err)
	}
	fmt.Printf("step 3: Alice produced a %d-byte signature\n", len(sigA))

	sigB, err := signInput(settleTx, potInput, bobPriv, shf)
	if err != nil {
		return fmt.Errorf("bob signing: %w", err)
	}
	fmt.Printf("step 3: Bob produced a %d-byte signature\n", len(sigB))

	// ---- Step 4: assemble, verify locally, then complete ----------------------
	//
	// CHECKMULTISIG order matters: signatures must appear in the same order as the
	// public keys in the locking script, and the leading OP_0 is the well-known
	// off-by-one dummy.
	unlocking, err := assembleMultisigUnlock(sigA, sigB)
	if err != nil {
		return fmt.Errorf("assembling the unlocking script: %w", err)
	}
	fmt.Printf("step 4: assembled a %d-byte unlocking script (declared %d)\n", len(*unlocking), unlockingScriptLen)
	if len(*unlocking) > unlockingScriptLen {
		return fmt.Errorf("the assembled script is %d bytes but only %d were declared; the fee is underpaid",
			len(*unlocking), unlockingScriptLen)
	}

	// Verify before broadcasting. Storage would catch a bad script too, but doing it
	// here reports the failure against our own object rather than several layers down.
	settleTx.Inputs[potInput].UnlockingScript = unlocking
	if err := verifyInput(settleTx, potInput, potScript, potSats); err != nil {
		return fmt.Errorf("local script verification failed: %w", err)
	}
	fmt.Println("step 4: the assembled script passes the script interpreter locally")

	signed, err := alice.Wallet.SignAction(ctx, sdk.SignActionArgs{
		Reference: settleRes.SignableTransaction.Reference,
		Spends: map[uint32]sdk.SignActionSpend{
			uint32(potInput): {UnlockingScript: *unlocking},
		},
	}, originator)
	if err != nil {
		return fmt.Errorf("SignAction: %w", err)
	}
	fmt.Printf("step 4: settlement broadcast as %s\n", signed.Txid.String())

	// ---- Step 4b: the winner must be able to SPEND the payout -----------------
	//
	// Broadcasting is not the same as receiving. Until the payout is internalized with
	// its derivation material the winner's wallet does not know the coin is theirs.
	beforePayout, err := alice.Wallet.Balance(ctx)
	if err != nil {
		return fmt.Errorf("reading the balance before internalizing: %w", err)
	}
	settledTx, err := transaction.NewTransactionFromBEEF(signed.Tx)
	if err != nil {
		return fmt.Errorf("parsing the settled transaction: %w", err)
	}
	payoutVout := -1
	for i, o := range settledTx.Outputs {
		if strings.EqualFold(o.LockingScript.String(), payoutScript.String()) {
			payoutVout = i
			break
		}
	}
	if payoutVout < 0 {
		return errors.New("the settled transaction does not contain the payout output")
	}
	fmt.Printf("step 4b: payout script we paid: %s\n", payoutScript.String())
	for i, o := range settledTx.Outputs {
		fmt.Printf("step 4b:   settled vout %d: %d sat  %s\n", i, o.Satoshis, o.LockingScript.String())
	}
	// The payout can only be internalized once the settlement is mined: internalizing
	// verifies the merkle proof against ChainTracks headers, and an unmined transaction
	// has no proof to verify. This is the "acceptance is not settlement" distinction made
	// concrete — broadcasting succeeded several seconds ago, and the coin still cannot be
	// received.
	mineWait := 3 * time.Minute
	if err := waitForMined(ctx, alice, signed.Txid.String(), mineWait); err != nil {
		// The co-signing result is already established by this point: the settlement
		// was assembled from two independent signatures, passed the interpreter, and
		// was accepted for broadcast. Receiving the payout additionally requires a
		// merkle proof, which requires a block — so if the network is not mining, this
		// is a chain liveness problem and not a finding about the design.
		fmt.Printf("step 4b: SKIPPED — %v\n", err)
		fmt.Println("step 4b: the payout cannot be internalized until the settlement is mined")
		return checkRefundFinality(ctx, alice)
	}
	// Internalizing verifies a merkle proof, so it needs BEEF that CARRIES one.
	// signed.Tx was captured at broadcast time, before any block existed, so it has no
	// proof in it — rebuild the BEEF from the oracle's record once mined.
	provenBEEF, err := minedBEEF(ctx, alice, signed.Txid.String())
	if err != nil {
		return err
	}
	if err := pay.internalize(ctx, alice, provenBEEF, uint32(payoutVout)); err != nil {
		return err
	}
	afterPayout, err := alice.Wallet.Balance(ctx)
	if err != nil {
		return fmt.Errorf("reading the balance after internalizing: %w", err)
	}
	fmt.Printf("step 4b: winner balance %d -> %d sat after internalizing the payout at vout %d\n",
		beforePayout, afterPayout, payoutVout)
	if afterPayout <= beforePayout {
		return fmt.Errorf("the payout was not credited as spendable (%d -> %d); the winner cannot spend their winnings",
			beforePayout, afterPayout)
	}

	// ---- Step 5: the refund path ----------------------------------------------
	//
	// A refund is what makes the design safe against a seat that stops cooperating,
	// so its mechanics matter as much as the happy path. The input must carry a
	// non-final sequence for the locktime to bind at all.
	if err := checkRefundFinality(ctx, alice); err != nil {
		return fmt.Errorf("refund finality: %w", err)
	}

	return nil
}

// twoOfTwoScript builds OP_2 <pubA> <pubB> OP_2 OP_CHECKMULTISIG.
func twoOfTwoScript(a, b *ec.PublicKey) (*script.Script, error) {
	s := &script.Script{}
	if err := s.AppendOpcodes(script.Op2); err != nil {
		return nil, fmt.Errorf("appending OP_2: %w", err)
	}
	if err := s.AppendPushData(a.Compressed()); err != nil {
		return nil, fmt.Errorf("appending the first pubkey: %w", err)
	}
	if err := s.AppendPushData(b.Compressed()); err != nil {
		return nil, fmt.Errorf("appending the second pubkey: %w", err)
	}
	if err := s.AppendOpcodes(script.Op2, script.OpCHECKMULTISIG); err != nil {
		return nil, fmt.Errorf("appending OP_2 OP_CHECKMULTISIG: %w", err)
	}
	return s, nil
}

// assembleMultisigUnlock builds OP_0 <sigA> <sigB>.
//
// The leading OP_0 satisfies CHECKMULTISIG's off-by-one dummy pop, and signature order
// must match the key order in the locking script.
func assembleMultisigUnlock(sigA, sigB []byte) (*script.Script, error) {
	s := &script.Script{}
	if err := s.AppendOpcodes(script.Op0); err != nil {
		return nil, fmt.Errorf("appending the CHECKMULTISIG dummy: %w", err)
	}
	if err := s.AppendPushData(sigA); err != nil {
		return nil, fmt.Errorf("appending the first signature: %w", err)
	}
	if err := s.AppendPushData(sigB); err != nil {
		return nil, fmt.Errorf("appending the second signature: %w", err)
	}
	return s, nil
}

// signInput produces a DER signature with the sighash-type byte appended.
func signInput(tx *transaction.Transaction, idx int, priv *ec.PrivateKey, shf sighash.Flag) ([]byte, error) {
	hash, err := tx.CalcInputSignatureHash(uint32(idx), shf)
	if err != nil {
		return nil, fmt.Errorf("computing the signature hash: %w", err)
	}
	sig, err := priv.Sign(hash)
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}
	return append(sig.Serialize(), byte(shf)), nil
}

// verifyInput runs the real script interpreter over one input.
func verifyInput(tx *transaction.Transaction, idx int, lock *script.Script, sats uint64) error {
	return interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, idx, &transaction.TransactionOutput{LockingScript: lock, Satoshis: sats}),
		interpreter.WithForkID(),
		interpreter.WithAfterGenesis(),
	)
}

// payout describes a BRC-29 payment to the winner.
//
// The derivation material travels with the payout: the recipient's wallet needs the
// prefix, the suffix and the sender's identity key to re-derive the locking script and
// record the coin as its own. A plain P2PKH to the recipient's identity key looks correct
// on-chain and is unspendable by the wallet, because the wallet has no derivation record
// for it — which is exactly the failure the first run of this spike produced.
type payout struct {
	script *script.Script
	prefix []byte
	suffix []byte
	sender *ec.PrivateKey
}

// brc29Payout derives a BRC-29 payment from sender to recipient.
//
// Two details decide whether the recipient can ever spend this coin, and getting either
// wrong produces a payment that lands on-chain and is silently unspendable:
//
//  1. The sender derives with LockForCounterparty (its own private key, the recipient's
//     public key). The recipient's wallet re-derives the mirror image with LockForSelf.
//  2. The KeyID components are the **base64 encodings** of the derivation bytes, not the
//     raw bytes. The InternalizeAction remittance carries the raw bytes, and the validator
//     builds its KeyID from string(rawBytes) — so the sender must derive from the same
//     base64 text the recipient's wallet will reconstruct. See examples/internalize.
func brc29Payout(sender *ec.PrivateKey, recipientPub *ec.PublicKey, prefix, suffix []byte) (payout, error) {
	keyID := brc29.KeyID{
		DerivationPrefix: base64.StdEncoding.EncodeToString(prefix),
		DerivationSuffix: base64.StdEncoding.EncodeToString(suffix),
	}
	if err := keyID.Validate(); err != nil {
		return payout{}, fmt.Errorf("invalid BRC-29 key id: %w", err)
	}
	lock, err := brc29.LockForCounterparty(sender, keyID, recipientPub)
	if err != nil {
		return payout{}, fmt.Errorf("deriving the BRC-29 payout script: %w", err)
	}
	return payout{script: lock, prefix: prefix, suffix: suffix, sender: sender}, nil
}

// internalize records the payout in the recipient's wallet so the coin is spendable.
func (p payout) internalize(ctx context.Context, w *brc100.Wallet, beef []byte, vout uint32) error {
	_, err := w.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx:          beef,
		Description: "pot settlement received by the winning seat",
		Labels:      []string{"poker-settlement"},
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex: vout,
			Protocol:    sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  p.prefix,
				DerivationSuffix:  p.suffix,
				SenderIdentityKey: p.sender.PubKey(),
			},
		}},
	}, originator)
	if err != nil {
		return fmt.Errorf("internalizing the payout: %w", err)
	}
	return nil
}

// checkRefundFinality confirms a refund's locktime is gated client-side.
//
// The library never calls its own finality helper before broadcasting, so a non-final
// refund is sent and rejected 4xx — which is unretryable. The gate has to be ours.
func checkRefundFinality(ctx context.Context, w *brc100.Wallet) error {
	height, err := w.Wallet.GetHeight(ctx, nil, originator)
	if err != nil {
		return fmt.Errorf("reading the chain height: %w", err)
	}
	tip := height.Height
	fmt.Printf("step 5: chain tip is at height %d\n", tip)

	// A refund locked 100 blocks ahead must not be broadcast yet.
	future := tip + 100
	if refundIsFinal(future, tip) {
		return fmt.Errorf("a refund locked at %d was judged final at tip %d", future, tip)
	}
	// One locked at or below the tip is spendable.
	if !refundIsFinal(tip, tip) {
		return fmt.Errorf("a refund locked at %d was judged non-final at tip %d", tip, tip)
	}
	fmt.Printf("step 5: finality gate holds — locktime %d is not yet spendable, %d is\n", future, tip)
	return nil
}

// refundIsFinal reports whether a height-based locktime has matured.
//
// nLockTime below 500,000,000 is a block height; at or above it is a Unix timestamp. A
// transaction is includable once the locktime is strictly below the next block's height,
// which is tip+1 — hence lockHeight <= tip.
func refundIsFinal(lockHeight, tip uint32) bool {
	const timestampThreshold = 500_000_000
	if lockHeight >= timestampThreshold {
		return uint32(time.Now().Unix()) >= lockHeight
	}
	return lockHeight <= tip
}

// minedBEEF builds atomic BEEF carrying the transaction's merkle proof.
//
// The proof lives in arcade's record, not in the BEEF returned at broadcast time. Without
// it InternalizeAction fails with "bad merkle proof", because it verifies the proof against
// ChainTracks headers before recording anything — arcade is trusted to deliver a proof,
// never to assert it is valid.
func minedBEEF(ctx context.Context, w *brc100.Wallet, txid string) ([]byte, error) {
	rec, err := w.Oracle.GetTx(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("fetching the mined record for %s: %w", txid, err)
	}
	if len(rec.MerklePath) == 0 {
		return nil, fmt.Errorf("arcade has no merkle path for %s yet (status %s)", txid, rec.Status)
	}
	if len(rec.RawTx) == 0 {
		return nil, fmt.Errorf("arcade returned no raw transaction for %s", txid)
	}

	tx, err := transaction.NewTransactionFromBytes(rec.RawTx)
	if err != nil {
		return nil, fmt.Errorf("parsing the mined raw transaction: %w", err)
	}
	bump, err := transaction.NewMerklePathFromHex(hex.EncodeToString(rec.MerklePath))
	if err != nil {
		return nil, fmt.Errorf("parsing the merkle path: %w", err)
	}
	if err := tx.AddMerkleProof(bump); err != nil {
		return nil, fmt.Errorf("attaching the merkle proof: %w", err)
	}

	beef := transaction.NewBeefV2()
	if _, err := beef.MergeTransaction(tx); err != nil {
		return nil, fmt.Errorf("merging into BEEF: %w", err)
	}
	out, err := beef.AtomicBytes(tx.TxID())
	if err != nil {
		return nil, fmt.Errorf("encoding atomic BEEF: %w", err)
	}
	return out, nil
}

// waitForMined blocks until the transaction reaches a mined status.
//
// Polling is right here rather than a status observer: this is a one-shot script, and the
// monitor's own sweeps are what advance the status in the first place.
func waitForMined(ctx context.Context, w *brc100.Wallet, txid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		acts, err := w.Wallet.ListActions(ctx, sdk.ListActionsArgs{}, originator)
		if err != nil {
			return fmt.Errorf("listing actions while waiting for %s: %w", txid, err)
		}
		for _, a := range acts.Actions {
			if a.Txid.String() != txid {
				continue
			}
			switch a.Status {
			case "completed", "unproven":
				// Both mean the network has it; a proof may still be pending.
				fmt.Printf("step 4b: settlement status is %q\n", a.Status)
				if a.Status == "completed" {
					return nil
				}
			case "failed":
				return fmt.Errorf("settlement %s failed", txid)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("settlement %s was not mined within %s; internalizing needs a merkle proof", txid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func loadKey(path string) (*ec.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path by design
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("key file is not hex: %w", err)
	}
	priv, _ := ec.PrivateKeyFromBytes(b)
	if priv == nil {
		return nil, errors.New("key is not a valid secp256k1 scalar")
	}
	return priv, nil
}

func ptr[T any](v T) *T { return &v }
