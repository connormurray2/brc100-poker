package webui

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/engine"
	"github.com/cmurray/brc100-poker/internal/protocol/table"
)

// LiveTable is a playable hand the browser drives.
//
// It exists because a viewer nobody can act in is not a poker game. The server owns the engine and
// is the only thing that advances it: a browser submits an intent and the engine decides whether it
// was legal, so a hostile client can send whatever it likes and still cannot act out of turn or
// bet chips it does not have.
type LiveTable struct {
	mu sync.Mutex

	id    string
	terms table.Terms

	// seatOf maps an identity key to its seat, which is how an action is attributed to a
	// player rather than believed.
	seatOf map[string]int
	seats  []table.Seat

	st    *engine.State
	money *table.MoneyTracker

	// hole holds each seat's cards, released only to that seat.
	hole map[int][]cards.Card

	// stalledSeat and stallReason surface a stall instead of hiding it.
	stalledSeat int
	stallReason string

	now func() time.Time
}

// NewLiveTable creates a playable table.
func NewLiveTable(terms table.Terms) (*LiveTable, error) {
	if err := terms.Validate(); err != nil {
		return nil, fmt.Errorf("webui: invalid terms: %w", err)
	}
	money, err := table.NewMoneyTracker(terms.Seats, terms.BuyInSatoshis,
		terms.BuyInSatoshis*uint64(terms.Seats), terms.RefundLockHeight)
	if err != nil {
		return nil, err
	}
	money.SetHand(terms.TableID)

	return &LiveTable{
		id:          terms.TableID,
		terms:       terms,
		seatOf:      make(map[string]int),
		money:       money,
		hole:        make(map[int][]cards.Card),
		stalledSeat: -1,
		now:         time.Now,
	}, nil
}

// Join seats a player and returns their seat index.
//
// One identity, one seat: letting an identity hold two would let a player see two hands and collude
// with themselves. An identity that rejoins gets the seat it already has, so a page refresh does
// not cost a player their seat.
func (l *LiveTable) Join(identityKey string) (int, error) {
	key := strings.ToLower(strings.TrimSpace(identityKey))
	if key == "" {
		return 0, errors.New("webui: an identity key is required to take a seat")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if seat, ok := l.seatOf[key]; ok {
		return seat, nil
	}
	if len(l.seats) >= l.terms.Seats {
		return 0, fmt.Errorf("webui: all %d seats are taken", l.terms.Seats)
	}
	if l.st != nil {
		return 0, errors.New("webui: the hand has already started")
	}

	seat := len(l.seats)
	l.seatOf[key] = seat
	l.seats = append(l.seats, table.Seat{
		Index:       seat,
		IdentityKey: key,
		JoinedAt:    l.now(),
		LastSeen:    l.now(),
	})
	return seat, nil
}

// Ready marks a seat as funded and holding its refund, then starts the hand once every seat is.
//
// The refund precondition is enforced here as well as in the table package, because a UI that could
// seat a player and take their stake without one would be a path around the guarantee.
func (l *LiveTable) Ready(identityKey string) error {
	key := strings.ToLower(strings.TrimSpace(identityKey))

	l.mu.Lock()
	defer l.mu.Unlock()

	seat, ok := l.seatOf[key]
	if !ok {
		return errors.New("webui: this identity holds no seat")
	}
	l.seats[seat].RefundHeld = true
	l.seats[seat].Funded = true
	if err := l.money.RefundHeld(seat); err != nil {
		return err
	}
	if err := l.money.Committed(seat); err != nil {
		return err
	}

	// Deal only when the table is full and every seat has committed.
	if l.st != nil || len(l.seats) < l.terms.Seats {
		return nil
	}
	for _, s := range l.seats {
		if !s.Funded {
			return nil
		}
	}
	return l.dealLocked()
}

func (l *LiveTable) dealLocked() error {
	deck, err := cards.Shuffled()
	if err != nil {
		return fmt.Errorf("webui: shuffling: %w", err)
	}
	stacks := make([]int64, len(l.seats))
	for i := range stacks {
		stacks[i] = int64(l.terms.BuyInSatoshis)
	}

	st, err := engine.New(engine.Config{
		Stacks:     stacks,
		Button:     0,
		SmallBlind: int64(l.terms.SmallBlind),
		BigBlind:   int64(l.terms.BigBlind),
		Deck:       deck,
	})
	if err != nil {
		return fmt.Errorf("webui: starting the hand: %w", err)
	}
	l.st = st
	for i, s := range st.Seats {
		l.hole[i] = s.Hole
	}
	return nil
}

// Act applies a player's action.
//
// The identity is resolved to a seat here rather than taken from the request, so a client cannot
// act for another player by claiming their seat number. Legality is the engine's decision.
func (l *LiveTable) Act(identityKey, action string, to int64) error {
	key := strings.ToLower(strings.TrimSpace(identityKey))

	l.mu.Lock()
	defer l.mu.Unlock()

	seat, ok := l.seatOf[key]
	if !ok {
		return errors.New("webui: this identity holds no seat")
	}
	if l.st == nil {
		return errors.New("webui: the hand has not started")
	}
	if l.st.Done {
		return errors.New("webui: the hand is over")
	}
	if l.st.ToAct != seat {
		return fmt.Errorf("webui: it is seat %d's turn, not seat %d's", l.st.ToAct, seat)
	}

	kind, err := parseAction(action)
	if err != nil {
		return err
	}
	if err := l.st.Apply(engine.Action{Kind: kind, Seat: seat, To: to}); err != nil {
		// The engine's refusal is the authoritative one and already explains itself.
		return fmt.Errorf("webui: %w", err)
	}
	l.seats[seat].LastSeen = l.now()

	if l.st.Done {
		payouts := make(map[int]uint64, len(l.st.Payouts))
		for s, v := range l.st.Payouts {
			payouts[s] = uint64(v)
		}
		l.money.Settled("", payouts)
	}
	return nil
}

func parseAction(a string) (engine.ActionKind, error) {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "fold":
		return engine.Fold, nil
	case "check":
		return engine.Check, nil
	case "call":
		return engine.Call, nil
	case "bet":
		return engine.Bet, nil
	case "raise":
		return engine.Raise, nil
	default:
		return 0, fmt.Errorf("webui: %q is not an action", a)
	}
}

