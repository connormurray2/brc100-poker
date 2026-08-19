// Package mentalpoker implements dealerless card dealing with true hole-card privacy.
//
// This file holds only the curve arithmetic the protocol needs. Deliberately absent:
// key generation, ECDSA signing, DER encoding, WIF, and address derivation. Those belong
// to the BRC-100 wallet and are not reimplemented here.
//
// The curve operations come from go-sdk's secp256k1 implementation rather than being
// hand-rolled. Upstream implemented its own Montgomery ladder over big.Int and documented
// honestly that it was not constant-time; using the library's implementation removes that
// concern from our surface.
package mentalpoker

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// curve is secp256k1 — the BSV curve. There is no second curve in this protocol.
func curve() *ec.KoblitzCurve { return ec.S256() }

// order returns the curve order N. Scalars live in [1, N-1].
func order() *big.Int { return curve().Params().N }

// ScalarSize is the byte length of a scalar.
const ScalarSize = 32

// Scalar is a secret masking value in [1, N-1].
//
// A scalar is a card's mask. Its secrecy is what keeps a hole card private, and its
// invertibility is what lets the mask be stripped again.
type Scalar struct {
	d *big.Int
}

// NewScalar generates a fresh random scalar from a cryptographically secure source.
func NewScalar() (Scalar, error) {
	n := order()
	for {
		// rand.Int returns a uniform value in [0, n-1]; retry on zero so the result
		// is in [1, n-1] without biasing the distribution.
		d, err := rand.Int(rand.Reader, n)
		if err != nil {
			return Scalar{}, fmt.Errorf("mentalpoker: reading random source: %w", err)
		}
		if d.Sign() != 0 {
			return Scalar{d: d}, nil
		}
	}
}

