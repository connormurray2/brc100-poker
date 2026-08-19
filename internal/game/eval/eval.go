// Package eval evaluates poker hands.
//
// Ported from the upstream `PokerEval`, which was chosen over the upstream `HandEval`
// because it returns an explicit category, guards short input, and does not carry
// HandEval's two bugs (a -1 sentinel score on fewer than five cards, and a hardcoded
// three-deep board loop that is wrong pre-river for Omaha-style hands). See
// docs/port-decisions.md.
package eval

import (
	"fmt"

	"github.com/cmurray/brc100-poker/internal/game/cards"
)

// Category is a poker hand category, ordered low to high.
type Category int

const (
	HighCard Category = iota
	Pair
	TwoPair
	Trips
	Straight
	Flush
	FullHouse
	Quads
	StraightFlush
)

func (c Category) String() string {
	switch c {
	case HighCard:
		return "high card"
	case Pair:
		return "pair"
	case TwoPair:
		return "two pair"
	case Trips:
		return "three of a kind"
	case Straight:
		return "straight"
	case Flush:
		return "flush"
	case FullHouse:
		return "full house"
	case Quads:
		return "four of a kind"
	case StraightFlush:
		return "straight flush"
	default:
		return fmt.Sprintf("category(%d)", int(c))
	}
}

// HandSize is the number of cards in a scored poker hand.
const HandSize = 5

// Result is a scored hand. Score is comparable: higher always beats lower, and equal
// scores are genuinely tied hands.
type Result struct {
	Category Category
	Score    int64
	// Best is the five cards that produced the score.
	Best [HandSize]cards.Card
}

// Score5 scores exactly five cards.
func Score5(hand [HandSize]cards.Card) (Result, error) {
	for _, c := range hand {
		if !c.Valid() {
			return Result{}, fmt.Errorf("eval: invalid card %+v", c)
		}
	}
	cat, score := score5(hand)
	return Result{Category: cat, Score: score, Best: hand}, nil
}

// Best returns the best five-card hand from five or more cards.
//
// Unlike the upstream HandEval, fewer than five cards is an error rather than a sentinel
// score: a caller that silently compares sentinel scores makes wrong decisions about money.
func Best(hand []cards.Card) (Result, error) {
	if len(hand) < HandSize {
		return Result{}, fmt.Errorf("eval: need at least %d cards, got %d", HandSize, len(hand))
	}
	for _, c := range hand {
		if !c.Valid() {
			return Result{}, fmt.Errorf("eval: invalid card %+v", c)
		}
	}

	var best Result
	found := false
	forEachCombination(len(hand), HandSize, func(idx []int) bool {
		var five [HandSize]cards.Card
		for k, i := range idx {
			five[k] = hand[i]
		}
		cat, score := score5(five)
		if !found || score > best.Score {
			best = Result{Category: cat, Score: score, Best: five}
			found = true
		}
		return true
	})
	return best, nil
}

// BestConstrained returns the best hand using exactly `useHole` hole cards plus board cards.
//
// This is the generic form the Omaha family needs. It is correct at every street because it
// searches whatever board is actually present, rather than assuming a five-card board — the
// bug that makes upstream's BestForVariant wrong pre-river.
//
// useHole <= 0 means "no constraint": all cards are pooled, as in Texas Hold'em.
func BestConstrained(hole, board []cards.Card, useHole int) (Result, error) {
	if useHole <= 0 {
		return Best(append(append(make([]cards.Card, 0, len(hole)+len(board)), hole...), board...))
	}
	if useHole > len(hole) {
		return Result{}, fmt.Errorf("eval: need %d hole cards, have %d", useHole, len(hole))
	}
	useBoard := HandSize - useHole
	if useBoard > len(board) {
		return Result{}, fmt.Errorf("eval: need %d board cards, have %d", useBoard, len(board))
	}
	for _, c := range append(append([]cards.Card{}, hole...), board...) {
		if !c.Valid() {
			return Result{}, fmt.Errorf("eval: invalid card %+v", c)
		}
	}

	var best Result
	found := false
	forEachCombination(len(hole), useHole, func(hIdx []int) bool {
		forEachCombination(len(board), useBoard, func(bIdx []int) bool {
			var five [HandSize]cards.Card
			n := 0
			for _, i := range hIdx {
				five[n] = hole[i]
				n++
			}
			for _, i := range bIdx {
				five[n] = board[i]
				n++
			}
			cat, score := score5(five)
			if !found || score > best.Score {
				best = Result{Category: cat, Score: score, Best: five}
				found = true
			}
			return true
		})
		return true
	})
	if !found {
		return Result{}, fmt.Errorf("eval: no valid five-card hand from %d hole and %d board cards", len(hole), len(board))
	}
	return best, nil
}

