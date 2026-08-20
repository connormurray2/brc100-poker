package agent

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// signRefundParams asks this seat to sign a refund returning a pot to itself.
// refundBalance is one seat's share of a shared refund.
type refundBalance struct {
	RecipientKey string `json:"recipientKey"`
	Satoshis     uint64 `json:"satoshis"`
}

type signRefundParams struct {
	HandID string `json:"handId"`
	// RawTxHex is the unsigned refund.
	RawTxHex string `json:"rawTxHex"`
	// PotInput is the index of the input spending the pot.
	PotInput int `json:"potInput"`
	// PotTxid, PotVout, PotSatoshis and PotScriptHex describe the pot being refunded. The
	// sighash commits to the input's value and script, so they must be attached before
	// signing or the signature would cover a different message.
	PotTxid      string `json:"potTxid"`
	PotVout      uint32 `json:"potVout"`
	PotSatoshis  uint64 `json:"potSatoshis"`
	PotScriptHex string `json:"potScriptHex"`
	// Seat is this seat's index, which fixes its position in the pot script.
	Seat int `json:"seat"`
	// Balances is what every seat must be paid, as identity keys and amounts. A seat verifies
	// the whole set, not only its own share: a refund that redistributes value between two
	// other seats is still a refund this seat should refuse to authorise.
	Balances []refundBalance `json:"balances"`
	// MaxLockHeight is the locktime of the refund this one replaces. The new refund must mature
	// strictly earlier, so the newest state is spendable first and a stale refund loses the
	// race. Without this a losing seat could fall back on an older, more favourable refund.
	MaxLockHeight uint32 `json:"maxLockHeight"`
	// Beneficiary and Seats are the older single-recipient form, retained so a table that has
	// not been upgraded still gets a clear refusal rather than a confusing one.
	Beneficiary string   `json:"beneficiary,omitempty"`
	Seats       []string `json:"seats,omitempty"`
	// MaxFee bounds what the refund may consume.
	MaxFee uint64 `json:"maxFee"`
}

// handleSignRefund signs a refund that returns a pot to this seat.
//
// Deliberately does NOT require a recorded stake, unlike signPot, and that is not a relaxation.
// A stake cannot be recorded until its refund exists -- recording one without a refund would leave
// a stake committed with no way to recover it -- so requiring a stake here would deadlock: no
// refund without a stake, no stake without a refund.
//
// The safety is self-contained instead. This wallet verifies that the transaction spends the named
// pot and pays it back to a script only this wallet can spend, with a locktime that actually binds.
// A transaction satisfying that can only ever return this seat's own money, so signing it costs
// nothing even if the caller is hostile. Anything else is refused.
func (a *Agent) handleSignRefund(caller *ec.PublicKey, params json.RawMessage) (any, error) {
	var p signRefundParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "signRefund params are not valid JSON"}
	}
	if p.HandID == "" || p.RawTxHex == "" || p.PotTxid == "" || p.PotScriptHex == "" {
		return nil, &substrate.Error{
			Code:    substrate.CodeBadRequest,
			Message: "signRefund needs a hand id, a transaction, and the pot it spends",
		}
	}
	if p.MaxFee == 0 {
		return nil, &substrate.Error{
			Code:    substrate.CodeBadRequest,
			Message: "signRefund needs a fee bound; without one a refund could burn the pot",
		}
	}

	tx, err := transaction.NewTransactionFromHex(p.RawTxHex)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "the refund is not parseable"}
	}
	if p.PotInput < 0 || p.PotInput >= len(tx.Inputs) {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "the pot input index is out of range"}
	}

	if len(p.Balances) == 0 {
		return nil, &substrate.Error{
			Code: substrate.CodeForbidden,
			Message: "this refund names no balances. A refund must pay every seat its own balance; " +
				"the older whole-pot form rewarded refusing to settle and is no longer signed",
		}
	}

	// The scripts every seat must be paid, derived here from identity keys rather than accepted
	// from the caller. A caller able to name a script could name its own and take the refund.
	want, err := a.sharedRefundExpectation(p)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: err.Error()}
	}

	// The gate: spends the named pot, pays every seat exactly its balance, matures earlier than
	// the refund it replaces, and stays inside the fee bound. A transaction passing this cannot
	// move value between seats and cannot be outrun by a staler, more favourable refund.
	if err := cosign.VerifySharedRefund(tx, want); err != nil {
		who := "an unknown caller"
		if caller != nil {
			who = short(caller.ToDERHex())
		}
		a.logger.Warn("refusing to sign a refund that does not return the pot to this seat",
			"handId", p.HandID, "caller", who, "reason", err.Error())
		return nil, &substrate.Error{Code: substrate.CodeDeclined, Message: err.Error()}
	}

	potScript, err := cosign.PotScriptFromHex(p.PotScriptHex)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: err.Error()}
	}
	tx.Inputs[p.PotInput].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      p.PotSatoshis,
		LockingScript: potScript,
	})

	sig, err := cosign.SignInput(tx, p.PotInput, p.Seat, a.priv)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	a.logger.Info("signed a refund returning the pot to this seat",
		"handId", p.HandID, "seat", p.Seat, "pot", fmt.Sprintf("%s:%d", p.PotTxid, p.PotVout))
	return signPotResult{Seat: sig.Seat, DER: hex.EncodeToString(sig.DER)}, nil
}

// sharedRefundExpectation derives what this wallet requires of a shared refund.
//
// Every script is computed from an identity key here rather than taken from the caller, so a table
// cannot make this wallet expect a script it controls. The balances themselves come from the table,
// which is safe because a seat cross-checks them against its own record of the session before
// signing -- and because a refund that does not total the pot is refused outright.
func (a *Agent) sharedRefundExpectation(p signRefundParams) (cosign.SharedRefundExpectation, error) {
	if a.priv == nil {
		return cosign.SharedRefundExpectation{}, fmt.Errorf("this wallet holds no key")
	}
	own := a.priv.PubKey().ToDERHex()

	balances := make(map[string]uint64, len(p.Balances))
	includesSelf := false
	for i, b := range p.Balances {
		if b.Satoshis == 0 {
			return cosign.SharedRefundExpectation{}, fmt.Errorf("balance %d is zero; omit it instead", i)
		}
		pub, err := ec.PublicKeyFromString(b.RecipientKey)
		if err != nil {
			return cosign.SharedRefundExpectation{}, fmt.Errorf("balance %d has an unusable key", i)
		}
		addr, err := script.NewAddressFromPublicKey(pub, false)
		if err != nil {
			return cosign.SharedRefundExpectation{}, fmt.Errorf("balance %d: deriving an address: %w", i, err)
		}
		lock, err := p2pkh.Lock(addr)
		if err != nil {
			return cosign.SharedRefundExpectation{}, fmt.Errorf("balance %d: deriving a script: %w", i, err)
		}
		balances[cosign.ScriptForKeyHex(lock)] = b.Satoshis
		if b.RecipientKey == own {
			includesSelf = true
		}
	}
	if !includesSelf {
		// A refund that pays this seat nothing is one it has no reason to authorise, and
		// signing it would help spend a pot this seat funded without being repaid.
		return cosign.SharedRefundExpectation{}, fmt.Errorf("this refund pays this seat nothing")
	}

	return cosign.SharedRefundExpectation{
		PotTxid:       p.PotTxid,
		PotVout:       p.PotVout,
		PotSatoshis:   p.PotSatoshis,
		Balances:      balances,
		MaxFee:        p.MaxFee,
		MaxLockHeight: p.MaxLockHeight,
	}, nil
}
