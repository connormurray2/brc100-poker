package cosign

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

func scriptFor(t *testing.T, tag byte) *script.Script {
	t.Helper()
	s := &script.Script{}
	if err := s.AppendPushData([]byte{tag, tag, tag}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOpcodes(script.OpDROP, script.OpTRUE); err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	potTxid = "1100000000000000000000000000000000000000000000000000000000000000"
	potVout = 0
	potSats = 5000
)

// settlement builds a proposal paying `winner` and optionally an extra output.
func settlement(t *testing.T, winner *script.Script, amount uint64, extra *script.Script, extraAmount uint64) Proposal {
	t.Helper()
	tx := transaction.NewTransaction()
	h, err := chainhash.NewHashFromHex(potTxid)
	if err != nil {
		t.Fatal(err)
	}
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       h,
		SourceTxOutIndex: potVout,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: amount, LockingScript: winner})
	if extra != nil {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: extraAmount, LockingScript: extra})
	}
	return Proposal{HandID: "hand-1", Tx: tx, PotInput: 0}
}

func expectation(winner *script.Script, amount uint64) Expectation {
	return Expectation{
		PotTxid:     potTxid,
		PotVout:     potVout,
		PotSatoshis: potSats,
		Payouts:     map[string]uint64{winner.String(): amount},
		MaxFee:      300,
	}
}

func TestVerifyProposalAcceptsAnHonestSettlement(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, 4800, nil, 0)
	if err := VerifyProposal(p, expectation(winner, 4800)); err != nil {
		t.Fatalf("an honest settlement was refused: %v", err)
	}
}

// The check that matters most: a settlement paying the wrong player must be refused.
func TestVerifyProposalRefusesTheWrongWinner(t *testing.T) {
	honest := scriptFor(t, 0xaa)
	thief := scriptFor(t, 0xbb)

	p := settlement(t, thief, 4800, nil, 0)
	err := VerifyProposal(p, expectation(honest, 4800))
	if err == nil {
		t.Fatal("a settlement paying the wrong recipient was accepted")
	}
	if !strings.Contains(err.Error(), "does not pay the expected recipient") {
		t.Errorf("unclear refusal: %v", err)
	}
}

func TestVerifyProposalRefusesAnAlteredAmount(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, 100, nil, 0)
	err := VerifyProposal(p, expectation(winner, 4800))
	if err == nil {
		t.Fatal("a settlement with the wrong payout amount was accepted")
	}
	if !strings.Contains(err.Error(), "expected 4800") {
		t.Errorf("refusal does not name the expected amount: %v", err)
	}
}

// An extra output is how a malicious table would skim the pot.
func TestVerifyProposalRefusesAnUnexpectedOutput(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	skim := scriptFor(t, 0xcc)
	p := settlement(t, winner, 4000, skim, 800)

	err := VerifyProposal(p, expectation(winner, 4000))
	if err == nil {
		t.Fatal("a settlement with an unaccounted output was accepted")
	}
	if !strings.Contains(err.Error(), "unexpected output") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// A declared second output (e.g. a legitimate split) is fine.
func TestVerifyProposalAcceptsDeclaredSplit(t *testing.T) {
	a := scriptFor(t, 0xaa)
	b := scriptFor(t, 0xbb)
	p := settlement(t, a, 2400, b, 2400)

	want := Expectation{
		PotTxid: potTxid, PotVout: potVout, PotSatoshis: potSats,
		Payouts: map[string]uint64{a.String(): 2400, b.String(): 2400},
		MaxFee:  300,
	}
	if err := VerifyProposal(p, want); err != nil {
		t.Fatalf("a declared split was refused: %v", err)
	}
}

func TestVerifyProposalRefusesTheWrongPot(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, 4800, nil, 0)

	want := expectation(winner, 4800)
	want.PotTxid = "2200000000000000000000000000000000000000000000000000000000000000"
	err := VerifyProposal(p, want)
	if err == nil {
		t.Fatal("a settlement spending a different pot was accepted")
	}
	if !strings.Contains(err.Error(), "this seat funded pot") {
		t.Errorf("unclear refusal: %v", err)
	}
}

