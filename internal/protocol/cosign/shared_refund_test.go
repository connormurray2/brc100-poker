package cosign

import (
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// A shared refund exists to remove the profit from refusing to settle. These assert that property
// directly, not merely that the transaction builds.

func sharedPot(t *testing.T, sats uint64, seats []*ec.PublicKey) FundedPot {
	t.Helper()
	lock, err := PotScript(seats)
	if err != nil {
		t.Fatal(err)
	}
	return FundedPot{
		Txid:     "1100000000000000000000000000000000000000000000000000000000000000",
		Vout:     0,
		Script:   lock,
		Satoshis: sats,
	}
}

// The point of the whole design: a seat that has lost recovers exactly what it holds, so refusing
// to settle gains it nothing.
func TestRefusingGainsNothingWhenTheRefundPaysBalances(t *testing.T) {
	a, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pot := sharedPot(t, 40000, []*ec.PublicKey{a.PubKey(), b.PubKey()})

	// Seat 1 is down 1000 after several hands.
	const fee = 300
	tx, err := BuildRefund(RefundArgs{
		Pot: pot,
		Recipients: []RefundOutput{
			{Recipient: a.PubKey(), Satoshis: 21000},
			{Recipient: b.PubKey(), Satoshis: 19000},
		},
		LockHeight: 500000,
		Fee:        fee,
	})
	if err != nil {
		t.Fatalf("building the shared refund: %v", err)
	}
	if len(tx.Outputs) != 2 {
		t.Fatalf("the refund has %d outputs, want one per seat", len(tx.Outputs))
	}

	// The losing seat must recover its balance, not its buy-in. Recovering the buy-in would
	// erase its losses and make refusing profitable.
	var paidToLoser uint64
	for _, o := range tx.Outputs {
		if o.Satoshis == 19000 {
			paidToLoser = o.Satoshis
		}
	}
	if paidToLoser != 19000 {
		t.Fatalf("the losing seat is paid %d; it must be its balance of 19000, not its 20000 buy-in", paidToLoser)
	}

	// The fee comes off the largest balance, since a small one may not cover it.
	var total uint64
	for _, o := range tx.Outputs {
		total += o.Satoshis
	}
	if total != pot.Satoshis-fee {
		t.Fatalf("outputs total %d, want the pot less the %d fee", total, fee)
	}
}

// The balances must account for the whole pot. An unexplained difference is indistinguishable
// from a skim.
func TestSharedRefundRequiresBalancesToTotalThePot(t *testing.T) {
	a, _ := ec.NewPrivateKey()
	b, _ := ec.NewPrivateKey()
	pot := sharedPot(t, 40000, []*ec.PublicKey{a.PubKey(), b.PubKey()})

	_, err := BuildRefund(RefundArgs{
		Pot: pot,
		Recipients: []RefundOutput{
			{Recipient: a.PubKey(), Satoshis: 21000},
			{Recipient: b.PubKey(), Satoshis: 15000}, // 4000 unaccounted for
		},
		LockHeight: 500000,
		Fee:        300,
	})
	if err == nil {
		t.Fatal("accepted balances that do not total the pot")
	}
}

// The locktime must bind, and the input must be non-final for it to.
func TestSharedRefundLocktimeBinds(t *testing.T) {
	a, _ := ec.NewPrivateKey()
	b, _ := ec.NewPrivateKey()
	pot := sharedPot(t, 40000, []*ec.PublicKey{a.PubKey(), b.PubKey()})

	if _, err := BuildRefund(RefundArgs{
		Pot:        pot,
		Recipients: []RefundOutput{{Recipient: a.PubKey(), Satoshis: 40000}},
		Fee:        300,
	}); err == nil {
		t.Fatal("accepted a refund with no locktime, which is spendable immediately")
	}

	tx, err := BuildRefund(RefundArgs{
		Pot:        pot,
		Recipients: []RefundOutput{{Recipient: a.PubKey(), Satoshis: 40000}},
		LockHeight: 500000,
		Fee:        300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.LockTime != 500000 {
		t.Fatalf("locktime is %d, want 500000", tx.LockTime)
	}
	if tx.Inputs[0].SequenceNumber == 0xffffffff {
		t.Fatal("the input is final, so the locktime does not bind")
	}
}

// Mixing the single- and multi-recipient forms would silently drop one of them.
func TestSharedRefundRejectsMixedForms(t *testing.T) {
	a, _ := ec.NewPrivateKey()
	b, _ := ec.NewPrivateKey()
	pot := sharedPot(t, 40000, []*ec.PublicKey{a.PubKey(), b.PubKey()})
	if _, err := BuildRefund(RefundArgs{
		Pot:        pot,
		Recipient:  a.PubKey(),
		Satoshis:   1000,
		Recipients: []RefundOutput{{Recipient: a.PubKey(), Satoshis: 40000}},
		LockHeight: 500000,
		Fee:        300,
	}); err == nil {
		t.Fatal("accepted both the single- and multi-recipient forms at once")
	}
}
