package cosign

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// Expectation is what a seat independently believes a settlement should say.
//
// Each seat builds this from its own record of the hand — not from anything the table sends
// — and refuses to sign a proposal that disagrees. That refusal is the property that makes a
// coordinating table service safe to have: it can stall a hand, but it can never move money,
// because a settlement it invented would not match any honest seat's expectation.
type Expectation struct {
	// PotTxid and PotVout identify the pot the settlement must spend.
	PotTxid string
	PotVout uint32
	// PotSatoshis is the pot's value.
	PotSatoshis uint64

	// Payouts is what each recipient must receive, keyed by hex locking script. Keyed by
	// script rather than by seat because that is what the transaction actually commits
	// to; the caller maps seats to scripts using the same derivation the payee will.
	Payouts map[string]uint64

	// MaxFee bounds what the settlement may consume in fees. Without it a proposal could
	// pay the winner a token amount and burn the rest.
	MaxFee uint64
}

// Proposal is a settlement a seat has been asked to sign.
type Proposal struct {
	// HandID ties the proposal to a hand, so a seat cannot be asked to sign a
	// settlement for a hand it is not in.
	HandID string
	Tx     *transaction.Transaction
	// PotInput is the index of the input spending the pot.
	PotInput int
}

// VerifyProposal checks a proposal against a seat's own expectation.
//
// Every check here answers "would signing this cost me money I did not agree to lose?". The
// errors are deliberately specific: a refusal has to be explicable to the other seats, since
// a seat that refuses without reason is indistinguishable from one that is stalling.
func VerifyProposal(p Proposal, want Expectation) error {
	if p.Tx == nil {
		return errors.New("cosign: the proposal carries no transaction")
	}
	if want.PotTxid == "" {
		return errors.New("cosign: the expectation names no pot")
	}
	if len(want.Payouts) == 0 {
		return errors.New("cosign: the expectation names no payouts; refusing to sign a settlement that pays nobody")
	}

	// The proposal must spend the pot this seat funded, and nothing else.
	if p.PotInput < 0 || p.PotInput >= len(p.Tx.Inputs) {
		return fmt.Errorf("cosign: pot input index %d is outside the transaction's %d inputs", p.PotInput, len(p.Tx.Inputs))
	}
	in := p.Tx.Inputs[p.PotInput]
	if in.SourceTXID == nil {
		return fmt.Errorf("cosign: input %d names no source transaction", p.PotInput)
	}
	if in.SourceTXID.String() != want.PotTxid || in.SourceTxOutIndex != want.PotVout {
		return fmt.Errorf("cosign: input %d spends %s:%d, but this seat funded pot %s:%d",
			p.PotInput, in.SourceTXID.String(), in.SourceTxOutIndex, want.PotTxid, want.PotVout)
	}

	// No other input may spend the same pot twice.
	for i, other := range p.Tx.Inputs {
		if i == p.PotInput || other.SourceTXID == nil {
			continue
		}
		if other.SourceTXID.String() == want.PotTxid && other.SourceTxOutIndex == want.PotVout {
			return fmt.Errorf("cosign: inputs %d and %d both spend the pot", p.PotInput, i)
		}
	}

	// Every expected payout must be present for exactly the expected amount, and no
	// output may exist that the expectation does not account for.
	got := make(map[string]uint64, len(p.Tx.Outputs))
	var totalOut uint64
	for i, o := range p.Tx.Outputs {
		if o.LockingScript == nil {
			return fmt.Errorf("cosign: output %d has no locking script", i)
		}
		key := strings.ToLower(o.LockingScript.String())
		got[key] += o.Satoshis
		totalOut += o.Satoshis
	}

	for wantScript, wantSats := range want.Payouts {
		key := strings.ToLower(wantScript)
		gotSats, present := got[key]
		if !present {
			return fmt.Errorf("cosign: the settlement does not pay the expected recipient %s…", truncateScript(key))
		}
		if gotSats != wantSats {
			return fmt.Errorf("cosign: recipient %s… receives %d sat, expected %d",
				truncateScript(key), gotSats, wantSats)
		}
		delete(got, key)
	}
	// Anything left over is an output the hand's outcome does not account for. A change
	// output back to the funder is legitimate but must be declared in the expectation,
	// precisely so it cannot be used to skim the pot.
	for key, sats := range got {
		return fmt.Errorf("cosign: the settlement contains an unexpected output of %d sat to %s…", sats, truncateScript(key))
	}

	// Value must be conserved within the declared fee bound.
	if totalOut > want.PotSatoshis {
		return fmt.Errorf("cosign: outputs total %d sat but the pot holds %d", totalOut, want.PotSatoshis)
	}
	fee := want.PotSatoshis - totalOut
	if want.MaxFee > 0 && fee > want.MaxFee {
		return fmt.Errorf("cosign: the settlement consumes %d sat in fees, above the %d limit", fee, want.MaxFee)
	}

	return nil
}

// truncateScript shortens a script for an error message.
func truncateScript(s string) string {
	const n = 16
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// VerifyRefund checks a refund returns a seat's own stake to itself.
//
// A refund is only a safety net if a seat can confirm it actually returns its money. Signing
// an unchecked refund would let a counterparty pre-position a transaction that pays them
// after the locktime.
func VerifyRefund(tx *transaction.Transaction, potTxid string, potVout uint32, stake uint64, ownScript *script.Script, maxFee uint64) error {
	if tx == nil {
		return errors.New("cosign: the refund carries no transaction")
	}
	if ownScript == nil {
		return errors.New("cosign: no script to check the refund pays to")
	}
	// A locktime only binds if at least one input is non-final. Without this the refund
	// could be mined immediately, which defeats the purpose of the timelock.
	if tx.LockTime == 0 {
		return errors.New("cosign: the refund has no locktime, so it is spendable immediately")
	}
	final := true
	for _, in := range tx.Inputs {
		if in.SequenceNumber != transaction.DefaultSequenceNumber {
			final = false
			break
		}
	}
	if final {
		return errors.New("cosign: every refund input is final, so the locktime does not bind")
	}

	idx, err := FindPotInput(tx, potTxid, potVout)
	if err != nil {
		return fmt.Errorf("cosign: the refund does not spend the pot: %w", err)
	}
	_ = idx

	want := strings.ToLower(ownScript.String())
	var paid uint64
	for i, o := range tx.Outputs {
		if o.LockingScript == nil {
			return fmt.Errorf("cosign: refund output %d has no locking script", i)
		}
		if strings.ToLower(o.LockingScript.String()) == want {
			paid += o.Satoshis
			continue
		}
		return fmt.Errorf("cosign: the refund pays %d sat to a script this seat does not control", o.Satoshis)
	}
	if paid == 0 {
		return errors.New("cosign: the refund pays nothing to this seat")
	}
	if paid > stake {
		return fmt.Errorf("cosign: the refund returns %d sat but this seat staked only %d", paid, stake)
	}
	if fee := stake - paid; maxFee > 0 && fee > maxFee {
		return fmt.Errorf("cosign: the refund consumes %d sat in fees, above the %d limit", fee, maxFee)
	}
	return nil
}
