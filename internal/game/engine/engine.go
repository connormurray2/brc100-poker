// Package engine runs a hand of Texas Hold'em as a deterministic state machine.
//
// The engine is the sole adjudicator of legality: an illegal or out-of-turn action is
// rejected and leaves state untouched. It is pure — the only external input is the agreed
// deck order, which in real play comes from the mental-poker protocol rather than any local
// shuffle.
//
// Determinism is a requirement, not a convenience: every seat replays the same action
// sequence independently and must reach byte-identical state, because that is what lets a
// seat verify a settlement against its own view of the hand before signing it.
package engine

import (
	"errors"
	"fmt"
	"sort"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/eval"
)

// Street is a betting round.
type Street int

const (
	Preflop Street = iota
	Flop
	Turn
	River
	Complete
)

func (s Street) String() string {
	switch s {
	case Preflop:
		return "preflop"
	case Flop:
		return "flop"
	case Turn:
		return "turn"
	case River:
		return "river"
	case Complete:
		return "complete"
	default:
		return fmt.Sprintf("street(%d)", int(s))
	}
}

// ActionKind is the type of a player action.
type ActionKind int

const (
	Fold ActionKind = iota
	Check
	Call
	Bet
	Raise
)

func (a ActionKind) String() string {
	switch a {
	case Fold:
		return "fold"
	case Check:
		return "check"
	case Call:
		return "call"
	case Bet:
		return "bet"
	case Raise:
		return "raise"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// Action is one player's action.
//
// For Bet and Raise, To is the total street commitment being targeted, not the increment.
// Targeting a total rather than a delta removes an ambiguity that otherwise has to be
// resolved identically by every independent replayer.
type Action struct {
	Kind ActionKind
	Seat int
	To   int64
}

// Table bounds. Two seats minimum for a hand; six is the slice's maximum.
const (
	MinSeats = 2
	MaxSeats = 6
)

// Seat is one player's state within a hand.
type Seat struct {
	Index int
	Stack int64
	Hole  []cards.Card

	Folded bool
	AllIn  bool

	// StreetCommit is chips committed on the current street.
	StreetCommit int64
	// TotalCommit is chips committed across the hand. Side pots layer on this.
	TotalCommit int64

	ActedThisStreet bool
}

// Legal describes what the seat to act may do.
type Legal struct {
	CanFold     bool
	CanCheck    bool
	CanCall     bool
	CanBetRaise bool

	// CallAmount is the chips needed to call, capped at the seat's stack.
	CallAmount int64
	// MinTo and MaxTo bound the total street commitment a bet or raise may target.
	MinTo int64
	MaxTo int64
}

// State is a hand in progress.
type State struct {
	Seats []*Seat
	Board []cards.Card

	Street Street
	Button int

	SmallBlind int64
	BigBlind   int64

	// CurrentBet is the highest street commitment to match.
	CurrentBet int64
	// MinRaise is the minimum raise increment.
	MinRaise int64

	// ToAct is the seat to act, or -1 when nobody may act.
	ToAct int

	Done bool

	// AwaitingBoard and AwaitingShowdown pause the hand for the deal protocol. In real
	// multiplayer nobody holds the deck, so the engine must stop and wait for seats to
	// disclose the scalars that reveal the next cards.
	AwaitingBoard     bool
	PendingBoardCount int
	AwaitingShowdown  bool

	// Payouts records chips won per seat, keyed by seat index.
	Payouts map[int]int64

	// UseHole constrains how many hole cards a hand must use. Zero means unconstrained,
	// as in Texas Hold'em.
	UseHole int

	deck          []cards.Card
	deckPos       int
	pendingStreet Street
	deferReveal   bool
}

// Config parameterises a new hand.
type Config struct {
	Stacks     []int64
	Button     int
	SmallBlind int64
	BigBlind   int64

	// Deck is the agreed deck order. In real play this comes from the mental-poker
	// protocol; only the positions the hand actually consumes are read.
	Deck []cards.Card

	// HoleCards per seat. Defaults to two.
	HoleCards int
	// UseHole constrains the number of hole cards a made hand must use.
	UseHole int

	// DeferReveal pauses at each board and at showdown so seats can disclose scalars.
	// Required for real multiplayer; off for local practice against a known deck.
	DeferReveal bool
}

// New starts a hand.
func New(cfg Config) (*State, error) {
	n := len(cfg.Stacks)
	if n < MinSeats {
		return nil, fmt.Errorf("engine: need at least %d seats, got %d", MinSeats, n)
	}
	if n > MaxSeats {
		return nil, fmt.Errorf("engine: at most %d seats, got %d", MaxSeats, n)
	}
	if cfg.Button < 0 || cfg.Button >= n {
		return nil, fmt.Errorf("engine: button %d out of range for %d seats", cfg.Button, n)
	}
	if cfg.BigBlind <= 0 || cfg.SmallBlind <= 0 {
		return nil, errors.New("engine: blinds must be positive")
	}
	if cfg.SmallBlind > cfg.BigBlind {
		return nil, errors.New("engine: small blind exceeds big blind")
	}
	for i, s := range cfg.Stacks {
		if s <= 0 {
			return nil, fmt.Errorf("engine: seat %d has a non-positive stack", i)
		}
	}

	holeCount := cfg.HoleCards
	if holeCount == 0 {
		holeCount = 2
	}
	// Hole cards plus the five-card board must fit in the supplied deck.
	if need := holeCount*n + 5; len(cfg.Deck) < need {
		return nil, fmt.Errorf("engine: deck has %d cards, need at least %d", len(cfg.Deck), need)
	}

	st := &State{
		Street:      Preflop,
		Button:      cfg.Button,
		SmallBlind:  cfg.SmallBlind,
		BigBlind:    cfg.BigBlind,
		Payouts:     map[int]int64{},
		UseHole:     cfg.UseHole,
		deck:        append([]cards.Card{}, cfg.Deck...),
		deferReveal: cfg.DeferReveal,
	}
	for i, stack := range cfg.Stacks {
		st.Seats = append(st.Seats, &Seat{Index: i, Stack: stack})
	}
	for _, s := range st.Seats {
		s.Hole = make([]cards.Card, holeCount)
		for k := 0; k < holeCount; k++ {
			s.Hole[k] = st.deck[st.deckPos]
			st.deckPos++
		}
	}

	// Heads-up is the special case: the button posts the small blind and acts first
	// pre-flop, then acts last on every later street.
	sbSeat, bbSeat := (cfg.Button+1)%n, (cfg.Button+2)%n
	if n == 2 {
		sbSeat, bbSeat = cfg.Button, (cfg.Button+1)%n
	}
	st.postBlind(sbSeat, cfg.SmallBlind)
	st.postBlind(bbSeat, cfg.BigBlind)

	st.CurrentBet = cfg.BigBlind
	st.MinRaise = cfg.BigBlind
	st.ToAct = st.nextAbleToAct(bbSeat)
	return st, nil
}

// Pot is the chips currently in the middle.
func (st *State) Pot() int64 {
	var total, paid int64
	for _, s := range st.Seats {
		total += s.TotalCommit
	}
	for _, v := range st.Payouts {
		paid += v
	}
	return total - paid
}

func (st *State) postBlind(seat int, amt int64) {
	s := st.Seats[seat]
	pay := min64(amt, s.Stack)
	s.Stack -= pay
	s.StreetCommit += pay
	s.TotalCommit += pay
	if s.Stack == 0 {
		s.AllIn = true
	}
}

// nextAbleToAct returns the next seat that can still act, or -1.
func (st *State) nextAbleToAct(from int) int {
	n := len(st.Seats)
	for k := 1; k <= n; k++ {
		i := (from + k) % n
		if s := st.Seats[i]; !s.Folded && !s.AllIn {
			return i
		}
	}
	return -1
}

func (st *State) liveCount() int {
	n := 0
	for _, s := range st.Seats {
		if !s.Folded {
			n++
		}
	}
	return n
}

func (st *State) ableToActCount() int {
	n := 0
	for _, s := range st.Seats {
		if !s.Folded && !s.AllIn {
			n++
		}
	}
	return n
}

// Legal returns the actions available to the seat to act.
func (st *State) Legal() Legal {
	var l Legal
	if st.Done || st.ToAct < 0 || st.AwaitingBoard || st.AwaitingShowdown {
		return l
	}
	s := st.Seats[st.ToAct]
	toCall := st.CurrentBet - s.StreetCommit

	l.CanFold = true
	l.CanCheck = toCall <= 0
	l.CanCall = toCall > 0 && s.Stack > 0
	l.CallAmount = min64(toCall, s.Stack)

	maxTo := s.StreetCommit + s.Stack
	minTo := st.CurrentBet + st.MinRaise
	if st.CurrentBet <= 0 {
		minTo = st.BigBlind
	}
	if maxTo > st.CurrentBet && s.Stack > 0 {
		l.CanBetRaise = true
		// A short all-in is always allowed even below the min raise, so a seat is
		// never forced to fold merely because its stack is small.
		l.MinTo = min64(minTo, maxTo)
		l.MaxTo = maxTo
	}
	return l
}

// Apply applies an action, or returns an error and leaves state unchanged.
func (st *State) Apply(a Action) error {
	if st.Done {
		return errors.New("engine: hand is complete")
	}
	if st.AwaitingBoard {
		return errors.New("engine: waiting for board cards to be revealed")
	}
	if st.AwaitingShowdown {
		return errors.New("engine: waiting for hole cards to be revealed")
	}
	if a.Seat != st.ToAct {
		return fmt.Errorf("engine: seat %d acted out of turn; seat %d is to act", a.Seat, st.ToAct)
	}

	s := st.Seats[st.ToAct]
	toCall := st.CurrentBet - s.StreetCommit
	l := st.Legal()

	switch a.Kind {
	case Fold:
		s.Folded = true

	case Check:
		if toCall > 0 {
			return fmt.Errorf("engine: cannot check facing a bet of %d", toCall)
		}

	case Call:
		if toCall <= 0 {
			return errors.New("engine: nothing to call")
		}
		st.commit(s, min64(toCall, s.Stack))

	case Bet, Raise:
		if !l.CanBetRaise {
			return errors.New("engine: cannot bet or raise")
		}
		allIn := a.To >= s.StreetCommit+s.Stack
		if !allIn && (a.To < l.MinTo || a.To > l.MaxTo) {
			return fmt.Errorf("engine: target %d outside the legal range [%d,%d]", a.To, l.MinTo, l.MaxTo)
		}
		add := min64(a.To, s.StreetCommit+s.Stack) - s.StreetCommit
		if add <= 0 {
			return errors.New("engine: a bet or raise must increase the commitment")
		}
		increment := (s.StreetCommit + add) - st.CurrentBet
		st.commit(s, add)
		if s.StreetCommit > st.CurrentBet {
			// Only a full raise resets the minimum; a short all-in does not.
			if increment >= st.MinRaise {
				st.MinRaise = increment
			}
			st.CurrentBet = s.StreetCommit
			// A raise reopens action for everyone who already acted.
			for _, o := range st.Seats {
				if o != s && !o.Folded && !o.AllIn {
					o.ActedThisStreet = false
				}
			}
		}

	default:
		return fmt.Errorf("engine: unknown action %v", a.Kind)
	}

	s.ActedThisStreet = true
	return st.advance()
}

func (st *State) commit(s *Seat, amt int64) {
	amt = min64(amt, s.Stack)
	s.Stack -= amt
	s.StreetCommit += amt
	s.TotalCommit += amt
	if s.Stack == 0 {
		s.AllIn = true
	}
}

func (st *State) advance() error {
	// Everyone else folded: the hand ends without further cards.
	if st.liveCount() == 1 {
		return st.settle()
	}

	closed := true
	for _, s := range st.Seats {
		if s.Folded || s.AllIn {
			continue
		}
		if !s.ActedThisStreet || s.StreetCommit != st.CurrentBet {
			closed = false
			break
		}
	}
	// With at most one seat able to act, the street closes once it has acted.
	if !closed && st.ableToActCount() <= 1 {
		allActed := true
		for _, s := range st.Seats {
			if !s.Folded && !s.AllIn && !s.ActedThisStreet {
				allActed = false
				break
			}
		}
		closed = allActed
	}

	if !closed {
		if next := st.nextAbleToAct(st.ToAct); next >= 0 {
			st.ToAct = next
			return nil
		}
	}
	return st.nextStreet()
}

func (st *State) nextStreet() error {
	if st.Street == River {
		return st.settle()
	}
	for _, s := range st.Seats {
		s.StreetCommit = 0
		s.ActedThisStreet = false
	}
	st.CurrentBet = 0
	st.MinRaise = st.BigBlind

	need := 1
	next := Turn
	switch st.Street {
	case Preflop:
		need, next = 3, Flop
	case Flop:
		next = Turn
	case Turn:
		next = River
	}

	if st.deferReveal {
		// Pause so seats disclose this street's scalars before anyone sees the cards.
		st.pendingStreet = next
		st.PendingBoardCount = need
		st.AwaitingBoard = true
		st.ToAct = -1
		return nil
	}
	for i := 0; i < need; i++ {
		st.Board = append(st.Board, st.deck[st.deckPos])
		st.deckPos++
	}
	st.Street = next
	return st.finishStreetTransition()
}

// SupplyBoard provides the revealed board cards for a deferred street.
func (st *State) SupplyBoard(revealed []cards.Card) error {
	if !st.AwaitingBoard {
		return errors.New("engine: not waiting for board cards")
	}
	if len(revealed) != st.PendingBoardCount {
		return fmt.Errorf("engine: got %d board cards, need %d", len(revealed), st.PendingBoardCount)
	}
	for _, c := range revealed {
		if !c.Valid() {
			return fmt.Errorf("engine: invalid board card %+v", c)
		}
	}
	st.Board = append(st.Board, revealed...)
	st.Street = st.pendingStreet
	st.AwaitingBoard = false
	st.PendingBoardCount = 0
	return st.finishStreetTransition()
}

func (st *State) finishStreetTransition() error {
	// With at most one seat able to act, remaining streets are dealt with no betting.
	if st.ableToActCount() <= 1 {
		if st.Street != River {
			return st.nextStreet()
		}
		return st.settle()
	}
	st.ToAct = st.nextAbleToAct(st.Button)
	if st.ToAct < 0 {
		return st.nextStreet()
	}
	return nil
}

// settle ends the hand. It returns an error only if scoring fails, which would mean the
// pot could not be awarded — a condition the caller must see rather than have swallowed.
func (st *State) settle() error {
	var live []*Seat
	for _, s := range st.Seats {
		if !s.Folded {
			live = append(live, s)
		}
	}

	// Uncontested: no hole cards needed, so this pays out even in deferred mode.
	if len(live) == 1 {
		var won int64
		for _, s := range st.Seats {
			won += s.TotalCommit
		}
		live[0].Stack += won
		st.Payouts[live[0].Index] = won
		st.Done = true
		st.Street = Complete
		st.ToAct = -1
		return nil
	}

	if st.deferReveal {
		// A real showdown: pause for hole-card reveals before scoring.
		st.AwaitingShowdown = true
		st.ToAct = -1
		return nil
	}
	st.Done = true
	st.Street = Complete
	st.ToAct = -1
	return st.scoreAndPay()
}

// SetRevealedHole records a seat's hole cards once revealed at showdown.
func (st *State) SetRevealedHole(seat int, hole []cards.Card) error {
	if seat < 0 || seat >= len(st.Seats) {
		return fmt.Errorf("engine: seat %d out of range", seat)
	}
	for _, c := range hole {
		if !c.Valid() {
			return fmt.Errorf("engine: invalid hole card %+v", c)
		}
	}
	st.Seats[seat].Hole = append([]cards.Card{}, hole...)
	return nil
}

// CompleteShowdown scores a deferred showdown once every live seat's hole cards are set.
func (st *State) CompleteShowdown() error {
	if !st.AwaitingShowdown {
		return errors.New("engine: not awaiting a showdown")
	}
	st.AwaitingShowdown = false
	st.Done = true
	st.Street = Complete
	st.ToAct = -1
	return st.scoreAndPay()
}

// scoreAndPay awards the pot, layering side pots by distinct total commitment.
//
// Chips are conserved by construction: each layer distributes exactly
// (layer size x eligible seats), and every layer is awarded to someone still live.
func (st *State) scoreAndPay() error {
	levelSet := map[int64]bool{}
	for _, s := range st.Seats {
		if s.TotalCommit > 0 {
			levelSet[s.TotalCommit] = true
		}
	}
	levels := make([]int64, 0, len(levelSet))
	for l := range levelSet {
		levels = append(levels, l)
	}
	sort.Slice(levels, func(i, j int) bool { return levels[i] < levels[j] })

	won := map[int]int64{}
	var prev int64
	for _, lvl := range levels {
		slice := lvl - prev
		prev = lvl

		var eligible, live []*Seat
		for _, s := range st.Seats {
			if s.TotalCommit >= lvl {
				eligible = append(eligible, s)
				if !s.Folded {
					live = append(live, s)
				}
			}
		}
		potAtLevel := slice * int64(len(eligible))
		if len(live) == 0 {
			// Everyone at this level folded. Award it to the best live hand from a
			// lower level so the chips are not lost.
			var fallback []*Seat
			for _, s := range st.Seats {
				if !s.Folded {
					fallback = append(fallback, s)
				}
			}
			if len(fallback) == 0 {
				return errors.New("engine: no live seat to award a pot layer to")
			}
			live = fallback
		}

		best := int64(-1)
		var winners []int
		for _, s := range live {
			r, err := eval.BestConstrained(s.Hole, st.Board, st.UseHole)
			if err != nil {
				return fmt.Errorf("engine: scoring seat %d: %w", s.Index, err)
			}
			switch {
			case r.Score > best:
				best = r.Score
				winners = []int{s.Index}
			case r.Score == best:
				winners = append(winners, s.Index)
			}
		}
		if len(winners) == 0 {
			return errors.New("engine: no winner for a pot layer")
		}
		sort.Ints(winners)

		each := potAtLevel / int64(len(winners))
		rem := potAtLevel - each*int64(len(winners))
		for _, w := range winners {
			won[w] += each
		}
		// The odd chip goes to the lowest seat index among the winners. Any rule works
		// so long as every independent replayer applies the same one.
		if rem > 0 {
			won[winners[0]] += rem
		}
	}

	for seat, amt := range won {
		st.Seats[seat].Stack += amt
		st.Payouts[seat] = amt
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
