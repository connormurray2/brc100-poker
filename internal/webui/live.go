package webui

import (
	"context"
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

	// coord drives the dealerless deal across seats' agents. When nil the table falls back to
	// a local shuffle, which is only appropriate when no seat has an agent — a demonstration,
	// not a hand for value.
	coord *Coordinator
	// agentURL maps a seat's identity key to its agent, registered when the seat joins.
	agentURL map[string]string
	// dealerless records whether the completed deal ran through agents, so the UI can say so
	// rather than leaving a player to assume.
	dealerless bool

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
		agentURL:    make(map[string]string),
		money:       money,
		hole:        make(map[int][]cards.Card),
		stalledSeat: -1,
		now:         time.Now,
	}, nil
}

// SetCoordinator attaches the coordinator that runs the dealerless deal.
func (l *LiveTable) SetCoordinator(c *Coordinator) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.coord = c
}

// RegisterAgent records where a seat's agent can be reached.
//
// Without an agent a seat cannot hold its own deal secrets, so a table where any seat lacks one
// cannot deal without a dealer. That is reported rather than silently downgraded.
func (l *LiveTable) RegisterAgent(identityKey, url string) error {
	key := strings.ToLower(strings.TrimSpace(identityKey))
	if key == "" || url == "" {
		return errors.New("webui: an identity key and an agent URL are both required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seatOf[key]; !ok {
		return errors.New("webui: this identity holds no seat")
	}
	l.agentURL[key] = url
	return nil
}

// Dealerless reports whether the current hand was dealt through agents.
func (l *LiveTable) Dealerless() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dealerless
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
	stacks := make([]int64, len(l.seats))
	for i := range stacks {
		stacks[i] = int64(l.terms.BuyInSatoshis)
	}

	// Prefer a dealerless deal: every seat holds its own secrets and nothing here can read a
	// card. Only fall back to a local shuffle when a seat has no agent, and say so.
	deck, dealerless, hole, err := l.buildDeckLocked()
	if err != nil {
		return err
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
	l.dealerless = dealerless

	if dealerless {
		// The cards each seat read for itself. The engine was fed the same cards so its
		// evaluation matches, but these are the authoritative record of what each agent
		// proved it could read.
		for seat, cs := range hole {
			l.hole[seat] = cs
		}
	} else {
		for i, s := range st.Seats {
			l.hole[i] = s.Hole
		}
	}
	return nil
}

// buildDeckLocked produces the hand's cards, dealerlessly when every seat has an agent.
//
// The engine needs a concrete ordered deck, so a coordinated deal is flattened into one: each
// seat's own cards first in seat order, then the board, matching how the engine consumes them. The
// coordinator only ever learns a seat's cards because that seat's agent read them and reported
// them, which is the same trust a player extends by sitting down.
func (l *LiveTable) buildDeckLocked() ([]cards.Card, bool, map[int][]cards.Card, error) {
	endpoints := make([]AgentEndpoint, 0, len(l.seats))
	for _, s := range l.seats {
		url, ok := l.agentURL[s.IdentityKey]
		if !ok {
			// A seat without an agent cannot hold secrets, so no dealerless deal is
			// possible for this table.
			endpoints = nil
			break
		}
		endpoints = append(endpoints, AgentEndpoint{
			Seat: s.Index, IdentityKey: s.IdentityKey, URL: url,
		})
	}

	if l.coord == nil || len(endpoints) != len(l.seats) {
		deck, err := cards.Shuffled()
		if err != nil {
			return nil, false, nil, fmt.Errorf("webui: shuffling: %w", err)
		}
		return deck, false, nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dealt, err := l.coord.Deal(ctx, l.id, endpoints, 2)
	if err != nil {
		// A failed deal is not silently downgraded to a dealt one: a player who was told
		// the hand is dealerless must not get a dealer instead.
		return nil, false, nil, fmt.Errorf("webui: the dealerless deal failed: %w", err)
	}

	deck := make([]cards.Card, 0, len(l.seats)*2+5)
	for seat := 0; seat < len(l.seats); seat++ {
		deck = append(deck, dealt.Hole[seat]...)
	}
	deck = append(deck, dealt.Board...)
	return deck, true, dealt.Hole, nil
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
