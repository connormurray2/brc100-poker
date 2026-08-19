package table

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/engine"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
)

// DefaultActionTimeout bounds how long a table waits for a seat to act.
const DefaultActionTimeout = 30 * time.Second

// DefaultDealTimeout bounds how long it waits for a required deal or reveal message.
//
// Longer than the action timeout because a deal step involves curve arithmetic over the whole
// deck and a round trip to every seat.
const DefaultDealTimeout = 60 * time.Second

// HandPlayer is one seat's participation in a hand.
//
// Each seat runs its own HandPlayer: it holds that seat's mental-poker secrets, applies the
// engine independently, and never trusts another seat's claim about the game state. Two seats
// replaying the same messages must reach identical state, which is what lets a seat verify a
// settlement against its own view before signing it.
type HandPlayer struct {
	session *Session
	table   *Table
	seat    int
	seats   int

	// deckSize is the number of cards dealt from.
	deckSize int

	mu sync.Mutex
	// global is this seat's shuffle scalar, removed during remasking.
	global mentalpoker.Scalar
	// perPosition holds this seat's independent per-position secrets. These are the
	// values whose selective disclosure deals a card.
	perPosition []mentalpoker.Scalar
	// deck is the deck as this seat last saw it.
	deck mentalpoker.Deck
	// disclosed collects other seats' scalars per position, so a card can be recovered
	// once enough of them have arrived.
	disclosed map[int]map[int]mentalpoker.Scalar
	// hole records the positions dealt to each seat.
	//
	// The betting engine and the board layout are deliberately NOT held here: the caller
	// owns them, so a seat's view of the money is assembled from cards it proved it can
	// read rather than from state this type quietly accumulated.
	hole map[int][]int
}

// HandConfig parameterises a HandPlayer.
type HandConfig struct {
	Session  *Session
	Table    *Table
	Seat     int
	Seats    int
	DeckSize int
}

// NewHandPlayer builds one seat's hand participation and generates its secrets.
func NewHandPlayer(cfg HandConfig) (*HandPlayer, error) {
	if cfg.Session == nil {
		return nil, errors.New("table: a session is required")
	}
	if cfg.Table == nil {
		return nil, errors.New("table: a table is required")
	}
	if cfg.Seats < MinSeats || cfg.Seats > MaxSeats {
		return nil, fmt.Errorf("table: seats must be %d..%d, got %d", MinSeats, MaxSeats, cfg.Seats)
	}
	if cfg.Seat < 0 || cfg.Seat >= cfg.Seats {
		return nil, fmt.Errorf("table: seat %d is outside 0..%d", cfg.Seat, cfg.Seats-1)
	}
	deckSize := cfg.DeckSize
	if deckSize == 0 {
		deckSize = cards.DeckSize
	}

	global, err := mentalpoker.NewScalar()
	if err != nil {
		return nil, fmt.Errorf("table: generating the shuffle scalar: %w", err)
	}
	perPosition, err := mentalpoker.NewScalars(deckSize)
	if err != nil {
		return nil, fmt.Errorf("table: generating per-position scalars: %w", err)
	}

	return &HandPlayer{
		session:     cfg.Session,
		table:       cfg.Table,
		seat:        cfg.Seat,
		seats:       cfg.Seats,
		deckSize:    deckSize,
		global:      global,
		perPosition: perPosition,
		disclosed:   make(map[int]map[int]mentalpoker.Scalar),
		hole:        make(map[int][]int),
	}, nil
}

// HolePositions assigns deck positions to seats and the board.
//
// A fixed, public assignment: which position belongs to which seat is not secret, only what
// card sits there. Deriving it deterministically means every seat agrees without negotiation.
func HolePositions(seats, holeCards int) (hole map[int][]int, board []int) {
	hole = make(map[int][]int, seats)
	next := 0
	for s := 0; s < seats; s++ {
		for c := 0; c < holeCards; c++ {
			hole[s] = append(hole[s], next)
			next++
		}
	}
	// Five community cards follow the hole cards.
	for i := 0; i < 5; i++ {
		board = append(board, next)
		next++
	}
	return hole, board
}

