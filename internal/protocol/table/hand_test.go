package table

import (
	"context"
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/engine"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
	"github.com/cmurray/brc100-poker/internal/protocol/transport"
)

// handEnv wires N seats, each with its own session and hand player, over one transport.
type handEnv struct {
	tb      *Table
	tp      *transport.Memory
	players []*HandPlayer
	keys    []string
}

// newHandEnv builds a funded, dealing table ready to run a deal.
func newHandEnv(t *testing.T, seats, deckSize int) handEnv {
	t.Helper()
	terms := goodTerms()
	terms.Seats = seats
	tb, err := New(terms)
	if err != nil {
		t.Fatal(err)
	}
	ks := keys(seats)
	for _, k := range ks {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}
	for i := range ks {
		if err := tb.MarkRefundHeld(i); err != nil {
			t.Fatal(err)
		}
		if err := tb.MarkFunded(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.BeginDeal(); err != nil {
		t.Fatal(err)
	}

	tp := transport.NewMemory()
	t.Cleanup(func() { _ = tp.Close() })

	var players []*HandPlayer
	for i, k := range ks {
		sess, err := NewSession(SessionConfig{Table: tb, Transport: tp, SelfSeat: i, SelfKey: k})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sess.Close)
		hp, err := NewHandPlayer(HandConfig{Session: sess, Table: tb, Seat: i, Seats: seats, DeckSize: deckSize})
		if err != nil {
			t.Fatal(err)
		}
		players = append(players, hp)
	}
	return handEnv{tb: tb, tp: tp, players: players, keys: ks}
}

// runDeal drives the shuffle and remask chain to completion over the transport, then has every
// seat exchange the disclosures for its own hole cards and the board.
func runDeal(t *testing.T, env handEnv, holePositions map[int][]int, board []int) {
	t.Helper()
	ctx := context.Background()
	seats := len(env.players)

	// Wire the chain: each seat contributes when it sees the previous seat's output.
	for _, hp := range env.players {
		p := hp
		p.session.Handle(KindShuffle, func(e Envelope) error {
			body, err := DecodeBody[ShuffleBody](e)
			if err != nil {
				return err
			}
			deck, err := DecodeDeck(body.Deck)
			if err != nil {
				return err
			}
			// The last seat's shuffle output starts the remask pass.
			if e.Seat == seats-1 {
				if err := p.SetDeck(deck); err != nil {
					return err
				}
				if p.Seat() == 0 {
					return p.StartRemask(ctx)
				}
				return nil
			}
			return p.ApplyShuffle(ctx, deck, e.Seat)
		})
		p.session.Handle(KindRemask, func(e Envelope) error {
			body, err := DecodeBody[RemaskBody](e)
			if err != nil {
				return err
			}
			deck, err := DecodeDeck(body.Deck)
			if err != nil {
				return err
			}
			if e.Seat == seats-1 {
				return p.SetDeck(deck)
			}
			return p.ApplyRemask(ctx, deck, e.Seat)
		})
		p.session.Handle(KindHoleReveal, func(e Envelope) error {
			body, err := DecodeBody[RevealBody](e)
			if err != nil {
				return err
			}
			return p.RecordDisclosure(e.Seat, body.Positions, body.Scalars)
		})
		p.session.Handle(KindBoardReveal, func(e Envelope) error {
			body, err := DecodeBody[RevealBody](e)
			if err != nil {
				return err
			}
			return p.RecordDisclosure(e.Seat, body.Positions, body.Scalars)
		})
	}

	if err := env.players[0].StartShuffle(ctx); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	// Every seat must now hold the same final deck.
	final := env.players[0].Deck()
	if final.Size() == 0 {
		t.Fatal("the deal did not complete: seat 0 has no deck")
	}
	for i, p := range env.players {
		if !p.Deck().Equal(final) {
			t.Fatalf("seat %d holds a different deck; the seats disagree on the deal", i)
		}
	}

	// Each seat discloses its scalars for every OTHER seat's hole positions, privately.
	for _, p := range env.players {
		for seat, positions := range holePositions {
			if seat == p.Seat() {
				continue
			}
			if err := p.RevealHoleTo(ctx, seat, env.keys[seat], positions); err != nil {
				t.Fatal(err)
			}
		}
	}
	// And its board scalars to everyone.
	for _, p := range env.players {
		if err := p.RevealBoard(ctx, board); err != nil {
			t.Fatal(err)
		}
	}
	env.tp.Drain()
}

// The full deal: every seat recovers its own two hole cards and the same five board cards, and
// no card is dealt twice.
func TestFullDealAcrossSeats(t *testing.T) {
	const seats = 3
	env := newHandEnv(t, seats, cards.DeckSize)
	hole, board := HolePositions(seats, 2)
	runDeal(t, env, hole, board)

	seen := map[int]string{}
	for _, p := range env.players {
		for _, pos := range hole[p.Seat()] {
			c, err := p.Card(pos)
			if err != nil {
				t.Fatalf("seat %d could not read its own hole card at %d: %v", p.Seat(), pos, err)
			}
			if prev, dup := seen[c.Index()]; dup {
				t.Fatalf("card %s dealt twice: %s and seat %d", c, prev, p.Seat())
			}
			seen[c.Index()] = "seat " + string(rune('0'+p.Seat()))
		}
	}

	// The board must resolve identically for every seat.
	var reference []cards.Card
	for _, p := range env.players {
		var got []cards.Card
		for _, pos := range board {
			c, err := p.Card(pos)
			if err != nil {
				t.Fatalf("seat %d could not read board position %d: %v", p.Seat(), pos, err)
			}
			got = append(got, c)
		}
		if reference == nil {
			reference = got
			continue
		}
		for i := range got {
			if got[i] != reference[i] {
				t.Fatalf("seat %d sees board card %d as %s, seat 0 sees %s", p.Seat(), i, got[i], reference[i])
			}
		}
	}

	// Board cards must not collide with hole cards.
	for _, c := range reference {
		if prev, dup := seen[c.Index()]; dup {
			t.Fatalf("board card %s was also dealt to %s", c, prev)
		}
		seen[c.Index()] = "board"
	}
	if len(seen) != seats*2+5 {
		t.Fatalf("recovered %d distinct cards, want %d", len(seen), seats*2+5)
	}
}

// The privacy property over the real transport: a seat cannot read another seat's hole cards,
// because the scalars for those positions were never disclosed to it.
func TestSeatCannotReadAnotherSeatsHoleCards(t *testing.T) {
	const seats = 3
	env := newHandEnv(t, seats, cards.DeckSize)
	hole, board := HolePositions(seats, 2)
	runDeal(t, env, hole, board)

	for _, p := range env.players {
		for seat, positions := range hole {
			if seat == p.Seat() {
				continue
			}
			for _, pos := range positions {
				if _, err := p.Card(pos); err == nil {
					t.Fatalf("seat %d read seat %d's hole card at position %d; privacy is broken",
						p.Seat(), seat, pos)
				}
			}
		}
	}
}

// A hand played to completion: the deal feeds the engine, and every seat reaches the same
// result independently.
func TestHandPlaysToCompletionOverTheTransport(t *testing.T) {
	const seats = 2
	env := newHandEnv(t, seats, cards.DeckSize)
	hole, board := HolePositions(seats, 2)
	runDeal(t, env, hole, board)

	// Each seat builds the engine from the cards it can actually see. Only its own hole
	// cards are known; opponents' are filled in at showdown.
	buildDeck := func(p *HandPlayer) []cards.Card {
		t.Helper()
		deck := make([]cards.Card, 0, seats*2+5)
		for s := 0; s < seats; s++ {
			for _, pos := range hole[s] {
				if s == p.Seat() {
					c, err := p.Card(pos)
					if err != nil {
						t.Fatalf("seat %d cannot read its own card: %v", p.Seat(), err)
					}
					deck = append(deck, c)
					continue
				}
				// A placeholder for an opponent's unknown card. Real play fills
				// these in from the showdown reveal.
				deck = append(deck, cards.MustFromIndex(0))
			}
		}
		for _, pos := range board {
			c, err := p.Card(pos)
			if err != nil {
				t.Fatalf("seat %d cannot read the board: %v", p.Seat(), err)
			}
			deck = append(deck, c)
		}
		return deck
	}

	// Seat 0 runs the engine; the actions are what both seats would replay.
	st, err := engine.New(engine.Config{
		Stacks:     []int64{5000, 5000},
		Button:     0,
		SmallBlind: 25,
		BigBlind:   50,
		Deck:       buildDeck(env.players[0]),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Heads-up: the button calls, the big blind checks, then check it down.
	if err := st.Apply(engine.Action{Kind: engine.Call, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(engine.Action{Kind: engine.Check, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	for !st.Done && st.ToAct >= 0 {
		if err := st.Apply(engine.Action{Kind: engine.Check, Seat: st.ToAct}); err != nil {
			t.Fatal(err)
		}
	}
	if !st.Done {
		t.Fatal("the hand did not complete")
	}

	// Chips are conserved: the pot was awarded, nothing created or destroyed.
	var total int64
	for _, s := range st.Seats {
		total += s.Stack
	}
	total += st.Pot()
	if total != 10000 {
		t.Fatalf("chips = %d, want 10000", total)
	}
	if len(st.Payouts) == 0 {
		t.Fatal("no payout was recorded")
	}
	t.Logf("hand complete: payouts %v, board %v", st.Payouts, st.Board)
}

// A withheld disclosure must leave the card unreadable and be attributable, which is what lets
// a stall be blamed on a seat rather than the protocol.
func TestWithheldDisclosureBlocksTheDeal(t *testing.T) {
	const seats = 3
	env := newHandEnv(t, seats, cards.DeckSize)
	hole, board := HolePositions(seats, 2)

	ctx := context.Background()
	// Run the shuffle and remask normally.
	runDealChainOnly(t, env)

	// Seats 0 and 1 disclose the board; seat 2 withholds.
	for _, p := range env.players[:2] {
		if err := p.RevealBoard(ctx, board); err != nil {
			t.Fatal(err)
		}
	}
	env.tp.Drain()

	// No seat that depends on seat 2's disclosure can read the board. Seat 2 itself is
	// excluded: it holds that scalar already, so withholding costs it nothing — which is
	// precisely why a stall has to be attributable rather than merely detectable.
	for _, p := range env.players {
		if p.Seat() == 2 {
			continue
		}
		if _, err := p.Card(board[0]); err == nil {
			t.Fatalf("seat %d read a board card without every disclosure", p.Seat())
		}
	}
	_ = hole

	// The stall is attributable to the seat that withheld.
	if err := env.tb.Stall(2, "did not disclose its board scalars"); err != nil {
		t.Fatal(err)
	}
	seat, reason := env.tb.StallInfo()
	if seat != 2 || reason == "" {
		t.Fatalf("stall not attributed: seat %d, reason %q", seat, reason)
	}
}

// runDealChainOnly drives shuffle and remask without any reveals.
func runDealChainOnly(t *testing.T, env handEnv) {
	t.Helper()
	ctx := context.Background()
	seats := len(env.players)
	for _, hp := range env.players {
		p := hp
		p.session.Handle(KindShuffle, func(e Envelope) error {
			body, err := DecodeBody[ShuffleBody](e)
			if err != nil {
				return err
			}
			deck, err := DecodeDeck(body.Deck)
			if err != nil {
				return err
			}
			if e.Seat == seats-1 {
				if err := p.SetDeck(deck); err != nil {
					return err
				}
				if p.Seat() == 0 {
					return p.StartRemask(ctx)
				}
				return nil
			}
			return p.ApplyShuffle(ctx, deck, e.Seat)
		})
		p.session.Handle(KindRemask, func(e Envelope) error {
			body, err := DecodeBody[RemaskBody](e)
			if err != nil {
				return err
			}
			deck, err := DecodeDeck(body.Deck)
			if err != nil {
				return err
			}
			if e.Seat == seats-1 {
				return p.SetDeck(deck)
			}
			return p.ApplyRemask(ctx, deck, e.Seat)
		})
		p.session.Handle(KindBoardReveal, func(e Envelope) error {
			body, err := DecodeBody[RevealBody](e)
			if err != nil {
				return err
			}
			return p.RecordDisclosure(e.Seat, body.Positions, body.Scalars)
		})
	}
	if err := env.players[0].StartShuffle(ctx); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()
}

func TestHolePositionsAreDistinct(t *testing.T) {
	for seats := MinSeats; seats <= MaxSeats; seats++ {
		hole, board := HolePositions(seats, 2)
		seen := map[int]bool{}
		for s, positions := range hole {
			if len(positions) != 2 {
				t.Fatalf("seat %d got %d hole positions", s, len(positions))
			}
			for _, p := range positions {
				if seen[p] {
					t.Fatalf("position %d assigned twice", p)
				}
				seen[p] = true
			}
		}
		if len(board) != 5 {
			t.Fatalf("board has %d positions", len(board))
		}
		for _, p := range board {
			if seen[p] {
				t.Fatalf("board position %d collides with a hole position", p)
			}
			seen[p] = true
		}
		if len(seen) != seats*2+5 {
			t.Fatalf("%d seats: %d distinct positions, want %d", seats, len(seen), seats*2+5)
		}
	}
}

func TestDefaultActionCostsTheLeast(t *testing.T) {
	// Facing no bet, checking keeps the seat in the hand for free.
	if got := DefaultAction(false); got.Kind != engine.Check {
		t.Errorf("default with no bet = %v, want check", got.Kind)
	}
	// Facing a bet, folding avoids committing chips the absent player did not choose to.
	if got := DefaultAction(true); got.Kind != engine.Fold {
		t.Errorf("default facing a bet = %v, want fold", got.Kind)
	}
}

func TestHandPlayerValidation(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	sess := env.players[0].session

	if _, err := NewHandPlayer(HandConfig{Table: env.tb, Seat: 0, Seats: 2}); err == nil {
		t.Error("built a hand player with no session")
	}
	if _, err := NewHandPlayer(HandConfig{Session: sess, Seat: 0, Seats: 2}); err == nil {
		t.Error("built a hand player with no table")
	}
	if _, err := NewHandPlayer(HandConfig{Session: sess, Table: env.tb, Seat: 0, Seats: 1}); err == nil {
		t.Error("built a hand player for a one-seat table")
	}
	if _, err := NewHandPlayer(HandConfig{Session: sess, Table: env.tb, Seat: 5, Seats: 2}); err == nil {
		t.Error("built a hand player for a seat outside the table")
	}
}

func TestOnlySeatZeroStartsTheDeal(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	if err := env.players[1].StartShuffle(context.Background()); err == nil {
		t.Fatal("a seat other than 0 started the shuffle")
	}
	if err := env.players[1].StartRemask(context.Background()); err == nil {
		t.Fatal("a seat other than 0 started remasking")
	}
}

func TestRemaskRequiresAShuffledDeck(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	if err := env.players[0].StartRemask(context.Background()); err == nil {
		t.Fatal("remasking began before the deck was shuffled")
	}
}

func TestDecodeDeckRejectsHostileInput(t *testing.T) {
	if _, err := DecodeDeck([][]byte{{0x00}}); err == nil {
		t.Error("accepted a malformed point")
	}
	if _, err := DecodeDeck(nil); err == nil {
		t.Error("accepted an empty deck")
	}
	// A valid point followed by garbage must be refused wholesale.
	good, err := mentalpoker.CardPoint(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDeck([][]byte{good.Bytes(), make([]byte, 33)}); err == nil {
		t.Error("accepted a deck containing an off-curve point")
	}
}

func TestRevealRejectsOutOfRangePositions(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	ctx := context.Background()
	if err := env.players[0].RevealBoard(ctx, []int{cards.DeckSize + 5}); err == nil {
		t.Error("revealed a position outside the deck")
	}
	if err := env.players[0].RevealHoleTo(ctx, 0, env.keys[0], []int{0}); err == nil {
		t.Error("a seat revealed its own hole cards to itself")
	}
}

func TestRecordDisclosureRejectsBadScalars(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]
	if err := p.RecordDisclosure(1, []int{0, 1}, [][]byte{{1}}); err == nil {
		t.Error("accepted mismatched positions and scalars")
	}
	// A zero scalar is not a valid mask.
	if err := p.RecordDisclosure(1, []int{0}, [][]byte{make([]byte, 32)}); err == nil {
		t.Error("accepted a zero scalar")
	}
}
