package eval

import (
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
)

func mustCards(t *testing.T, spec ...string) []cards.Card {
	t.Helper()
	out := make([]cards.Card, 0, len(spec))
	for _, s := range spec {
		out = append(out, parseCard(t, s))
	}
	return out
}

func parseCard(t *testing.T, s string) cards.Card {
	t.Helper()
	if len(s) != 2 {
		t.Fatalf("bad card spec %q", s)
	}
	ranks := map[byte]int{'2': 2, '3': 3, '4': 4, '5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
		'T': 10, 'J': 11, 'Q': 12, 'K': 13, 'A': 14}
	suits := map[byte]cards.Suit{'s': cards.Spades, 'h': cards.Hearts, 'd': cards.Diamonds, 'c': cards.Clubs}
	r, ok := ranks[s[0]]
	if !ok {
		t.Fatalf("bad rank in %q", s)
	}
	su, ok := suits[s[1]]
	if !ok {
		t.Fatalf("bad suit in %q", s)
	}
	return cards.Card{Rank: r, Suit: su}
}

func five(t *testing.T, spec ...string) [HandSize]cards.Card {
	t.Helper()
	c := mustCards(t, spec...)
	if len(c) != HandSize {
		t.Fatalf("need %d cards, got %d", HandSize, len(c))
	}
	var out [HandSize]cards.Card
	copy(out[:], c)
	return out
}

func TestCategories(t *testing.T) {
	tests := []struct {
		name string
		hand []string
		want Category
	}{
		{"high card", []string{"Ah", "Kd", "9c", "7s", "3h"}, HighCard},
		{"pair", []string{"Ah", "Ad", "9c", "7s", "3h"}, Pair},
		{"two pair", []string{"Ah", "Ad", "9c", "9s", "3h"}, TwoPair},
		{"trips", []string{"Ah", "Ad", "Ac", "9s", "3h"}, Trips},
		{"straight", []string{"9h", "8d", "7c", "6s", "5h"}, Straight},
		{"flush", []string{"Ah", "Kh", "9h", "7h", "3h"}, Flush},
		{"full house", []string{"Ah", "Ad", "Ac", "9s", "9h"}, FullHouse},
		{"quads", []string{"Ah", "Ad", "Ac", "As", "9h"}, Quads},
		{"straight flush", []string{"9h", "8h", "7h", "6h", "5h"}, StraightFlush},
		{"royal flush", []string{"Ah", "Kh", "Qh", "Jh", "Th"}, StraightFlush},
		{"wheel straight", []string{"Ah", "2d", "3c", "4s", "5h"}, Straight},
		{"wheel straight flush", []string{"Ah", "2h", "3h", "4h", "5h"}, StraightFlush},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Score5(five(t, tc.hand...))
			if err != nil {
				t.Fatal(err)
			}
			if got.Category != tc.want {
				t.Fatalf("category = %v, want %v", got.Category, tc.want)
			}
		})
	}
}

// The wheel is the WEAKEST straight, not the strongest: the ace plays low.
func TestWheelIsTheWeakestStraight(t *testing.T) {
	wheel, err := Score5(five(t, "Ah", "2d", "3c", "4s", "5h"))
	if err != nil {
		t.Fatal(err)
	}
	sixHigh, err := Score5(five(t, "6h", "5d", "4c", "3s", "2h"))
	if err != nil {
		t.Fatal(err)
	}
	broadway, err := Score5(five(t, "Ah", "Kd", "Qc", "Js", "Th"))
	if err != nil {
		t.Fatal(err)
	}
	if wheel.Score >= sixHigh.Score {
		t.Errorf("wheel (%d) should rank below six-high straight (%d)", wheel.Score, sixHigh.Score)
	}
	if sixHigh.Score >= broadway.Score {
		t.Errorf("six-high (%d) should rank below broadway (%d)", sixHigh.Score, broadway.Score)
	}
}

func TestKickersDecideTies(t *testing.T) {
	tests := []struct{ name, hi, lo string }{
		{"pair kicker", "AhAdKc7s3h", "AhAdQc7s3h"},
		{"two pair kicker", "AhAd9c9sKh", "AhAd9c9sQh"},
		{"trips kicker", "AhAdAcKs3h", "AhAdAcQs3h"},
		{"high card kicker", "AhKd9c7s3h", "AhKd9c7s2h"},
		{"flush kicker", "AhKh9h7h3h", "AhKh9h7h2h"},
		{"quads kicker", "AhAdAcAsKh", "AhAdAcAsQh"},
		{"full house over", "AhAdAc9s9h", "KhKdKc9s9h"},
		{"full house pair", "AhAdAcKsKh", "AhAdAcQsQh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			split := func(s string) []string {
				var out []string
				for i := 0; i < len(s); i += 2 {
					out = append(out, s[i:i+2])
				}
				return out
			}
			hi, err := Score5(five(t, split(tc.hi)...))
			if err != nil {
				t.Fatal(err)
			}
			lo, err := Score5(five(t, split(tc.lo)...))
			if err != nil {
				t.Fatal(err)
			}
			if hi.Score <= lo.Score {
				t.Errorf("%s (%d) should beat %s (%d)", tc.hi, hi.Score, tc.lo, lo.Score)
			}
		})
	}
}

