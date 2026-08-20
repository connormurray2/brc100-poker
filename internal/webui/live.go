package webui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// handNo counts hands played this session, so each deal gets a distinct hand ID. Reusing
	// one would let a wallet's replay cache reject the second hand's requests.
	handNo int
	// button is the dealer position, rotated between hands so the blinds move.
	button int
	// stacks carries chips between hands. A session is a sequence of hands with the same
	// money, not a series of independent buy-ins.
	stacks []int64
	// sitting out records seats that asked to leave; they are dealt out and the table stops
	// when too few remain.
	sittingOut map[int]bool

	// pots puts real value behind a hand. Nil means the table plays for chips, which the view
	// reports rather than leaving a player to assume value is at stake.
	pots *PotManager
	// settlementTxID is the broadcast settlement of the last completed hand, if any.
	settlementTxID string
	// readyToStart means every seat has committed and a value hand is waiting on its pot.
	readyToStart bool
	// openPotHand is the hand ID the currently funded pot belongs to. Held rather than
	// recomputed, because handNo advances the moment a hand ends and handIDLocked would then
	// name the next hand, not the one whose pot is still open.
	openPotHand string

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

// SetPotManager attaches the manager that funds pots and settles on chain.
func (l *LiveTable) SetPotManager(m *PotManager) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pots = m
}

// ForValue reports whether hands at this table move real coins.
func (l *LiveTable) ForValue() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pots.Enabled()
}

// SettlementTxID returns the last completed hand's settlement, if it was settled on chain.
func (l *LiveTable) SettlementTxID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.settlementTxID
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
	// A value table funds its pot first, and that cannot happen here: it broadcasts a
	// transaction and collects a refund signature from every seat, which is a round trip per
	// seat and would block this lock for seconds. RunHands starts the hand instead.
	if l.pots.Enabled() {
		l.readyToStart = true
		return nil
	}
	return l.dealLocked()
}

// PendingStart reports that every seat has committed and a value hand is waiting to be funded.
func (l *LiveTable) PendingStart() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readyToStart && l.st == nil
}

// StartFundedHand funds the pot and deals the first hand of a session.
func (l *LiveTable) StartFundedHand(ctx context.Context, height uint32) error {
	if err := l.OpenPotForHand(ctx, height); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readyToStart = false
	if l.st != nil {
		return nil
	}
	return l.dealLocked()
}

// handIDLocked is the identifier for the hand about to be played.
//
// It must change every hand. A seat's wallet caches its deal secrets per hand ID and deliberately
// returns the existing ones if asked to commit twice -- that is what stops a seat re-rolling its
// secrets after seeing another seat's contribution. Passing a constant therefore does not merely
// look wrong, it deals the identical hand forever.
func (l *LiveTable) handIDLocked() string {
	return fmt.Sprintf("%s-h%d", l.id, l.handNo)
}