func TestVerifyProposalRefusesTheWrongVout(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, 4800, nil, 0)
	want := expectation(winner, 4800)
	want.PotVout = 7
	if err := VerifyProposal(p, want); err == nil {
		t.Fatal("a settlement spending a different output of the pot tx was accepted")
	}
}

// Burning the pot as fee must be refused even though every payout is correct.
func TestVerifyProposalRefusesExcessiveFee(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, 100, nil, 0)

	want := expectation(winner, 100)
	err := VerifyProposal(p, want)
	if err == nil {
		t.Fatal("a settlement burning most of the pot as fee was accepted")
	}
	if !strings.Contains(err.Error(), "fees") {
		t.Errorf("unclear refusal: %v", err)
	}
}

func TestVerifyProposalRefusesOverspend(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, potSats+1, nil, 0)
	if err := VerifyProposal(p, expectation(winner, potSats+1)); err == nil {
		t.Fatal("a settlement paying out more than the pot holds was accepted")
	}
}

func TestVerifyProposalRefusesDoubleSpendOfThePot(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	p := settlement(t, winner, 4800, nil, 0)
	h, err := chainhash.NewHashFromHex(potTxid)
	if err != nil {
		t.Fatal(err)
	}
	p.Tx.AddInput(&transaction.TransactionInput{SourceTXID: h, SourceTxOutIndex: potVout})

	err = VerifyProposal(p, expectation(winner, 4800))
	if err == nil {
		t.Fatal("a settlement spending the pot twice was accepted")
	}
	if !strings.Contains(err.Error(), "both spend the pot") {
		t.Errorf("unclear refusal: %v", err)
	}
}

func TestVerifyProposalValidation(t *testing.T) {
	winner := scriptFor(t, 0xaa)
	good := expectation(winner, 4800)

	if err := VerifyProposal(Proposal{}, good); err == nil {
		t.Error("accepted a proposal with no transaction")
	}
	p := settlement(t, winner, 4800, nil, 0)
	if err := VerifyProposal(p, Expectation{}); err == nil {
		t.Error("accepted an expectation naming no pot")
	}
	// An expectation with no payouts is legitimate for a seat that won nothing: it cannot
	// derive another seat's payout script, because that needs the sender's private key. Such a
	// seat is still protected by the pot outpoint and the fee bound, so it may sign.
	noPayouts := good
	noPayouts.Payouts = nil
	if err := VerifyProposal(p, noPayouts); err != nil {
		t.Errorf("a losing seat with a fee bound could not sign: %v", err)
	}
	// With neither payouts nor a fee bound, nothing constrains the settlement at all.
	unbounded := noPayouts
	unbounded.MaxFee = 0
	if err := VerifyProposal(p, unbounded); err == nil {
		t.Error("accepted an expectation with neither payouts nor a fee bound")
	}
	bad := settlement(t, winner, 4800, nil, 0)
	bad.PotInput = 9
	if err := VerifyProposal(bad, good); err == nil {
		t.Error("accepted an out-of-range pot input index")
	}
}

// --- refunds ---------------------------------------------------------------

func refund(t *testing.T, pays *script.Script, amount uint64, lockTime uint32, sequence uint32) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	h, err := chainhash.NewHashFromHex(potTxid)
	if err != nil {
		t.Fatal(err)
	}
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       h,
		SourceTxOutIndex: potVout,
		SequenceNumber:   sequence,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: amount, LockingScript: pays})
	tx.LockTime = lockTime
	return tx
}

const nonFinalSequence = transaction.DefaultSequenceNumber - 1

func TestVerifyRefundAcceptsAnHonestRefund(t *testing.T) {
	own := scriptFor(t, 0xaa)
	tx := refund(t, own, 4900, 30000, nonFinalSequence)
	if err := VerifyRefund(tx, potTxid, potVout, 5000, own, 300); err != nil {
		t.Fatalf("an honest refund was refused: %v", err)
	}
}

