// Command evalvectors prints the corpus that gate (e) holds the on-chain evaluator to:
// seven-card hands with the category and score from the Go reference evaluator.
//
// Regenerate the checked-in fixture (both TypeScript suites read it) with:
//
//	go run ./scripts/gates/evalvectors -n 2000 -seed 1 > packages/protocol/test/fixtures/eval-vectors.json
//
// Output: a JSON array of {"cards": [7 indices 0..51], "category": 0..8, "score": int64}. A card index is
// (rank-2)*4 + suit, as cards.Card.Index. -n is the total corpus size, made of:
//   - a fixed set: the awkward hands (wheel, two trips, three pairs, a six-card flush, a steel wheel, board
//     plays, ties) and one clear example of every category;
//   - extra seeded hands for rare categories, so each category has at least minPerCategory vectors;
//   - plain seeded random hands for the remainder.
//
// The whole corpus is shuffled by the seed, so neighbouring vectors are unrelated. Same -n and -seed give the
// same bytes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/eval"
)

const minPerCategory = 25

type vector struct {
	Cards    []int `json:"cards"`
	Category int   `json:"category"`
	Score    int64 `json:"score"`
}

// c builds a card index from a rank label and a suit (0..3), e.g. c('A', 0).
func c(rank byte, suit int) int {
	ranks := map[byte]int{'2': 0, '3': 1, '4': 2, '5': 3, '6': 4, '7': 5, '8': 6, '9': 7, 'T': 8, 'J': 9, 'Q': 10, 'K': 11, 'A': 12}
	r, ok := ranks[rank]
	if !ok {
		panic("bad rank " + string(rank))
	}
	return r*4 + suit
}

// fixedHands are the hands where evaluators diverge, plus one clear example of each category.
func fixedHands() [][]int {
	return [][]int{
		// wheel: A-2-3-4-5 is the lowest straight, with a seventh card that must not matter
		{c('A', 0), c('2', 1), c('3', 2), c('4', 3), c('5', 0), c('9', 1), c('7', 2)},
		// wheel with a six: the six-high straight beats it
		{c('A', 0), c('2', 1), c('3', 2), c('4', 3), c('5', 0), c('6', 1), c('K', 2)},
		// steel wheel: A-2-3-4-5 of one suit, a straight flush with five high
		{c('A', 0), c('2', 0), c('3', 0), c('4', 0), c('5', 0), c('K', 1), c('Q', 2)},
		// steel wheel beside a higher flush card: still five-high
		{c('A', 3), c('2', 3), c('3', 3), c('4', 3), c('5', 3), c('9', 3), c('K', 1)},
		// royal flush
		{c('A', 1), c('K', 1), c('Q', 1), c('J', 1), c('T', 1), c('2', 2), c('3', 3)},
		// straight flush inside a longer straight
		{c('4', 1), c('5', 1), c('6', 1), c('7', 1), c('8', 1), c('9', 2), c('T', 0)},
		// two trips: the full house takes the higher trips and a pair from the lower
		{c('T', 0), c('T', 1), c('T', 2), c('6', 0), c('6', 1), c('6', 2), c('3', 3)},
		// two trips, the lower set first in the deal
		{c('6', 0), c('6', 1), c('6', 2), c('T', 0), c('T', 1), c('T', 2), c('A', 3)},
		// trips and two pairs: the higher pair completes the full house
		{c('9', 0), c('9', 1), c('9', 2), c('K', 0), c('K', 1), c('4', 0), c('4', 1)},
		// three pairs: best two pairs plus the best remaining card, which may come from the third pair
		{c('T', 0), c('T', 1), c('6', 0), c('6', 1), c('4', 2), c('4', 3), c('2', 0)},
		{c('T', 0), c('T', 1), c('6', 0), c('6', 1), c('4', 2), c('4', 3), c('7', 0)},
		{c('9', 0), c('9', 1), c('5', 0), c('5', 1), c('3', 2), c('3', 3), c('A', 0)},
		// a six-card flush keeps its top five
		{c('A', 2), c('Q', 2), c('T', 2), c('8', 2), c('6', 2), c('4', 2), c('K', 0)},
		// a six-card flush that does not contain the best offsuit card
		{c('K', 1), c('9', 1), c('7', 1), c('5', 1), c('3', 1), c('2', 1), c('A', 0)},
		// a seven-card flush
		{c('A', 0), c('K', 0), c('9', 0), c('7', 0), c('5', 0), c('3', 0), c('2', 0)},
		// a flush and a straight at once: the flush wins
		{c('5', 1), c('6', 1), c('7', 2), c('8', 1), c('9', 3), c('Q', 1), c('3', 1)},
		// quads with a trips kicker available: the kicker is the single best other rank
		{c('8', 0), c('8', 1), c('8', 2), c('8', 3), c('5', 0), c('5', 1), c('5', 2)},
		// quads with an ace kicker
		{c('2', 0), c('2', 1), c('2', 2), c('2', 3), c('A', 0), c('K', 1), c('Q', 2)},
		// trips with two kickers out of four
		{c('7', 0), c('7', 1), c('7', 2), c('A', 0), c('K', 1), c('3', 2), c('2', 3)},
		// pair with three kickers out of five
		{c('J', 0), c('J', 1), c('A', 0), c('9', 1), c('7', 2), c('4', 3), c('2', 0)},
		// a six-card straight: the top five
		{c('3', 0), c('4', 1), c('5', 2), c('6', 3), c('7', 0), c('8', 1), c('K', 2)},
		// a seven-card straight
		{c('4', 0), c('5', 1), c('6', 2), c('7', 3), c('8', 0), c('9', 1), c('T', 2)},
		// broadway straight
		{c('A', 0), c('K', 1), c('Q', 2), c('J', 3), c('T', 0), c('2', 1), c('3', 2)},
		// high card
		{c('A', 0), c('J', 1), c('9', 2), c('7', 3), c('5', 0), c('3', 1), c('2', 2)},
		// board plays: the board is a straight, and the two hole cards play nothing.
		// The next two hands share the board, so all three tie at the Go score.
		{c('5', 0), c('6', 1), c('7', 2), c('8', 3), c('9', 0), c('2', 1), c('3', 2)},
		{c('5', 0), c('6', 1), c('7', 2), c('8', 3), c('9', 0), c('2', 3), c('4', 1)},
		// board plays: a royal-flush board in spades
		{c('A', 0), c('K', 0), c('Q', 0), c('J', 0), c('T', 0), c('2', 1), c('3', 1)},
		{c('A', 0), c('K', 0), c('Q', 0), c('J', 0), c('T', 0), c('4', 2), c('5', 3)},
		// board plays: a full-house board
		{c('Q', 0), c('Q', 1), c('Q', 2), c('4', 0), c('4', 1), c('2', 2), c('3', 3)},
		{c('Q', 0), c('Q', 1), c('Q', 2), c('4', 0), c('4', 1), c('2', 3), c('3', 2)},
		// board plays: ace-high board, nothing in the hole improves it
		{c('A', 0), c('K', 1), c('9', 2), c('7', 3), c('4', 0), c('3', 1), c('2', 2)},
		{c('A', 0), c('K', 1), c('9', 2), c('7', 3), c('4', 0), c('3', 2), c('2', 1)},
	}
}

