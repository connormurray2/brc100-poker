package agent

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// recordStakeParams is what a player's own client sends to arm its wallet for a hand.
//
// Deliberately does NOT carry the payout locking scripts. The wallet derives those itself, from
// the sender's public key and its own private key, because a caller able to name the scripts could
// name itself as the payee and the wallet would sign that away.
type recordStakeParams struct {
	HandID string `json:"handId"`
	// The pot this stake went into.
	PotTxid      string `json:"potTxid"`
	PotVout      uint32 `json:"potVout"`
	PotSatoshis  uint64 `json:"potSatoshis"`
	PotScriptHex string `json:"potScriptHex"`
	Seat         int    `json:"seat"`
	// SenderIdentityKey is the party that derived the payouts, whose public key is half of
	// the mirror derivation.
	SenderIdentityKey string `json:"senderIdentityKey"`
	// Payouts is what each seat must receive: amounts and derivation material only.
	Payouts []recordStakePayout `json:"payouts"`
	// MaxFee bounds what a settlement may burn. Without it a proposal could pay a token
	// amount and consume the rest as fee.
	MaxFee uint64 `json:"maxFee"`
	// RefundTxHex is this seat's signed refund, retained so the player can broadcast it
	// unilaterally if the hand stalls.
	RefundTxHex string `json:"refundTxHex"`
}

type recordStakePayout struct {
	// RecipientKey is the payee's identity key. When it is this wallet's own key the script
	// is derived with LockForSelf; otherwise with the sender's public key.
	RecipientKey string `json:"recipientKey"`
	Satoshis     uint64 `json:"satoshis"`
	Prefix       string `json:"prefix"`
	Suffix       string `json:"suffix"`
}

type recordStakeResult struct {
	Recorded bool `json:"recorded"`
	// Payouts echoes the scripts the wallet derived, so a client can confirm they match what
	// it expects without the wallet ever having accepted a script from outside.
	Payouts map[string]uint64 `json:"payouts"`
}

// handleRecordStake records what this seat expects of a settlement.
//
// Only the wallet's owner may call this. A stake the table could write would encode the table's
// expectation of the payouts rather than the player's, which would turn the second signing gate
// into a rubber stamp.
func (a *Agent) handleRecordStake(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	var p recordStakeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "params are not valid JSON"}
	}
	if p.HandID == "" || p.PotTxid == "" || p.PotScriptHex == "" {
		return nil, &substrate.Error{
			Code:    substrate.CodeBadRequest,
			Message: "a stake needs a hand id, a pot outpoint and the pot's locking script",
		}
	}
	if len(p.Payouts) == 0 {
		return nil, &substrate.Error{
			Code:    substrate.CodeBadRequest,
			Message: "a stake with no expected payouts would accept a settlement paying nobody",
		}
	}
	if p.RefundTxHex == "" {
		return nil, &substrate.Error{
			Code:    substrate.CodeBadRequest,
			Message: "refusing to record a stake with no refund held; a stall could trap it",
		}
	}

	sender, err := ec.PublicKeyFromString(p.SenderIdentityKey)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "the sender identity key is unusable"}
	}

	own := a.priv.PubKey().ToDERHex()
	payouts := make(map[string]uint64, len(p.Payouts))
	for i, po := range p.Payouts {
		lock, err := a.derivePayoutScript(sender, po, own)
		if err != nil {
			return nil, &substrate.Error{
				Code:    substrate.CodeBadRequest,
				Message: fmt.Sprintf("payout %d: %s", i, err.Error()),
			}
		}
		payouts[hex.EncodeToString(*lock)] = po.Satoshis
	}

	if err := a.RecordStake(Stake{
		HandID:      p.HandID,
		PotTxid:     p.PotTxid,
		PotVout:     p.PotVout,
		PotSatoshis: p.PotSatoshis,
		Seat:        p.Seat,
		Expectation: cosign.Expectation{
			PotTxid:      p.PotTxid,
			PotVout:      p.PotVout,
			PotSatoshis:  p.PotSatoshis,
			Payouts:      payouts,
			MaxFee:       p.MaxFee,
			PotScriptHex: p.PotScriptHex,
		},
		RefundHeld:  true,
		RefundTxHex: p.RefundTxHex,
	}); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: err.Error()}
	}

	a.logger.Info("recorded a stake", "hand", p.HandID, "seat", p.Seat,
		"pot", fmt.Sprintf("%s:%d", p.PotTxid, p.PotVout), "satoshis", p.PotSatoshis)
	return recordStakeResult{Recorded: true, Payouts: payouts}, nil
}

// derivePayoutScript computes the locking script this wallet expects for one payout.
//
// Two directions, and using the wrong one yields a script the payee cannot recognise. For a payout
// to this wallet, LockForSelf with the sender's public key and this wallet's private key. For a
// payout to anyone else, LockForCounterparty is unavailable — it needs the sender's private key —
// so the script cannot be derived and the amount is recorded against the recipient's own claim.
// That is safe: what protects this seat is that its OWN payout is correct and the fee is bounded.
func (a *Agent) derivePayoutScript(sender *ec.PublicKey, po recordStakePayout, ownKey string) (*script.Script, error) {
	prefix, err := base64.StdEncoding.DecodeString(po.Prefix)
	if err != nil || len(prefix) == 0 {
		return nil, errors.New("the derivation prefix is not valid base64")
	}
	suffix, err := base64.StdEncoding.DecodeString(po.Suffix)
	if err != nil || len(suffix) == 0 {
		return nil, errors.New("the derivation suffix is not valid base64")
	}
	if po.Satoshis == 0 {
		return nil, errors.New("a payout of nothing is not a payout")
	}

	keyID := brc29.KeyID{DerivationPrefix: po.Prefix, DerivationSuffix: po.Suffix}
	if err := keyID.Validate(); err != nil {
		return nil, fmt.Errorf("invalid derivation material: %w", err)
	}

	if po.RecipientKey == ownKey {
		// The mirror image of the sender's LockForCounterparty. This is the script that
		// matters most to this seat, and it is derived rather than accepted.
		lock, err := brc29.LockForSelf(sender, keyID, a.priv)
		if err != nil {
			return nil, fmt.Errorf("deriving this seat's own payout: %w", err)
		}
		return lock, nil
	}

	recipient, err := ec.PublicKeyFromString(po.RecipientKey)
	if err != nil {
		return nil, errors.New("the recipient key is unusable")
	}
	// Another seat's payout. Without the sender's private key the exact script cannot be
	// recomputed here, so the wallet records the amount against a script derived from the
	// recipient's identity key. A settlement whose other payouts differ will fail this seat's
	// check, which is the conservative direction: this seat refuses rather than signs.
	lock, err := brc29.LockForCounterparty(a.priv, keyID, recipient)
	if err != nil {
		return nil, fmt.Errorf("deriving seat payout: %w", err)
	}
	return lock, nil
}

// RecordStakeJSON records a stake from a JSON body, for the player's own client.
//
// Shares the handler's validation and derivation exactly, so the local path cannot drift from the
// substrate path and become the weaker of the two.
func (a *Agent) RecordStakeJSON(body []byte) (any, error) {
	res, err := a.handleRecordStake(nil, body)
	if err != nil {
		var se *substrate.Error
		if errors.As(err, &se) {
			return nil, errors.New(se.Message)
		}
		return nil, err
	}
	return res, nil
}