// StartShuffle begins the deal from the public base deck.
//
// Only seat 0 starts; every other seat contributes when it receives the previous seat's
// output, which is what makes the shuffle sequential and gives each seat a turn to hide the
// order from the others.
func (h *HandPlayer) StartShuffle(ctx context.Context) error {
	if h.seat != 0 {
		return fmt.Errorf("table: only seat 0 starts the shuffle, not seat %d", h.seat)
	}
	base, err := mentalpoker.BaseDeck(h.deckSize)
	if err != nil {
		return fmt.Errorf("table: building the base deck: %w", err)
	}
	return h.contributeShuffle(ctx, base)
}

// ApplyShuffle applies another seat's shuffle output and contributes this seat's own.
func (h *HandPlayer) ApplyShuffle(ctx context.Context, deck mentalpoker.Deck, fromSeat int) error {
	// Contribute only when it is this seat's turn: the shuffle is a chain, and applying
	// out of turn would produce a deck the other seats cannot reproduce.
	if fromSeat != h.seat-1 {
		return nil
	}
	return h.contributeShuffle(ctx, deck)
}

func (h *HandPlayer) contributeShuffle(ctx context.Context, in mentalpoker.Deck) error {
	perm, err := mentalpoker.NewPermutation(h.deckSize)
	if err != nil {
		return fmt.Errorf("table: generating a permutation: %w", err)
	}

	h.mu.Lock()
	out, err := in.ShuffleStep(h.global, perm)
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("table: applying seat %d's shuffle: %w", h.seat, err)
	}

	h.mu.Lock()
	h.deck = out
	h.mu.Unlock()

	return h.session.Send(ctx, KindShuffle, ShuffleBody{Deck: encodeDeck(out)})
}

// ApplyRemask applies another seat's remask output and contributes this seat's own.
func (h *HandPlayer) ApplyRemask(ctx context.Context, deck mentalpoker.Deck, fromSeat int) error {
	if fromSeat != h.seat-1 {
		return nil
	}
	return h.contributeRemask(ctx, deck)
}

// StartRemask begins the remasking pass, which seat 0 does once shuffling completes.
func (h *HandPlayer) StartRemask(ctx context.Context) error {
	if h.seat != 0 {
		return fmt.Errorf("table: only seat 0 starts remasking, not seat %d", h.seat)
	}
	h.mu.Lock()
	deck := h.deck
	h.mu.Unlock()
	if deck.Size() == 0 {
		return errors.New("table: cannot remask before the deck has been shuffled")
	}
	return h.contributeRemask(ctx, deck)
}

func (h *HandPlayer) contributeRemask(ctx context.Context, in mentalpoker.Deck) error {
	h.mu.Lock()
	out, err := in.RemaskStep(h.global, h.perPosition)
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("table: applying seat %d's remask: %w", h.seat, err)
	}

	h.mu.Lock()
	h.deck = out
	h.mu.Unlock()

	return h.session.Send(ctx, KindRemask, RemaskBody{Deck: encodeDeck(out)})
}

