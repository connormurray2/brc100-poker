package engine

import (
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
)

// orderedDeck gives a deterministic deck so tests are reproducible.
func orderedDeck() []cards.Card { return cards.Ordered() }

func newHand(t *testing.T, stacks []int64, button int) *State {
	t.Helper()
	st, err := New(Config{
		Stacks:     stacks,
		Button:     button,
		SmallBlind: 1,
		BigBlind:   2,
		Deck:       orderedDeck(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

// Chips can never be created or destroyed. This is the single most important property in
// the engine: a violation is money appearing or vanishing.
func assertChipsConserved(t *testing.T, st *State, startTotal int64) {
	t.Helper()
	var end int64
	for _, s := range st.Seats {
		end += s.Stack
	}
	end += st.Pot()
	if end != startTotal {
		t.Fatalf("chips not conserved: started with %d, hold %d", startTotal, end)
	}
}

func totalStacks(stacks []int64) int64 {
	var t int64
	for _, s := range stacks {
		t += s
	}
	return t
}

func TestNewValidatesConfig(t *testing.T) {
	deck := orderedDeck()
	tests := map[string]Config{
		"one seat":           {Stacks: []int64{100}, SmallBlind: 1, BigBlind: 2, Deck: deck},
		"seven seats":        {Stacks: []int64{100, 100, 100, 100, 100, 100, 100}, SmallBlind: 1, BigBlind: 2, Deck: deck},
		"button too high":    {Stacks: []int64{100, 100}, Button: 2, SmallBlind: 1, BigBlind: 2, Deck: deck},
		"negative button":    {Stacks: []int64{100, 100}, Button: -1, SmallBlind: 1, BigBlind: 2, Deck: deck},
		"zero blinds":        {Stacks: []int64{100, 100}, Deck: deck},
		"sb exceeds bb":      {Stacks: []int64{100, 100}, SmallBlind: 5, BigBlind: 2, Deck: deck},
		"zero stack":         {Stacks: []int64{100, 0}, SmallBlind: 1, BigBlind: 2, Deck: deck},
		"deck far too small": {Stacks: []int64{100, 100}, SmallBlind: 1, BigBlind: 2, Deck: deck[:4]},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// Heads-up: the button posts the small blind and acts first pre-flop.
func TestHeadsUpBlindsAndFirstAction(t *testing.T) {
	st := newHand(t, []int64{100, 100}, 0)
	if st.Seats[0].TotalCommit != 1 {
		t.Errorf("button committed %d, want the small blind of 1", st.Seats[0].TotalCommit)
	}
	if st.Seats[1].TotalCommit != 2 {
		t.Errorf("non-button committed %d, want the big blind of 2", st.Seats[1].TotalCommit)
	}
	if st.ToAct != 0 {
		t.Errorf("ToAct = %d, want the button (0) to act first pre-flop heads-up", st.ToAct)
	}
}

// Three-handed: blinds sit to the left of the button.
func TestMultiwayBlindPositions(t *testing.T) {
	st := newHand(t, []int64{100, 100, 100}, 0)
	if st.Seats[1].TotalCommit != 1 {
		t.Errorf("seat 1 committed %d, want the small blind", st.Seats[1].TotalCommit)
	}
	if st.Seats[2].TotalCommit != 2 {
		t.Errorf("seat 2 committed %d, want the big blind", st.Seats[2].TotalCommit)
	}
	if st.ToAct != 0 {
		t.Errorf("ToAct = %d, want seat 0 (under the gun)", st.ToAct)
	}
}

func TestOutOfTurnActionRejected(t *testing.T) {
	st := newHand(t, []int64{100, 100, 100}, 0)
	seat := st.Seats[2]
	stack, total, street := seat.Stack, seat.TotalCommit, seat.StreetCommit
	toAct := st.ToAct

	if err := st.Apply(Action{Kind: Call, Seat: 2}); err == nil {
		t.Fatal("out-of-turn action was accepted")
	}
	if seat.Stack != stack || seat.TotalCommit != total || seat.StreetCommit != street {
		t.Error("seat state changed despite a rejected action")
	}
	if st.ToAct != toAct {
		t.Error("turn advanced despite a rejected action")
	}
}

func TestCannotCheckFacingABet(t *testing.T) {
	st := newHand(t, []int64{100, 100, 100}, 0)
	// Seat 0 faces the big blind of 2.
	if err := st.Apply(Action{Kind: Check, Seat: 0}); err == nil {
		t.Fatal("checking into a live bet was accepted")
	}
	l := st.Legal()
	if l.CanCheck {
		t.Error("Legal reports CanCheck while facing a bet")
	}
	if !l.CanFold || !l.CanCall || !l.CanBetRaise {
		t.Error("Legal should offer fold, call and raise while facing a bet")
	}
}

func TestUndersizedRaiseRejected(t *testing.T) {
	st := newHand(t, []int64{100, 100}, 0)
	// Current bet is 2, min raise 2, so the minimum target is 4.
	if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 3}); err == nil {
		t.Fatal("a raise to 3 was accepted when the minimum target is 4")
	}
	if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 4}); err != nil {
		t.Fatalf("a legal minimum raise was rejected: %v", err)
	}
}

func TestCannotWagerMoreThanStack(t *testing.T) {
	st := newHand(t, []int64{10, 100}, 0)
	// Targeting far beyond the stack is clamped to all-in rather than accepted as-is.
	if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 1000}); err != nil {
		t.Fatalf("all-in raise rejected: %v", err)
	}
	if st.Seats[0].Stack != 0 {
		t.Errorf("stack = %d, want 0 after going all-in", st.Seats[0].Stack)
	}
	if st.Seats[0].TotalCommit != 10 {
		t.Errorf("committed %d, want the whole 10-chip stack", st.Seats[0].TotalCommit)
	}
	if !st.Seats[0].AllIn {
		t.Error("seat is not marked all-in")
	}
}