// A refund with no locktime is spendable immediately, which defeats its purpose.
func TestVerifyRefundRequiresALocktime(t *testing.T) {
	own := scriptFor(t, 0xaa)
	tx := refund(t, own, 4900, 0, nonFinalSequence)
	err := VerifyRefund(tx, potTxid, potVout, 5000, own, 300)
	if err == nil {
		t.Fatal("a refund with no locktime was accepted")
	}
	if !strings.Contains(err.Error(), "spendable immediately") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// A final sequence number means the locktime does not bind at all.
func TestVerifyRefundRequiresANonFinalInput(t *testing.T) {
	own := scriptFor(t, 0xaa)
	tx := refund(t, own, 4900, 30000, transaction.DefaultSequenceNumber)
	err := VerifyRefund(tx, potTxid, potVout, 5000, own, 300)
	if err == nil {
		t.Fatal("a refund whose locktime cannot bind was accepted")
	}
	if !strings.Contains(err.Error(), "does not bind") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// The refund must pay THIS seat, not a counterparty.
func TestVerifyRefundRefusesPayingSomeoneElse(t *testing.T) {
	own := scriptFor(t, 0xaa)
	other := scriptFor(t, 0xbb)
	tx := refund(t, other, 4900, 30000, nonFinalSequence)
	err := VerifyRefund(tx, potTxid, potVout, 5000, own, 300)
	if err == nil {
		t.Fatal("a refund paying a counterparty was accepted")
	}
	if !strings.Contains(err.Error(), "does not control") {
		t.Errorf("unclear refusal: %v", err)
	}
}

func TestVerifyRefundRefusesOverpayment(t *testing.T) {
	own := scriptFor(t, 0xaa)
	tx := refund(t, own, 9000, 30000, nonFinalSequence)
	if err := VerifyRefund(tx, potTxid, potVout, 5000, own, 300); err == nil {
		t.Fatal("a refund returning more than the stake was accepted")
	}
}

func TestVerifyRefundRefusesExcessiveFee(t *testing.T) {
	own := scriptFor(t, 0xaa)
	tx := refund(t, own, 100, 30000, nonFinalSequence)
	if err := VerifyRefund(tx, potTxid, potVout, 5000, own, 300); err == nil {
		t.Fatal("a refund consuming most of the stake as fee was accepted")
	}
}

func TestVerifyRefundRefusesTheWrongPot(t *testing.T) {
	own := scriptFor(t, 0xaa)
	tx := refund(t, own, 4900, 30000, nonFinalSequence)
	other := "3300000000000000000000000000000000000000000000000000000000000000"
	if err := VerifyRefund(tx, other, potVout, 5000, own, 300); err == nil {
		t.Fatal("a refund not spending the pot was accepted")
	}
}

func TestVerifyRefundValidation(t *testing.T) {
	own := scriptFor(t, 0xaa)
	if err := VerifyRefund(nil, potTxid, potVout, 5000, own, 300); err == nil {
		t.Error("accepted a nil refund")
	}
	tx := refund(t, own, 4900, 30000, nonFinalSequence)
	if err := VerifyRefund(tx, potTxid, potVout, 5000, nil, 300); err == nil {
		t.Error("accepted a nil own-script")
	}
}

// A funder remainder is tolerated only inside the fee allowance, and the value bound still holds.
//
// This is the safety net for the change output that stalled real hands. It must not become a way
// to skim: an output larger than the fee bound is still refused.
func TestFunderRemainderIsBoundedByTheFeeAllowance(t *testing.T) {
	winner := scriptFor(t, 0x51)
	stray := scriptFor(t, 0x52)

	want := expectation(winner, 3800)
	want.MaxFee = 1400

	// A remainder inside the allowance is the funding wallet reclaiming what the fee left.
	if err := VerifyProposal(settlement(t, winner, 3800, stray, 1000), want); err != nil {
		t.Errorf("a remainder inside the fee allowance was refused: %v", err)
	}
	// Beyond it, the total fee would exceed the bound, so it must be refused.
	if err := VerifyProposal(settlement(t, winner, 3800, stray, 1399), want); err == nil {
		t.Error("a remainder was accepted that pushed the fee past the bound")
	}
}