func (l *LiveTable) dealLocked() error {
	// Stacks carry between hands. The first hand starts everyone at the buy-in; later hands
	// inherit whatever the previous one left, which is what makes this a session rather than a
	// sequence of unrelated hands.
	if l.stacks == nil {
		l.stacks = make([]int64, len(l.seats))
		for i := range l.stacks {
			l.stacks[i] = int64(l.terms.BuyInSatoshis)
		}
	}
	stacks := make([]int64, len(l.stacks))
	copy(stacks, l.stacks)

	// Prefer a dealerless deal: every seat holds its own secrets and nothing here can read a
	// card. Only fall back to a local shuffle when a seat has no agent, and say so.
	deck, dealerless, hole, err := l.buildDeckLocked()
	if err != nil {
		return err
	}

	st, err := engine.New(engine.Config{
		Stacks:     stacks,
		Button:     l.button,
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
// UPGRADE PATH — read docs/wallet-native-deal.md before changing this.
//
// A dealerless deal here still depends on masking scalars held outside the player's BRC-100 wallet,
// because stripping a mask needs multiplication by the modular inverse of a derived key and no
// BRC-100 method exposes that. Masking alone is already possible over the interface; stripping is
// the gap. Tracked upstream in bsv-blockchain/ts-stack#488 and bsv-blockchain/BRCs#230.
//
// When a wallet ships the capability, this does NOT need restructuring: feature-detect
// multiplyPoint and route mask/strip through the wallet, keeping this path as the fallback for
// wallets that lack it. The algebra is identical either way.
//
// The engine needs a concrete ordered deck, so a coordinated deal is flattened into one: each
// seat's own cards first in seat order, then the board, matching how the engine consumes them. The
// coordinator only ever learns a seat's cards because that seat's agent read them and reported
// them, which is the same trust a player extends by sitting down.
func (l *LiveTable) buildDeckLocked() ([]cards.Card, bool, map[int][]cards.Card, error) {
	// A seat without a registered wallet cannot hold secrets, so no dealerless deal is possible.
	endpoints := l.endpointsLocked()

	if l.coord == nil || len(endpoints) != len(l.seats) {
		deck, err := cards.Shuffled()
		if err != nil {
			return nil, false, nil, fmt.Errorf("webui: shuffling: %w", err)
		}
		return deck, false, nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dealt, err := l.coord.Deal(ctx, l.handIDLocked(), endpoints, 2)
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
		// The txid is filled in by SettleOnChain once every seat has signed. Recording an
		// empty one here would tell a player the hand settled on chain when it has not.
		l.money.Settled(l.settlementTxID, payouts)
		l.finishHandLocked()
	}
	return nil
}

// finishHandLocked records the result of a completed hand against the session.
//
// It does not deal the next one. Dealing runs a full round trip to every seat's wallet, which can
// take seconds, and doing that while holding the table lock inside an action handler would block
// every other player's poll. NextHand does it, driven from outside the lock.
func (l *LiveTable) finishHandLocked() {
	// Carry the resulting stacks forward. The engine's seat stacks already reflect the payouts.
	if l.stacks == nil || len(l.stacks) != len(l.st.Seats) {
		l.stacks = make([]int64, len(l.st.Seats))
	}
	for i, s := range l.st.Seats {
		l.stacks[i] = s.Stack
	}
	l.handNo++
	// Move the button so the blinds rotate; a fixed button would tax the same seat every hand.
	if n := len(l.seats); n > 0 {
		l.button = (l.button + 1) % n
	}
}

// HandOver reports whether a hand has finished and the table is ready to deal another.
func (l *LiveTable) HandOver() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st != nil && l.st.Done
}

// SitOut takes a seat out of the game. That seat is dealt out of subsequent hands.
//
// A player leaving is the only thing that stops a session, so this is how a table ends.
func (l *LiveTable) SitOut(identityKey string) error {
	key := strings.ToLower(strings.TrimSpace(identityKey))
	l.mu.Lock()
	defer l.mu.Unlock()
	seat, ok := l.seatOf[key]
	if !ok {
		return errors.New("webui: this identity holds no seat")
	}
	if l.sittingOut == nil {
		l.sittingOut = make(map[int]bool)
	}
	l.sittingOut[seat] = true
	return nil
}

// SittingOut reports whether a seat has left the game.
func (l *LiveTable) SittingOut(seat int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sittingOut[seat]
}

// NextHand deals the following hand of a session.
//
// Refuses unless the previous hand is genuinely finished, so a caller cannot cut a hand short by
// asking for another. Returns false when the table should stop: a seat has left, or a seat has no
// chips and cannot post a blind.
func (l *LiveTable) NextHand() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.st == nil || !l.st.Done {
		return false, nil
	}
	for seat := range l.sittingOut {
		if l.sittingOut[seat] {
			return false, nil
		}
	}
	// A seat that cannot cover the big blind cannot be dealt in, and a heads-up table with one
	// such seat has nothing left to play for. Stopping is the honest outcome; silently dealing a
	// hand nobody can bet in is not.
	for _, stack := range l.stacks {
		if stack < int64(l.terms.BigBlind) {
			return false, nil
		}
	}

	// A fresh hand ID per hand. Wallets cache nonces against replay, and a repeated hand ID
	// would make the second hand's deal requests look like replays of the first.
	l.money.SetHand(l.handIDLocked())
	l.hole = make(map[int][]cards.Card, len(l.seats))
	l.st = nil
	if err := l.dealLocked(); err != nil {
		return false, err
	}
	return true, nil
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
		ForValue:         l.pots.Enabled(),
		SettlementTxID:   l.settlementTxID,
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

// RunHands deals hand after hand until the table stops.
//
// Started once when a session begins. It waits for the current hand to finish, pauses so players
// can read the result, then deals the next. A dealing failure stops the session rather than
// retrying: a deal that cannot complete usually means a seat's wallet has gone, and spinning on it
// would bury the reason under repeated identical errors.
func (l *LiveTable) RunHands(ctx context.Context, settle time.Duration, heightFn func(context.Context) (uint32, error), log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		// A value table's first hand waits here for its pot: every seat has committed, and
		// funding plus refunds must complete before any cards exist.
		if l.PendingStart() {
			height, err := heightFn(ctx)
			if err != nil {
				if log != nil {
					log.Error("cannot read the chain height to fund a pot", "error", err)
				}
				l.recordStall("the table could not read the chain height: " + err.Error())
				return
			}
			if err := l.StartFundedHand(ctx, height); err != nil {
				if log != nil {
					log.Error("the pot could not be funded; the session stops here", "error", err)
				}
				// Tell the players why. A table that simply stops looks identical to one
				// waiting for someone, and a player cannot act on a spinner.
				l.recordStall(err.Error())
				return
			}
			if log != nil {
				log.Info("hand started with a funded pot", "table", l.id)
			}
			continue
		}

		if !l.HandOver() {
			continue
		}
		// Let players see the showdown before the table moves on.
		select {
		case <-ctx.Done():
			return
		case <-time.After(settle):
		}
		// Settle the hand that just finished before dealing another. Doing it in this order
		// means a pot is never left open while a new hand's pot is funded, which would leave
		// two live pots and no way for a player to tell which refund protects which stake.
		if l.ForValue() {
			// Seats arm from their browsers, so allow time for that before treating a
			// missing expectation as a failure. Bounded, because a player who closed their
			// tab would otherwise hold the table open indefinitely.
			var txid string
			var err error
			deadline := time.Now().Add(45 * time.Second)
			for {
				txid, err = l.SettleOnChain(ctx)
				if !errors.Is(err, ErrAwaitingSeats) {
					break
				}
				if time.Now().After(deadline) {
					err = errors.New("webui: a seat never recorded its stake; its browser " +
						"may have been closed. Every seat still holds a refund")
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			if err != nil {
				if log != nil {
					log.Error("the hand could not be settled on chain; the session stops here",
						"error", err)
				}
				l.recordStall(err.Error())
				return
			}
			if txid != "" && log != nil {
				log.Info("hand settled on chain", "txid", txid)
			}
		}

		// For a value table the next hand needs its own pot, so clear the finished hand and
		// go back through the funded start path.
		if l.ForValue() {
			if ok, err := l.PrepareNextHand(); err != nil || !ok {
				if err != nil && log != nil {
					log.Error("the next hand could not be prepared", "error", err)
				}
				if log != nil && err == nil {
					log.Info("the session ended: a seat left or is out of chips")
				}
				return
			}
			continue
		}

		dealt, err := l.NextHand()
		if err != nil {
			if log != nil {
				log.Error("the next hand could not be dealt; the session stops here", "error", err)
			}
			return
		}
		if !dealt {
			if log != nil {
				log.Info("the session ended: a seat left or is out of chips")
			}
			return
		}
		if log != nil {
			log.Info("dealt the next hand", "table", l.id)
		}
	}
}

// ErrAwaitingSeats means a settlement is ready but not every seat has recorded its expectation.
//
// A distinct error rather than a silent nil: returning nil made the caller treat an unsettled hand
// as a settled one, so the session stopped both settling and dealing without saying why.
var ErrAwaitingSeats = errors.New("webui: waiting for every seat to record its stake")

// SettleOnChain spends the pot to the winners of the completed hand.
//
// Driven from outside the table lock: it collects a signature from every seat, which is a round
// trip to each player's wallet and can take seconds. Holding the lock across that would freeze
// every other player's poll.
//
// A refusing seat leaves the pot unspent and every seat still holding its refund. That is the
// designed outcome of a non-custodial pot, not a loss, and it is reported as a stall.
func (l *LiveTable) SettleOnChain(ctx context.Context) (string, error) {
	l.mu.Lock()
	if l.pots == nil || !l.pots.Enabled() || l.st == nil || !l.st.Done || l.settlementTxID != "" {
		l.mu.Unlock()
		return "", nil
	}
	if !l.pots.AllArmed(l.openPotHand, l.endpointsLocked()) {
		// Every seat must have recorded its expectation before any is asked to sign, or the
		// unarmed ones decline and the hand reads as stalled. Distinguished from a settled
		// hand by ErrAwaitingSeats, so the caller waits rather than moving on.
		l.mu.Unlock()
		return "", ErrAwaitingSeats
	}
	if l.openPotHand == "" {
		// No pot to settle. Returning silently would leave the money unaccounted for, so say
		// so rather than let the session continue as though it had paid out.
		l.mu.Unlock()
		return "", errors.New("webui: the hand finished but no pot is open for it")
	}
	handID := l.openPotHand
	payouts := make(map[int]uint64, len(l.st.Payouts))
	for seat, v := range l.st.Payouts {
		if v > 0 {
			payouts[seat] = uint64(v)
		}
	}
	seats := l.endpointsLocked()
	l.mu.Unlock()

	if len(payouts) == 0 || len(seats) == 0 {
		return "", nil
	}

	txid, err := l.pots.Settle(ctx, handID, seats, payouts)
	if err != nil {
		l.mu.Lock()
		l.stallReason = err.Error()
		l.mu.Unlock()
		return "", err
	}

	l.mu.Lock()
	l.settlementTxID = txid
	l.openPotHand = ""
	l.money.Settled(txid, payouts)
	l.mu.Unlock()
	return txid, nil
}

// endpointsLocked lists the seats as the coordinator addresses them.
func (l *LiveTable) endpointsLocked() []AgentEndpoint {
	out := make([]AgentEndpoint, 0, len(l.seats))
	for _, s := range l.seats {
		url, ok := l.agentURL[s.IdentityKey]
		if !ok {
			return nil
		}
		out = append(out, AgentEndpoint{Seat: s.Index, IdentityKey: s.IdentityKey, URL: url})
	}
	return out
}

// OpenPotForHand funds the pot and distributes refunds before a hand is dealt.
//
// Runs outside the table lock: funding broadcasts a transaction and every refund needs a
// signature from every seat, which is a round trip per seat.
//
// The ordering is the safety property. No seat's stake is treated as committed until that seat
// holds a fully-signed refund, so a player whose opponent disappears waits for a locktime rather
// than losing their money.
func (l *LiveTable) OpenPotForHand(ctx context.Context, height uint32) error {
	l.mu.Lock()
	if l.pots == nil || !l.pots.Enabled() {
		l.mu.Unlock()
		return nil // a chips-only table; the view says so
	}
	handID := l.handIDLocked()
	seats := l.endpointsLocked()
	pot := l.terms.BuyInSatoshis * uint64(len(l.seats))
	l.mu.Unlock()

	if len(seats) < 2 {
		return errors.New("webui: a pot needs every seat to have a reachable wallet")
	}

	if _, err := l.pots.OpenPot(ctx, handID, seats, pot, height); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.openPotHand = handID
	l.money.SetHeight(height)
	for _, s := range seats {
		// Refunds exist for every seat before any stake is called committed.
		if err := l.money.RefundHeld(s.Seat); err != nil {
			return err
		}
	}
	for _, s := range seats {
		if err := l.money.Committed(s.Seat); err != nil {
			return err
		}
	}
	l.settlementTxID = ""
	return nil
}

// PrepareNextHand advances a value table to the next hand, leaving it waiting for a pot.
//
// Returns false when the session should stop, matching NextHand's contract.
func (l *LiveTable) PrepareNextHand() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.st == nil || !l.st.Done {
		return false, nil
	}
	for seat := range l.sittingOut {
		if l.sittingOut[seat] {
			return false, nil
		}
	}
	for _, stack := range l.stacks {
		if stack < int64(l.terms.BigBlind) {
			return false, nil
		}
	}
	l.money.SetHand(l.handIDLocked())
	l.hole = make(map[int][]cards.Card, len(l.seats))
	l.st = nil
	l.readyToStart = true
	return true, nil
}

// StakeForSeat describes the open pot for one seat, so its client can arm its own wallet.
//
// Returns the amounts and derivation material only. The wallet derives the scripts, which is why a
// table cannot use this to make a seat expect the wrong payout.
func (l *LiveTable) StakeForSeat(seat int) (StakeInfo, bool) {
	l.mu.Lock()
	if l.pots == nil || !l.pots.Enabled() || l.st == nil {
		l.mu.Unlock()
		return StakeInfo{}, false
	}
	handID := l.openPotHand
	seats := l.endpointsLocked()
	// The expected payouts are only known once the hand is decided; before that a seat records
	// the pot and the fee bound, which is what its refund protects.
	payouts := make(map[int]uint64)
	if l.st.Done {
		for s, v := range l.st.Payouts {
			if v > 0 {
				payouts[s] = uint64(v)
			}
		}
	}
	l.mu.Unlock()
	return l.pots.StakeFor(handID, seat, seats, payouts)
}

// recordStall makes a stopped session visible to the players.
func (l *LiveTable) recordStall(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stallReason = reason
	l.readyToStart = false
}

// MarkSeatArmed records that a seat's wallet now holds its expectation for the open hand.
func (l *LiveTable) MarkSeatArmed(seat int) {
	l.mu.Lock()
	hand := l.openPotHand
	pots := l.pots
	l.mu.Unlock()
	if pots != nil && hand != "" {
		pots.MarkArmed(hand, seat)
	}
}
