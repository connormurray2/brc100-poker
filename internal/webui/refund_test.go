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

	// One refund paying each seat its own balance, which is what makes refusing to settle
	// unprofitable. Built through the manager so the balances and the ladder come from it.
	lp := &livePot{
		pot: pot, seats: pubs,
		balances:    map[int]uint64{0: potSats / 2, 1: potSats / 2},
		lockHeight:  500000,
		floorHeight: 1000,
	}
	refund, err := cosign.BuildRefund(cosign.RefundArgs{
		Pot: pot,
		Recipients: []cosign.RefundOutput{
			{Recipient: pubs[0], Satoshis: potSats / 2},
			{Recipient: pubs[1], Satoshis: potSats / 2},
		},
		LockHeight: 500000, Fee: refundFee,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No stake has been recorded with either wallet. Both must still sign.
	sigs, err := pm.collectSharedRefundSignatures(context.Background(), "refund-session", refund, 0, seats, lp)
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

	// A thief's key, not a seat at this table. The refund pays the pot to them.
	thief, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	bad, err := cosign.BuildRefund(cosign.RefundArgs{
		Pot: pot,
		Recipients: []cosign.RefundOutput{
			{Recipient: thief.PubKey(), Satoshis: potSats},
		},
		LockHeight: 500000, Fee: refundFee,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The table claims the thief holds the whole balance. A seat paid nothing must refuse.
	lp := &livePot{
		pot: pot, seats: []*ec.PublicKey{thief.PubKey()},
		balances:    map[int]uint64{0: potSats},
		lockHeight:  500000,
		floorHeight: 1000,
	}
	only := []AgentEndpoint{{Seat: 0, IdentityKey: pubs[0].ToDERHex(), URL: ags[0].url}}
	if _, err := pm.collectSharedRefundSignatures(context.Background(), "theft", bad, 0, only, lp); err == nil {
		t.Fatal("a wallet signed a refund paying a third party")
	}
}

// The end-to-end property: after a hand moves the balances, the seats sign a refund reflecting the
// new state, and it matures earlier than the one it replaces.
//
// This is what makes refusing unprofitable across a session. A seat that loses a hand and then
// refuses to sign the new refund still holds the previous one, which matures LATER -- so an honest
// seat broadcasting the newer refund wins the race, and the refuser gains nothing by stalling.
func TestApplyHandResignsALadderedRefund(t *testing.T) {
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
	seats := []AgentEndpoint{
		{Seat: 0, IdentityKey: pubs[0].ToDERHex(), URL: ags[0].url},
		{Seat: 1, IdentityKey: pubs[1].ToDERHex(), URL: ags[1].url},
	}
	lock, err := cosign.PotScript(pubs)
	if err != nil {
		t.Fatal(err)
	}
	const potSats = 40000
	lp := &livePot{
		pot: cosign.FundedPot{
			Txid:     "cc00000000000000000000000000000000000000000000000000000000000033",
			Vout:     0,
			Script:   lock,
			Satoshis: potSats,
		},
		seats:       pubs,
		balances:    map[int]uint64{0: 20000, 1: 20000},
		lockHeight:  500000,
		floorHeight: 400000,
	}
	pm.mu.Lock()
	pm.pots["session-1"] = lp
	pm.mu.Unlock()

	// Sign the opening refund at the starting balances.
	if err := pm.resignRefund(context.Background(), "session-1", lp, seats, 500000); err != nil {
		t.Fatalf("signing the opening refund: %v", err)
	}
	first := lp.refund

	// Seat 0 wins 1500. The refund must be re-signed for the new balances.
	if err := pm.ApplyHand(context.Background(), "session-1", seats, map[int]uint64{0: 21500, 1: 18500}); err != nil {
		t.Fatalf("applying the hand: %v", err)
	}
	if lp.refund == first {
		t.Fatal("the refund was not replaced after the hand")
	}
	if lp.lockHeight != 500000-ladderStep {
		t.Fatalf("locktime is %d, want one ladder step earlier than 500000", lp.lockHeight)
	}
	// The newer refund must mature FIRST, or a stale one could outrun it.
	if lp.refund.LockTime >= first.LockTime {
		t.Fatalf("the new refund matures at %d, not earlier than the old %d",
			lp.refund.LockTime, first.LockTime)
	}

	// The losing seat is paid its new, lower balance -- not the 20000 it started with.
	var paid18500 bool
	for _, o := range lp.refund.Outputs {
		if o.Satoshis == 18500 {
			paid18500 = true
		}
		if o.Satoshis == 20000 {
			t.Fatal("a seat is still paid its opening balance, so losses were erased")
		}
	}
	if !paid18500 {
		t.Fatal("the losing seat is not paid its new balance")
	}

	// Balances that do not total the pot must be refused: the difference is unexplained value.
	if err := pm.ApplyHand(context.Background(), "session-1", seats, map[int]uint64{0: 21500, 1: 1000}); err == nil {
		t.Fatal("accepted balances that do not total the pot")
	}
}
