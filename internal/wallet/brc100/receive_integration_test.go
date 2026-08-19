//go:build integration

// Integration tests requiring a funded teratestnet wallet and a mining network.
// Run with: make integration
package brc100

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"
)

// The real receiving case: a wallet that did NOT build the transaction receives a BRC-29
// payment and can spend it. This is what a poker winner does with a pot settlement.
func TestReceiveBRC29PaymentIntoFreshWallet(t *testing.T) {
	senderHexBytes, err := os.ReadFile("../../../secrets/pay.key")
	if err != nil {
		t.Skip("no funded sender key")
	}
	senderHex := strings.TrimSpace(string(senderHexBytes))
	sraw, _ := hex.DecodeString(senderHex)
	senderPriv, _ := ec.PrivateKeyFromBytes(sraw)

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

	// A brand-new recipient wallet with its own key and its own database.
	recipientPriv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	recipientHex := hex.EncodeToString(recipientPriv.Serialize())
	recipient, err := New(ctx, Options{
		Backend: BackendSQLite, SQLitePath: filepath.Join(t.TempDir(), "winner.db"),
		StorageName: "poker-winner", PrivateKeyHex: recipientHex,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recipient.Close(ctx) }()
	if err := recipient.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if bal, _ := recipient.Wallet.Balance(ctx); bal != 0 {
		t.Fatalf("fresh recipient balance = %d, want 0", bal)
	}

	rawPrefix := []byte("poker-hand-0007")
	rawSuffix := []byte("winner-seat-2")
	keyID := brc29.KeyID{
		DerivationPrefix: base64.StdEncoding.EncodeToString(rawPrefix),
		DerivationSuffix: base64.StdEncoding.EncodeToString(rawSuffix),
	}
	lock, err := brc29.LockForCounterparty(senderPriv, keyID, recipientPriv.PubKey())
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
	t.Logf("paid %d sat to the winner in %s", payAmount, txid)

	// Wait for a merkle proof: internalizing verifies one.
	deadline := time.Now().Add(6 * time.Minute)
	var proven []byte
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
			beef := transaction.NewBeefV2()
			if _, err := beef.MergeTransaction(tx); err != nil {
				t.Fatal(err)
			}
			if proven, err = beef.AtomicBytes(tx.TxID()); err != nil {
				t.Fatal(err)
			}
			t.Logf("proof available at height %d", rec.BlockHeight)
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("no merkle proof for %s within the wait window; teratestnet is not mining", txid)
		}
		time.Sleep(20 * time.Second)
	}

	ires, err := recipient.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx:          proven,
		Description: "pot settlement received by the winning seat",
		Labels:      []string{"poker-settlement"},
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex: 0,
			Protocol:    sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  rawPrefix,
				DerivationSuffix:  rawSuffix,
				SenderIdentityKey: senderPriv.PubKey(),
			},
		}},
	}, "poker.local")
	if err != nil {
		t.Fatalf("InternalizeAction on the recipient wallet: %v", err)
	}
	t.Logf("internalize accepted: %v", ires.Accepted)

	after, err := recipient.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("winner balance: %d sat", after)
	if after != payAmount {
		t.Fatalf("winner balance = %d, want %d", after, payAmount)
	}

	out, err := recipient.Wallet.ListOutputs(ctx, sdk.ListOutputsArgs{Basket: "default"}, "poker.local")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out.Outputs {
		t.Logf("  winner output %s  %d sat  spendable=%v", o.Outpoint.String(), o.Satoshis, o.Spendable)
		if !o.Spendable {
			t.Error("the winner's payout is not spendable")
		}
	}
}

func boolPtr(b bool) *bool { return &b }
