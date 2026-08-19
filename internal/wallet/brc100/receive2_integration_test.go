//go:build integration

package brc100

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"
)

// Proves a winner can receive AND spend a BRC-29 payout, using a persisted recipient key so
// the wait for a block can span runs. Blocks on teratestnet arrive roughly every ten
// minutes, so the window is generous.
func TestReceiveAndSpendPayout(t *testing.T) {
	senderRaw, err := os.ReadFile("../../../secrets/pay.key")
	if err != nil {
		t.Skip("no funded sender key")
	}
	senderHex := strings.TrimSpace(string(senderRaw))
	sb, _ := hex.DecodeString(senderHex)
	senderPriv, _ := ec.PrivateKeyFromBytes(sb)

	recipRaw, err := os.ReadFile("../../../secrets/winner.key")
	if err != nil {
		t.Skip("no winner key; generate one with cmd/keygen -out secrets/winner.key")
	}
	recipHex := strings.TrimSpace(string(recipRaw))
	rb, _ := hex.DecodeString(recipHex)
	recipPriv, _ := ec.PrivateKeyFromBytes(rb)

	ctx := context.Background()
	sender, err := New(ctx, Options{
		Backend: BackendSQLite, SQLitePath: "../../../secrets/pay.db",
		StorageName: "poker-fund", PrivateKeyHex: senderHex,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sender.Close(ctx) }()
	if err := sender.Start(ctx); err != nil {
		t.Fatal(err)
	}

	recipient, err := New(ctx, Options{
		Backend: BackendSQLite, SQLitePath: "../../../secrets/winner.db",
		StorageName: "poker-winner", PrivateKeyHex: recipHex,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recipient.Close(ctx) }()
	if err := recipient.Start(ctx); err != nil {
		t.Fatal(err)
	}

	before, err := recipient.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("winner balance before: %d sat", before)

	rawPrefix := []byte("poker-hand-recv")
	rawSuffix := []byte("winner-seat-0")
	keyID := brc29.KeyID{
		DerivationPrefix: base64.StdEncoding.EncodeToString(rawPrefix),
		DerivationSuffix: base64.StdEncoding.EncodeToString(rawSuffix),
	}
	lock, err := brc29.LockForCounterparty(senderPriv, keyID, recipPriv.PubKey())
	if err != nil {
		t.Fatal(err)
	}

	const payAmount = 4800
	res, err := sender.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: "pay the winning seat its pot share",
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:     *lock,
			Satoshis:          payAmount,
			OutputDescription: "winner payout output",
		}},
		Options: &sdk.CreateActionOptions{SignAndProcess: boolPtr(true), RandomizeOutputs: boolPtr(false)},
	}, "poker.local")
	if err != nil {
		t.Fatalf("paying the winner: %v", err)
	}
	txid := res.Txid.String()
	t.Logf("paid %d sat in %s; waiting for a block", payAmount, txid)

	deadline := time.Now().Add(20 * time.Minute)
	var proven []byte
	var vout uint32
	for {
		rec, err := sender.Oracle.GetTx(ctx, txid)
		if err == nil && len(rec.MerklePath) > 0 && len(rec.RawTx) > 0 {
			tx, err := transaction.NewTransactionFromBytes(rec.RawTx)
			if err != nil {
				t.Fatal(err)
			}
			bump, err := transaction.NewMerklePathFromHex(hex.EncodeToString(rec.MerklePath))
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.AddMerkleProof(bump); err != nil {
				t.Fatal(err)
			}
			for i, o := range tx.Outputs {
				if strings.EqualFold(o.LockingScript.String(), lock.String()) {
					vout = uint32(i)
				}
			}
			beef := transaction.NewBeefV2()
			if _, err := beef.MergeTransaction(tx); err != nil {
				t.Fatal(err)
			}
			if proven, err = beef.AtomicBytes(tx.TxID()); err != nil {
				t.Fatal(err)
			}
			t.Logf("mined at height %d, payout at vout %d", rec.BlockHeight, vout)
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("no block for %s within 20 minutes", txid)
		}
		time.Sleep(30 * time.Second)
	}

	ires, err := recipient.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx:          proven,
		Description: "pot settlement received by the winning seat",
		Labels:      []string{"poker-settlement"},
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex: vout,
			Protocol:    sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  rawPrefix,
				DerivationSuffix:  rawSuffix,
				SenderIdentityKey: senderPriv.PubKey(),
			},
		}},
	}, "poker.local")
	if err != nil {
		t.Fatalf("InternalizeAction on the recipient: %v", err)
	}
	t.Logf("internalize accepted: %v", ires.Accepted)

	after, err := recipient.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("winner balance after: %d sat (delta %+d)", after, int64(after)-int64(before))
	if after <= before {
		t.Fatalf("the payout was not credited as spendable: %d -> %d", before, after)
	}

	// Receiving is not enough: the winner must be able to SPEND it.
	out, err := recipient.Wallet.ListOutputs(ctx, sdk.ListOutputsArgs{Basket: "default"}, "poker.local")
	if err != nil {
		t.Fatal(err)
	}
	spendable := false
	for _, o := range out.Outputs {
		t.Logf("  output %s  %d sat  spendable=%v", o.Outpoint.String(), o.Satoshis, o.Spendable)
		if o.Spendable {
			spendable = true
		}
	}
	if !spendable {
		t.Fatal("the winner holds no spendable output")
	}
}
