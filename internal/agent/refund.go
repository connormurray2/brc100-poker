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
	// Beneficiary is the seat this refund returns the pot to, as an identity key. Every seat
	// signs every seat's refund, so this is usually not the signer.
	Beneficiary string `json:"beneficiary"`
	// Seats are the identity keys of every seat at the table, in seat order. The signer
	// requires the beneficiary to be one of them: a refund paying anyone outside the table
	// would move money away from it, which no seat should ever authorise.
	Seats []string `json:"seats"`
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

	// The script this wallet expects to be paid. Derived here, never accepted from the caller:
	// a caller able to name it could name its own script and take the refund.
	ownScript, err := a.refundScript(p)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: err.Error()}
	}

	// This is the whole gate: spends the named pot, pays it back to this wallet, locktime binds,
	// fee bounded. A transaction passing this cannot move money anywhere but back to this seat.
	if err := cosign.VerifyRefund(tx, p.PotTxid, p.PotVout, p.PotSatoshis, ownScript, p.MaxFee); err != nil {
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

// refundScript derives the script a refund must pay to.
//
// The beneficiary is one seat, and every seat signs it, so this is usually not the signer's own
// key. That is safe because the refund spends the pot outpoint, which only one transaction can
// ever claim, and every other seat holds an equivalent refund to itself. What must be refused is a
// beneficiary from outside the table: that would move the pot away from the players entirely.
//
// The script is derived here rather than accepted, so a caller cannot name its own.
func (a *Agent) refundScript(p signRefundParams) (*script.Script, error) {
	if p.Beneficiary == "" {
		return nil, fmt.Errorf("the refund names no beneficiary")
	}
	if len(p.Seats) < 2 {
		return nil, fmt.Errorf("the refund names no table to check the beneficiary against")
	}
	own := a.priv.PubKey().ToDERHex()
	atTable, signerPresent := false, false
	for _, k := range p.Seats {
		if k == p.Beneficiary {
			atTable = true
		}
		if k == own {
			signerPresent = true
		}
	}
	if !signerPresent {
		return nil, fmt.Errorf("this seat is not at the table this refund names")
	}
	if !atTable {
		return nil, fmt.Errorf("the refund pays %s, who holds no seat at this table", short(p.Beneficiary))
	}

	pub, err := ec.PublicKeyFromString(p.Beneficiary)
	if err != nil {
		return nil, fmt.Errorf("the beneficiary key is unusable")
	}
	addr, err := script.NewAddressFromPublicKey(pub, false)
	if err != nil {
		return nil, fmt.Errorf("deriving the beneficiary address: %w", err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, fmt.Errorf("deriving the refund script: %w", err)
	}
	return lock, nil
}
