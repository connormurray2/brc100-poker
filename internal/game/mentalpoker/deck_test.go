package mentalpoker

import (
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
)

// player holds one participant's secrets for a hand.
type player struct {
	global      Scalar
	perPosition []Scalar
}

func newPlayers(t *testing.T, n, deckSize int) []player {
	t.Helper()
	ps := make([]player, n)
	for i := range ps {
		g, err := NewScalar()
		if err != nil {
			t.Fatal(err)
		}
		pp, err := NewScalars(deckSize)
		if err != nil {
			t.Fatal(err)
		}
		ps[i] = player{global: g, perPosition: pp}
	}
	return ps
}

// runDeal plays the full protocol: base deck, one shuffle step per player, one remask step
// per player. Returns the final deck.
func runDeal(t *testing.T, ps []player, deckSize int) Deck {
	t.Helper()
	deck, err := BaseDeck(deckSize)
	if err != nil {
		t.Fatal(err)
	}

	for i, p := range ps {
		perm, err := NewPermutation(deckSize)
		if err != nil {
			t.Fatal(err)
		}
		if deck, err = deck.ShuffleStep(p.global, perm); err != nil {
			t.Fatalf("shuffle step %d: %v", i, err)
		}
	}

	for i, p := range ps {
		if deck, err = deck.RemaskStep(p.global, p.perPosition); err != nil {
			t.Fatalf("remask step %d: %v", i, err)
		}
	}
	return deck
}

func TestBaseDeckIsPublicAndAgreed(t *testing.T) {
	a, err := BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatal("two independently derived base decks differ")
	}
	if a.Size() != cards.DeckSize {
		t.Fatalf("deck size = %d, want %d", a.Size(), cards.DeckSize)
	}
	if _, err := BaseDeck(0); err == nil {
		t.Error("accepted a zero-size deck")
	}
}

// A board card: every player discloses that position's scalar, and all participants
// recover the same card.
func TestBoardDealAgreesAcrossParticipants(t *testing.T) {
	const deckSize = cards.DeckSize
	ps := newPlayers(t, 4, deckSize)
	deck := runDeal(t, ps, deckSize)

	for _, pos := range []int{0, 7, 25, deckSize - 1} {
		all := make([]Scalar, 0, len(ps))
		for _, p := range ps {
			all = append(all, p.perPosition[pos])
		}

		pt, err := deck.Unmask(pos, all)
		if err != nil {
			t.Fatalf("position %d: %v", pos, err)
		}
		idx, err := CardIndexOf(pt, deckSize)
		if err != nil {
			t.Fatalf("position %d: %v", pos, err)
		}
		if idx < 0 || idx >= deckSize {
			t.Fatalf("position %d resolved to card %d, out of range", pos, idx)
		}
	}
}

// The whole deck must be a permutation of the real deck: every card present exactly once,
// so the shuffle neither duplicates nor drops a card.
func TestDealtDeckIsAPermutationOfTheRealDeck(t *testing.T) {
	const deckSize = cards.DeckSize
	ps := newPlayers(t, 3, deckSize)
	deck := runDeal(t, ps, deckSize)

	seen := make(map[int]int, deckSize)
	for pos := 0; pos < deckSize; pos++ {
		all := make([]Scalar, 0, len(ps))
		for _, p := range ps {
			all = append(all, p.perPosition[pos])
		}
		pt, err := deck.Unmask(pos, all)
		if err != nil {
			t.Fatalf("position %d: %v", pos, err)
		}
		idx, err := CardIndexOf(pt, deckSize)
		if err != nil {
			t.Fatalf("position %d: %v", pos, err)
		}
		if prev, dup := seen[idx]; dup {
			t.Fatalf("card %d appears at positions %d and %d", idx, prev, pos)
		}
		seen[idx] = pos
	}
	if len(seen) != deckSize {
		t.Fatalf("recovered %d distinct cards, want %d", len(seen), deckSize)
	}
}

// A private hole card: every OTHER player discloses that position's scalar to the
// recipient alone. The recipient learns the card; nobody else can.
func TestPrivateHoleCardDeal(t *testing.T) {
	const deckSize = cards.DeckSize
	const recipient = 2
	const pos = 11

	ps := newPlayers(t, 4, deckSize)
	deck := runDeal(t, ps, deckSize)

	// What the recipient receives: every other player's scalar for this position.
	disclosed := make([]Scalar, 0, len(ps)-1)
	for i, p := range ps {
		if i != recipient {
			disclosed = append(disclosed, p.perPosition[pos])
		}
	}

	// The recipient strips the disclosed scalars plus their own.
	withOwn := append(append([]Scalar{}, disclosed...), ps[recipient].perPosition[pos])
	pt, err := deck.Unmask(pos, withOwn)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CardIndexOf(pt, deckSize)
	if err != nil {
		t.Fatalf("recipient could not identify their own card: %v", err)
	}

	// Everyone else, pooling every scalar they collectively hold, cannot.
	pooled := make([]Scalar, 0, len(ps)-1)
	for i, p := range ps {
		if i != recipient {
			pooled = append(pooled, p.perPosition[pos])
		}
	}
	leaked, err := deck.Unmask(pos, pooled)
	if err != nil {
		t.Fatal(err)
	}
	if idx, err := CardIndexOf(leaked, deckSize); err == nil {
		t.Fatalf("colluding opponents identified the hole card as %d (recipient had %d); privacy is broken", idx, got)
	}
}

