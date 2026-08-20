package webui

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// PotManager puts real value behind a hand.
//
// The table funds an n-of-n pot, every seat holds a signed refund before anything is at risk, and
// settlement needs a signature from each seat. The table can therefore stall a hand and can never
// take the money -- which is the whole point of the architecture, and the part that in-memory chip
// accounting does not demonstrate.
type PotManager struct {
	wallet     cosign.PotFunder
	tableKey   *ec.PrivateKey
	originator string
	coord      *Coordinator
	log        *slog.Logger

	mu   sync.Mutex
	pots map[string]*livePot // by hand ID
}

// livePot is one hand's funded pot.
type livePot struct {
	pot   cosign.FundedPot
	seats []*ec.PublicKey
	// refunds are each seat's fully-signed refund, held before the stake is committed.
	refunds map[int]*transaction.Transaction
	// lockHeight is when those refunds mature.
	lockHeight uint32
}

// NewPotManager builds a pot manager.
//
// A nil wallet is legitimate: a table configured without one plays for chips, and says so, rather
// than pretending value is at stake.
func NewPotManager(w cosign.PotFunder, tableKey *ec.PrivateKey, originator string, coord *Coordinator, log *slog.Logger) (*PotManager, error) {
	if tableKey == nil {
		return nil, errors.New("webui: a table key is required to manage pots")
	}
	if originator == "" {
		return nil, errors.New("webui: an originator is required")
	}
	return &PotManager{
		wallet: w, tableKey: tableKey, originator: originator, coord: coord, log: log,
		pots: make(map[string]*livePot),
	}, nil
}

// Enabled reports whether this manager can put value behind a hand.
func (m *PotManager) Enabled() bool { return m != nil && m.wallet != nil }

// OpenPot funds the pot for a hand and builds every seat's refund.
//
// Refunds come before the pot is treated as committed, and that ordering is the safety property:
// a seat whose stake is at risk without a signed refund could have it trapped by any other seat
// simply going away.
func (m *PotManager) OpenPot(ctx context.Context, handID string, seats []AgentEndpoint, satoshis uint64, currentHeight uint32) (*livePot, error) {
	if !m.Enabled() {
		return nil, errors.New("webui: this table has no funding wallet")
	}
	if len(seats) < 2 {
		return nil, errors.New("webui: a pot needs at least two seats")
	}

	pubs := make([]*ec.PublicKey, len(seats))
	for i, s := range seats {
		pub, err := ec.PublicKeyFromString(s.IdentityKey)
		if err != nil {
			return nil, fmt.Errorf("webui: seat %d has an unusable identity key: %w", s.Seat, err)
		}
		pubs[i] = pub
	}

	pot, err := cosign.FundPot(ctx, cosign.FundPotArgs{
		Wallet:      m.wallet,
		Originator:  m.originator,
		Seats:       pubs,
		Satoshis:    satoshis,
		Basket:      "poker-pot",
		Description: "fund the pot for hand " + handID,
	})
	if err != nil {
		return nil, fmt.Errorf("webui: funding the pot: %w", err)
	}

	// A refund matures well after a hand should have finished, so it is a backstop rather than
	// a race against settlement.
	lockHeight := currentHeight + 144
	lp := &livePot{pot: pot, seats: pubs, refunds: make(map[int]*transaction.Transaction), lockHeight: lockHeight}

	// Each seat's refund returns the whole pot to that seat. Only one can ever be broadcast,
	// because they all spend the same outpoint, and the settlement spends it first when the
	// hand completes normally.
	for i, s := range seats {
		refund, err := cosign.BuildRefund(cosign.RefundArgs{
			Pot: pot, Recipient: pubs[i], Satoshis: satoshis - 300, LockHeight: lockHeight,
		})
		if err != nil {
			return nil, fmt.Errorf("webui: building seat %d's refund: %w", s.Seat, err)
		}
		sigs, err := m.collectRefundSignatures(ctx, handID, refund, 0, seats, pot, s.IdentityKey)
		if err != nil {
			return nil, fmt.Errorf("webui: signing seat %d's refund: %w", s.Seat, err)
		}
		unlock, err := cosign.Assemble(sigs, len(seats))
		if err != nil {
			return nil, fmt.Errorf("webui: assembling seat %d's refund: %w", s.Seat, err)
		}
		refund.Inputs[0].UnlockingScript = unlock
		// Verified here rather than trusted: a refund that does not satisfy the pot is worse
		// than none, because a player would believe they were protected.
		if err := cosign.VerifyScript(refund, 0, pot.Script, pot.Satoshis); err != nil {
			return nil, fmt.Errorf("webui: seat %d's refund does not satisfy the pot: %w", s.Seat, err)
		}
		lp.refunds[s.Seat] = refund
	}

	m.mu.Lock()
	m.pots[handID] = lp
	m.mu.Unlock()
	if m.log != nil {
		m.log.Info("pot funded", "hand", handID, "txid", pot.Txid, "vout", pot.Vout,
			"satoshis", pot.Satoshis, "refundHeight", lockHeight)
	}
	return lp, nil
}

