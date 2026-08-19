package webui

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// AgentEndpoint is one seat's agent, as the coordinator reaches it.
type AgentEndpoint struct {
	Seat int
	// IdentityKey is the seat's public key, which is also the audience its agent expects.
	IdentityKey string
	// URL is where the agent serves the substrate.
	URL string
}

// Coordinator sequences a dealerless deal across seats' agents.
//
// It orchestrates and reads nothing. Every scalar stays inside an agent, so the coordinator can
// drive a deal it cannot see — which is what makes this dealerless rather than merely private
// between players.
type Coordinator struct {
	// caller is the table's own key. It authenticates to each agent, which must have granted
	// it, and is never a party to the deal.
	caller     *ec.PrivateKey
	originator string
	client     *http.Client
}

// NewCoordinator builds a deal coordinator.
func NewCoordinator(caller *ec.PrivateKey, originator string) (*Coordinator, error) {
	if caller == nil {
		return nil, errors.New("webui: a caller key is required to authenticate to agents")
	}
	if originator == "" {
		return nil, errors.New("webui: an originator is required")
	}
	return &Coordinator{
		caller:     caller,
		originator: originator,
		// A bounded timeout: an agent that stops answering must stall the deal visibly
		// rather than hold it open forever.
		client: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// DealtHand is the outcome of a coordinated deal.
type DealtHand struct {
	// FinalDeck is the deck every seat agreed on, hex-encoded points.
	FinalDeck []string
	// Hole maps a seat to the cards it read for itself.
	Hole map[int][]cards.Card
	// Board is the community cards, which every seat agreed on.
	Board []cards.Card
	// HolePositions and BoardPositions record which deck positions were used.
	HolePositions  map[int][]int
	BoardPositions []int
}

// Deal runs a complete dealerless deal.
//
// The sequence matters and each step is a reason: commit before anything is shuffled so no seat can
// choose secrets after seeing another's contribution; shuffle in a chain so every seat gets a turn
// to hide the order; remask so each position carries its own per-seat secret; then disclose only
// what each recipient is owed.
func (c *Coordinator) Deal(ctx context.Context, handID string, agents []AgentEndpoint, holeCards int) (DealtHand, error) {
	if len(agents) < 2 {
		return DealtHand{}, fmt.Errorf("webui: a deal needs at least 2 seats, got %d", len(agents))
	}
	seats := len(agents)
	const deckSize = cards.DeckSize

	// 1. Every seat commits before the deal begins.
	for _, ag := range agents {
		var res struct {
			ShuffleCommitment string `json:"shuffleCommitment"`
			RemaskCommitment  string `json:"remaskCommitment"`
		}
		if err := c.call(ctx, ag, substrate.MethodDealCommit, map[string]any{
			"handId": handID, "deckSize": deckSize, "seats": seats,
		}, &res); err != nil {
			return DealtHand{}, fmt.Errorf("webui: seat %d would not commit: %w", ag.Seat, err)
		}
		if res.ShuffleCommitment == "" || res.RemaskCommitment == "" {
			return DealtHand{}, fmt.Errorf("webui: seat %d published an empty commitment", ag.Seat)
		}
	}

	// 2. The shuffle chain.
	base, err := mentalpoker.BaseDeck(deckSize)
	if err != nil {
		return DealtHand{}, err
	}
	deck := encodeDeckHex(base)
	for _, ag := range agents {
		var res struct {
			Deck []string `json:"deck"`
		}
		if err := c.call(ctx, ag, substrate.MethodDealShuffle, map[string]any{
			"handId": handID, "deck": deck,
		}, &res); err != nil {
			return DealtHand{}, fmt.Errorf("webui: seat %d would not shuffle: %w", ag.Seat, err)
		}
		if len(res.Deck) != deckSize {
			return DealtHand{}, fmt.Errorf("webui: seat %d returned a %d-card deck", ag.Seat, len(res.Deck))
		}
		deck = res.Deck
	}

	// 3. The remask pass.
	for _, ag := range agents {
		var res struct {
			Deck []string `json:"deck"`
		}
		if err := c.call(ctx, ag, substrate.MethodDealRemask, map[string]any{
			"handId": handID, "deck": deck,
		}, &res); err != nil {
			return DealtHand{}, fmt.Errorf("webui: seat %d would not remask: %w", ag.Seat, err)
		}
		deck = res.Deck
	}

	// 4. Hand the completed deck back to every seat. An agent only saw the deck as it was
	//    when its own pass ran, and reading a card needs the final one.
	for _, ag := range agents {
		var ok struct{}
		if err := c.call(ctx, ag, substrate.MethodDealFinal, map[string]any{
			"handId": handID, "deck": deck,
		}, &ok); err != nil {
			return DealtHand{}, fmt.Errorf("webui: seat %d would not accept the final deck: %w", ag.Seat, err)
		}
	}

	holePositions, boardPositions := holeAndBoard(seats, holeCards)
	out := DealtHand{
		FinalDeck:      deck,
		Hole:           make(map[int][]cards.Card, seats),
		HolePositions:  holePositions,
		BoardPositions: boardPositions,
	}

	// 5. Private deals. For each recipient, every OTHER seat discloses that recipient's
	//    positions — and only those. The coordinator relays the scalars without being able to
	//    use them: reading the card also needs the recipient's own secret.
	for _, recipient := range agents {
		positions := holePositions[recipient.Seat]
		disclosures := make(map[int]map[string]string, len(positions))
		for _, p := range positions {
			disclosures[p] = map[string]string{}
		}

		for _, discloser := range agents {
			if discloser.Seat == recipient.Seat {
				continue
			}
			var res struct {
				Positions []int    `json:"positions"`
				Scalars   []string `json:"scalars"`
			}
			if err := c.call(ctx, discloser, substrate.MethodDealReveal, map[string]any{
				"handId": handID, "positions": positions,
			}, &res); err != nil {
				return DealtHand{}, fmt.Errorf("webui: seat %d would not disclose to seat %d: %w",
					discloser.Seat, recipient.Seat, err)
			}
			if len(res.Positions) != len(res.Scalars) {
				return DealtHand{}, fmt.Errorf("webui: seat %d disclosed mismatched positions and scalars", discloser.Seat)
			}
			for i, p := range res.Positions {
				disclosures[p][strconv.Itoa(discloser.Seat)] = res.Scalars[i]
			}
		}

		// The recipient reads its own cards. The coordinator never learns them.
		for _, p := range positions {
			var res struct {
				Card string `json:"card"`
			}
			if err := c.call(ctx, recipient, substrate.MethodDealCard, map[string]any{
				"handId": handID, "position": p, "disclosures": disclosures[p],
			}, &res); err != nil {
				return DealtHand{}, fmt.Errorf("webui: seat %d could not read its own card at %d: %w",
					recipient.Seat, p, err)
			}
			card, err := parseCard(res.Card)
			if err != nil {
				return DealtHand{}, err
			}
			out.Hole[recipient.Seat] = append(out.Hole[recipient.Seat], card)
		}
	}

	// 6. The board. Every seat discloses these positions to everyone, so all seats can read
	//    them — and so can the coordinator, which is correct: community cards are public.
	boardDisclosures := make(map[int]map[string]string, len(boardPositions))
	for _, p := range boardPositions {
		boardDisclosures[p] = map[string]string{}
	}
	for _, ag := range agents {
		var res struct {
			Positions []int    `json:"positions"`
			Scalars   []string `json:"scalars"`
		}
		if err := c.call(ctx, ag, substrate.MethodDealReveal, map[string]any{
			"handId": handID, "positions": boardPositions,
		}, &res); err != nil {
			return DealtHand{}, fmt.Errorf("webui: seat %d would not disclose the board: %w", ag.Seat, err)
		}
		for i, p := range res.Positions {
			boardDisclosures[p][strconv.Itoa(ag.Seat)] = res.Scalars[i]
		}
	}
	// Read each board card through seat 0's agent, then confirm a second seat agrees. A board
	// card every seat cannot reproduce identically is not a board card.
	for _, p := range boardPositions {
		first, err := c.readBoard(ctx, agents[0], handID, p, boardDisclosures[p])
		if err != nil {
			return DealtHand{}, err
		}
		second, err := c.readBoard(ctx, agents[1], handID, p, boardDisclosures[p])
		if err != nil {
			return DealtHand{}, err
		}
		if first != second {
			return DealtHand{}, fmt.Errorf("webui: seats disagree about board position %d: %s vs %s", p, first, second)
		}
		card, err := parseCard(first)
		if err != nil {
			return DealtHand{}, err
		}
		out.Board = append(out.Board, card)
	}
	return out, nil
}

// readBoard asks one agent for a board card, excluding its own disclosure since its scalar comes
// from its own secrets.
func (c *Coordinator) readBoard(ctx context.Context, ag AgentEndpoint, handID string, pos int, all map[string]string) (string, error) {
	others := make(map[string]string, len(all))
	for seat, sc := range all {
		if seat != strconv.Itoa(ag.Seat) {
			others[seat] = sc
		}
	}
	var res struct {
		Card string `json:"card"`
	}
	if err := c.call(ctx, ag, substrate.MethodDealCard, map[string]any{
		"handId": handID, "position": pos, "disclosures": others,
	}, &res); err != nil {
		return "", fmt.Errorf("webui: seat %d could not read board position %d: %w", ag.Seat, pos, err)
	}
	return res.Card, nil
}

// call makes one authenticated substrate request to an agent and verifies the response.
func (c *Coordinator) call(ctx context.Context, ag AgentEndpoint, method substrate.Method, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encoding params: %w", err)
	}
	req := substrate.Request{Method: method, Originator: c.originator, Params: raw}
	// The audience is the seat's identity key, so a request built for one agent cannot be
	// replayed against another.
	if err := substrate.SignRequest(&req, c.caller, ag.IdentityKey); err != nil {
		return fmt.Errorf("signing the request: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding the request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ag.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("calling the agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var envelope substrate.Response
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decoding the response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %s", envelope.Error.Code, envelope.Error.Message)
	}
	// The response must authenticate, or the coordinator cannot tell the seat's real agent
	// from a substituted endpoint.
	if err := substrate.VerifyResponse(envelope, ag.IdentityKey, req.Nonce); err != nil {
		return fmt.Errorf("the agent's response did not authenticate: %w", err)
	}
	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("decoding the result: %w", err)
		}
	}
	return nil
}