func TestIdenticalHandsTieExactly(t *testing.T) {
	a, err := Score5(five(t, "Ah", "Kd", "9c", "7s", "3h"))
	if err != nil {
		t.Fatal(err)
	}
	// Same ranks, different suits: no flush either way, so exactly tied.
	b, err := Score5(five(t, "Ac", "Ks", "9d", "7h", "3c"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Score != b.Score {
		t.Fatalf("scores differ: %d vs %d; rank-identical hands must tie", a.Score, b.Score)
	}
}

// Fewer than five cards must fail, not return a sentinel score. Upstream's HandEval
// returned -1 here, which silently loses money comparisons.
func TestBestRejectsShortInput(t *testing.T) {
	for n := 0; n < HandSize; n++ {
		hand := cards.Ordered()[:n]
		if _, err := Best(hand); err == nil {
			t.Errorf("Best with %d cards succeeded, want an error", n)
		}
	}
}

func TestBestOfSeven(t *testing.T) {
	// Seven cards containing a nut flush; Best must find it.
	hand := mustCards(t, "Ah", "Kh", "9h", "7h", "3h", "2d", "5c")
	got, err := Best(hand)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != Flush {
		t.Fatalf("category = %v, want flush", got.Category)
	}

	// Seven cards where the best hand is a straight flush hidden among extras.
	hand = mustCards(t, "9h", "8h", "7h", "6h", "5h", "Ad", "Ac")
	got, err = Best(hand)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != StraightFlush {
		t.Fatalf("category = %v, want straight flush", got.Category)
	}
}

// The Omaha-style constraint: exactly two hole cards must be used. This is the case
// upstream's BestForVariant got wrong pre-river.
func TestBestConstrainedUsesExactlyTwoHoleCards(t *testing.T) {
	// Three board hearts and two hole hearts: a flush is reachable within the rule.
	hole := mustCards(t, "Ah", "Kh", "Qd", "2c")
	board := mustCards(t, "9h", "7h", "3h", "4s", "5c")

	got, err := BestConstrained(hole, board, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != Flush {
		t.Fatalf("category = %v, want flush from exactly two hole hearts + three board hearts", got.Category)
	}

	// Four hole hearts but only two board hearts. Pooling all cards would find a flush
	// (four hole hearts + one board heart); the exactly-two rule forbids it, so the
	// constrained search must NOT report one.
	holeAllHearts := mustCards(t, "Ah", "Kh", "Qh", "Jh")
	twoHeartBoard := mustCards(t, "9h", "7h", "3d", "4s", "8c")

	pooled, err := Best(append(append([]cards.Card{}, holeAllHearts...), twoHeartBoard...))
	if err != nil {
		t.Fatal(err)
	}
	if pooled.Category != Flush {
		t.Fatalf("unconstrained pool category = %v, want flush (fixture check)", pooled.Category)
	}

	got, err = BestConstrained(holeAllHearts, twoHeartBoard, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category == Flush {
		t.Fatal("reported a flush using more than two hole cards; the exactly-two rule was not enforced")
	}
}

// Pre-river boards must work. Upstream's hardcoded three-deep board loop broke here.
func TestBestConstrainedWorksPreRiver(t *testing.T) {
	hole := mustCards(t, "Ah", "Kh", "Qd", "Jc")

	// Flop only: 3 board cards, so 2 hole + 3 board = 5. Must succeed.
	flop := mustCards(t, "9h", "7h", "3d")
	if _, err := BestConstrained(hole, flop, 2); err != nil {
		t.Fatalf("flop: %v", err)
	}

	// Turn: 4 board cards.
	turn := mustCards(t, "9h", "7h", "3d", "2s")
	if _, err := BestConstrained(hole, turn, 2); err != nil {
		t.Fatalf("turn: %v", err)
	}

	// Two board cards cannot supply the three required.
	if _, err := BestConstrained(hole, mustCards(t, "9h", "7h"), 2); err == nil {
		t.Error("expected an error when the board cannot supply three cards")
	}
}

func TestBestConstrainedUnconstrainedPoolsAll(t *testing.T) {
	hole := mustCards(t, "Ah", "Kh")
	board := mustCards(t, "Qh", "Jh", "Th", "2d", "3c")
	got, err := BestConstrained(hole, board, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != StraightFlush {
		t.Fatalf("category = %v, want straight flush (royal)", got.Category)
	}
}

func TestInvalidCardsRejected(t *testing.T) {
	bad := []cards.Card{{Rank: 0, Suit: cards.Spades}, {Rank: 3, Suit: cards.Hearts},
		{Rank: 4, Suit: cards.Clubs}, {Rank: 5, Suit: cards.Diamonds}, {Rank: 6, Suit: cards.Spades}}
	if _, err := Best(bad); err == nil {
		t.Error("Best accepted an invalid card")
	}
	var arr [HandSize]cards.Card
	copy(arr[:], bad)
	if _, err := Score5(arr); err == nil {
		t.Error("Score5 accepted an invalid card")
	}
}
