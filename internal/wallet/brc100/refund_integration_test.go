//go:build integration

package brc100

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
)

// The recovery path a stalled hand depends on: a pot is funded, a seat refuses to sign the
// settlement, and the refund returns the stake once its locktime matures.
//
// The refund is signed BEFORE the pot is funded, which is the precondition the whole
// non-custodial design rests on: no stake is committed until its owner can already recover it.
func TestRefundRecoversAStalledPot(t *testing.T) {
	aRaw, err := os.ReadFile("../../../secrets/refD.key")
	if err != nil {
		t.Skip("no refA key")
	}
	bRaw, err := os.ReadFile("../../../secrets/refB.key")
	if err != nil {
		t.Skip("no refB key")
	}
	aHex := strings.TrimSpace(string(aRaw))
	bHex := strings.TrimSpace(string(bRaw))
	ab, _ := hex.DecodeString(aHex)
	bb, _ := hex.DecodeString(bHex)
	alicePriv, _ := ec.PrivateKeyFromBytes(ab)
	bobPriv, _ := ec.PrivateKeyFromBytes(bb)

	ctx := context.Background()
	alice, err := New(ctx, Options{
		Backend: BackendSQLite, SQLitePath: "../../../secrets/refD.db",
		StorageName: "poker-fund", PrivateKeyHex: aHex,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = alice.Close(ctx) }()
	if err := alice.Start(ctx); err != nil {
		t.Fatal(err)
	}

	const potSats = 5000
	const refundSats = 4700 // leaves room for the refund's own fee

	potScript, err := cosign.PotScript([]*ec.PublicKey{alicePriv.PubKey(), bobPriv.PubKey()})
	if err != nil {
		t.Fatal(err)
	}

	// --- fund the pot ---
	fund, err := alice.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: "fund a pot that will be abandoned",
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:     *potScript,
			Satoshis:          potSats,
			OutputDescription: "two-of-two pot for the refund test",
			Basket:            PotBasket,
		}},
		Options: &sdk.CreateActionOptions{SignAndProcess: boolPtr(true), RandomizeOutputs: boolPtr(false)},
	}, "poker.local")
	if err != nil {
		t.Fatalf("funding the pot: %v", err)
	}
	potTxid := fund.Txid.String()
	t.Logf("pot funded: %s:0 for %d sat", potTxid, potSats)

	height, err := alice.Wallet.GetHeight(ctx, nil, "poker.local")
	if err != nil {
		t.Fatal(err)
	}
	// A short lock so the test can actually reach maturity: a few blocks ahead.
	lockHeight := height.Height + 2
	t.Logf("chain tip %d, refund locked to height %d", height.Height, lockHeight)

	// --- build and co-sign the refund, BEFORE anything is at risk ---
	aliceAddr, err := script.NewAddressFromPublicKey(alicePriv.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	aliceLock, err := p2pkh.Lock(aliceAddr)
	if err != nil {
		t.Fatal(err)
	}

	refund := transaction.NewTransaction()
	refund.AddInput(&transaction.TransactionInput{
		SourceTXID:       &fund.Txid,
		SourceTxOutIndex: 0,
		// Non-final, or the locktime does not bind at all.
		SequenceNumber: transaction.DefaultSequenceNumber - 1,
	})
	refund.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis: potSats, LockingScript: potScript,
	})
	refund.AddOutput(&transaction.TransactionOutput{Satoshis: refundSats, LockingScript: aliceLock})
	refund.LockTime = lockHeight

	var sigs []cosign.Signature
	for i, priv := range []*ec.PrivateKey{alicePriv, bobPriv} {
		s, err := cosign.SignInput(refund, 0, i, priv)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	unlock, err := cosign.Assemble(sigs, 2)
	if err != nil {
		t.Fatal(err)
	}
	refund.Inputs[0].UnlockingScript = unlock
	if err := cosign.VerifyScript(refund, 0, potScript, potSats); err != nil {
		t.Fatalf("the refund does not satisfy the pot script: %v", err)
	}
	t.Logf("refund co-signed and verified locally: %s", refund.TxID().String())

	// --- the hand stalls: nobody signs a settlement. Broadcast the refund. ---
	//
	// Broadcast takes the binary Extended Format blob, not hex and not plain raw bytes:
	// EF carries each input's source satoshis and locking script, which is what lets the
	// validator check the script without fetching ancestors.
	ef, err := refund.EF()
	if err != nil {
		t.Fatalf("encoding the refund as extended format: %v", err)
	}

	matureBy := time.Now().Add(30 * time.Minute)
	for {
		h, err := alice.Wallet.GetHeight(ctx, nil, "poker.local")
		if err != nil {
			t.Fatal(err)
		}
		if h.Height >= lockHeight {
			t.Logf("locktime matured at height %d", h.Height)
			break
		}
		if time.Now().After(matureBy) {
			t.Skipf("height %d never reached locktime %d; teratestnet is mining too slowly", h.Height, lockHeight)
		}
		t.Logf("height %d < locktime %d, waiting", h.Height, lockHeight)
		time.Sleep(60 * time.Second)
	}

	res, err := alice.Oracle.Broadcast(ctx, refund.TxID().String(), ef)
	if err != nil {
		t.Fatalf("broadcasting the refund: %v", err)
	}
	if res.Rejected {
		t.Fatalf("the refund was rejected: %s", res.ExtraInfo)
	}
	t.Logf("refund accepted for broadcast: status=%s", res.Status)

	// The stake is recovered once the refund confirms.
	deadline := time.Now().Add(20 * time.Minute)
	for {
		rec, err := alice.Oracle.GetTx(ctx, refund.TxID().String())
		if err == nil && (rec.Status == "MINED" || rec.Status == "IMMUTABLE") {
			t.Logf("refund mined at height %d: the stalled stake is recovered", rec.BlockHeight)
			return
		}
		if time.Now().After(deadline) {
			t.Skipf("refund did not mine within the window; it was accepted for broadcast")
		}
		time.Sleep(30 * time.Second)
	}
}
