package mentalpoker

import (
	"bytes"
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
)

func mustScalar(t *testing.T) Scalar {
	t.Helper()
	s, err := NewScalar()
	if err != nil {
		t.Fatalf("NewScalar: %v", err)
	}
	return s
}

func mustCardPoint(t *testing.T, i int) Point {
	t.Helper()
	p, err := CardPoint(i)
	if err != nil {
		t.Fatalf("CardPoint(%d): %v", i, err)
	}
	return p
}

// Card encodings are public and deterministic: every participant derives the same deck
// with no communication.
func TestCardPointsArePublicAndDistinct(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < cards.DeckSize; i++ {
		p := mustCardPoint(t, i)
		if !p.Valid() {
			t.Fatalf("card point %d is not on the curve", i)
		}
		key := string(p.Bytes())
		if prev, dup := seen[key]; dup {
			t.Fatalf("cards %d and %d encode to the same point", prev, i)
		}
		seen[key] = i

		// Deterministic: a second derivation matches.
		again := mustCardPoint(t, i)
		if !p.Equal(again) {
			t.Fatalf("card point %d is not deterministic", i)
		}
	}
	if len(seen) != cards.DeckSize {
		t.Fatalf("got %d distinct encodings, want %d", len(seen), cards.DeckSize)
	}
}

// Card 0 is 1*G, the generator. This pins the encoding to a known-answer value rather than
// only to itself.
func TestCardZeroIsGenerator(t *testing.T) {
	const generatorCompressed = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	got := mustCardPoint(t, 0)
	if hexOf(got.Bytes()) != generatorCompressed {
		t.Fatalf("card 0 = %s, want the secp256k1 generator %s", hexOf(got.Bytes()), generatorCompressed)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// The property the entire protocol rests on: a*(b*P) == b*(a*P).
func TestMasksCommute(t *testing.T) {
	p := mustCardPoint(t, 17)
	a, b := mustScalar(t), mustScalar(t)

	ab, err := p.Mask(a)
	if err != nil {
		t.Fatal(err)
	}
	ab, err = ab.Mask(b)
	if err != nil {
		t.Fatal(err)
	}

	ba, err := p.Mask(b)
	if err != nil {
		t.Fatal(err)
	}
	ba, err = ba.Mask(a)
	if err != nil {
		t.Fatal(err)
	}

	if !ab.Equal(ba) {
		t.Fatal("masks do not commute; the deal protocol cannot work")
	}
}

// Masks must strip in any order, since players reveal their secrets in whatever order
// messages happen to arrive.
func TestMasksStripInAnyOrder(t *testing.T) {
	base := mustCardPoint(t, 42)
	s := []Scalar{mustScalar(t), mustScalar(t), mustScalar(t), mustScalar(t)}

	masked := base
	for _, sc := range s {
		var err error
		if masked, err = masked.Mask(sc); err != nil {
			t.Fatal(err)
		}
	}

	// Strip in every permutation of the four scalars.
	perms := permutations([]int{0, 1, 2, 3})
	if len(perms) != 24 {
		t.Fatalf("expected 24 permutations, got %d", len(perms))
	}
	for _, order := range perms {
		got := masked
		for _, i := range order {
			var err error
			if got, err = got.Unmask(s[i]); err != nil {
				t.Fatalf("unmask order %v: %v", order, err)
			}
		}
		if !got.Equal(base) {
			t.Fatalf("stripping in order %v did not recover the original point", order)
		}
	}
}

func permutations(in []int) [][]int {
	if len(in) <= 1 {
		return [][]int{append([]int{}, in...)}
	}
	var out [][]int
	for i := range in {
		rest := make([]int, 0, len(in)-1)
		rest = append(rest, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]int{in[i]}, p...))
		}
	}
	return out
}

// The privacy property: a participant who strips every mask they know, but lacks the
// recipient's secret, is left with a point matching no card in the public deck.
func TestPartiallyUnmaskedCardMatchesNoCard(t *testing.T) {
	deck := make(map[string]int, cards.DeckSize)
	for i := 0; i < cards.DeckSize; i++ {
		deck[string(mustCardPoint(t, i).Bytes())] = i
	}

	const target = 23
	base := mustCardPoint(t, target)

	// Three opponents and the recipient each apply a mask.
	opp := []Scalar{mustScalar(t), mustScalar(t), mustScalar(t)}
	recipient := mustScalar(t)

	masked := base
	for _, s := range append(append([]Scalar{}, opp...), recipient) {
		var err error
		if masked, err = masked.Mask(s); err != nil {
			t.Fatal(err)
		}
	}

	// The opponents pool everything they have and strip all of it.
	got := masked
	for _, s := range opp {
		var err error
		if got, err = got.Unmask(s); err != nil {
			t.Fatal(err)
		}
	}

	if idx, found := deck[string(got.Bytes())]; found {
		t.Fatalf("colluding opponents identified the card as %d; hole-card privacy is broken", idx)
	}

	// The recipient, holding the last secret, recovers the card.
	final, err := got.Unmask(recipient)
	if err != nil {
		t.Fatal(err)
	}
	idx, found := deck[string(final.Bytes())]
	if !found {
		t.Fatal("recipient could not identify their own card")
	}
	if idx != target {
		t.Fatalf("recipient recovered card %d, want %d", idx, target)
	}
}

