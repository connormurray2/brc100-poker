package eval

import (
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
)

// The number of distinct five-card hands from a 52-card deck: C(52,5).
const allFiveCardHands = 2598960

// Known frequencies of each category over all C(52,5) hands. These are textbook values,
// independent of this implementation, which is what makes them a real check: an evaluator
// that miscategorises even one hand shifts two of these counts.
var wantCategoryCounts = map[Category]int{
	HighCard:      1302540,
	Pair:          1098240,
	TwoPair:       123552,
	Trips:         54912,
	Straight:      10200,
	Flush:         5108,
	FullHouse:     3744,
	Quads:         624,
	StraightFlush: 40,
}

// Scores every distinct five-card hand and checks the category distribution against known
// frequencies, that scores are consistent with categories, and that Best agrees with
// Score5 on exactly five cards.
//
// This is the test that proves the port is faithful. Upstream verified the same property
// by brute force; reproducing it here is the point of porting rather than rewriting.
func TestAllFiveCardHands(t *testing.T) {
	deck := cards.Ordered()
	counts := make(map[Category]int, len(wantCategoryCounts))

	// Score ranges must not overlap across categories: a higher category must always
	// outscore a lower one, or comparisons decide pots wrongly.
	minScore := make(map[Category]int64, len(wantCategoryCounts))
	maxScore := make(map[Category]int64, len(wantCategoryCounts))

	total := 0
	var hand [HandSize]cards.Card
	for a := 0; a < 52; a++ {
		hand[0] = deck[a]
		for b := a + 1; b < 52; b++ {
			hand[1] = deck[b]
			for c := b + 1; c < 52; c++ {
				hand[2] = deck[c]
				for d := c + 1; d < 52; d++ {
					hand[3] = deck[d]
					for e := d + 1; e < 52; e++ {
						hand[4] = deck[e]

						cat, score := score5(hand)
						counts[cat]++
						total++

						if n, ok := minScore[cat]; !ok || score < n {
							minScore[cat] = score
						}
						if n, ok := maxScore[cat]; !ok || score > n {
							maxScore[cat] = score
						}
					}
				}
			}
		}
	}

	if total != allFiveCardHands {
		t.Fatalf("scored %d hands, want %d", total, allFiveCardHands)
	}

	for cat, want := range wantCategoryCounts {
		if got := counts[cat]; got != want {
			t.Errorf("%v: %d hands, want %d", cat, got, want)
		}
	}
	for cat := range counts {
		if _, known := wantCategoryCounts[cat]; !known {
			t.Errorf("unexpected category %v produced %d hands", cat, counts[cat])
		}
	}

	// Category score ranges must be strictly ordered and non-overlapping.
	order := []Category{HighCard, Pair, TwoPair, Trips, Straight, Flush, FullHouse, Quads, StraightFlush}
	for i := 1; i < len(order); i++ {
		lower, higher := order[i-1], order[i]
		if maxScore[lower] >= minScore[higher] {
			t.Errorf("score ranges overlap: %v max %d >= %v min %d",
				lower, maxScore[lower], higher, minScore[higher])
		}
	}
}

// Best over exactly five cards must agree with Score5, over a large random sample of the
// full space. (Running Best over all 2.6M hands is the same work with more allocation;
// a sample is enough to catch a divergence.)
func TestBestAgreesWithScore5(t *testing.T) {
	deck := cards.Ordered()
	checked := 0
	var hand [HandSize]cards.Card

	// Stride through the space deterministically rather than randomly, so a failure is
	// reproducible.
	for a := 0; a < 52; a++ {
		for b := a + 1; b < 52; b++ {
			for c := b + 1; c < 52; c++ {
				for d := c + 1; d < 52; d++ {
					for e := d + 1; e < 52; e++ {
						if (a+b+c+d+e)%97 != 0 {
							continue
						}
						hand = [HandSize]cards.Card{deck[a], deck[b], deck[c], deck[d], deck[e]}

						direct, err := Score5(hand)
						if err != nil {
							t.Fatalf("Score5(%v): %v", hand, err)
						}
						best, err := Best(hand[:])
						if err != nil {
							t.Fatalf("Best(%v): %v", hand, err)
						}
						if direct.Score != best.Score || direct.Category != best.Category {
							t.Fatalf("hand %v: Score5 = (%v,%d), Best = (%v,%d)",
								hand, direct.Category, direct.Score, best.Category, best.Score)
						}
						checked++
					}
				}
			}
		}
	}
	if checked < 10000 {
		t.Fatalf("only checked %d hands; the sample stride is too sparse to be meaningful", checked)
	}
	t.Logf("checked %d hands", checked)
}
