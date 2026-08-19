// Package table coordinates the seats at one poker table.
//
// The table service is a convenience, not an authority. It orders messages, tracks who is
// seated, and proposes transactions — but it holds no pot key and no player key, so a
// malicious or compromised table can stall a hand and can never move money. Every claim it
// makes is checked independently by each seat.
package table

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Seat bounds, matching the engine.
const (
	MinSeats = 2
	MaxSeats = 6
)

// Phase is where a table is in its lifecycle.
type Phase string

const (
	// PhaseOpen means seats are still being filled.
	PhaseOpen Phase = "open"
	// PhaseFunding means the roster is fixed and seats are committing buy-ins.
	PhaseFunding Phase = "funding"
	// PhaseDealing means the mental-poker deal is in progress.
	PhaseDealing Phase = "dealing"
	// PhaseBetting means the hand is being played.
	PhaseBetting Phase = "betting"
	// PhaseSettling means the hand is over and the pot is being paid.
	PhaseSettling Phase = "settling"
	// PhaseClosed means the table is finished.
	PhaseClosed Phase = "closed"
	// PhaseStalled means the hand cannot proceed and recovery is the only path left.
	PhaseStalled Phase = "stalled"
)

// Terms are the table's advertised conditions.
//
// A prospective player sees these before committing anything, and they are frozen the moment
// any seat funds — otherwise a table could advertise one buy-in and charge another.
type Terms struct {
	TableID string
	// BuyInSatoshis is what each seat stakes.
	BuyInSatoshis uint64
	SmallBlind    uint64
	BigBlind      uint64
	// Seats is the number of seats this table plays with.
	Seats int
	// RefundLockHeight is the height at which a seat's refund becomes spendable. A
	// player needs this before funding: it is how long a stall can cost them.
	RefundLockHeight uint32
}

// Validate checks the terms are playable.
func (t Terms) Validate() error {
	var errs []error
	if t.TableID == "" {
		errs = append(errs, errors.New("table id is required"))
	}
	if t.Seats < MinSeats || t.Seats > MaxSeats {
		errs = append(errs, fmt.Errorf("seats must be %d..%d, got %d", MinSeats, MaxSeats, t.Seats))
	}
	if t.BuyInSatoshis == 0 {
		errs = append(errs, errors.New("buy-in must be positive"))
	}
	if t.BigBlind == 0 || t.SmallBlind == 0 {
		errs = append(errs, errors.New("blinds must be positive"))
	}
	if t.SmallBlind > t.BigBlind {
		errs = append(errs, errors.New("small blind exceeds big blind"))
	}
	if t.BigBlind > t.BuyInSatoshis {
		errs = append(errs, errors.New("big blind exceeds the buy-in"))
	}
	if t.RefundLockHeight == 0 {
		errs = append(errs, errors.New("a refund lock height is required: a player must know how long a stall can cost them"))
	}
	return errors.Join(errs...)
}

// Seat is one occupied seat.
type Seat struct {
	// Index is the seat's position, which fixes betting order and the pot script's key
	// order.
	Index int
	// IdentityKey is the player's proven public key, hex-encoded.
	IdentityKey string
	// Funded reports whether this seat's buy-in is committed.
	Funded bool
	// RefundHeld reports whether this seat holds its signed refund. No stake may be
	// committed before this is true.
	RefundHeld bool
	// JoinedAt is when the seat was taken.
	JoinedAt time.Time
	// LastSeen is the last time this seat was heard from, for timeout handling.
	LastSeen time.Time
}

// Table is one table's coordination state.
type Table struct {
	mu    sync.RWMutex
	terms Terms
	phase Phase
	seats []*Seat
	// byIdentity indexes seats by identity key so a second join is detectable.
	byIdentity map[string]*Seat
	// stallReason records why a table stalled, so the responsible seat is attributable.
	stallReason string
	// stalledSeat is the seat blamed for a stall, or -1.
	stalledSeat int

	now func() time.Time
}

// New creates an open table.
func New(terms Terms) (*Table, error) {
	if err := terms.Validate(); err != nil {
		return nil, fmt.Errorf("table: invalid terms: %w", err)
	}
	return &Table{
		terms:       terms,
		phase:       PhaseOpen,
		byIdentity:  make(map[string]*Seat),
		stalledSeat: -1,
		now:         time.Now,
	}, nil
}