// SetDeck records the final deck, once every seat has remasked.
func (h *HandPlayer) SetDeck(deck mentalpoker.Deck) error {
	if deck.Size() != h.deckSize {
		return fmt.Errorf("table: final deck has %d positions, want %d", deck.Size(), h.deckSize)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deck = deck
	return nil
}

// RevealHoleTo discloses this seat's scalars for another seat's hole positions.
//
// Called once per opponent. Disclosing only the recipient's positions is what keeps every
// other position secret: a per-position secret can be given away without giving away the deck.
func (h *HandPlayer) RevealHoleTo(ctx context.Context, recipientSeat int, recipientKey string, positions []int) error {
	if recipientSeat == h.seat {
		return fmt.Errorf("table: seat %d does not reveal its own hole cards to itself", h.seat)
	}
	h.mu.Lock()
	scalars := make([][]byte, 0, len(positions))
	for _, p := range positions {
		if p < 0 || p >= len(h.perPosition) {
			h.mu.Unlock()
			return fmt.Errorf("table: position %d is outside the deck", p)
		}
		scalars = append(scalars, h.perPosition[p].Bytes())
	}
	h.mu.Unlock()

	return h.session.SendPrivate(ctx, KindHoleReveal, recipientKey,
		RevealBody{Positions: positions, Scalars: scalars})
}

// RevealBoard discloses this seat's scalars for board positions to everyone.
func (h *HandPlayer) RevealBoard(ctx context.Context, positions []int) error {
	h.mu.Lock()
	scalars := make([][]byte, 0, len(positions))
	for _, p := range positions {
		if p < 0 || p >= len(h.perPosition) {
			h.mu.Unlock()
			return fmt.Errorf("table: position %d is outside the deck", p)
		}
		scalars = append(scalars, h.perPosition[p].Bytes())
	}
	h.mu.Unlock()

	return h.session.Send(ctx, KindBoardReveal,
		RevealBody{Positions: positions, Scalars: scalars})
}

// RecordDisclosure stores a scalar another seat disclosed for a position.
func (h *HandPlayer) RecordDisclosure(fromSeat int, positions []int, scalars [][]byte) error {
	if len(positions) != len(scalars) {
		return fmt.Errorf("table: %d positions and %d scalars", len(positions), len(scalars))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, p := range positions {
		s, err := mentalpoker.ScalarFromBytes(scalars[i])
		if err != nil {
			return fmt.Errorf("table: seat %d disclosed an invalid scalar for position %d: %w", fromSeat, p, err)
		}
		if h.disclosed[p] == nil {
			h.disclosed[p] = make(map[int]mentalpoker.Scalar)
		}
		h.disclosed[p][fromSeat] = s
	}
	return nil
}

// Card recovers the card at a deck position, if enough scalars are known.
//
// Returns an error when a required scalar is missing. That is the privacy property working,
// not a failure: a seat that cannot recover a card is a seat that was not meant to.
func (h *HandPlayer) Card(position int) (cards.Card, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.deck.Size() == 0 {
		return cards.Card{}, errors.New("table: no deck yet")
	}

	// Every seat's scalar is needed: this seat's own, plus each disclosure.
	scalars := make([]mentalpoker.Scalar, 0, h.seats)
	if position < 0 || position >= len(h.perPosition) {
		return cards.Card{}, fmt.Errorf("table: position %d is outside the deck", position)
	}
	scalars = append(scalars, h.perPosition[position])
	for seat := 0; seat < h.seats; seat++ {
		if seat == h.seat {
			continue
		}
		s, ok := h.disclosed[position][seat]
		if !ok {
			return cards.Card{}, fmt.Errorf("table: seat %d has not disclosed position %d", seat, position)
		}
		scalars = append(scalars, s)
	}

	pt, err := h.deck.Unmask(position, scalars)
	if err != nil {
		return cards.Card{}, err
	}
	idx, err := mentalpoker.CardIndexOf(pt, h.deckSize)
	if err != nil {
		return cards.Card{}, err
	}
	return cards.FromIndex(idx)
}

// Deck returns the deck as this seat last saw it.
func (h *HandPlayer) Deck() mentalpoker.Deck {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deck
}

// Seat returns this player's seat index.
func (h *HandPlayer) Seat() int { return h.seat }

// DefaultAction is what a table applies for a seat that does not act in time.
//
// Check when facing no bet, fold when facing one: the choice that costs an absent player the
// least while still letting the hand proceed.
func DefaultAction(facingBet bool) engine.Action {
	if facingBet {
		return engine.Action{Kind: engine.Fold}
	}
	return engine.Action{Kind: engine.Check}
}

func encodeDeck(d mentalpoker.Deck) [][]byte {
	pts := d.Points()
	out := make([][]byte, len(pts))
	for i, p := range pts {
		out[i] = p.Bytes()
	}
	return out
}

// DecodeDeck parses a deck from the wire, validating every point.
func DecodeDeck(raw [][]byte) (mentalpoker.Deck, error) {
	pts := make([]mentalpoker.Point, 0, len(raw))
	for i, b := range raw {
		p, err := mentalpoker.PointFromBytes(b)
		if err != nil {
			return mentalpoker.Deck{}, fmt.Errorf("table: deck position %d: %w", i, err)
		}
		pts = append(pts, p)
	}
	return mentalpoker.DeckFromPoints(pts)
}