// NewScalars generates n independent scalars — one player's per-position secrets.
func NewScalars(n int) ([]Scalar, error) {
	if n < 0 {
		return nil, fmt.Errorf("mentalpoker: negative scalar count %d", n)
	}
	out := make([]Scalar, n)
	for i := range out {
		s, err := NewScalar()
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

// ScalarFromBytes parses a 32-byte big-endian scalar, rejecting anything out of range.
//
// Zero and values >= N are refused rather than reduced. A reduced scalar would still
// "work" arithmetically while no longer being the value the sender committed to, which is
// exactly the kind of silent divergence this protocol cannot tolerate.
func ScalarFromBytes(b []byte) (Scalar, error) {
	if len(b) != ScalarSize {
		return Scalar{}, fmt.Errorf("mentalpoker: scalar must be %d bytes, got %d", ScalarSize, len(b))
	}
	d := new(big.Int).SetBytes(b)
	if d.Sign() == 0 {
		return Scalar{}, errors.New("mentalpoker: scalar is zero")
	}
	if d.Cmp(order()) >= 0 {
		return Scalar{}, errors.New("mentalpoker: scalar is not less than the curve order")
	}
	return Scalar{d: d}, nil
}

// Bytes returns the scalar as 32 big-endian bytes.
func (s Scalar) Bytes() []byte {
	if s.d == nil {
		return nil
	}
	out := make([]byte, ScalarSize)
	s.d.FillBytes(out)
	return out
}

// Valid reports whether the scalar is usable.
func (s Scalar) Valid() bool {
	return s.d != nil && s.d.Sign() != 0 && s.d.Cmp(order()) < 0
}

// Inverse returns s^-1 mod N, the value that strips a mask this scalar applied.
func (s Scalar) Inverse() (Scalar, error) {
	if !s.Valid() {
		return Scalar{}, errors.New("mentalpoker: cannot invert an invalid scalar")
	}
	inv := new(big.Int).ModInverse(s.d, order())
	if inv == nil {
		return Scalar{}, errors.New("mentalpoker: scalar has no inverse")
	}
	return Scalar{d: inv}, nil
}

// Mul returns s*t mod N, composing two masks into one.
func (s Scalar) Mul(t Scalar) (Scalar, error) {
	if !s.Valid() || !t.Valid() {
		return Scalar{}, errors.New("mentalpoker: cannot multiply an invalid scalar")
	}
	p := new(big.Int).Mul(s.d, t.d)
	p.Mod(p, order())
	if p.Sign() == 0 {
		// Unreachable for non-zero factors mod a prime, but a zero mask would be
		// unstrippable, so refuse rather than trust the algebra silently.
		return Scalar{}, errors.New("mentalpoker: scalar product is zero")
	}
	return Scalar{d: p}, nil
}

// Equal reports scalar equality.
func (s Scalar) Equal(t Scalar) bool {
	if s.d == nil || t.d == nil {
		return s.d == nil && t.d == nil
	}
	return s.d.Cmp(t.d) == 0
}

// Point is a curve point: either a card encoding or a masked card.
type Point struct {
	x, y *big.Int
}

// PointSize is the byte length of a compressed point.
const PointSize = 33

// CardPoint returns the fixed public encoding of card index i as (i+1)*G.
//
// Every participant derives these identically with no communication, which is what makes
// the starting deck public and agreed. Index 0 maps to 1*G rather than 0*G because the
// identity is not a usable card encoding.
func CardPoint(i int) (Point, error) {
	if i < 0 {
		return Point{}, fmt.Errorf("mentalpoker: negative card index %d", i)
	}
	k := big.NewInt(int64(i) + 1)
	if k.Cmp(order()) >= 0 {
		return Point{}, fmt.Errorf("mentalpoker: card index %d out of range", i)
	}
	x, y := curve().ScalarBaseMult(k.Bytes())
	return Point{x: x, y: y}, nil
}

// PointFromBytes parses a compressed point and verifies it is on the curve.
//
// Hostile input is rejected here rather than propagated: an off-curve point would make
// later arithmetic meaningless, and accepting one is a way to attack the deal.
func PointFromBytes(b []byte) (Point, error) {
	if len(b) != PointSize {
		return Point{}, fmt.Errorf("mentalpoker: point must be %d bytes, got %d", PointSize, len(b))
	}
	pub, err := ec.ParsePubKey(b)
	if err != nil {
		return Point{}, fmt.Errorf("mentalpoker: parsing point: %w", err)
	}
	if pub.X == nil || pub.Y == nil {
		return Point{}, errors.New("mentalpoker: parsed point has no coordinates")
	}
	if !curve().IsOnCurve(pub.X, pub.Y) {
		return Point{}, errors.New("mentalpoker: point is not on the curve")
	}
	return Point{x: pub.X, y: pub.Y}, nil
}

// Bytes returns the compressed encoding.
func (p Point) Bytes() []byte {
	if !p.Valid() {
		return nil
	}
	pub := &ec.PublicKey{Curve: curve(), X: p.x, Y: p.y}
	return pub.Compressed()
}

// Valid reports whether the point is a usable on-curve point.
func (p Point) Valid() bool {
	return p.x != nil && p.y != nil && curve().IsOnCurve(p.x, p.y)
}

// Mask applies a scalar to the point, returning s*P.
//
// This is both masking and unmasking: applying the inverse scalar strips the mask. Because
// scalar multiplication commutes — a*(b*P) == b*(a*P) — masks from different players can be
// stripped in any order, which is the property the whole protocol rests on.
func (p Point) Mask(s Scalar) (Point, error) {
	if !p.Valid() {
		return Point{}, errors.New("mentalpoker: cannot mask an invalid point")
	}
	if !s.Valid() {
		return Point{}, errors.New("mentalpoker: cannot mask with an invalid scalar")
	}
	x, y := curve().ScalarMult(p.x, p.y, s.Bytes())
	out := Point{x: x, y: y}
	if !out.Valid() {
		return Point{}, errors.New("mentalpoker: masking produced an invalid point")
	}
	return out, nil
}

// Unmask strips a mask this scalar applied.
func (p Point) Unmask(s Scalar) (Point, error) {
	inv, err := s.Inverse()
	if err != nil {
		return Point{}, err
	}
	return p.Mask(inv)
}

// Equal reports point equality.
func (p Point) Equal(q Point) bool {
	if p.x == nil || p.y == nil || q.x == nil || q.y == nil {
		return p.x == nil && p.y == nil && q.x == nil && q.y == nil
	}
	return p.x.Cmp(q.x) == 0 && p.y.Cmp(q.y) == 0
}