// Terms returns the advertised terms.
func (t *Table) Terms() Terms {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.terms
}

// Phase returns the current phase.
func (t *Table) Phase() Phase {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.phase
}

// Seats returns a snapshot of the occupied seats in index order.
func (t *Table) Seats() []Seat {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Seat, 0, len(t.seats))
	for _, s := range t.seats {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// Join seats a player.
//
// The identity key must already be proven by the substrate or transport layer; this records
// the binding and enforces one seat per identity. Allowing an identity two seats would let
// one player see two hands and collude with themselves.
func (t *Table) Join(identityKey string) (int, error) {
	key := strings.ToLower(strings.TrimSpace(identityKey))
	if key == "" {
		return 0, errors.New("table: an identity key is required to take a seat")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.phase != PhaseOpen {
		return 0, fmt.Errorf("table: seats cannot be taken while the table is %s", t.phase)
	}
	if _, seated := t.byIdentity[key]; seated {
		return 0, fmt.Errorf("table: identity %s… already holds a seat", short(key))
	}
	if len(t.seats) >= t.terms.Seats {
		return 0, fmt.Errorf("table: all %d seats are taken", t.terms.Seats)
	}

	now := t.now()
	s := &Seat{Index: len(t.seats), IdentityKey: key, JoinedAt: now, LastSeen: now}
	t.seats = append(t.seats, s)
	t.byIdentity[key] = s
	return s.Index, nil
}

// SeatOf returns the seat index for an identity.
func (t *Table) SeatOf(identityKey string) (int, error) {
	key := strings.ToLower(strings.TrimSpace(identityKey))
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.byIdentity[key]
	if !ok {
		return 0, fmt.Errorf("table: identity %s… holds no seat", short(key))
	}
	return s.Index, nil
}

// Authorise confirms a message claiming to act for a seat comes from that seat's identity.
//
// This is the check that stops one player acting as another. It is deliberately a single
// function so there is one place to get it right.
func (t *Table) Authorise(seatIndex int, identityKey string) error {
	key := strings.ToLower(strings.TrimSpace(identityKey))
	t.mu.RLock()
	defer t.mu.RUnlock()

	if seatIndex < 0 || seatIndex >= len(t.seats) {
		return fmt.Errorf("table: seat %d does not exist", seatIndex)
	}
	if t.seats[seatIndex].IdentityKey != key {
		return fmt.Errorf("table: identity %s… is not seat %d", short(key), seatIndex)
	}
	return nil
}

// Touch records that a seat was heard from.
func (t *Table) Touch(seatIndex int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if seatIndex < 0 || seatIndex >= len(t.seats) {
		return fmt.Errorf("table: seat %d does not exist", seatIndex)
	}
	t.seats[seatIndex].LastSeen = t.now()
	return nil
}

// CloseRoster fixes the seat list and moves to funding.
//
// Terms are frozen from this point: a table that could change the buy-in after players
// commit could charge them anything.
func (t *Table) CloseRoster() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase != PhaseOpen {
		return fmt.Errorf("table: the roster can only be closed from %s, not %s", PhaseOpen, t.phase)
	}
	if len(t.seats) < MinSeats {
		return fmt.Errorf("table: %d seats are taken, need at least %d", len(t.seats), MinSeats)
	}
	t.phase = PhaseFunding
	return nil
}

// MarkRefundHeld records that a seat holds its signed refund.
func (t *Table) MarkRefundHeld(seatIndex int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if seatIndex < 0 || seatIndex >= len(t.seats) {
		return fmt.Errorf("table: seat %d does not exist", seatIndex)
	}
	t.seats[seatIndex].RefundHeld = true
	return nil
}

// MarkFunded records a seat's buy-in.
//
// Refusing to record funding before the refund is held enforces the precondition in code
// rather than leaving it to the caller's discipline: a stake committed without a refund is a
// stake that a stall can trap.
func (t *Table) MarkFunded(seatIndex int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.phase != PhaseFunding {
		return fmt.Errorf("table: buy-ins are only accepted while funding, not %s", t.phase)
	}
	if seatIndex < 0 || seatIndex >= len(t.seats) {
		return fmt.Errorf("table: seat %d does not exist", seatIndex)
	}
	s := t.seats[seatIndex]
	if !s.RefundHeld {
		return fmt.Errorf("table: seat %d has no refund yet; funding before a refund is held would let a stall trap the stake", seatIndex)
	}
	s.Funded = true
	return nil
}

// FullyFunded reports whether every seat has committed.
func (t *Table) FullyFunded() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.fullyFundedLocked()
}