// holeAndBoard assigns deck positions. Public and deterministic: which position belongs to whom is
// not secret, only what card sits there.
func holeAndBoard(seats, holeCards int) (map[int][]int, []int) {
	hole := make(map[int][]int, seats)
	next := 0
	for s := 0; s < seats; s++ {
		for i := 0; i < holeCards; i++ {
			hole[s] = append(hole[s], next)
			next++
		}
	}
	board := make([]int, 0, 5)
	for i := 0; i < 5; i++ {
		board = append(board, next)
		next++
	}
	return hole, board
}

func encodeDeckHex(d mentalpoker.Deck) []string {
	pts := d.Points()
	out := make([]string, 0, len(pts))
	for _, p := range pts {
		out = append(out, hex.EncodeToString(p.Bytes()))
	}
	return out
}

// parseCard reads a card back from its wire form, e.g. "Ah".
func parseCard(s string) (cards.Card, error) {
	if len(s) < 2 {
		return cards.Card{}, fmt.Errorf("webui: %q is not a card", s)
	}
	ranks := map[byte]int{'2': 2, '3': 3, '4': 4, '5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
		'T': 10, 'J': 11, 'Q': 12, 'K': 13, 'A': 14}
	suits := map[byte]cards.Suit{'s': cards.Spades, 'h': cards.Hearts, 'd': cards.Diamonds, 'c': cards.Clubs}

	r, ok := ranks[s[0]]
	if !ok {
		return cards.Card{}, fmt.Errorf("webui: %q has no rank", s)
	}
	su, ok := suits[s[len(s)-1]]
	if !ok {
		return cards.Card{}, fmt.Errorf("webui: %q has no suit", s)
	}
	return cards.Card{Rank: r, Suit: su}, nil
}
