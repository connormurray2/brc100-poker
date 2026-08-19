package agent

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
	"github.com/cmurray/brc100-poker/internal/game/proof"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// dealSecrets are one seat's mental-poker secrets for one hand.
//
// These never leave the agent. That is the whole basis of a dealerless deal: the table sequences
// the chain by asking each agent in turn to apply its pass, and can read nothing, because reading a
// card requires a scalar only the agent holds.
type dealSecrets struct {
	deckSize int
	global   mentalpoker.Scalar
	perm     mentalpoker.Permutation
	// perPosition are the per-position secrets whose selective disclosure deals a card.
	perPosition []mentalpoker.Scalar

	shuffleCommit []byte
	remaskCommit  []byte

	// deck is the deck as this agent last saw it.
	deck mentalpoker.Deck
	// disclosed holds scalars other seats sent this agent, per position and seat.
	disclosed map[int]map[int]mentalpoker.Scalar
	// seats is how many seats must disclose before a card can be read.
	seats int
}

// dealStore holds deal secrets per hand.
type dealStore struct {
	mu    sync.Mutex
	hands map[string]*dealSecrets
}

func newDealStore() *dealStore {
	return &dealStore{hands: make(map[string]*dealSecrets)}
}

func (d *dealStore) get(handID string) (*dealSecrets, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.hands[handID]
	return s, ok
}

// begin generates this seat's secrets for a hand and returns its commitments.
//
// The permutation is generated here, at commit time, rather than when the shuffle runs. That
// ordering is the point: generating it later would leave the commitment binding nothing.
func (d *dealStore) begin(handID string, deckSize, seats int) (*dealSecrets, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Re-committing for a hand already in progress would let a seat pick new secrets after
	// seeing another seat's contribution.
	if existing, ok := d.hands[handID]; ok {
		return existing, nil
	}

	global, err := mentalpoker.NewScalar()
	if err != nil {
		return nil, err
	}
	perm, err := mentalpoker.NewPermutation(deckSize)
	if err != nil {
		return nil, err
	}
	perPosition, err := mentalpoker.NewScalars(deckSize)
	if err != nil {
		return nil, err
	}
	sc, err := proof.CommitShuffle(global, perm)
	if err != nil {
		return nil, err
	}
	rc, err := proof.CommitRemask(global, perPosition)
	if err != nil {
		return nil, err
	}

	s := &dealSecrets{
		deckSize: deckSize, seats: seats,
		global: global, perm: perm, perPosition: perPosition,
		shuffleCommit: sc, remaskCommit: rc,
		disclosed: make(map[int]map[int]mentalpoker.Scalar),
	}
	d.hands[handID] = s
	return s, nil
}

// --- wire types ------------------------------------------------------------

type dealCommitParams struct {
	HandID   string `json:"handId"`
	DeckSize int    `json:"deckSize"`
	Seats    int    `json:"seats"`
}

type dealCommitResult struct {
	ShuffleCommitment string `json:"shuffleCommitment"`
	RemaskCommitment  string `json:"remaskCommitment"`
}

type dealPassParams struct {
	HandID string   `json:"handId"`
	Deck   []string `json:"deck"`
}

type dealPassResult struct {
	Deck []string `json:"deck"`
}

// dealFinalParams records the completed deck with every seat.
type dealFinalParams struct {
	HandID string   `json:"handId"`
	Deck   []string `json:"deck"`
}

type dealRevealParams struct {
	HandID    string `json:"handId"`
	Positions []int  `json:"positions"`
}

type dealRevealResult struct {
	Positions []int    `json:"positions"`
	Scalars   []string `json:"scalars"`
}

type dealCardParams struct {
	HandID string `json:"handId"`
	// Position is the deck position to read.
	Position int `json:"position"`
	// Disclosures are other seats' scalars for this position, keyed by seat.
	Disclosures map[string]string `json:"disclosures"`
}

type dealCardResult struct {
	Card string `json:"card"`
}

// --- handlers --------------------------------------------------------------

func (a *Agent) handleDealCommit(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	var p dealCommitParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "dealCommit params are not valid JSON"}
	}
	if p.HandID == "" {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "a hand id is required"}
	}
	if p.DeckSize <= 0 {
		p.DeckSize = cards.DeckSize
	}
	if p.Seats < 2 {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "a deal needs at least 2 seats"}
	}

	s, err := a.deals.begin(p.HandID, p.DeckSize, p.Seats)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	return dealCommitResult{
		ShuffleCommitment: hex.EncodeToString(s.shuffleCommit),
		RemaskCommitment:  hex.EncodeToString(s.remaskCommit),
	}, nil
}

func (a *Agent) handleDealShuffle(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	s, deck, serr := a.dealInput(params)
	if serr != nil {
		return nil, serr
	}

	s.mu().Lock()
	out, err := deck.ShuffleStep(s.global, s.perm)
	s.mu().Unlock()
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	s.setDeck(out)
	return dealPassResult{Deck: encodeDeck(out)}, nil
}

func (a *Agent) handleDealRemask(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	s, deck, serr := a.dealInput(params)
	if serr != nil {
		return nil, serr
	}

	out, err := deck.RemaskStep(s.global, s.perPosition)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	s.setDeck(out)
	return dealPassResult{Deck: encodeDeck(out)}, nil
}

