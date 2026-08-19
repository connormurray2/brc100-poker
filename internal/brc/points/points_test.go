package points

import (
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
)

func newWallet(t *testing.T) *Wallet {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWallet(k)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func dealProtocol() Protocol {
	return Protocol{Level: LevelApp, Name: "mental-poker-deal"}
}

func cardPoint(t *testing.T, i int) string {
	t.Helper()
	p, err := mentalpoker.CardPoint(i)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ec.PublicKeyFromBytes(p.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return pub.ToDERHex()
}

// The property the whole proposal exists to enable: masks applied by independent wallets commute, so
// they can be stripped in any order. Without this, mental poker is impossible.
func TestMultiplicationCommutesAcrossWallets(t *testing.T) {
	a, b := newWallet(t), newWallet(t)
	base := cardPoint(t, 17)

	args := func(point string) MultiplyArgs {
		return MultiplyArgs{
			DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"},
			Point:      point,
		}
	}

	// a then b
	ab, err := a.MultiplyPoint(args(base))
	if err != nil {
		t.Fatal(err)
	}
	ab, err = b.MultiplyPoint(args(ab))
	if err != nil {
		t.Fatal(err)
	}

	// b then a
	ba, err := b.MultiplyPoint(args(base))
	if err != nil {
		t.Fatal(err)
	}
	ba, err = a.MultiplyPoint(args(ba))
	if err != nil {
		t.Fatal(err)
	}

	if ab != ba {
		t.Fatal("masks from independent wallets do not commute; the primitive is unusable for mental poker")
	}
}

// invert must recover the original point, or a mask could be applied and never removed.
func TestInvertRemovesTheMask(t *testing.T) {
	w := newWallet(t)
	base := cardPoint(t, 5)

	masked, err := w.MultiplyPoint(MultiplyArgs{
		DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"},
		Point:      base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if masked == base {
		t.Fatal("multiplication did not change the point")
	}

	back, err := w.MultiplyPoint(MultiplyArgs{
		DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"},
		Point:      masked,
		Invert:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if back != base {
		t.Fatal("inverting did not recover the original point")
	}
}

// A full masked deal: three wallets mask, then unmask in a different order.
func TestThreeWayMaskAndUnmaskInAnyOrder(t *testing.T) {
	ws := []*Wallet{newWallet(t), newWallet(t), newWallet(t)}
	base := cardPoint(t, 42)

	mask := func(w *Wallet, p string, invert bool) string {
		t.Helper()
		out, err := w.MultiplyPoint(MultiplyArgs{
			DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"},
			Point:      p, Invert: invert,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	masked := base
	for _, w := range ws {
		masked = mask(w, masked, false)
	}

	// Strip in reverse, then in forward order: both must recover the card.
	for _, order := range [][]int{{2, 1, 0}, {0, 1, 2}, {1, 0, 2}} {
		got := masked
		for _, i := range order {
			got = mask(ws[i], got, true)
		}
		if got != base {
			t.Fatalf("stripping in order %v did not recover the point", order)
		}
	}
}

// Key separation: the same point under different derivations must produce unrelated results, or a
// protocol could not use one wallet for two independent hands.
func TestDerivationSeparatesKeys(t *testing.T) {
	w := newWallet(t)
	base := cardPoint(t, 9)

	run := func(protoName, keyID, counterparty string) string {
		t.Helper()
		out, err := w.MultiplyPoint(MultiplyArgs{
			DeriveArgs: DeriveArgs{
				Protocol:     Protocol{Level: LevelApp, Name: protoName},
				KeyID:        keyID,
				Counterparty: counterparty,
			},
			Point: base,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	baseline := run("mental-poker-deal", "hand-1", "")
	for name, got := range map[string]string{
		"different keyID":    run("mental-poker-deal", "hand-2", ""),
		"different protocol": run("some-other-protocol", "hand-1", ""),
	} {
		if got == baseline {
			t.Errorf("%s produced the same result; keys are not separated", name)
		}
	}

	// The same derivation must be stable, or a mask could not be removed later.
	if run("mental-poker-deal", "hand-1", "") != baseline {
		t.Error("the same derivation produced different results")
	}
}

// The derived point must match what multiplying the generator gives, so a counterparty can verify
// which key was used.
func TestDerivePointMatchesTheKeyUsed(t *testing.T) {
	w := newWallet(t)
	args := DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"}

	point, err := w.DerivePoint(args)
	if err != nil {
		t.Fatal(err)
	}
	if point == "" {
		t.Fatal("no point was derived")
	}

	// Multiplying the generator by the same derived key must give the same point.
	gen := cardPoint(t, 0) // card 0 is 1*G, the generator
	viaMultiply, err := w.MultiplyPoint(MultiplyArgs{DeriveArgs: args, Point: gen})
	if err != nil {
		t.Fatal(err)
	}
	if viaMultiply != point {
		t.Fatalf("derivePoint gave %s but multiplying G gave %s", point, viaMultiply)
	}
}

// Invalid-point rejection is load-bearing: accepting an off-curve point is the standard
// invalid-curve attack and would leak structure about the scalar.
func TestInvalidPointsAreRejected(t *testing.T) {
	w := newWallet(t)
	args := DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"}

	for name, point := range map[string]string{
		"empty":      "",
		"not hex":    "zzzz",
		"too short":  "02aabb",
		"all zeros":  strings.Repeat("00", PointSize),
		"off curve":  "02" + strings.Repeat("ff", 32),
		"bad prefix": "05" + strings.Repeat("11", 32),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := w.MultiplyPoint(MultiplyArgs{DeriveArgs: args, Point: point}); err == nil {
				t.Fatal("an invalid point was accepted")
			}
		})
	}
}

func TestProtocolValidation(t *testing.T) {
	w := newWallet(t)
	base := cardPoint(t, 1)

	tests := map[string]DeriveArgs{
		"level too high":   {Protocol: Protocol{Level: 3, Name: "valid-name"}, KeyID: "k"},
		"level negative":   {Protocol: Protocol{Level: -1, Name: "valid-name"}, KeyID: "k"},
		"name too short":   {Protocol: Protocol{Level: LevelApp, Name: "abc"}, KeyID: "k"},
		"name too long":    {Protocol: Protocol{Level: LevelApp, Name: strings.Repeat("a", 401)}, KeyID: "k"},
		"no key id":        {Protocol: dealProtocol()},
		"key id too long":  {Protocol: dealProtocol(), KeyID: strings.Repeat("k", 801)},
		"bad counterparty": {Protocol: dealProtocol(), KeyID: "k", Counterparty: "not-a-key"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := w.MultiplyPoint(MultiplyArgs{DeriveArgs: args, Point: base}); err == nil {
				t.Fatal("invalid derivation arguments were accepted")
			}
			if _, err := w.DerivePoint(args); err == nil {
				t.Fatal("derivePoint accepted invalid arguments")
			}
		})
	}
}

func TestCounterpartyScopesTheKey(t *testing.T) {
	w := newWallet(t)
	other, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	base := cardPoint(t, 3)

	self, err := w.MultiplyPoint(MultiplyArgs{
		DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "k"},
		Point:      base,
	})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := w.MultiplyPoint(MultiplyArgs{
		DeriveArgs: DeriveArgs{
			Protocol: dealProtocol(), KeyID: "k",
			Counterparty: other.PubKey().ToDERHex(),
		},
		Point: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if self == scoped {
		t.Fatal("a counterparty-scoped key matched the self key")
	}
}

func TestNewWalletRequiresARootKey(t *testing.T) {
	if _, err := NewWallet(nil); err == nil {
		t.Fatal("built a wallet with no root key")
	}
}

// Two wallets must never derive the same protocol key, or one player could read another's cards.
func TestDifferentWalletsDeriveDifferentKeys(t *testing.T) {
	a, b := newWallet(t), newWallet(t)
	args := DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"}

	pa, err := a.DerivePoint(args)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.DerivePoint(args)
	if err != nil {
		t.Fatal(err)
	}
	if pa == pb {
		t.Fatal("two wallets derived the same protocol key")
	}
}

// The primitive must be usable for a real deal: mask the whole deck, then unmask one position.
func TestWholeDeckMaskAndSelectiveUnmask(t *testing.T) {
	ws := []*Wallet{newWallet(t), newWallet(t)}
	const deckSize = 52

	// Mask every card with every wallet, as a shuffle pass would.
	deck := make([]string, deckSize)
	for i := 0; i < deckSize; i++ {
		deck[i] = cardPoint(t, i)
	}
	for _, w := range ws {
		for i := range deck {
			out, err := w.MultiplyPoint(MultiplyArgs{
				DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"},
				Point:      deck[i],
			})
			if err != nil {
				t.Fatal(err)
			}
			deck[i] = out
		}
	}

	// Unmask one position only: the rest stay masked, which is what deals a single card.
	const pos = 20
	got := deck[pos]
	for _, w := range ws {
		out, err := w.MultiplyPoint(MultiplyArgs{
			DeriveArgs: DeriveArgs{Protocol: dealProtocol(), KeyID: "hand-1"},
			Point:      got, Invert: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		got = out
	}
	if got != cardPoint(t, pos) {
		t.Fatal("unmasking one position did not recover its card")
	}
	// Another position must still be unreadable.
	if deck[0] == cardPoint(t, 0) {
		t.Fatal("an unrevealed position was left unmasked")
	}
}