// A seat whose stack is below the call amount may still commit everything.
func TestShortAllInBelowCallIsAllowed(t *testing.T) {
	st, err := New(Config{
		Stacks: []int64{100, 100, 3}, Button: 0,
		SmallBlind: 1, BigBlind: 2, Deck: orderedDeck(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seat 0 raises to 20; seat 2 has 3 chips minus its blind contribution.
	if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 20}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Fold, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	l := st.Legal()
	if !l.CanCall {
		t.Fatal("a short stack must be able to call for its remaining chips")
	}
	if err := st.Apply(Action{Kind: Call, Seat: 2}); err != nil {
		t.Fatalf("short all-in call rejected: %v", err)
	}
	if !st.Seats[2].AllIn {
		t.Error("short caller is not all-in")
	}
}

// Everyone folding to one seat ends the hand immediately with no further cards.
func TestFoldToOneWinnerEndsHand(t *testing.T) {
	stacks := []int64{100, 100, 100}
	start := totalStacks(stacks)
	st := newHand(t, stacks, 0)

	if err := st.Apply(Action{Kind: Fold, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Fold, Seat: 1}); err != nil {
		t.Fatal(err)
	}

	if !st.Done {
		t.Fatal("hand did not complete when all but one seat folded")
	}
	if len(st.Board) != 0 {
		t.Errorf("board has %d cards; none should be dealt after everyone folds", len(st.Board))
	}
	if st.Payouts[2] != 3 {
		t.Errorf("winner received %d, want the 3-chip pot", st.Payouts[2])
	}
	assertChipsConserved(t, st, start)
}

// A raise reopens action for seats that had already acted.
func TestRaiseReopensAction(t *testing.T) {
	st := newHand(t, []int64{100, 100, 100}, 0)

	if err := st.Apply(Action{Kind: Call, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Call, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	// The big blind raises; seats 0 and 1 must act again.
	if err := st.Apply(Action{Kind: Raise, Seat: 2, To: 10}); err != nil {
		t.Fatal(err)
	}
	if st.Street != Preflop {
		t.Fatalf("street advanced to %v despite an unmatched raise", st.Street)
	}
	if st.ToAct != 0 {
		t.Errorf("ToAct = %d, want seat 0 to act again after the raise", st.ToAct)
	}
}

func TestStreetsProgressInOrder(t *testing.T) {
	st := newHand(t, []int64{100, 100}, 0)

	// Pre-flop: button calls, big blind checks.
	if err := st.Apply(Action{Kind: Call, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Check, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	if st.Street != Flop {
		t.Fatalf("street = %v, want flop", st.Street)
	}
	if len(st.Board) != 3 {
		t.Fatalf("board has %d cards on the flop, want 3", len(st.Board))
	}

	checkAround := func(want Street, wantBoard int) {
		t.Helper()
		for i := 0; i < 2; i++ {
			seat := st.ToAct
			if seat < 0 {
				t.Fatalf("nobody to act on %v", st.Street)
			}
			if err := st.Apply(Action{Kind: Check, Seat: seat}); err != nil {
				t.Fatalf("check on %v: %v", st.Street, err)
			}
		}
		if st.Street != want {
			t.Fatalf("street = %v, want %v", st.Street, want)
		}
		if len(st.Board) != wantBoard {
			t.Fatalf("board has %d cards, want %d", len(st.Board), wantBoard)
		}
	}
	checkAround(Turn, 4)
	checkAround(River, 5)
}

// The layered side pot: a short all-in is only eligible for the main pot.
func TestShortAllInCreatesSidePot(t *testing.T) {
	// Seat 2 is short. All three go all-in, so every pot layer is contested.
	stacks := []int64{100, 100, 20}
	start := totalStacks(stacks)
	st, err := New(Config{
		Stacks: stacks, Button: 0, SmallBlind: 1, BigBlind: 2, Deck: orderedDeck(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Call, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Call, Seat: 2}); err != nil {
		t.Fatal(err)
	}

	if !st.Done {
		t.Fatalf("hand did not complete; street = %v, toAct = %d", st.Street, st.ToAct)
	}
	assertChipsConserved(t, st, start)

	// The short seat can win at most the main pot: its own 20 from each of three seats.
	if got := st.Payouts[2]; got > 60 {
		t.Errorf("short all-in seat won %d, more than the 60-chip main pot it was eligible for", got)
	}
}

func TestSplitPotDividesEvenly(t *testing.T) {
	// Hole cards are dealt per seat in order: seat 0 takes deck[0..1], seat 1 takes
	// deck[2..3]. Give each seat an ace and a king so both make the identical best
	// five-card hand, and keep the board rainbow enough that neither can make a flush.
	deck := []cards.Card{
		{Rank: 14, Suit: cards.Spades}, // seat 0
		{Rank: 13, Suit: cards.Spades}, // seat 0
		{Rank: 14, Suit: cards.Hearts}, // seat 1
		{Rank: 13, Suit: cards.Hearts}, // seat 1
		{Rank: 2, Suit: cards.Clubs},   // board
		{Rank: 5, Suit: cards.Diamonds},
		{Rank: 9, Suit: cards.Clubs},
		{Rank: 7, Suit: cards.Diamonds},
		{Rank: 3, Suit: cards.Diamonds},
	}
	stacks := []int64{100, 100}
	start := totalStacks(stacks)
	st, err := New(Config{Stacks: stacks, Button: 0, SmallBlind: 1, BigBlind: 2, Deck: deck})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Apply(Action{Kind: Call, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Check, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	for st.Street != Complete && !st.Done {
		seat := st.ToAct
		if seat < 0 {
			break
		}
		if err := st.Apply(Action{Kind: Check, Seat: seat}); err != nil {
			t.Fatal(err)
		}
	}

	if !st.Done {
		t.Fatal("hand did not complete")
	}
	assertChipsConserved(t, st, start)
	if st.Payouts[0] != st.Payouts[1] {
		t.Errorf("split pot uneven: seat 0 got %d, seat 1 got %d", st.Payouts[0], st.Payouts[1])
	}
}

// The odd chip must be assigned by a deterministic rule so every replayer agrees.
func TestOddChipIsDeterministic(t *testing.T) {
	run := func() map[int]int64 {
		deck := []cards.Card{
			{Rank: 14, Suit: cards.Spades}, // seat 0
			{Rank: 13, Suit: cards.Spades}, // seat 0
			{Rank: 14, Suit: cards.Hearts}, // seat 1
			{Rank: 13, Suit: cards.Hearts}, // seat 1
			{Rank: 2, Suit: cards.Clubs},   // board
			{Rank: 5, Suit: cards.Diamonds},
			{Rank: 9, Suit: cards.Clubs},
			{Rank: 7, Suit: cards.Diamonds},
			{Rank: 3, Suit: cards.Diamonds},
		}
		// Unequal stacks so the tied pot is odd and a two-way split leaves a
		// remainder. Seat 1 can only match 51 of seat 0's 52, so the pot is 103.
		st, err := New(Config{Stacks: []int64{52, 51}, Button: 0, SmallBlind: 1, BigBlind: 2, Deck: deck})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 52}); err != nil {
			t.Fatal(err)
		}
		if err := st.Apply(Action{Kind: Call, Seat: 1}); err != nil {
			t.Fatal(err)
		}
		for !st.Done && st.ToAct >= 0 {
			if err := st.Apply(Action{Kind: Check, Seat: st.ToAct}); err != nil {
				t.Fatal(err)
			}
		}

		// Guard against a vacuous pass: if the pot divided evenly this test would
		// prove nothing about the odd-chip rule.
		var total int64
		for _, v := range st.Payouts {
			total += v
		}
		if len(st.Payouts) == 2 && total%2 == 0 {
			t.Fatalf("pot of %d splits evenly; this fixture does not exercise the odd chip", total)
		}
		return st.Payouts
	}

	first := run()
	for i := 0; i < 5; i++ {
		again := run()
		if len(again) != len(first) {
			t.Fatalf("payout shape differs between runs")
		}
		for seat, amt := range first {
			if again[seat] != amt {
				t.Fatalf("payout for seat %d differs between runs: %d vs %d", seat, amt, again[seat])
			}
		}
	}
}

// Two independent replayers applying the same actions must reach identical state.
func TestDeterministicReplay(t *testing.T) {
	actions := []Action{
		{Kind: Call, Seat: 0},
		{Kind: Check, Seat: 1},
		{Kind: Bet, Seat: 1, To: 5},
		{Kind: Call, Seat: 0},
		{Kind: Check, Seat: 1},
		{Kind: Check, Seat: 0},
		{Kind: Check, Seat: 1},
		{Kind: Check, Seat: 0},
	}

	replay := func() *State {
		st := newHand(t, []int64{100, 100}, 0)
		for _, a := range actions {
			if st.Done {
				break
			}
			// Skip actions that are not currently legal for that seat: both replayers
			// make the same skips, so the sequences stay aligned.
			if a.Seat != st.ToAct {
				continue
			}
			_ = st.Apply(a)
		}
		return st
	}

	a, b := replay(), replay()
	if a.Street != b.Street {
		t.Errorf("street differs: %v vs %v", a.Street, b.Street)
	}
	if a.Pot() != b.Pot() {
		t.Errorf("pot differs: %d vs %d", a.Pot(), b.Pot())
	}
	if a.ToAct != b.ToAct {
		t.Errorf("toAct differs: %d vs %d", a.ToAct, b.ToAct)
	}
	if len(a.Board) != len(b.Board) {
		t.Fatalf("board length differs: %d vs %d", len(a.Board), len(b.Board))
	}
	for i := range a.Board {
		if a.Board[i] != b.Board[i] {
			t.Errorf("board card %d differs: %v vs %v", i, a.Board[i], b.Board[i])
		}
	}
	for i := range a.Seats {
		if a.Seats[i].Stack != b.Seats[i].Stack {
			t.Errorf("seat %d stack differs: %d vs %d", i, a.Seats[i].Stack, b.Seats[i].Stack)
		}
	}
}

// Deferred reveal is what real multiplayer needs: the engine must stop and wait rather than
// deal from a deck it is not allowed to know.
func TestDeferredRevealPausesForBoardAndShowdown(t *testing.T) {
	stacks := []int64{100, 100}
	start := totalStacks(stacks)
	st, err := New(Config{
		Stacks: stacks, Button: 0, SmallBlind: 1, BigBlind: 2,
		Deck: orderedDeck(), DeferReveal: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Apply(Action{Kind: Call, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Check, Seat: 1}); err != nil {
		t.Fatal(err)
	}

	if !st.AwaitingBoard {
		t.Fatal("engine did not pause for the flop reveal")
	}
	if st.PendingBoardCount != 3 {
		t.Fatalf("pending board count = %d, want 3", st.PendingBoardCount)
	}
	// Acting while awaiting a reveal must be refused.
	if err := st.Apply(Action{Kind: Check, Seat: 0}); err == nil {
		t.Error("an action was accepted while awaiting the board")
	}
	// The wrong number of cards must be refused.
	if err := st.SupplyBoard([]cards.Card{{Rank: 5, Suit: cards.Clubs}}); err == nil {
		t.Error("SupplyBoard accepted the wrong number of cards")
	}

	flop := []cards.Card{
		{Rank: 5, Suit: cards.Clubs}, {Rank: 9, Suit: cards.Diamonds}, {Rank: 3, Suit: cards.Hearts},
	}
	if err := st.SupplyBoard(flop); err != nil {
		t.Fatalf("SupplyBoard: %v", err)
	}
	if st.Street != Flop || len(st.Board) != 3 {
		t.Fatalf("after supplying the flop: street = %v, board = %d", st.Street, len(st.Board))
	}

	// Play to showdown, supplying each street.
	supply := func(n int, base int) {
		t.Helper()
		for !st.Done && !st.AwaitingBoard && !st.AwaitingShowdown && st.ToAct >= 0 {
			if err := st.Apply(Action{Kind: Check, Seat: st.ToAct}); err != nil {
				t.Fatal(err)
			}
		}
		if st.AwaitingBoard {
			cs := make([]cards.Card, n)
			for i := range cs {
				cs[i] = cards.MustFromIndex(base + i)
			}
			if err := st.SupplyBoard(cs); err != nil {
				t.Fatal(err)
			}
		}
	}
	supply(1, 30)
	supply(1, 40)

	for !st.Done && !st.AwaitingShowdown && st.ToAct >= 0 {
		if err := st.Apply(Action{Kind: Check, Seat: st.ToAct}); err != nil {
			t.Fatal(err)
		}
	}

	if !st.AwaitingShowdown {
		t.Fatal("engine did not pause for the showdown reveal")
	}
	if err := st.SetRevealedHole(1, []cards.Card{{Rank: 14, Suit: cards.Spades}, {Rank: 14, Suit: cards.Clubs}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteShowdown(); err != nil {
		t.Fatalf("CompleteShowdown: %v", err)
	}
	if !st.Done {
		t.Fatal("hand not complete after the showdown")
	}
	assertChipsConserved(t, st, start)
}

func TestUncontestedPaysEvenWhenDeferred(t *testing.T) {
	stacks := []int64{100, 100}
	start := totalStacks(stacks)
	st, err := New(Config{
		Stacks: stacks, Button: 0, SmallBlind: 1, BigBlind: 2,
		Deck: orderedDeck(), DeferReveal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Fold, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if !st.Done {
		t.Fatal("an uncontested hand must pay immediately, even in deferred mode")
	}
	if st.AwaitingShowdown {
		t.Error("an uncontested hand should not await a showdown; no hole cards are needed")
	}
	assertChipsConserved(t, st, start)
}

func TestActionsRejectedAfterCompletion(t *testing.T) {
	st := newHand(t, []int64{100, 100}, 0)
	if err := st.Apply(Action{Kind: Fold, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if !st.Done {
		t.Fatal("hand should be complete")
	}
	if err := st.Apply(Action{Kind: Check, Seat: 1}); err == nil {
		t.Error("an action was accepted after the hand completed")
	}
	if st.Legal().CanFold {
		t.Error("Legal offers actions on a completed hand")
	}
}

// Every seat all-in must still deal out the board and settle.
func TestAllInRunsOutTheBoard(t *testing.T) {
	stacks := []int64{50, 50}
	start := totalStacks(stacks)
	st, err := New(Config{Stacks: stacks, Button: 0, SmallBlind: 1, BigBlind: 2, Deck: orderedDeck()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Raise, Seat: 0, To: 50}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(Action{Kind: Call, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	if !st.Done {
		t.Fatalf("all-in hand did not settle; street = %v", st.Street)
	}
	if len(st.Board) != 5 {
		t.Errorf("board has %d cards, want a full 5-card runout", len(st.Board))
	}
	assertChipsConserved(t, st, start)
}