func (t *Table) fullyFundedLocked() bool {
	if len(t.seats) < MinSeats {
		return false
	}
	for _, s := range t.seats {
		if !s.Funded {
			return false
		}
	}
	return true
}

// UnfundedSeats lists seats that have not committed, so a partial pot is attributable.
func (t *Table) UnfundedSeats() []int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []int
	for _, s := range t.seats {
		if !s.Funded {
			out = append(out, s.Index)
		}
	}
	return out
}

// BeginDeal moves to dealing once every seat has funded.
//
// A hand must not start on a partial pot: seats that funded would have value at risk in a
// hand that cannot pay out.
func (t *Table) BeginDeal() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase != PhaseFunding {
		return fmt.Errorf("table: dealing begins from %s, not %s", PhaseFunding, t.phase)
	}
	if !t.fullyFundedLocked() {
		return errors.New("table: refusing to deal on a partial pot")
	}
	t.phase = PhaseDealing
	return nil
}

// Advance moves the table to the next phase, rejecting an out-of-order transition.
//
// Ordered progression is what keeps every seat's view of the hand in step; a message for a
// step that is not current is rejected rather than applied early.
func (t *Table) Advance(to Phase) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	allowed := map[Phase][]Phase{
		PhaseOpen:     {PhaseFunding, PhaseClosed},
		PhaseFunding:  {PhaseDealing, PhaseStalled, PhaseClosed},
		PhaseDealing:  {PhaseBetting, PhaseStalled},
		PhaseBetting:  {PhaseSettling, PhaseStalled},
		PhaseSettling: {PhaseClosed, PhaseStalled},
		PhaseStalled:  {PhaseClosed},
		PhaseClosed:   nil,
	}
	for _, ok := range allowed[t.phase] {
		if ok == to {
			t.phase = to
			return nil
		}
	}
	return fmt.Errorf("table: cannot move from %s to %s", t.phase, to)
}

// Stall marks the table stalled and records who is responsible.
//
// Attribution matters: a seat that stops responding is indistinguishable from one that is
// deliberately griefing, and the other seats need to know which seat to blame when they
// fall back on refunds.
func (t *Table) Stall(seatIndex int, reason string) error {
	if reason == "" {
		return errors.New("table: a stall must record a reason so it is explicable to the other seats")
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.phase == PhaseClosed {
		return errors.New("table: a closed table cannot stall")
	}
	if seatIndex >= len(t.seats) {
		return fmt.Errorf("table: seat %d does not exist", seatIndex)
	}
	t.phase = PhaseStalled
	t.stalledSeat = seatIndex
	t.stallReason = reason
	return nil
}

// StallInfo reports why the table stalled and which seat is blamed, or -1 for none.
func (t *Table) StallInfo() (int, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.stalledSeat, t.stallReason
}

// TimedOutSeats lists seats not heard from within the limit.
func (t *Table) TimedOutSeats(limit time.Duration) []int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cutoff := t.now().Add(-limit)
	var out []int
	for _, s := range t.seats {
		if s.LastSeen.Before(cutoff) {
			out = append(out, s.Index)
		}
	}
	return out
}

// Close finishes the table.
func (t *Table) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseClosed
}

// IdentityKeys returns the seats' identity keys in seat order.
//
// The order is the pot script's key order, so it must be stable and index-aligned: a
// reordering would produce a different pot script and invalidate every signature.
func (t *Table) IdentityKeys() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, len(t.seats))
	for _, s := range t.seats {
		out[s.Index] = s.IdentityKey
	}
	return out
}

func short(key string) string {
	const n = 12
	if len(key) <= n {
		return key
	}
	return key[:n]
}