// Settle pays the winners and broadcasts, collecting a signature from every seat.
//
// A seat that refuses leaves the pot unspent and every seat still holding a refund, which is the
// designed outcome rather than a loss.
func (m *PotManager) Settle(ctx context.Context, handID string, seats []AgentEndpoint, payouts map[int]uint64) (string, error) {
	if !m.Enabled() {
		return "", errors.New("webui: this table has no funding wallet")
	}
	m.mu.Lock()
	lp, ok := m.pots[handID]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("webui: no pot is open for hand %s", handID)
	}

	// One payout per winning seat, each derived so only that seat can spend it.
	list := make([]cosign.Payout, 0, len(payouts))
	for _, s := range seats {
		amount, won := payouts[s.Seat]
		if !won || amount == 0 {
			continue
		}
		pub, err := ec.PublicKeyFromString(s.IdentityKey)
		if err != nil {
			return "", fmt.Errorf("webui: seat %d has an unusable identity key: %w", s.Seat, err)
		}
		p := cosign.Payout{
			RecipientKey: pub,
			Satoshis:     amount,
			Prefix:       []byte(handID),
			Suffix:       []byte(fmt.Sprintf("seat-%d", s.Seat)),
		}
		if err := cosign.DerivePayout(m.tableKey, &p); err != nil {
			return "", fmt.Errorf("webui: deriving seat %d's payout: %w", s.Seat, err)
		}
		list = append(list, p)
	}
	if len(list) == 0 {
		return "", errors.New("webui: a settlement with no payouts pays nobody")
	}

	settlement, err := cosign.BuildSettlement(ctx, cosign.SettleArgs{
		Wallet:      m.wallet,
		Originator:  m.originator,
		Pot:         lp.pot,
		Payouts:     list,
		Seats:       len(seats),
		Description: "settle hand " + handID,
	})
	if err != nil {
		return "", fmt.Errorf("webui: building the settlement: %w", err)
	}

	sigs, err := m.collectSignatures(ctx, handID, settlement.Tx, settlement.PotInput, seats)
	if err != nil {
		// An action built but never broadcast leaves change against an all-zero txid, which
		// can block the funding wallet entirely. Abandoning it is not optional.
		if abortErr := cosign.Abandon(ctx, m.wallet, m.originator, settlement); abortErr != nil && m.log != nil {
			m.log.Error("could not abandon the unsigned settlement", "error", abortErr)
		}
		return "", fmt.Errorf("webui: collecting settlement signatures: %w", err)
	}

	res, err := cosign.Complete(ctx, m.wallet, m.originator, settlement, lp.pot, sigs, len(seats))
	if err != nil {
		return "", fmt.Errorf("webui: completing the settlement: %w", err)
	}
	txid := res.Txid.String()
	if m.log != nil {
		m.log.Info("settlement broadcast", "hand", handID, "txid", txid)
	}

	m.mu.Lock()
	delete(m.pots, handID)
	m.mu.Unlock()
	return txid, nil
}