// score5 is the core scorer. It packs the category and kickers into one comparable integer.
func score5(five [HandSize]cards.Card) (Category, int64) {
	var rankCount [cards.MaxRank + 1]int
	var suitCount [4]int
	for _, c := range five {
		rankCount[c.Rank]++
		suitCount[c.Suit]++
	}

	flush := false
	for _, n := range suitCount {
		if n == HandSize {
			flush = true
		}
	}

	// Straight high, or 0 for none. The wheel (A-2-3-4-5) scores as a five-high straight,
	// which is what makes it the weakest straight rather than the strongest.
	straightHigh := 0
	var present [cards.MaxRank + 1]bool
	for _, c := range five {
		present[c.Rank] = true
	}
	for hi := cards.MaxRank; hi >= 6; hi-- {
		if present[hi] && present[hi-1] && present[hi-2] && present[hi-3] && present[hi-4] {
			straightHigh = hi
			break
		}
	}
	if straightHigh == 0 && present[14] && present[2] && present[3] && present[4] && present[5] {
		straightHigh = 5
	}

	// Ranks ordered by count descending, then rank descending. This ordering is exactly
	// the kicker precedence, so the tiebreak slices below are just prefixes of it.
	byCount := make([]int, 0, HandSize)
	for r := cards.MaxRank; r >= cards.MinRank; r-- {
		if rankCount[r] > 0 {
			byCount = append(byCount, r)
		}
	}
	sortByCountThenRank(byCount, &rankCount)

	groupSize := func(i int) int {
		if i >= len(byCount) {
			return 0
		}
		return rankCount[byCount[i]]
	}

	var cat Category
	var tiebreak []int

	switch {
	case straightHigh > 0 && flush:
		cat, tiebreak = StraightFlush, []int{straightHigh}
	case groupSize(0) == 4:
		cat, tiebreak = Quads, []int{byCount[0], byCount[1]}
	case groupSize(0) == 3 && groupSize(1) == 2:
		cat, tiebreak = FullHouse, []int{byCount[0], byCount[1]}
	case flush:
		cat, tiebreak = Flush, descendingRanks(five)
	case straightHigh > 0:
		cat, tiebreak = Straight, []int{straightHigh}
	case groupSize(0) == 3:
		cat, tiebreak = Trips, byCount
	case groupSize(0) == 2 && groupSize(1) == 2:
		cat, tiebreak = TwoPair, byCount
	case groupSize(0) == 2:
		cat, tiebreak = Pair, byCount
	default:
		cat, tiebreak = HighCard, descendingRanks(five)
	}

	// Pack: category, then kickers, each in a 4-bit field (ranks are < 16). Padding to a
	// fixed width keeps scores from different categories comparable.
	score := int64(cat)
	for _, t := range tiebreak {
		score = score*16 + int64(t)
	}
	for pad := len(tiebreak); pad < HandSize; pad++ {
		score *= 16
	}
	return cat, score
}

// sortByCountThenRank orders ranks by group size descending, then rank descending.
// Insertion sort: at most five elements.
func sortByCountThenRank(rs []int, count *[cards.MaxRank + 1]int) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0; j-- {
			a, b := rs[j-1], rs[j]
			if count[a] > count[b] || (count[a] == count[b] && a >= b) {
				break
			}
			rs[j-1], rs[j] = rs[j], rs[j-1]
		}
	}
}

func descendingRanks(five [HandSize]cards.Card) []int {
	out := make([]int, 0, HandSize)
	for r := cards.MaxRank; r >= cards.MinRank; r-- {
		for _, c := range five {
			if c.Rank == r {
				out = append(out, r)
			}
		}
	}
	return out
}

// forEachCombination calls fn with each k-subset of [0,n) in lexicographic order.
// fn returning false stops the iteration.
func forEachCombination(n, k int, fn func(idx []int) bool) {
	if k <= 0 || k > n {
		return
	}
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	for {
		if !fn(idx) {
			return
		}
		i := k - 1
		for i >= 0 && idx[i] == n-k+i {
			i--
		}
		if i < 0 {
			return
		}
		idx[i]++
		for j := i + 1; j < k; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
}
