package mentalpoker

import (
	"errors"
	"fmt"
)

// Deck is a sequence of card encodings, masked or not.
//
// A deck's *positions* are public; what card sits at each position is not. Position order
// after shuffling reveals nothing, because each player contributed a secret permutation.
type Deck struct {
	points []Point
}

// BaseDeck returns the public starting deck for n cards.
//
// Every participant derives this identically with no communication, which is what lets
// them agree on the deck without anyone dealing it.
func BaseDeck(n int) (Deck, error) {
	if n <= 0 {
		return Deck{}, fmt.Errorf("mentalpoker: deck size must be positive, got %d", n)
	}
	pts := make([]Point, n)
	for i := range pts {
		p, err := CardPoint(i)
		if err != nil {
			return Deck{}, err
		}
		pts[i] = p
	}
	return Deck{points: pts}, nil
}

// DeckFromPoints builds a deck from received wire points, validating every one.
//
// Validation happens here so a hostile or corrupt point cannot reach the arithmetic. An
// off-curve point would make every later operation meaningless.
func DeckFromPoints(pts []Point) (Deck, error) {
	if len(pts) == 0 {
		return Deck{}, errors.New("mentalpoker: deck is empty")
	}
	out := make([]Point, len(pts))
	for i, p := range pts {
		if !p.Valid() {
			return Deck{}, fmt.Errorf("mentalpoker: deck position %d holds an invalid point", i)
		}
		out[i] = p
	}
	return Deck{points: out}, nil
}

// Size returns the number of positions.
func (d Deck) Size() int { return len(d.points) }

// At returns the point at a position.
func (d Deck) At(i int) (Point, error) {
	if i < 0 || i >= len(d.points) {
		return Point{}, fmt.Errorf("mentalpoker: position %d out of range [0,%d)", i, len(d.points))
	}
	return d.points[i], nil
}

// Points returns a copy of the deck's points, safe for the caller to retain.
func (d Deck) Points() []Point {
	out := make([]Point, len(d.points))
	copy(out, d.points)
	return out
}

// Equal reports whether two decks hold the same points in the same order.
func (d Deck) Equal(o Deck) bool {
	if len(d.points) != len(o.points) {
		return false
	}
	for i := range d.points {
		if !d.points[i].Equal(o.points[i]) {
			return false
		}
	}
	return true
}

// Permutation is a shuffle: Permutation[i] is the position the card at i moves to.
type Permutation []int

// ValidatePermutation checks that p is a permutation of [0,n).
//
// A malformed permutation could duplicate or drop a card, so this is checked rather than
// trusted — including when it arrives from another player.
func ValidatePermutation(p Permutation, n int) error {
	if len(p) != n {
		return fmt.Errorf("mentalpoker: permutation has %d entries, want %d", len(p), n)
	}
	seen := make([]bool, n)
	for i, v := range p {
		if v < 0 || v >= n {
			return fmt.Errorf("mentalpoker: permutation[%d] = %d out of range [0,%d)", i, v, n)
		}
		if seen[v] {
			return fmt.Errorf("mentalpoker: permutation maps two positions to %d", v)
		}
		seen[v] = true
	}
	return nil
}

// NewPermutation returns a uniformly random permutation of [0,n) from a secure source.
func NewPermutation(n int) (Permutation, error) {
	if n < 0 {
		return nil, fmt.Errorf("mentalpoker: negative permutation size %d", n)
	}
	p := make(Permutation, n)
	for i := range p {
		p[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j, err := randIndex(i + 1)
		if err != nil {
			return nil, err
		}
		p[i], p[j] = p[j], p[i]
	}
	return p, nil
}

// ShuffleStep is one player's contribution to the shuffle.
//
// The player applies a single secret scalar to every card and then permutes. Using one
// global scalar here (rather than per-card scalars) is what lets the player later remove
// their own contribution in a single operation during the remask step.
func (d Deck) ShuffleStep(global Scalar, perm Permutation) (Deck, error) {
	if !global.Valid() {
		return Deck{}, errors.New("mentalpoker: shuffle scalar is invalid")
	}
	if err := ValidatePermutation(perm, len(d.points)); err != nil {
		return Deck{}, err
	}

	out := make([]Point, len(d.points))
	for i, p := range d.points {
		masked, err := p.Mask(global)
		if err != nil {
			return Deck{}, fmt.Errorf("mentalpoker: masking position %d: %w", i, err)
		}
		out[perm[i]] = masked
	}
	return Deck{points: out}, nil
}

// RemaskStep removes a player's global shuffle scalar and applies independent per-position
// scalars in its place.
//
// This is the step that makes private dealing possible. After shuffling, one scalar
// protects every position, so revealing it would expose the whole deck. Afterwards each
// position is protected by its own secret, so a player can disclose the secret for one
// position without telling anyone anything about the others.
func (d Deck) RemaskStep(global Scalar, perPosition []Scalar) (Deck, error) {
	if !global.Valid() {
		return Deck{}, errors.New("mentalpoker: global scalar is invalid")
	}
	if len(perPosition) != len(d.points) {
		return Deck{}, fmt.Errorf("mentalpoker: got %d per-position scalars, want %d", len(perPosition), len(d.points))
	}
	inv, err := global.Inverse()
	if err != nil {
		return Deck{}, err
	}

	out := make([]Point, len(d.points))
	for i, p := range d.points {
		if !perPosition[i].Valid() {
			return Deck{}, fmt.Errorf("mentalpoker: per-position scalar %d is invalid", i)
		}
		stripped, err := p.Mask(inv)
		if err != nil {
			return Deck{}, fmt.Errorf("mentalpoker: stripping global mask at position %d: %w", i, err)
		}
		masked, err := stripped.Mask(perPosition[i])
		if err != nil {
			return Deck{}, fmt.Errorf("mentalpoker: applying per-position mask at %d: %w", i, err)
		}
		out[i] = masked
	}
	return Deck{points: out}, nil
}

// Unmask strips a set of scalars from one position.
//
// Used both for a private deal (the recipient strips every other player's disclosed scalar
// plus their own) and for a board card (everyone strips all disclosed scalars).
func (d Deck) Unmask(position int, scalars []Scalar) (Point, error) {
	p, err := d.At(position)
	if err != nil {
		return Point{}, err
	}
	for i, s := range scalars {
		if !s.Valid() {
			return Point{}, fmt.Errorf("mentalpoker: scalar %d for position %d is invalid", i, position)
		}
		if p, err = p.Unmask(s); err != nil {
			return Point{}, fmt.Errorf("mentalpoker: unmasking position %d: %w", position, err)
		}
	}
	return p, nil
}

// CardIndexOf identifies which card a fully unmasked point is.
//
// Returns an error when the point matches no card, which is the expected outcome for a
// participant who lacks a required scalar. That is the privacy property, not a failure:
// callers must not treat "no match" as a card.
func CardIndexOf(p Point, deckSize int) (int, error) {
	if !p.Valid() {
		return 0, errors.New("mentalpoker: cannot identify an invalid point")
	}
	want := p.Bytes()
	for i := 0; i < deckSize; i++ {
		base, err := CardPoint(i)
		if err != nil {
			return 0, err
		}
		if string(base.Bytes()) == string(want) {
			return i, nil
		}
	}
	return 0, errors.New("mentalpoker: point matches no card; a required scalar is missing")
}