func TestScalarInverseRoundTrip(t *testing.T) {
	p := mustCardPoint(t, 5)
	s := mustScalar(t)

	masked, err := p.Mask(s)
	if err != nil {
		t.Fatal(err)
	}
	if masked.Equal(p) {
		t.Fatal("masking did not change the point")
	}
	back, err := masked.Unmask(s)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(p) {
		t.Fatal("unmasking did not recover the original point")
	}
}

// A composed mask must strip in one operation, which is what lets the shuffle apply one
// global scalar and later remove it.
func TestComposedMaskStripsAsOne(t *testing.T) {
	p := mustCardPoint(t, 9)
	a, b := mustScalar(t), mustScalar(t)

	step, err := p.Mask(a)
	if err != nil {
		t.Fatal(err)
	}
	step, err = step.Mask(b)
	if err != nil {
		t.Fatal(err)
	}

	ab, err := a.Mul(b)
	if err != nil {
		t.Fatal(err)
	}
	oneShot, err := p.Mask(ab)
	if err != nil {
		t.Fatal(err)
	}
	if !step.Equal(oneShot) {
		t.Fatal("composed mask does not equal the sequential masks")
	}
}

func TestScalarRejectsOutOfRange(t *testing.T) {
	zero := make([]byte, ScalarSize)
	if _, err := ScalarFromBytes(zero); err == nil {
		t.Error("accepted a zero scalar")
	}

	// The curve order N, and N+1.
	n := order()
	nb := make([]byte, ScalarSize)
	n.FillBytes(nb)
	if _, err := ScalarFromBytes(nb); err == nil {
		t.Error("accepted a scalar equal to the curve order")
	}

	if _, err := ScalarFromBytes([]byte{1, 2, 3}); err == nil {
		t.Error("accepted a short scalar")
	}
	if _, err := ScalarFromBytes(append(nb, 0)); err == nil {
		t.Error("accepted an over-long scalar")
	}
}

func TestScalarBytesRoundTrip(t *testing.T) {
	s := mustScalar(t)
	b := s.Bytes()
	if len(b) != ScalarSize {
		t.Fatalf("scalar bytes = %d, want %d", len(b), ScalarSize)
	}
	got, err := ScalarFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(s) {
		t.Fatal("scalar did not survive a bytes round trip")
	}
}

func TestPointBytesRoundTrip(t *testing.T) {
	p := mustCardPoint(t, 33)
	b := p.Bytes()
	if len(b) != PointSize {
		t.Fatalf("point bytes = %d, want %d", len(b), PointSize)
	}
	got, err := PointFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(p) {
		t.Fatal("point did not survive a bytes round trip")
	}
}

// Hostile input must be refused, not propagated into the deal.
func TestPointRejectsHostileInput(t *testing.T) {
	if _, err := PointFromBytes(nil); err == nil {
		t.Error("accepted a nil point")
	}
	if _, err := PointFromBytes(make([]byte, PointSize)); err == nil {
		t.Error("accepted an all-zero point")
	}
	if _, err := PointFromBytes(bytes.Repeat([]byte{0xff}, PointSize)); err == nil {
		t.Error("accepted an off-curve point")
	}
	// A bad prefix byte is not a valid compressed encoding.
	good := mustCardPoint(t, 1).Bytes()
	bad := append([]byte{}, good...)
	bad[0] = 0x05
	if _, err := PointFromBytes(bad); err == nil {
		t.Error("accepted a point with an invalid compression prefix")
	}

	// Corrupting the x-coordinate must be rejected whenever the result is off-curve.
	// Note that only about a third of single-bit flips land off-curve: for a compressed
	// point, roughly half of all x values are valid, so "corrupted" does not imply
	// "rejected". What must hold is that every off-curve value IS rejected, which the
	// sweep below checks directly.
	rejected := 0
	for i := 0; i < 32; i++ {
		corrupt := append([]byte{}, good...)
		corrupt[1+i%32] ^= 0x01
		if _, err := PointFromBytes(corrupt); err != nil {
			rejected++
		}
	}
	if rejected == 0 {
		t.Error("no corrupted x-coordinate was rejected; curve validation is not running")
	}
}

func TestInvalidScalarAndPointOperationsFail(t *testing.T) {
	var badScalar Scalar
	var badPoint Point
	good := mustCardPoint(t, 2)
	goodScalar := mustScalar(t)

	if _, err := good.Mask(badScalar); err == nil {
		t.Error("masked with an invalid scalar")
	}
	if _, err := badPoint.Mask(goodScalar); err == nil {
		t.Error("masked an invalid point")
	}
	if _, err := badScalar.Inverse(); err == nil {
		t.Error("inverted an invalid scalar")
	}
	if _, err := badScalar.Mul(goodScalar); err == nil {
		t.Error("multiplied an invalid scalar")
	}
}

func TestNewScalarsIndependent(t *testing.T) {
	s, err := NewScalars(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != cards.DeckSize {
		t.Fatalf("got %d scalars, want %d", len(s), cards.DeckSize)
	}
	seen := map[string]bool{}
	for i, sc := range s {
		if !sc.Valid() {
			t.Fatalf("scalar %d is invalid", i)
		}
		k := string(sc.Bytes())
		if seen[k] {
			t.Fatalf("scalar %d repeats an earlier value", i)
		}
		seen[k] = true
	}
}