// handleDealReveal discloses this seat's scalars for named positions.
//
// This is the method that actually deals a card, and the one place the agent gives anything away.
// It discloses only the positions asked for, which is what lets a seat hand over one card's secret
// without handing over the deck.
func (a *Agent) handleDealReveal(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	var p dealRevealParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "dealReveal params are not valid JSON"}
	}
	s, ok := a.deals.get(p.HandID)
	if !ok {
		return nil, &substrate.Error{Code: substrate.CodeForbidden,
			Message: fmt.Sprintf("this agent holds no deal secrets for hand %q", p.HandID)}
	}
	if len(p.Positions) == 0 {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "no positions to reveal"}
	}

	out := dealRevealResult{Positions: p.Positions}
	for _, pos := range p.Positions {
		if pos < 0 || pos >= len(s.perPosition) {
			return nil, &substrate.Error{Code: substrate.CodeBadRequest,
				Message: fmt.Sprintf("position %d is outside the deck", pos)}
		}
		out.Scalars = append(out.Scalars, hex.EncodeToString(s.perPosition[pos].Bytes()))
	}
	return out, nil
}

// handleDealCard reads a card this seat has enough scalars for.
//
// The agent does the reading rather than returning raw scalars to the caller, so its own
// per-position secret is never sent anywhere. A position the seat was not dealt fails, which is the
// privacy property working rather than an error to route around.
// handleDealFinal records the deck as it stands once every seat has contributed.
//
// Needed because an agent only sees the deck as it was when its own pass ran. Reading a card
// requires the FINAL deck, so the coordinator hands it back to every seat once the chain completes.
// Every point is revalidated here rather than trusted: this is a deck the agent did not compute.
func (a *Agent) handleDealFinal(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	s, deck, serr := a.dealInput(params)
	if serr != nil {
		return nil, serr
	}
	s.setDeck(deck)
	return map[string]bool{"ok": true}, nil
}

func (a *Agent) handleDealCard(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	var p dealCardParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "dealCard params are not valid JSON"}
	}
	s, ok := a.deals.get(p.HandID)
	if !ok {
		return nil, &substrate.Error{Code: substrate.CodeForbidden,
			Message: fmt.Sprintf("this agent holds no deal secrets for hand %q", p.HandID)}
	}
	if p.Position < 0 || p.Position >= len(s.perPosition) {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "position is outside the deck"}
	}

	deck := s.getDeck()
	if deck.Size() == 0 {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "the deal has not completed"}
	}

	// This seat's own scalar plus every disclosure it was given.
	scalars := []mentalpoker.Scalar{s.perPosition[p.Position]}
	for _, hexScalar := range p.Disclosures {
		raw, err := hex.DecodeString(hexScalar)
		if err != nil {
			return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "a disclosure is not valid hex"}
		}
		sc, err := mentalpoker.ScalarFromBytes(raw)
		if err != nil {
			return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: err.Error()}
		}
		scalars = append(scalars, sc)
	}

	pt, err := deck.Unmask(p.Position, scalars)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	idx, err := mentalpoker.CardIndexOf(pt, s.deckSize)
	if err != nil {
		// Not enough scalars: the card is not this seat's to read.
		return nil, &substrate.Error{Code: substrate.CodeForbidden,
			Message: "this seat cannot read that position: a required disclosure is missing"}
	}
	c, err := cards.FromIndex(idx)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	return dealCardResult{Card: c.String()}, nil
}

// dealInput decodes a pass request and resolves the hand's secrets.
func (a *Agent) dealInput(params json.RawMessage) (*dealSecrets, mentalpoker.Deck, *substrate.Error) {
	var p dealPassParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, mentalpoker.Deck{}, &substrate.Error{Code: substrate.CodeBadRequest, Message: "params are not valid JSON"}
	}
	s, ok := a.deals.get(p.HandID)
	if !ok {
		return nil, mentalpoker.Deck{}, &substrate.Error{Code: substrate.CodeForbidden,
			Message: fmt.Sprintf("this agent has not committed to hand %q", p.HandID)}
	}

	pts := make([]mentalpoker.Point, 0, len(p.Deck))
	for i, h := range p.Deck {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, mentalpoker.Deck{}, &substrate.Error{Code: substrate.CodeBadRequest,
				Message: fmt.Sprintf("deck position %d is not valid hex", i)}
		}
		// Every point is validated before any arithmetic touches it: an off-curve point
		// would make the rest of the deal meaningless.
		pt, err := mentalpoker.PointFromBytes(raw)
		if err != nil {
			return nil, mentalpoker.Deck{}, &substrate.Error{Code: substrate.CodeBadRequest,
				Message: fmt.Sprintf("deck position %d: %v", i, err)}
		}
		pts = append(pts, pt)
	}
	deck, err := mentalpoker.DeckFromPoints(pts)
	if err != nil {
		return nil, mentalpoker.Deck{}, &substrate.Error{Code: substrate.CodeBadRequest, Message: err.Error()}
	}
	if deck.Size() != s.deckSize {
		return nil, mentalpoker.Deck{}, &substrate.Error{Code: substrate.CodeBadRequest,
			Message: fmt.Sprintf("deck has %d positions, this hand uses %d", deck.Size(), s.deckSize)}
	}
	return s, deck, nil
}

func encodeDeck(d mentalpoker.Deck) []string {
	pts := d.Points()
	out := make([]string, 0, len(pts))
	for _, p := range pts {
		out = append(out, hex.EncodeToString(p.Bytes()))
	}
	return out
}

// dealSecrets locking. A single mutex per store guards these, since a deal is sequential.
var dealMu sync.Mutex

func (s *dealSecrets) mu() *sync.Mutex { return &dealMu }

func (s *dealSecrets) setDeck(d mentalpoker.Deck) {
	dealMu.Lock()
	defer dealMu.Unlock()
	s.deck = d
}

func (s *dealSecrets) getDeck() mentalpoker.Deck {
	dealMu.Lock()
	defer dealMu.Unlock()
	return s.deck
}