func decode(idx []int) ([]cards.Card, error) {
	out := make([]cards.Card, len(idx))
	for i, v := range idx {
		cd, err := cards.FromIndex(v)
		if err != nil {
			return nil, err
		}
		out[i] = cd
	}
	return out, nil
}

func score(idx []int) (vector, error) {
	hand, err := decode(idx)
	if err != nil {
		return vector{}, err
	}
	r, err := eval.Best(hand)
	if err != nil {
		return vector{}, err
	}
	return vector{Cards: idx, Category: int(r.Category), Score: r.Score}, nil
}

func randomHand(rng *rand.Rand) []int {
	return rng.Perm(52)[:7]
}

func main() {
	n := flag.Int("n", 2000, "total number of vectors")
	seed := flag.Uint64("seed", 1, "seed for the random hands and the final shuffle")
	flag.Parse()
	if err := run(*n, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "evalvectors:", err)
		os.Exit(1)
	}
}

func run(n int, seed uint64) error {
	// All of the eval categories, 0..8. Guard the mapping the contract relies on.
	if int(eval.HighCard) != 0 || int(eval.StraightFlush) != 8 {
		return fmt.Errorf("eval.Category no longer runs 0..8")
	}
	rng := rand.New(rand.NewPCG(seed, 0x706f6b6572)) // "poker"

	var out []vector
	var perCat [9]int
	add := func(idx []int) error {
		v, err := score(idx)
		if err != nil {
			return err
		}
		out = append(out, v)
		perCat[v.Category]++
		return nil
	}

	for _, h := range fixedHands() {
		if err := add(h); err != nil {
			return err
		}
	}
	// Rare categories: rejection-sample until every category has enough vectors.
	for guard := 0; guard < 50_000_000; guard++ {
		short := false
		for _, k := range perCat {
			if k < minPerCategory {
				short = true
			}
		}
		if !short {
			break
		}
		h := randomHand(rng)
		v, err := score(h)
		if err != nil {
			return err
		}
		if perCat[v.Category] < minPerCategory {
			out = append(out, v)
			perCat[v.Category]++
		}
	}
	for _, k := range perCat {
		if k < minPerCategory {
			return fmt.Errorf("could not fill every category: %v", perCat)
		}
	}
	if len(out) > n {
		return fmt.Errorf("-n %d is below the %d vectors needed for coverage", n, len(out))
	}
	for len(out) < n {
		if err := add(randomHand(rng)); err != nil {
			return err
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })

	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}
