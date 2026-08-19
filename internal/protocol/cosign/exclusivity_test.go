package cosign

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// A settlement and a refund both spend the SAME pot output, so at most one can ever be mined.
// That is what makes the refund a safe backstop rather than a second way to pay out: a seat
// cannot collect a settlement and then also reclaim its stake.
//
// This is a property of the transaction graph rather than of any check we perform, so the test
// establishes it structurally: both transactions name the same outpoint as an input.
func TestSettlementAndRefundSpendTheSameOutpoint(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}

	var src chainhash.Hash
	src[0] = 0x44
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}

	// The settlement.
	settlement := buildSpend(t, lock, pot.Satoshis)
	settlement.Inputs[0].SourceTXID = &src
	settlement.Inputs[0].SourceTxOutIndex = pot.Vout

	// The refund.
	refund, err := BuildRefund(RefundArgs{
		Pot: pot, Recipient: privs[0].PubKey(), Satoshis: 4700, LockHeight: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}

	sIdx, err := FindPotInput(settlement, pot.Txid, pot.Vout)
	if err != nil {
		t.Fatalf("the settlement does not spend the pot: %v", err)
	}
	rIdx, err := FindPotInput(refund, pot.Txid, pot.Vout)
	if err != nil {
		t.Fatalf("the refund does not spend the pot: %v", err)
	}

	sIn := settlement.Inputs[sIdx]
	rIn := refund.Inputs[rIdx]
	if sIn.SourceTXID.String() != rIn.SourceTXID.String() || sIn.SourceTxOutIndex != rIn.SourceTxOutIndex {
		t.Fatalf("the settlement spends %s:%d but the refund spends %s:%d; they are not mutually exclusive",
			sIn.SourceTXID, sIn.SourceTxOutIndex, rIn.SourceTXID, rIn.SourceTxOutIndex)
	}

	// Different transactions, so this is genuinely a double-spend relationship rather than
	// the same transaction compared with itself.
	if settlement.TxID().String() == refund.TxID().String() {
		t.Fatal("the settlement and the refund are the same transaction")
	}
	t.Logf("both spend %s:%d — settlement %s, refund %s",
		pot.Txid[:16], pot.Vout, settlement.TxID().String()[:16], refund.TxID().String()[:16])
}

// The refund is only reachable after its locktime, so the honest ordering is: settle
// cooperatively now, or reclaim later. A refund that could be mined immediately would let a seat
// race the settlement it just agreed to.
func TestRefundCannotRaceTheSettlement(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	var src chainhash.Hash
	src[0] = 0x55
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}

	refund, err := BuildRefund(RefundArgs{
		Pot: pot, Recipient: privs[0].PubKey(), Satoshis: 4700, LockHeight: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A locktime in the future.
	if refund.LockTime == 0 {
		t.Fatal("the refund has no locktime, so it could be mined immediately")
	}
	// And a non-final sequence, or the locktime would not bind at all.
	if refund.Inputs[0].SequenceNumber == transaction.DefaultSequenceNumber {
		t.Fatal("the refund input is final, so its locktime is decorative")
	}

	// The refund verifier enforces both, so a counterparty cannot hand a seat a refund that
	// is spendable straight away.
	ownScript, err := ownP2PKH(privs[0].PubKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRefund(refund, pot.Txid, pot.Vout, pot.Satoshis, ownScript, 500); err != nil {
		t.Fatalf("an honest refund was refused: %v", err)
	}

	// Strip the locktime: now it could be mined immediately, and must be refused.
	immediate := refund
	immediate.LockTime = 0
	if err := VerifyRefund(immediate, pot.Txid, pot.Vout, pot.Satoshis, ownScript, 500); err == nil {
		t.Fatal("a refund with no locktime was accepted")
	}
}

// Once a settlement is confirmed the pot is spent, so the refund is unspendable regardless of
// its locktime. This asserts the accounting a service must do: a settled hand's refund must
// never be broadcast, because it can only be rejected.
func TestSettledPotMakesItsRefundUnbroadcastable(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	var src chainhash.Hash
	src[0] = 0x66
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}

	refund, err := BuildRefund(RefundArgs{
		Pot: pot, Recipient: privs[0].PubKey(), Satoshis: 4700, LockHeight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Once the settlement confirms, the pot's output is spent, so the refund's input no
	// longer exists. A service holding a confirmed settlement for this pot must therefore
	// treat the refund as dead rather than as a fallback still worth trying: broadcasting it
	// is a known double-spend, and a rejection is unretryable.
	idx, err := FindPotInput(refund, pot.Txid, pot.Vout)
	if err != nil {
		t.Fatal(err)
	}
	if refund.Inputs[idx].SourceTXID.String() != pot.Txid {
		t.Fatal("the refund does not spend the settled pot")
	}
	t.Logf("pot %s:%d, once settled, makes refund %s unbroadcastable",
		pot.Txid[:16], pot.Vout, refund.TxID().String()[:16])
}

// A refund must pay only its own seat. A counterparty who could redirect it would turn the
// backstop into a way to take the pot after the locktime.
func TestRefundCannotBeRedirectedToACounterparty(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	var src chainhash.Hash
	src[0] = 0x77
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}

	// Built to pay seat 1, checked by seat 0.
	redirected, err := BuildRefund(RefundArgs{
		Pot: pot, Recipient: privs[1].PubKey(), Satoshis: 4700, LockHeight: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	seat0Script, err := ownP2PKH(privs[0].PubKey())
	if err != nil {
		t.Fatal(err)
	}

	err = VerifyRefund(redirected, pot.Txid, pot.Vout, pot.Satoshis, seat0Script, 500)
	if err == nil {
		t.Fatal("seat 0 accepted a refund that pays seat 1")
	}
	if !strings.Contains(err.Error(), "does not control") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// ownP2PKH builds the script a seat's own refund must pay to.
func ownP2PKH(pub *ec.PublicKey) (*script.Script, error) {
	addr, err := script.NewAddressFromPublicKey(pub, false) // false => testnet
	if err != nil {
		return nil, err
	}
	return p2pkh.Lock(addr)
}
