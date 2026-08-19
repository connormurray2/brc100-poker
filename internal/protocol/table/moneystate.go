package table

import (
	"fmt"
	"sync"
	"time"
)

// MoneyPhase is where a seat's funds are, from that seat's point of view.
//
// Deliberately separate from the table's Phase. A player does not need to know that the deal is
// on its remask pass; they need to know whether their money is committed, returned, or stuck.
type MoneyPhase string

const (
	// MoneyUncommitted means nothing is at risk yet.
	MoneyUncommitted MoneyPhase = "uncommitted"
	// MoneyRefundHeld means a signed refund exists but the stake has not been committed.
	// This is the only state from which committing is safe.
	MoneyRefundHeld MoneyPhase = "refund-held"
	// MoneyCommitted means the stake is in the pot and the hand is live.
	MoneyCommitted MoneyPhase = "committed"
	// MoneySettling means a settlement has been proposed or signed. The outcome is not yet
	// certain, which is exactly why this is distinct from settled.
	MoneySettling MoneyPhase = "settling"
	// MoneySettled means the hand paid out and the result is confirmed.
	MoneySettled MoneyPhase = "settled"
	// MoneyStalled means the hand cannot complete and recovery is the remaining path.
	MoneyStalled MoneyPhase = "stalled"
	// MoneyRecovered means the stake was returned by refund.
	MoneyRecovered MoneyPhase = "recovered"
)