// collectSignatures asks every seat to sign one input, and verifies each answer.
//
// A signature is verified against the seat that claims to have produced it, so a bad one is
// attributed rather than merely rejected: the table can say which seat is at fault.
func (m *PotManager) collectSignatures(ctx context.Context, handID string, tx *transaction.Transaction, inputIndex int, seats []AgentEndpoint) ([]cosign.Signature, error) {
	if m.coord == nil {
		return nil, errors.New("webui: no coordinator is configured to reach the seats")
	}
	rawHex := hex.EncodeToString(tx.Bytes())

	sigs := make([]cosign.Signature, 0, len(seats))
	for _, s := range seats {
		var out struct {
			Seat int    `json:"seat"`
			DER  string `json:"der"`
		}
		params := map[string]any{"handId": handID, "rawTxHex": rawHex, "potInput": inputIndex}
		if err := m.coord.call(ctx, s, substrate.MethodSignPot, params, &out); err != nil {
			return nil, fmt.Errorf("seat %d would not sign: %w", s.Seat, err)
		}
		der, err := hex.DecodeString(out.DER)
		if err != nil {
			return nil, fmt.Errorf("seat %d returned a non-hex signature: %w", s.Seat, err)
		}
		sig := cosign.Signature{Seat: out.Seat, DER: der}

		pub, err := ec.PublicKeyFromString(s.IdentityKey)
		if err != nil {
			return nil, fmt.Errorf("seat %d has an unusable identity key: %w", s.Seat, err)
		}
		if err := cosign.VerifySignature(tx, inputIndex, sig, pub); err != nil {
			return nil, fmt.Errorf("seat %d's signature is invalid: %w", s.Seat, err)
		}
		sigs = append(sigs, sig)
	}
	return sigs, nil
}

// StakeInfo is what a seat's client needs to arm its own wallet for a hand.
//
// Amounts and derivation material only. The scripts are deliberately absent: the wallet derives
// them itself, so a table that lied here would produce a settlement the seat refuses.
type StakeInfo struct {
	HandID       string            `json:"handId"`
	PotTxid      string            `json:"potTxid"`
	PotVout      uint32            `json:"potVout"`
	PotSatoshis  uint64            `json:"potSatoshis"`
	PotScriptHex string            `json:"potScriptHex"`
	SenderKey    string            `json:"senderIdentityKey"`
	MaxFee       uint64            `json:"maxFee"`
	Payouts      []StakeInfoPayout `json:"payouts"`
	Refunds      map[int]string    `json:"-"`
}

// StakeInfoPayout is one expected payout, as amounts and derivation material.
type StakeInfoPayout struct {
	RecipientKey string `json:"recipientKey"`
	Satoshis     uint64 `json:"satoshis"`
	Prefix       string `json:"prefix"`
	Suffix       string `json:"suffix"`
}

// StakeFor describes a hand's pot for one seat, including that seat's own refund.
func (m *PotManager) StakeFor(handID string, seat int, seats []AgentEndpoint, payouts map[int]uint64) (StakeInfo, bool) {
	m.mu.Lock()
	lp, ok := m.pots[handID]
	m.mu.Unlock()
	if !ok {
		return StakeInfo{}, false
	}

	info := StakeInfo{
		HandID:       handID,
		PotTxid:      lp.pot.Txid,
		PotVout:      lp.pot.Vout,
		PotSatoshis:  lp.pot.Satoshis,
		PotScriptHex: hex.EncodeToString(*lp.pot.Script),
		SenderKey:    m.tableKey.PubKey().ToDERHex(),
		// A settlement may not burn more than this. Bounding the fee is what stops a
		// proposal paying a token amount and consuming the rest.
		MaxFee: 1000,
	}
	for _, s := range seats {
		amount, won := payouts[s.Seat]
		if !won || amount == 0 {
			continue
		}
		info.Payouts = append(info.Payouts, StakeInfoPayout{
			RecipientKey: s.IdentityKey,
			Satoshis:     amount,
			Prefix:       base64.StdEncoding.EncodeToString([]byte(handID)),
			Suffix:       base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("seat-%d", s.Seat))),
		})
	}
	if tx, ok := lp.refunds[seat]; ok {
		info.Refunds = map[int]string{seat: hex.EncodeToString(tx.Bytes())}
	}
	return info, true
}