// Stall records that the hand cannot proceed, and who is responsible.
func (l *LiveTable) Stall(seat int, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason == "" {
		return errors.New("webui: a stall must have a reason a player can act on")
	}
	l.stalledSeat = seat
	l.stallReason = reason
	return l.money.Stalled(seat, reason)
}

// SeatOf resolves an identity to its seat, or -1.
func (l *LiveTable) SeatOf(identityKey string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seat, ok := l.seatOf[strings.ToLower(strings.TrimSpace(identityKey))]; ok {
		return seat
	}
	return -1
}

// LegalFor reports the actions available to a seat, so the UI can offer exactly those.
//
// Offering an action the engine will refuse is worse than offering none: a player clicks it, is
// told no, and learns nothing about why.
func (l *LiveTable) LegalFor(seat int) LegalView {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out LegalView
	if l.st == nil || l.st.Done || l.st.ToAct != seat {
		return out
	}
	lg := l.st.Legal()
	out.YourTurn = true
	out.CanFold = lg.CanFold
	out.CanCheck = lg.CanCheck
	out.CanCall = lg.CanCall
	out.CanBetRaise = lg.CanBetRaise
	out.CallAmount = lg.CallAmount
	out.MinTo = lg.MinTo
	out.MaxTo = lg.MaxTo
	return out
}

// LegalView is what a seat may do right now.
type LegalView struct {
	YourTurn    bool  `json:"yourTurn"`
	CanFold     bool  `json:"canFold"`
	CanCheck    bool  `json:"canCheck"`
	CanCall     bool  `json:"canCall"`
	CanBetRaise bool  `json:"canBetRaise"`
	CallAmount  int64 `json:"callAmount"`
	MinTo       int64 `json:"minTo"`
	MaxTo       int64 `json:"maxTo"`
}

// View renders the table for one seat. seat < 0 is an observer.
func (l *LiveTable) View(seat int) TableView {
	l.mu.Lock()
	defer l.mu.Unlock()

	phase := "waiting for players"
	switch {
	case l.stallReason != "":
		phase = "stalled"
	case l.st == nil:
		if len(l.seats) >= l.terms.Seats {
			phase = "waiting for buy-ins"
		}
	case l.st.Done:
		phase = "hand complete"
	default:
		phase = "in play"
	}

	v := TableView{
		TableID:          l.id,
		Phase:            phase,
		Seats:            l.terms.Seats,
		BuyInSatoshis:    l.terms.BuyInSatoshis,
		SmallBlind:       l.terms.SmallBlind,
		BigBlind:         l.terms.BigBlind,
		RefundLockHeight: l.terms.RefundLockHeight,
		StalledSeat:      l.stalledSeat,
		StallReason:      l.stallReason,
		ToAct:            -1,
		UpdatedAt:        l.now(),
	}
	if l.st != nil {
		v.Street = l.st.Street.String()
		v.Pot = l.st.Pot()
		v.ToAct = l.st.ToAct
		v.Board = BoardStrings(l.st.Board)
	}

	v.Players = FromEngine(l.st, l.seats, l.money)
	// Hole cards for the requesting seat only. Another seat's cards are never included, so
	// there is nothing for a browser to leak.
	if seat >= 0 {
		if h, ok := l.hole[seat]; ok {
			for i := range v.Players {
				if v.Players[i].Seat == seat {
					v.Players[i].Hole = cardStrings(h)
				}
			}
		}
	}
	return v
}

// Winners returns the payouts once the hand is complete.
func (l *LiveTable) Winners() map[int]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st == nil || !l.st.Done {
		return nil
	}
	out := make(map[int]int64, len(l.st.Payouts))
	for s, v := range l.st.Payouts {
		out[s] = v
	}
	return out
}

func cardStrings(cs []cards.Card) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}
