package agent

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// dealAgent is one seat's agent, driven the way a table would drive it.
type dealAgent struct {
	a    *Agent
	seat int
}

func newDealAgents(t *testing.T, seats int) []dealAgent {
	t.Helper()
	out := make([]dealAgent, 0, seats)
	for i := 0; i < seats; i++ {
		e := newEnv(t, approveAll())
		out = append(out, dealAgent{a: e.agent, seat: i})
	}
	return out
}

func call[T any](t *testing.T, fn func(*ec.PublicKey, json.RawMessage) (any, error), params any) T {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	res, err := fn(nil, raw)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	// Round-trip through JSON, exactly as the wire would.
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func callErr(t *testing.T, fn func(*ec.PublicKey, json.RawMessage) (any, error), params any) error {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fn(nil, raw)
	return err
}

// A deal driven entirely through the substrate: the coordinator sequences the chain and reads
// nothing, because every secret stays inside an agent.
//
// This is the property the whole design rests on, so it is asserted end to end rather than inferred
// from the parts.
func TestDealerlessDealThroughAgents(t *testing.T) {
	const seats = 3
	const deckSize = cards.DeckSize
	const handID = "browser-hand-1"

	ags := newDealAgents(t, seats)

	// 1. Every seat commits BEFORE the deal, so none can choose secrets after seeing another's
	//    contribution.
	for _, ag := range ags {
		res := call[dealCommitResult](t, ag.a.handleDealCommit, dealCommitParams{
			HandID: handID, DeckSize: deckSize, Seats: seats,
		})
		if res.ShuffleCommitment == "" || res.RemaskCommitment == "" {
			t.Fatalf("seat %d published an empty commitment", ag.seat)
		}
		if res.ShuffleCommitment == res.RemaskCommitment {
			t.Fatalf("seat %d's shuffle and remask commitments are identical", ag.seat)
		}
	}

	// 2. The shuffle chain. The coordinator passes the deck from agent to agent and never sees a
	//    scalar.
	base, err := mentalpoker.BaseDeck(deckSize)
	if err != nil {
		t.Fatal(err)
	}
	deck := encodeDeck(base)
	for _, ag := range ags {
		res := call[dealPassResult](t, ag.a.handleDealShuffle, dealPassParams{HandID: handID, Deck: deck})
		if len(res.Deck) != deckSize {
			t.Fatalf("seat %d returned a %d-card deck", ag.seat, len(res.Deck))
		}
		deck = res.Deck
	}

	// 3. The remask pass, giving every position its own per-seat secret.
	for _, ag := range ags {
		res := call[dealPassResult](t, ag.a.handleDealRemask, dealPassParams{HandID: handID, Deck: deck})
		deck = res.Deck
	}

	// The coordinator hands the completed deck back to every seat: an agent only saw the deck
	// as it was when its own pass ran.
	for _, ag := range ags {
		call[map[string]bool](t, ag.a.handleDealFinal, dealFinalParams{HandID: handID, Deck: deck})
	}

	// Every agent must now hold the same final deck, or the seats disagree about the deal.
	for _, ag := range ags {
		s, ok := ag.a.deals.get(handID)
		if !ok {
			t.Fatalf("seat %d holds no secrets", ag.seat)
		}
		got := encodeDeck(s.getDeck())
		for i := range got {
			if got[i] != deck[i] {
				t.Fatalf("seat %d disagrees about deck position %d", ag.seat, i)
			}
		}
	}

	// 4. Deal seat 0 its hole cards: every OTHER seat discloses those positions to it.
	holePositions := []int{0, 1}
	disclosures := map[string]string{}
	for _, ag := range ags {
		if ag.seat == 0 {
			continue
		}
		res := call[dealRevealResult](t, ag.a.handleDealReveal, dealRevealParams{
			HandID: handID, Positions: holePositions,
		})
		if len(res.Scalars) != len(holePositions) {
			t.Fatalf("seat %d disclosed %d scalars for %d positions", ag.seat, len(res.Scalars), len(holePositions))
		}
		// Keyed by seat, as the coordinator would relay them.
		disclosures[string(rune('0'+ag.seat))] = res.Scalars[0]
	}

	// Seat 0 reads its own card through its own agent.
	got := call[dealCardResult](t, ags[0].a.handleDealCard, dealCardParams{
		HandID: handID, Position: holePositions[0], Disclosures: disclosures,
	})
	if got.Card == "" {
		t.Fatal("seat 0 could not read its own card")
	}
	t.Logf("seat 0 read its own hole card: %s", got.Card)

	// 5. The privacy property: another seat, given the SAME disclosures, cannot read it —
	//    because seat 0's own scalar is required and never left seat 0's agent.
	err = callErr(t, ags[1].a.handleDealCard, dealCardParams{
		HandID: handID, Position: holePositions[0], Disclosures: disclosures,
	})
	if err == nil {
		t.Fatal("another seat read seat 0's hole card; the deal is not private")
	}
	var se *substrate.Error
	if !asSubstrateError(err, &se) || se.Code != substrate.CodeForbidden {
		t.Errorf("error = %v, want forbidden", err)
	}
	t.Logf("seat 1 correctly refused: %s", se.Message)
}

// A board card resolves identically for every seat once all seats disclose.
func TestBoardCardAgreesAcrossAgents(t *testing.T) {
	const seats = 2
	const handID = "browser-board"
	ags := newDealAgents(t, seats)

	for _, ag := range ags {
		call[dealCommitResult](t, ag.a.handleDealCommit, dealCommitParams{
			HandID: handID, DeckSize: cards.DeckSize, Seats: seats,
		})
	}
	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	deck := encodeDeck(base)
	for _, ag := range ags {
		deck = call[dealPassResult](t, ag.a.handleDealShuffle, dealPassParams{HandID: handID, Deck: deck}).Deck
	}
	for _, ag := range ags {
		deck = call[dealPassResult](t, ag.a.handleDealRemask, dealPassParams{HandID: handID, Deck: deck}).Deck
	}
	for _, ag := range ags {
		call[map[string]bool](t, ag.a.handleDealFinal, dealFinalParams{HandID: handID, Deck: deck})
	}

	const boardPos = 20
	// Each seat discloses the board position to everyone.
	scalars := map[int]string{}
	for _, ag := range ags {
		res := call[dealRevealResult](t, ag.a.handleDealReveal, dealRevealParams{
			HandID: handID, Positions: []int{boardPos},
		})
		scalars[ag.seat] = res.Scalars[0]
	}

	var seen string
	for _, ag := range ags {
		// A seat supplies every OTHER seat's disclosure; its own comes from its secrets.
		d := map[string]string{}
		for seat, sc := range scalars {
			if seat != ag.seat {
				d[string(rune('0'+seat))] = sc
			}
		}
		got := call[dealCardResult](t, ag.a.handleDealCard, dealCardParams{
			HandID: handID, Position: boardPos, Disclosures: d,
		})
		if seen == "" {
			seen = got.Card
			continue
		}
		if got.Card != seen {
			t.Fatalf("seat %d reads the board card as %s, another seat reads %s", ag.seat, got.Card, seen)
		}
	}
	t.Logf("every seat agrees the board card is %s", seen)
}

// A seat cannot re-commit for a hand in progress, which would let it pick new secrets after seeing
// another seat's contribution.
func TestCommitmentsAreStablePerHand(t *testing.T) {
	ags := newDealAgents(t, 2)
	first := call[dealCommitResult](t, ags[0].a.handleDealCommit, dealCommitParams{
		HandID: "h", DeckSize: cards.DeckSize, Seats: 2,
	})
	second := call[dealCommitResult](t, ags[0].a.handleDealCommit, dealCommitParams{
		HandID: "h", DeckSize: cards.DeckSize, Seats: 2,
	})
	if first.ShuffleCommitment != second.ShuffleCommitment {
		t.Fatal("a seat produced new commitments for a hand already in progress")
	}
}

// A pass for a hand the agent never committed to is refused: without a commitment the pass cannot
// be verified later, so accepting it would make the proof optional.
func TestPassRequiresACommitment(t *testing.T) {
	ags := newDealAgents(t, 2)
	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	err = callErr(t, ags[0].a.handleDealShuffle, dealPassParams{
		HandID: "never-committed", Deck: encodeDeck(base),
	})
	if err == nil {
		t.Fatal("a shuffle was applied for a hand with no commitment")
	}
	if !strings.Contains(err.Error(), "has not committed") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// Hostile deck input must be refused before any arithmetic touches it.
func TestDealRejectsHostileDeck(t *testing.T) {
	ags := newDealAgents(t, 2)
	call[dealCommitResult](t, ags[0].a.handleDealCommit, dealCommitParams{
		HandID: "h", DeckSize: 4, Seats: 2,
	})

	// An off-curve point.
	bad := make([]string, 4)
	for i := range bad {
		bad[i] = hex.EncodeToString(make([]byte, 33))
	}
	if err := callErr(t, ags[0].a.handleDealShuffle, dealPassParams{HandID: "h", Deck: bad}); err == nil {
		t.Error("an off-curve deck was accepted")
	}

	// A deck of the wrong size for this hand.
	base, err := mentalpoker.BaseDeck(8)
	if err != nil {
		t.Fatal(err)
	}
	if err := callErr(t, ags[0].a.handleDealShuffle, dealPassParams{HandID: "h", Deck: encodeDeck(base)}); err == nil {
		t.Error("a wrong-size deck was accepted")
	}
}

func TestRevealValidation(t *testing.T) {
	ags := newDealAgents(t, 2)
	call[dealCommitResult](t, ags[0].a.handleDealCommit, dealCommitParams{
		HandID: "h", DeckSize: cards.DeckSize, Seats: 2,
	})

	if err := callErr(t, ags[0].a.handleDealReveal, dealRevealParams{HandID: "h"}); err == nil {
		t.Error("revealed with no positions")
	}
	if err := callErr(t, ags[0].a.handleDealReveal, dealRevealParams{
		HandID: "h", Positions: []int{9999},
	}); err == nil {
		t.Error("revealed a position outside the deck")
	}
	if err := callErr(t, ags[0].a.handleDealReveal, dealRevealParams{
		HandID: "unknown", Positions: []int{0},
	}); err == nil {
		t.Error("revealed for a hand the agent holds no secrets for")
	}
}

func TestDealCommitValidation(t *testing.T) {
	ags := newDealAgents(t, 2)
	if err := callErr(t, ags[0].a.handleDealCommit, dealCommitParams{Seats: 2}); err == nil {
		t.Error("committed with no hand id")
	}
	if err := callErr(t, ags[0].a.handleDealCommit, dealCommitParams{HandID: "h", Seats: 1}); err == nil {
		t.Error("committed to a one-seat deal")
	}
}

// Reading a card before the deal completes must fail rather than return a wrong answer.
func TestCardBeforeDealCompletes(t *testing.T) {
	ags := newDealAgents(t, 2)
	call[dealCommitResult](t, ags[0].a.handleDealCommit, dealCommitParams{
		HandID: "h", DeckSize: cards.DeckSize, Seats: 2,
	})
	if err := callErr(t, ags[0].a.handleDealCard, dealCardParams{HandID: "h", Position: 0}); err == nil {
		t.Fatal("a card was read before the deal completed")
	}
}