// RefundFor returns a seat's signed refund, so a player can be handed the transaction that
// recovers their stake without depending on this service staying up.
func (m *PotManager) RefundFor(handID string, seat int) (string, uint32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.pots[handID]
	if !ok {
		return "", 0, false
	}
	tx, ok := lp.refunds[seat]
	if !ok {
		return "", 0, false
	}
	return hex.EncodeToString(tx.Bytes()), lp.lockHeight, true
}

// collectRefundSignatures asks every seat to sign one seat's refund.
//
// Refunds use signRefund rather than signPot, and that is not a shortcut. A stake cannot be
// recorded until its refund exists, so requiring a recorded stake to sign a refund would deadlock
// the table: no refund without a stake, no stake without a refund. signRefund carries its own
// safety instead -- each wallet verifies the transaction returns the pot to the named seat and
// refuses otherwise.
func (m *PotManager) collectRefundSignatures(ctx context.Context, handID string, tx *transaction.Transaction, inputIndex int, seats []AgentEndpoint, pot cosign.FundedPot, beneficiary string) ([]cosign.Signature, error) {
	if m.coord == nil {
		return nil, errors.New("webui: no coordinator is configured to reach the seats")
	}
	rawHex := hex.EncodeToString(tx.Bytes())
	potScriptHex := hex.EncodeToString(*pot.Script)
	tableKeys := make([]string, 0, len(seats))
	for _, s := range seats {
		tableKeys = append(tableKeys, s.IdentityKey)
	}

	sigs := make([]cosign.Signature, 0, len(seats))
	for _, s := range seats {
		var out struct {
			Seat int    `json:"seat"`
			DER  string `json:"der"`
		}
		params := map[string]any{
			"handId":       handID,
			"rawTxHex":     rawHex,
			"potInput":     inputIndex,
			"potTxid":      pot.Txid,
			"potVout":      pot.Vout,
			"potSatoshis":  pot.Satoshis,
			"potScriptHex": potScriptHex,
			"seat":         s.Seat,
			"beneficiary":  beneficiary,
			"seats":        tableKeys,
			// The fee this refund consumes. BuildRefund pays out the pot less 300, so the
			// bound must admit that and no more.
			"maxFee": uint64(400),
		}
		if err := m.coord.call(ctx, s, substrate.MethodSignRefund, params, &out); err != nil {
			return nil, fmt.Errorf("seat %d would not sign: %w", s.Seat, err)
		}
		der, err := hex.DecodeString(out.DER)
		if err != nil {
			return nil, fmt.Errorf("seat %d returned a non-hex signature: %w", s.Seat, err)
		}
		sig := cosign.Signature{Seat: out.Seat, DER: der}

		pub, err := ec.PublicKeyFromString(s.IdentityKey)
		if err != nil {
			return nil, fmt.Errorf("seat %d has an unusable identity key: %w", s.Seat, err)
		}
		if err := cosign.VerifySignature(tx, inputIndex, sig, pub); err != nil {
			return nil, fmt.Errorf("seat %d's signature is invalid: %w", s.Seat, err)
		}
		sigs = append(sigs, sig)
	}
	return sigs, nil
}
