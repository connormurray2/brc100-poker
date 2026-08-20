package cosign

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// SharedRefundExpectation is what a seat requires of a shared refund before signing it.
//
// Every seat signs every refund, so a seat is not checking that it is the sole beneficiary. It is
// checking that the transaction restores the balances the hands produced -- including balances
// belonging to other seats, which it can compute from its own record of the session.
type SharedRefundExpectation struct {
	PotTxid     string
	PotVout     uint32
	PotSatoshis uint64
	// Balances is what each seat must be paid, keyed by hex locking script. Every seat in the
	// session must appear, so a refund omitting one is refused.
	Balances map[string]uint64
	// MaxFee bounds what the refund may consume.
	MaxFee uint64
	// MaxLockHeight is the highest locktime this refund may carry.
	//
	// This is the ladder: each new refund must mature strictly EARLIER than the one it
	// replaces, so the newest state is spendable first and a stale refund loses the race. A
	// seat that accepted a later locktime would be signing a refund its counterparty could
	// outrun with an older, more favourable one.
	MaxLockHeight uint32
}

// VerifySharedRefund checks a refund pays every seat its balance and matures soon enough.
//
// The two properties together are what remove the profit from refusing to settle: a seat recovers
// exactly what it holds, and it cannot fall back on an older refund that paid it more.
func VerifySharedRefund(tx *transaction.Transaction, want SharedRefundExpectation) error {
	if tx == nil {
		return errors.New("cosign: the refund carries no transaction")
	}
	if want.PotTxid == "" {
		return errors.New("cosign: the expectation names no pot")
	}
	if len(want.Balances) == 0 {
		return errors.New("cosign: the expectation names no balances; a refund paying nobody is not a refund")
	}
	if want.MaxFee == 0 {
		return errors.New("cosign: a shared refund needs a fee bound")
	}

	// The locktime must bind, or the refund is spendable immediately and the ladder is
	// meaningless.
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
	if want.MaxLockHeight > 0 && tx.LockTime > want.MaxLockHeight {
		return fmt.Errorf(
			"cosign: the refund matures at %d, later than the %d it replaces; a stale refund could outrun it",
			tx.LockTime, want.MaxLockHeight)
	}

	if _, err := FindPotInput(tx, want.PotTxid, want.PotVout); err != nil {
		return fmt.Errorf("cosign: the refund does not spend the pot: %w", err)
	}

	// Every output must be a balance this seat expects, and every expected balance must be
	// present. Both directions matter: a missing one shorts a seat, an extra one is a skim.
	got := make(map[string]uint64, len(tx.Outputs))
	var totalOut uint64
	for i, o := range tx.Outputs {
		if o.LockingScript == nil {
			return fmt.Errorf("cosign: refund output %d has no locking script", i)
		}
		got[strings.ToLower(o.LockingScript.String())] += o.Satoshis
		totalOut += o.Satoshis
	}

	// The fee is deducted from the largest balance, so exactly one seat may be short by up to
	// the fee. Any other shortfall is a redistribution and must be refused.
	shortfalls := 0
	for wantScript, wantSats := range want.Balances {
		key := strings.ToLower(wantScript)
		gotSats, present := got[key]
		if !present {
			return fmt.Errorf("cosign: the refund pays nothing to %s…, which holds %d sat",
				truncateScript(key), wantSats)
		}
		switch {
		case gotSats == wantSats:
		case gotSats < wantSats && wantSats-gotSats <= want.MaxFee:
			shortfalls++
		default:
			return fmt.Errorf("cosign: %s… is paid %d sat but holds %d",
				truncateScript(key), gotSats, wantSats)
		}
		delete(got, key)
	}
	if shortfalls > 1 {
		return errors.New("cosign: more than one seat is short; the fee comes from one balance only")
	}
	for key, sats := range got {
		return fmt.Errorf("cosign: the refund contains an unexpected output of %d sat to %s…",
			sats, truncateScript(key))
	}

	if totalOut > want.PotSatoshis {
		return fmt.Errorf("cosign: the refund pays out %d sat but the pot holds %d", totalOut, want.PotSatoshis)
	}
	if fee := want.PotSatoshis - totalOut; fee > want.MaxFee {
		return fmt.Errorf("cosign: the refund consumes %d sat in fees, above the %d limit", fee, want.MaxFee)
	}
	return nil
}

// ScriptForKeyHex is the P2PKH script a balance is paid to, as the verifier keys them.
func ScriptForKeyHex(lock *script.Script) string {
	if lock == nil {
		return ""
	}
	return strings.ToLower(lock.String())
}