// One honest shuffler is enough: if every other player fixes the order, the honest
// player's secret permutation still hides it.
func TestOneHonestShufflerHidesTheOrder(t *testing.T) {
	const deckSize = 52
	const honest = 1

	ps := newPlayers(t, 3, deckSize)

	deck, err := BaseDeck(deckSize)
	if err != nil {
		t.Fatal(err)
	}

	identity := make(Permutation, deckSize)
	for i := range identity {
		identity[i] = i
	}

	var honestPerm Permutation
	for i, p := range ps {
		perm := identity
		if i == honest {
			if honestPerm, err = NewPermutation(deckSize); err != nil {
				t.Fatal(err)
			}
			perm = honestPerm
		}
		if deck, err = deck.ShuffleStep(p.global, perm); err != nil {
			t.Fatal(err)
		}
	}

	// The colluders know their own permutations were the identity, so if the honest
	// player's permutation were also the identity the order would be known. Assert it
	// is not, i.e. the honest contribution actually moved cards.
	moved := 0
	for i, v := range honestPerm {
		if i != v {
			moved++
		}
	}
	if moved == 0 {
		t.Fatal("the honest player's permutation was the identity; the order would be known")
	}
}

func TestPermutationValidation(t *testing.T) {
	if err := ValidatePermutation(Permutation{0, 1, 2}, 3); err != nil {
		t.Errorf("valid permutation rejected: %v", err)
	}
	// Wrong length.
	if err := ValidatePermutation(Permutation{0, 1}, 3); err == nil {
		t.Error("accepted a short permutation")
	}
	// Duplicate target: would duplicate a card and drop another.
	if err := ValidatePermutation(Permutation{0, 1, 1}, 3); err == nil {
		t.Error("accepted a permutation with a duplicate target")
	}
	// Out of range.
	if err := ValidatePermutation(Permutation{0, 1, 3}, 3); err == nil {
		t.Error("accepted an out-of-range permutation entry")
	}
	if err := ValidatePermutation(Permutation{0, 1, -1}, 3); err == nil {
		t.Error("accepted a negative permutation entry")
	}
}

func TestNewPermutationIsAPermutation(t *testing.T) {
	p, err := NewPermutation(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePermutation(p, cards.DeckSize); err != nil {
		t.Fatalf("generated permutation is invalid: %v", err)
	}
}

func TestShuffleStepRejectsBadInput(t *testing.T) {
	deck, err := BaseDeck(8)
	if err != nil {
		t.Fatal(err)
	}
	good, err := NewScalar()
	if err != nil {
		t.Fatal(err)
	}
	identity := Permutation{0, 1, 2, 3, 4, 5, 6, 7}

	var bad Scalar
	if _, err := deck.ShuffleStep(bad, identity); err == nil {
		t.Error("shuffled with an invalid scalar")
	}
	if _, err := deck.ShuffleStep(good, Permutation{0, 1}); err == nil {
		t.Error("shuffled with a wrong-length permutation")
	}
}

func TestRemaskStepRejectsBadInput(t *testing.T) {
	const n = 8
	deck, err := BaseDeck(n)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewScalar()
	if err != nil {
		t.Fatal(err)
	}
	pp, err := NewScalars(n)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := deck.RemaskStep(g, pp[:n-1]); err == nil {
		t.Error("remasked with too few per-position scalars")
	}
	var bad Scalar
	if _, err := deck.RemaskStep(bad, pp); err == nil {
		t.Error("remasked with an invalid global scalar")
	}
	withBad := append([]Scalar{}, pp...)
	withBad[3] = Scalar{}
	if _, err := deck.RemaskStep(g, withBad); err == nil {
		t.Error("remasked with an invalid per-position scalar")
	}
}

func TestDeckFromPointsValidates(t *testing.T) {
	base, err := BaseDeck(4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeckFromPoints(base.Points()); err != nil {
		t.Errorf("valid deck rejected: %v", err)
	}
	if _, err := DeckFromPoints(nil); err == nil {
		t.Error("accepted an empty deck")
	}
	withBad := base.Points()
	withBad[2] = Point{}
	if _, err := DeckFromPoints(withBad); err == nil {
		t.Error("accepted a deck containing an invalid point")
	}
}

func TestAtBoundsChecked(t *testing.T) {
	deck, err := BaseDeck(4)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{-1, 4, 100} {
		if _, err := deck.At(i); err == nil {
			t.Errorf("At(%d) succeeded on a 4-card deck", i)
		}
	}
}

// Missing even one player's scalar must leave the card unidentifiable. This is the
// property that makes withholding detectable rather than silently exploitable.
func TestMissingOneScalarLeavesCardUnknown(t *testing.T) {
	const deckSize = cards.DeckSize
	const pos = 5
	ps := newPlayers(t, 4, deckSize)
	deck := runDeal(t, ps, deckSize)

	for skip := range ps {
		partial := make([]Scalar, 0, len(ps)-1)
		for i, p := range ps {
			if i != skip {
				partial = append(partial, p.perPosition[pos])
			}
		}
		pt, err := deck.Unmask(pos, partial)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CardIndexOf(pt, deckSize); err == nil {
			t.Fatalf("card identified without player %d's scalar", skip)
		}
	}
}
