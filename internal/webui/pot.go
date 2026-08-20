package webui

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	// armed records which seats have recorded their stake for a hand, so settlement can wait
	// for them instead of racing their browsers and being declined.
	armed map[string]map[int]bool
}

// livePot is a session's funded pot.
//
// One pot spans many hands. Hands move `balances`; the chain is touched only at buy-in and exit.
// That is what removes the profit from refusing to settle: a refusing seat recovers its current
// balance, which is exactly what settling would pay it.
type livePot struct {
	pot   cosign.FundedPot
	seats []*ec.PublicKey
	// balances is each seat's current holding. Sums to the pot at all times.
	balances map[int]uint64
	// refund is the current fully-signed refund, paying every seat its balance. Replaced after
	// each hand; a seat always holds the latest.
	refund *transaction.Transaction
	// lockHeight is when the current refund matures. Decreases with each new refund so the
	// newest state is spendable first and a stale refund loses the race.
	lockHeight uint32
	// floorHeight is where the ladder runs out and the session must settle and reopen.
	floorHeight uint32
}

// ladderStep is how much earlier each new refund matures than the one it replaces.
//
// Four blocks is enough to make the ordering unambiguous under normal propagation while allowing a
// useful number of hands before the ladder bottoms out.
const ladderStep = 4

