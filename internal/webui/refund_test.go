package webui

import (
	"context"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
)

// The deadlock this fixes: a stake cannot be recorded without a refund, so a wallet must be able
// to sign a refund without a recorded stake. It signs because it verifies the refund returns the
// pot to itself, not because it trusts the caller.
func TestSeatsSignRefundsWithNoRecordedStake(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 2, tableKey)
	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tableKey, "table.poker.local", coord, nil)
	if err != nil {
		t.Fatal(err)
	}

	seats := make([]AgentEndpoint, 0, len(ags))
	pubs := make([]*ec.PublicKey, 0, len(ags))
	for _, a := range ags {
		seats = append(seats, AgentEndpoint{
			Seat: a.seat, IdentityKey: a.key.PubKey().ToDERHex(), URL: a.url,
		})
		pubs = append(pubs, a.key.PubKey())
	}

	lock, err := cosign.PotScript(pubs)
	if err != nil {
		t.Fatal(err)
	}
	const potSats = 10000
	pot := cosign.FundedPot{
		Txid:     "aa00000000000000000000000000000000000000000000000000000000000011",
		Vout:     0,
		Script:   lock,
		Satoshis: potSats,
	}

	// Seat 0's refund: the whole pot back to seat 0, timelocked.
	refund, err := cosign.BuildRefund(cosign.RefundArgs{
		Pot: pot, Recipient: pubs[0], Satoshis: potSats - 300, LockHeight: 500000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No stake has been recorded with either wallet. Both must still sign.
	sigs, err := pm.collectRefundSignatures(context.Background(), "refund-hand", refund, 0, seats, pot, pubs[0].ToDERHex())
	if err != nil {
		t.Fatalf("seats would not sign a refund without a recorded stake: %v", err)
	}
	if len(sigs) != len(seats) {
		t.Fatalf("collected %d signatures for %d seats", len(sigs), len(seats))
	}

	// And the assembled refund must actually satisfy the pot, or it protects nobody.
	unlock, err := cosign.Assemble(sigs, len(seats))
	if err != nil {
		t.Fatal(err)
	}
	refund.Inputs[0].UnlockingScript = unlock
	if err := cosign.VerifyScript(refund, 0, lock, potSats); err != nil {
		t.Fatalf("the signed refund does not satisfy the pot: %v", err)
	}
}

// A wallet must refuse to sign a "refund" that pays someone else, which is the attack signRefund
// has to withstand given it needs no recorded stake.
func TestSeatRefusesARefundPayingSomeoneElse(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 2, tableKey)
	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tableKey, "table.poker.local", coord, nil)
	if err != nil {
		t.Fatal(err)
	}

	pubs := []*ec.PublicKey{ags[0].key.PubKey(), ags[1].key.PubKey()}
	lock, err := cosign.PotScript(pubs)
	if err != nil {
		t.Fatal(err)
	}
	const potSats = 10000
	pot := cosign.FundedPot{
		Txid:     "bb00000000000000000000000000000000000000000000000000000000000022",
		Vout:     0,
		Script:   lock,
		Satoshis: potSats,
	}

	// A thief's key, not a seat at this table.
	thief, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	bad, err := cosign.BuildRefund(cosign.RefundArgs{
		Pot: pot, Recipient: thief.PubKey(), Satoshis: potSats - 300, LockHeight: 500000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Ask seat 0 to sign it, claiming it is seat 0's refund.
	// Both seats are named so the table shape is real; the beneficiary is the thief.
	only := []AgentEndpoint{
		{Seat: 0, IdentityKey: pubs[0].ToDERHex(), URL: ags[0].url},
		{Seat: 1, IdentityKey: pubs[1].ToDERHex(), URL: ags[1].url},
	}
	if _, err := pm.collectRefundSignatures(context.Background(), "theft", bad, 0, only, pot, thief.PubKey().ToDERHex()); err == nil {
		t.Fatal("a wallet signed a refund paying a third party")
	}
}
