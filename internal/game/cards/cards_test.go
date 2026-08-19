package cards

import (
	"testing"
)

// The index mapping is protocol-visible: it selects the curve point for a card in the
// mental-poker deck, so it must round-trip exactly and cover 0..51 bijectively.
func TestIndexRoundTripsBijectively(t *testing.T) {
	seen := make(map[int]Card, DeckSize)
	for i := 0; i < DeckSize; i++ {
		c, err := FromIndex(i)
		if err != nil {
			t.Fatalf("FromIndex(%d): %v", i, err)
		}
		if got := c.Index(); got != i {
			t.Fatalf("card %v index = %d, want %d", c, got, i)
		}
		if prev, dup := seen[i]; dup {
			t.Fatalf("index %d produced both %v and %v", i, prev, c)
		}
		seen[i] = c
		if !c.Valid() {
			t.Errorf("card %v from index %d is not valid", c, i)
		}
	}
	if len(seen) != DeckSize {
		t.Fatalf("covered %d indices, want %d", len(seen), DeckSize)
	}
}

func TestFromIndexRejectsOutOfRange(t *testing.T) {
	for _, i := range []int{-1, DeckSize, DeckSize + 1, 1 << 20} {
		if _, err := FromIndex(i); err == nil {
			t.Errorf("FromIndex(%d) succeeded, want an error", i)
		}
	}
}

func TestOrderedIsCanonical(t *testing.T) {
	d := Ordered()
	if len(d) != DeckSize {
		t.Fatalf("deck size = %d, want %d", len(d), DeckSize)
	}
	for i, c := range d {
		if c.Index() != i {
			t.Fatalf("Ordered()[%d] = %v with index %d", i, c, c.Index())
		}
	}
	// Every rank appears in all four suits.
	counts := map[int]int{}
	for _, c := range d {
		counts[c.Rank]++
	}
	for r := MinRank; r <= MaxRank; r++ {
		if counts[r] != 4 {
			t.Errorf("rank %d appears %d times, want 4", r, counts[r])
		}
	}
}

func TestShuffledIsAPermutation(t *testing.T) {
	d, err := Shuffled()
	if err != nil {
		t.Fatalf("Shuffled: %v", err)
	}
	if len(d) != DeckSize {
		t.Fatalf("deck size = %d, want %d", len(d), DeckSize)
	}
	seen := make(map[int]bool, DeckSize)
	for _, c := range d {
		if seen[c.Index()] {
			t.Fatalf("card %v appears twice", c)
		}
		seen[c.Index()] = true
	}
	if len(seen) != DeckSize {
		t.Fatalf("shuffled deck covers %d distinct cards, want %d", len(seen), DeckSize)
	}
}

// Two shuffles must differ. A 1/52! collision is not a realistic flake.
func TestShuffledActuallyShuffles(t *testing.T) {
	a, err := Shuffled()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Shuffled()
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("two shuffles produced identical order")
	}
}

func TestLabels(t *testing.T) {
	tests := map[int]string{14: "A", 13: "K", 12: "Q", 11: "J", 10: "T", 9: "9", 2: "2"}
	for rank, want := range tests {
		c := Card{Rank: rank, Suit: Spades}
		if got := c.RankLabel(); got != want {
			t.Errorf("rank %d label = %q, want %q", rank, got, want)
		}
	}
	if got := (Card{Rank: 14, Suit: Hearts}).String(); got != "Ah" {
		t.Errorf("string = %q, want %q", got, "Ah")
	}
	if !Hearts.Red() || !Diamonds.Red() || Spades.Red() || Clubs.Red() {
		t.Error("suit colours are wrong")
	}
}

func TestInvalidCards(t *testing.T) {
	for _, c := range []Card{
		{Rank: 0, Suit: Spades},
		{Rank: 1, Suit: Spades},
		{Rank: 15, Suit: Spades},
		{Rank: 10, Suit: Suit(9)},
	} {
		if c.Valid() {
			t.Errorf("card %+v reports valid", c)
		}
	}
}
