// Package cards is the card and deck model.
//
// Card indices 0..51 are the canonical wire and protocol representation: the mental-poker
// deck maps index i to a fixed public curve point, so the index ordering here is part of
// the protocol and must not be reordered casually.
package cards

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// Suit is a card suit. The numeric values participate in the card index.
type Suit uint8

const (
	Spades Suit = iota
	Hearts
	Diamonds
	Clubs
)

// Suits is every suit in index order.
var Suits = [4]Suit{Spades, Hearts, Diamonds, Clubs}

func (s Suit) String() string {
	switch s {
	case Spades:
		return "s"
	case Hearts:
		return "h"
	case Diamonds:
		return "d"
	case Clubs:
		return "c"
	default:
		return "?"
	}
}

// Glyph returns the suit symbol for display.
func (s Suit) Glyph() rune {
	switch s {
	case Spades:
		return '♠'
	case Hearts:
		return '♥'
	case Diamonds:
		return '♦'
	case Clubs:
		return '♣'
	default:
		return '?'
	}
}

// Red reports whether the suit is red.
func (s Suit) Red() bool { return s == Hearts || s == Diamonds }

// Rank bounds. Aces are high here (14); the evaluator handles the ace-low wheel itself.
const (
	MinRank = 2
	MaxRank = 14 // ace

	// DeckSize is the number of cards in a standard deck.
	DeckSize = 52
)

// Card is a single playing card.
type Card struct {
	Rank int
	Suit Suit
}

// Index returns the canonical 0..51 index.
//
// This ordering is protocol-visible: it selects the curve point representing the card in
// the mental-poker deck.
func (c Card) Index() int { return (c.Rank-MinRank)*4 + int(c.Suit) }

// FromIndex builds a card from its canonical index.
func FromIndex(i int) (Card, error) {
	if i < 0 || i >= DeckSize {
		return Card{}, fmt.Errorf("cards: index %d out of range [0,%d)", i, DeckSize)
	}
	return Card{Rank: i/4 + MinRank, Suit: Suit(i % 4)}, nil
}

// MustFromIndex is FromIndex for known-good input, such as a loop over the deck.
func MustFromIndex(i int) Card {
	c, err := FromIndex(i)
	if err != nil {
		panic(err)
	}
	return c
}

// RankLabel returns the rank as it is written.
func (c Card) RankLabel() string {
	switch c.Rank {
	case 14:
		return "A"
	case 13:
		return "K"
	case 12:
		return "Q"
	case 11:
		return "J"
	case 10:
		return "T"
	default:
		return fmt.Sprint(c.Rank)
	}
}

func (c Card) String() string { return c.RankLabel() + c.Suit.String() }

// Valid reports whether the card is a real card.
func (c Card) Valid() bool {
	return c.Rank >= MinRank && c.Rank <= MaxRank && c.Suit <= Clubs
}

// Ordered returns the deck in canonical index order.
func Ordered() []Card {
	d := make([]Card, DeckSize)
	for i := range d {
		d[i] = MustFromIndex(i)
	}
	return d
}

// Shuffled returns a locally shuffled deck using a cryptographically secure source.
//
// This is for practice play and tests only. Real multiplayer dealing never uses it: the
// deck order in a real hand comes from the mental-poker protocol, where no single party
// knows the order. A local shuffle would mean trusting whoever ran it.
func Shuffled() ([]Card, error) {
	d := Ordered()
	// Fisher-Yates, drawing each index from crypto/rand.
	for i := len(d) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, fmt.Errorf("cards: reading random source: %w", err)
		}
		j := n.Int64()
		d[i], d[j] = d[j], d[i]
	}
	return d, nil
}