// MoneyState is what a player is told about their own funds.
//
// Every field exists to answer a question a player actually asks. The design deliberately does not
// hide a stall behind a spinner: a player whose money is stuck is entitled to know that it is
// stuck, who is responsible, and when they can get it back.
type MoneyState struct {
	HandID string     `json:"handId"`
	Seat   int        `json:"seat"`
	Phase  MoneyPhase `json:"phase"`

	// StakeSatoshis is what this seat committed.
	StakeSatoshis uint64 `json:"stakeSatoshis"`
	// PotSatoshis is the whole pot, so a player can see what is being played for.
	PotSatoshis uint64 `json:"potSatoshis"`

	// RefundHeld reports whether a signed refund exists. False while committed is a bug, not
	// a state: the table refuses to accept a stake without one.
	RefundHeld bool `json:"refundHeld"`
	// RefundSpendableAtHeight is when the refund matures. A player needs this before funding,
	// because it is the worst case a stall can cost them.
	RefundSpendableAtHeight uint32 `json:"refundSpendableAtHeight"`
	// CurrentHeight lets a client render "about N blocks away" without a second call.
	CurrentHeight uint32 `json:"currentHeight,omitempty"`

	// PayoutSatoshis is what this seat receives, once known.
	PayoutSatoshis uint64 `json:"payoutSatoshis,omitempty"`
	// SettlementTxID identifies the settlement, so a player can check it independently.
	SettlementTxID string `json:"settlementTxid,omitempty"`
	// PayoutSpendable reports whether the payout has been received into the player's wallet.
	// Broadcasting is not receiving: a payout needs a merkle proof before it can be
	// internalized, and only then can it be spent.
	PayoutSpendable bool `json:"payoutSpendable"`

	// StalledSeat is the seat responsible for a stall, or -1.
	StalledSeat int `json:"stalledSeat"`
	// StallReason explains it in terms a player can act on.
	StallReason string `json:"stallReason,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// Summary renders the state as a sentence for a player-facing client.
//
// Written as prose rather than a status code because the states a player most needs to understand
// are the unhappy ones, and a code does not tell them what to do.
func (m MoneyState) Summary() string {
	switch m.Phase {
	case MoneyUncommitted:
		return "Nothing committed yet."
	case MoneyRefundHeld:
		return fmt.Sprintf("Refund held, spendable from block %d. Safe to commit %d sat.",
			m.RefundSpendableAtHeight, m.StakeSatoshis)
	case MoneyCommitted:
		return fmt.Sprintf("%d sat committed to a %d sat pot. If the hand stalls you can reclaim it from block %d.",
			m.StakeSatoshis, m.PotSatoshis, m.RefundSpendableAtHeight)
	case MoneySettling:
		return "Settlement in progress. Not final until it confirms."
	case MoneySettled:
		if m.PayoutSatoshis == 0 {
			return "Hand settled. You did not win this pot."
		}
		if !m.PayoutSpendable {
			return fmt.Sprintf("Hand settled, %d sat to you. Not spendable until the settlement is mined.",
				m.PayoutSatoshis)
		}
		return fmt.Sprintf("Hand settled, %d sat received and spendable.", m.PayoutSatoshis)
	case MoneyStalled:
		who := "a seat"
		if m.StalledSeat >= 0 {
			who = fmt.Sprintf("seat %d", m.StalledSeat)
		}
		blocks := ""
		if m.CurrentHeight > 0 && m.RefundSpendableAtHeight > m.CurrentHeight {
			blocks = fmt.Sprintf(" (about %d blocks away)", m.RefundSpendableAtHeight-m.CurrentHeight)
		}
		return fmt.Sprintf("Hand stalled by %s: %s. Your %d sat is recoverable from block %d%s.",
			who, m.StallReason, m.StakeSatoshis, m.RefundSpendableAtHeight, blocks)
	case MoneyRecovered:
		return fmt.Sprintf("Stake of %d sat recovered by refund.", m.StakeSatoshis)
	default:
		return fmt.Sprintf("Unknown money phase %q.", m.Phase)
	}
}

// AtRisk reports whether the player has value committed with no confirmed outcome.
//
// The states where a client should keep the player informed rather than letting the hand fade into
// the background.
func (m MoneyState) AtRisk() bool {
	switch m.Phase {
	case MoneyCommitted, MoneySettling, MoneyStalled:
		return true
	default:
		return false
	}
}

// MoneyTracker maintains each seat's money state.
//
// One per table. It is the table's view of what to tell each player, and it is deliberately not
// authoritative about the chain: a seat's own agent holds the record it verifies settlements
// against, and this exists so a player is not left guessing.
type MoneyTracker struct {
	mu     sync.RWMutex
	states map[int]*MoneyState
	now    func() time.Time
}

// NewMoneyTracker builds a tracker for a table's seats.
func NewMoneyTracker(seats int, stake, pot uint64, refundHeight uint32) (*MoneyTracker, error) {
	if seats < MinSeats || seats > MaxSeats {
		return nil, fmt.Errorf("table: seats must be %d..%d, got %d", MinSeats, MaxSeats, seats)
	}
	if refundHeight == 0 {
		return nil, fmt.Errorf("table: a refund height is required: a player must be told when a stall ends")
	}

	t := &MoneyTracker{states: make(map[int]*MoneyState, seats), now: time.Now}
	for seat := 0; seat < seats; seat++ {
		t.states[seat] = &MoneyState{
			HandID:                  "",
			Seat:                    seat,
			Phase:                   MoneyUncommitted,
			StakeSatoshis:           stake,
			PotSatoshis:             pot,
			RefundSpendableAtHeight: refundHeight,
			StalledSeat:             -1,
			UpdatedAt:               t.now(),
		}
	}
	return t, nil
}

// State returns one seat's money state.
func (t *MoneyTracker) State(seat int) (MoneyState, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.states[seat]
	if !ok {
		return MoneyState{}, fmt.Errorf("table: seat %d does not exist", seat)
	}
	return *s, nil
}

// All returns every seat's state, in seat order.
func (t *MoneyTracker) All() []MoneyState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]MoneyState, 0, len(t.states))
	for seat := 0; seat < len(t.states); seat++ {
		if s, ok := t.states[seat]; ok {
			out = append(out, *s)
		}
	}
	return out
}

// SetHand records which hand these states belong to.
func (t *MoneyTracker) SetHand(handID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.states {
		s.HandID = handID
		s.UpdatedAt = t.now()
	}
}

// RefundHeld records that a seat holds its signed refund.
func (t *MoneyTracker) RefundHeld(seat int) error {
	return t.update(seat, func(s *MoneyState) error {
		s.RefundHeld = true
		if s.Phase == MoneyUncommitted {
			s.Phase = MoneyRefundHeld
		}
		return nil
	})
}

// Committed records that a seat's stake is in the pot.
//
// Refuses a seat with no refund. The table enforces this too, but a player-facing state that could
// show "committed, no refund" would be showing a state the system must never reach.
func (t *MoneyTracker) Committed(seat int) error {
	return t.update(seat, func(s *MoneyState) error {
		if !s.RefundHeld {
			return fmt.Errorf("table: seat %d has no refund; it must not be shown as committed", seat)
		}
		s.Phase = MoneyCommitted
		return nil
	})
}

// Settling records that a settlement is in progress.
func (t *MoneyTracker) Settling(txid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.states {
		if s.Phase == MoneyCommitted {
			s.Phase = MoneySettling
			s.SettlementTxID = txid
			s.UpdatedAt = t.now()
		}
	}
}

// Settled records the outcome, with each seat's payout.
//
// Payouts are keyed by seat; a seat absent from the map won this nothing, which is a legitimate
// outcome rather than missing data.
func (t *MoneyTracker) Settled(txid string, payouts map[int]uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for seat, s := range t.states {
		s.Phase = MoneySettled
		s.SettlementTxID = txid
		s.PayoutSatoshis = payouts[seat]
		s.UpdatedAt = t.now()
	}
}

// PayoutReceived records that a seat's payout is spendable in its own wallet.
//
// Separate from Settled because broadcasting is not receiving: a payout needs a merkle proof before
// it can be internalized, and a player told "settled" while the coin is not yet spendable would
// reasonably think something is wrong.
func (t *MoneyTracker) PayoutReceived(seat int) error {
	return t.update(seat, func(s *MoneyState) error {
		s.PayoutSpendable = true
		return nil
	})
}

// Stalled records that the hand cannot complete, and who is responsible.
func (t *MoneyTracker) Stalled(seat int, reason string) error {
	if reason == "" {
		return fmt.Errorf("table: a stall must have a reason a player can act on")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.states {
		// A settled seat is not retroactively stalled: its money already moved.
		if s.Phase == MoneySettled || s.Phase == MoneyRecovered {
			continue
		}
		s.Phase = MoneyStalled
		s.StalledSeat = seat
		s.StallReason = reason
		s.UpdatedAt = t.now()
	}
	return nil
}

// Recovered records that a seat reclaimed its stake by refund.
func (t *MoneyTracker) Recovered(seat int) error {
	return t.update(seat, func(s *MoneyState) error {
		s.Phase = MoneyRecovered
		return nil
	})
}

// SetHeight records the chain tip, so a client can say how far away a refund is.
func (t *MoneyTracker) SetHeight(height uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.states {
		s.CurrentHeight = height
	}
}

func (t *MoneyTracker) update(seat int, fn func(*MoneyState) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.states[seat]
	if !ok {
		return fmt.Errorf("table: seat %d does not exist", seat)
	}
	if err := fn(s); err != nil {
		return err
	}
	s.UpdatedAt = t.now()
	return nil
}