// refundFee is what a shared refund may consume. It pays one output per seat, so it is larger
// than a single-recipient refund.
const refundFee = 500

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
		pots:  make(map[string]*livePot),
		armed: make(map[string]map[int]bool),
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

	// One refund paying every seat its own balance, rather than one refund per seat paying
	// that seat the whole pot. The per-seat form created a first-broadcast race AND rewarded
	// refusing to settle; see docs/session-pot-design.md.
	balances := make(map[int]uint64, len(seats))
	stake := satoshis / uint64(len(seats))
	for _, s := range seats {
		balances[s.Seat] = stake
	}

	lockHeight := currentHeight + 144
	lp := &livePot{
		pot: pot, seats: pubs, balances: balances,
		lockHeight:  lockHeight,
		floorHeight: currentHeight + 12, // the ladder must stay comfortably ahead of the tip
	}
	if err := m.resignRefund(ctx, handID, lp, seats, lockHeight); err != nil {
		return nil, err
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
	// Reserve the fee before deriving anything. The engine's payouts sum to the whole pot, so
	// paying them in full would leave the funding wallet to conjure a fee -- which it does by
	// emitting change back to itself, an output no seat declared and every seat then refuses.
	// Taking the fee off the top means the outputs sum to exactly the pot less the fee, and no
	// change output is created.
	payouts = reserveFee(payouts, settlementFee)

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

	// Refuse to describe a stake before the hand is decided. The expectation is the set of
	// payouts a seat will accept, and one built mid-hand names none -- so the wallet would
	// later refuse the settlement that pays the winner. Better to have no stake recorded yet
	// than a stake recording the wrong thing.
	if len(payouts) == 0 {
		return StakeInfo{}, false
	}
	// Exactly the reservation Settle applies. If these diverge the seat expects an amount the
	// settlement never pays, which is the same failure as deriving the wrong script.
	payouts = reserveFee(payouts, settlementFee)

	info := StakeInfo{
		HandID:       handID,
		PotTxid:      lp.pot.Txid,
		PotVout:      lp.pot.Vout,
		PotSatoshis:  lp.pot.Satoshis,
		PotScriptHex: hex.EncodeToString(*lp.pot.Script),
		SenderKey:    m.tableKey.PubKey().ToDERHex(),
		// Must admit the fee Settle reserves, or a seat refuses the very settlement the
		// table builds. Headroom above it, not below.
		MaxFee: settlementFee + 200,
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
	// One shared refund, so every seat is handed the same transaction.
	if lp.refund != nil {
		info.Refunds = map[int]string{seat: hex.EncodeToString(lp.refund.Bytes())}
	}
	return info, true
}

// RefundFor returns the session's signed refund, so a player holds the transaction that recovers
// their balance without depending on this service staying up.
//
// One transaction for the whole session, not one per seat: it pays every seat its balance, so it
// does not matter who broadcasts it.
func (m *PotManager) RefundFor(sessionID string, _ int) (string, uint32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.pots[sessionID]
	if !ok || lp.refund == nil {
		return "", 0, false
	}
	return hex.EncodeToString(lp.refund.Bytes()), lp.lockHeight, true
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
			// A wallet built before signRefund existed reports the method as unserved. That
			// is a version mismatch rather than a refusal, and saying so is the difference
			// between a player restarting their wallet and giving up.
			if strings.Contains(err.Error(), "not served") {
				return nil, fmt.Errorf(
					"seat %d is running a wallet too old for this table: it does not serve "+
						"signRefund. Restart cmd/agent from the current source", s.Seat)
			}
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

// MarkArmed records that a seat has recorded its stake for a hand.
func (m *PotManager) MarkArmed(handID string, seat int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.armed[handID] == nil {
		m.armed[handID] = make(map[int]bool)
	}
	m.armed[handID][seat] = true
}

// AllArmed reports whether every seat has recorded its stake for a hand.
//
// Settlement waits on this. A seat asked to sign before it has an expectation declines, and that
// decline reads as a stall even though nothing is wrong -- the browser simply had not polled yet.
func (m *PotManager) AllArmed(handID string, seats []AgentEndpoint) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	got := m.armed[handID]
	for _, s := range seats {
		if !got[s.Seat] {
			return false
		}
	}
	return len(seats) > 0
}

// settlementFee is what a settlement is allowed to consume.
//
// A settlement spends one n-of-n input whose unlocking script carries a signature per seat, so it
// is larger than an ordinary spend. Generous enough to cover a six-seat table at the configured
// rate, and bounded so it cannot be used to drain the pot.
const settlementFee = 1200

// reserveFee takes the fee off the top of the payouts.
//
// Deducted from the largest payout, because the winner can absorb it while a seat owed a small
// side pot might be smaller than the fee itself. Returns a new map; the caller's is untouched.
func reserveFee(payouts map[int]uint64, fee uint64) map[int]uint64 {
	if len(payouts) == 0 || fee == 0 {
		return payouts
	}
	out := make(map[int]uint64, len(payouts))
	largest, largestSeat := uint64(0), -1
	for seat, amount := range payouts {
		out[seat] = amount
		if amount > largest {
			largest, largestSeat = amount, seat
		}
	}
	if largestSeat < 0 || largest <= fee {
		// Nothing can absorb the fee. Leave the payouts alone and let the settlement fail
		// loudly rather than silently paying someone nothing.
		return out
	}
	out[largestSeat] = largest - fee
	return out
}

// resignRefund builds and collects signatures for the refund of the current state.
//
// Called at buy-in and again after every hand. Each refund matures earlier than the one it
// replaces, so a seat cannot fall back on an older state that paid it more.
func (m *PotManager) resignRefund(ctx context.Context, sessionID string, lp *livePot, seats []AgentEndpoint, lockHeight uint32) error {
	recipients := make([]cosign.RefundOutput, 0, len(seats))
	for i, s := range seats {
		amount := lp.balances[s.Seat]
		if amount == 0 {
			// A seat with nothing left is omitted: an output of zero is unspendable dust and
			// would make the balances fail to total the pot.
			continue
		}
		recipients = append(recipients, cosign.RefundOutput{Recipient: lp.seats[i], Satoshis: amount})
	}
	if len(recipients) == 0 {
		return errors.New("webui: no seat holds a balance to refund")
	}

	refund, err := cosign.BuildRefund(cosign.RefundArgs{
		Pot: lp.pot, Recipients: recipients, LockHeight: lockHeight, Fee: refundFee,
	})
	if err != nil {
		return fmt.Errorf("webui: building the shared refund: %w", err)
	}

	sigs, err := m.collectSharedRefundSignatures(ctx, sessionID, refund, 0, seats, lp)
	if err != nil {
		return fmt.Errorf("webui: signing the shared refund: %w", err)
	}
	unlock, err := cosign.Assemble(sigs, len(seats))
	if err != nil {
		return fmt.Errorf("webui: assembling the shared refund: %w", err)
	}
	refund.Inputs[0].UnlockingScript = unlock
	if err := cosign.VerifyScript(refund, 0, lp.pot.Script, lp.pot.Satoshis); err != nil {
		return fmt.Errorf("webui: the shared refund does not satisfy the pot: %w", err)
	}

	lp.refund = refund
	lp.lockHeight = lockHeight
	if m.log != nil {
		m.log.Info("refund re-signed for the current balances",
			"session", sessionID, "lockHeight", lockHeight, "balances", lp.balances)
	}
	return nil
}

// ApplyHand moves the session balances and re-signs the refund for the new state.
//
// The refund must be re-signed before the next hand deals, or a seat that loses the next hand
// could fall back on a refund reflecting the balance it held before this one.
func (m *PotManager) ApplyHand(ctx context.Context, sessionID string, seats []AgentEndpoint, balances map[int]uint64) error {
	m.mu.Lock()
	lp, ok := m.pots[sessionID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("webui: no session pot is open for %s", sessionID)
	}

	var total uint64
	for _, v := range balances {
		total += v
	}
	if total != lp.pot.Satoshis {
		// The balances are the authoritative split of the pot, so they must account for all
		// of it. A mismatch means the engine and the pot disagree, which must stop the session
		// rather than settle a number nobody can verify.
		return fmt.Errorf("webui: balances total %d but the pot holds %d", total, lp.pot.Satoshis)
	}

	next := lp.lockHeight - ladderStep
	if next <= lp.floorHeight {
		return fmt.Errorf("webui: the refund ladder is exhausted at height %d; the session must "+
			"settle and reopen", lp.lockHeight)
	}

	m.mu.Lock()
	lp.balances = balances
	// Arming is per state, not per session. A seat armed for the previous balances has an
	// expectation the settlement will not match, so clearing this forces it to re-arm.
	delete(m.armed, sessionID)
	m.mu.Unlock()
	return m.resignRefund(ctx, sessionID, lp, seats, next)
}

// Balances returns the session's current balances.
func (m *PotManager) Balances(sessionID string) (map[int]uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.pots[sessionID]
	if !ok {
		return nil, false
	}
	out := make(map[int]uint64, len(lp.balances))
	for k, v := range lp.balances {
		out[k] = v
	}
	return out, true
}

// collectSharedRefundSignatures asks every seat to sign the shared refund.
func (m *PotManager) collectSharedRefundSignatures(ctx context.Context, sessionID string, tx *transaction.Transaction, inputIndex int, seats []AgentEndpoint, lp *livePot) ([]cosign.Signature, error) {
	if m.coord == nil {
		return nil, errors.New("webui: no coordinator is configured to reach the seats")
	}
	rawHex := hex.EncodeToString(tx.Bytes())
	potScriptHex := hex.EncodeToString(*lp.pot.Script)

	// Every seat is told the whole balance set, so each can verify the refund restores the
	// state rather than only checking its own share.
	shares := make([]map[string]any, 0, len(seats))
	for i, s := range seats {
		if lp.balances[s.Seat] == 0 {
			continue
		}
		shares = append(shares, map[string]any{
			"recipientKey": lp.seats[i].ToDERHex(),
			"satoshis":     lp.balances[s.Seat],
		})
	}

	sigs := make([]cosign.Signature, 0, len(seats))
	for _, s := range seats {
		var out struct {
			Seat int    `json:"seat"`
			DER  string `json:"der"`
		}
		params := map[string]any{
			"handId":        sessionID,
			"rawTxHex":      rawHex,
			"potInput":      inputIndex,
			"potTxid":       lp.pot.Txid,
			"potVout":       lp.pot.Vout,
			"potSatoshis":   lp.pot.Satoshis,
			"potScriptHex":  potScriptHex,
			"seat":          s.Seat,
			"balances":      shares,
			"maxFee":        refundFee,
			"maxLockHeight": tx.LockTime,
		}
		if err := m.coord.call(ctx, s, substrate.MethodSignRefund, params, &out); err != nil {
			if strings.Contains(err.Error(), "not served") {
				return nil, fmt.Errorf(
					"seat %d is running a wallet too old for this table: it does not serve "+
						"signRefund. Restart cmd/agent from the current source", s.Seat)
			}
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
