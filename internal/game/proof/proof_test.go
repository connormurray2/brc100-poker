package proof

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
)

func mustScalar(t *testing.T) mentalpoker.Scalar {
	t.Helper()
	s, err := mentalpoker.NewScalar()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustPerm(t *testing.T, n int) mentalpoker.Permutation {
	t.Helper()
	p, err := mentalpoker.NewPermutation(n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustDeck(t *testing.T, n int) mentalpoker.Deck {
	t.Helper()
	d, err := mentalpoker.BaseDeck(n)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func nonce(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, NonceSize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// --- shuffle proof ---------------------------------------------------------

func TestHonestShuffleVerifies(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	global, perm := mustScalar(t), mustPerm(t, n)

	commitment, err := CommitShuffle(global, perm)
	if err != nil {
		t.Fatal(err)
	}
	output, err := input.ShuffleStep(global, perm)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyShuffle(input, output, global, perm, commitment); err != nil {
		t.Fatalf("an honest shuffle was rejected: %v", err)
	}
}

// The cheating vector this closes: a player commits to one transformation and applies another,
// biasing the deck while the deal still looks well-formed.
func TestShuffleWithADifferentTransformationIsCaught(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	committedGlobal, committedPerm := mustScalar(t), mustPerm(t, n)
	commitment, err := CommitShuffle(committedGlobal, committedPerm)
	if err != nil {
		t.Fatal(err)
	}

	// The player actually applies a different scalar.
	cheatGlobal := mustScalar(t)
	cheated, err := input.ShuffleStep(cheatGlobal, committedPerm)
	if err != nil {
		t.Fatal(err)
	}

	// Claiming the committed opening against the cheated output fails.
	err = VerifyShuffle(input, cheated, committedGlobal, committedPerm, commitment)
	if err == nil {
		t.Fatal("a shuffle that used a different scalar than committed was accepted")
	}
	if !strings.Contains(err.Error(), "does not match the committed transformation") {
		t.Errorf("unclear rejection: %v", err)
	}

	// Opening with the scalar actually used fails too, because it does not match the
	// commitment.
	err = VerifyShuffle(input, cheated, cheatGlobal, committedPerm, commitment)
	if err == nil {
		t.Fatal("an opening that disagrees with the commitment was accepted")
	}
	if !strings.Contains(err.Error(), "does not match its commitment") {
		t.Errorf("unclear rejection: %v", err)
	}
}

func TestShuffleWithADifferentPermutationIsCaught(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	global := mustScalar(t)
	committedPerm := mustPerm(t, n)
	commitment, err := CommitShuffle(global, committedPerm)
	if err != nil {
		t.Fatal(err)
	}

	other := mustPerm(t, n)
	cheated, err := input.ShuffleStep(global, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyShuffle(input, cheated, global, committedPerm, commitment); err == nil {
		t.Fatal("a shuffle that permuted differently than committed was accepted")
	}
}

// A malformed permutation would duplicate one card and drop another.
func TestShuffleProofRejectsAMalformedPermutation(t *testing.T) {
	const n = 8
	input := mustDeck(t, n)
	global := mustScalar(t)
	bad := mentalpoker.Permutation{0, 1, 1, 3, 4, 5, 6, 7} // 1 twice, 2 missing

	commitment, err := CommitShuffle(global, bad)
	if err != nil {
		t.Fatal(err)
	}
	// The opening matches its commitment, so only the bijection check catches this.
	if err := VerifyShuffle(input, input, global, bad, commitment); err == nil {
		t.Fatal("a malformed permutation was accepted")
	}
}

func TestShuffleProofRejectsATamperedCommitment(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	global, perm := mustScalar(t), mustPerm(t, n)
	commitment, err := CommitShuffle(global, perm)
	if err != nil {
		t.Fatal(err)
	}
	output, err := input.ShuffleStep(global, perm)
	if err != nil {
		t.Fatal(err)
	}

	tampered := append([]byte{}, commitment...)
	tampered[0] ^= 0x01
	if err := VerifyShuffle(input, output, global, perm, tampered); err == nil {
		t.Fatal("a tampered commitment was accepted")
	}
	if err := VerifyShuffle(input, output, global, perm, nil); err == nil {
		t.Fatal("a missing commitment was accepted")
	}
}

// --- remask proof ----------------------------------------------------------

func TestHonestRemaskVerifies(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	global := mustScalar(t)
	perPosition, err := mentalpoker.NewScalars(n)
	if err != nil {
		t.Fatal(err)
	}

	commitment, err := CommitRemask(global, perPosition)
	if err != nil {
		t.Fatal(err)
	}
	output, err := input.RemaskStep(global, perPosition)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRemask(input, output, global, perPosition, commitment); err != nil {
		t.Fatalf("an honest remask was rejected: %v", err)
	}
}

func TestRemaskWithDifferentScalarsIsCaught(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	global := mustScalar(t)
	committed, err := mentalpoker.NewScalars(n)
	if err != nil {
		t.Fatal(err)
	}
	commitment, err := CommitRemask(global, committed)
	if err != nil {
		t.Fatal(err)
	}

	// Swap one position's scalar: a single altered card is enough to bias a deal.
	cheat := append([]mentalpoker.Scalar{}, committed...)
	cheat[7] = mustScalar(t)
	cheated, err := input.RemaskStep(global, cheat)
	if err != nil {
		t.Fatal(err)
	}

	if err := VerifyRemask(input, cheated, global, committed, commitment); err == nil {
		t.Fatal("a remask that altered one position was accepted")
	}
}

func TestRemaskProofRejectsWrongScalarCount(t *testing.T) {
	const n = 52
	input := mustDeck(t, n)
	global := mustScalar(t)
	short, err := mentalpoker.NewScalars(n - 1)
	if err != nil {
		t.Fatal(err)
	}
	commitment, err := CommitRemask(global, short)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRemask(input, input, global, short, commitment); err == nil {
		t.Fatal("a remask with too few scalars was accepted")
	}
}

// Domain separation: a shuffle commitment must not open as a remask commitment.
func TestCommitmentsAreDomainSeparated(t *testing.T) {
	const n = 4
	global := mustScalar(t)
	perm := mustPerm(t, n)
	perPosition, err := mentalpoker.NewScalars(n)
	if err != nil {
		t.Fatal(err)
	}

	shuffle, err := CommitShuffle(global, perm)
	if err != nil {
		t.Fatal(err)
	}
	remask, err := CommitRemask(global, perPosition)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(shuffle, remask) {
		t.Fatal("a shuffle and a remask commitment collided")
	}

	seat, err := CommitSeatNonce(nonce(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(seat, shuffle) || bytes.Equal(seat, remask) {
		t.Fatal("a seat commitment collided with a deal commitment")
	}
}

func TestCommitValidation(t *testing.T) {
	var bad mentalpoker.Scalar
	if _, err := CommitShuffle(bad, mustPerm(t, 4)); err == nil {
		t.Error("committed to an invalid scalar")
	}
	if _, err := CommitShuffle(mustScalar(t), nil); err == nil {
		t.Error("committed to an empty permutation")
	}
	if _, err := CommitRemask(bad, []mentalpoker.Scalar{mustScalar(t)}); err == nil {
		t.Error("committed to an invalid global scalar")
	}
	if _, err := CommitRemask(mustScalar(t), nil); err == nil {
		t.Error("committed to an empty scalar set")
	}
	withBad := []mentalpoker.Scalar{mustScalar(t), {}}
	if _, err := CommitRemask(mustScalar(t), withBad); err == nil {
		t.Error("committed to a set containing an invalid scalar")
	}
}

// --- seat ordering ---------------------------------------------------------

func TestHonestSeatNonceVerifies(t *testing.T) {
	n := nonce(t)
	c, err := CommitSeatNonce(n)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySeatNonce(c, n); err != nil {
		t.Fatalf("an honest nonce was rejected: %v", err)
	}
}

// A player must not be able to change their nonce after seeing the others, which is what would
// let them steer the seat order.
func TestChangedSeatNonceIsCaught(t *testing.T) {
	committed := nonce(t)
	c, err := CommitSeatNonce(committed)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySeatNonce(c, nonce(t)); err == nil {
		t.Fatal("a nonce different from the committed one was accepted")
	}
	// Even a single flipped bit.
	flipped := append([]byte{}, committed...)
	flipped[0] ^= 0x01
	if err := VerifySeatNonce(c, flipped); err == nil {
		t.Fatal("a nonce with one flipped bit was accepted")
	}
}

func TestSeatNonceSizeIsEnforced(t *testing.T) {
	if _, err := CommitSeatNonce(nil); err == nil {
		t.Error("committed to an empty nonce")
	}
	if _, err := CommitSeatNonce(make([]byte, 8)); err == nil {
		t.Error("committed to a short nonce")
	}
}

// The joint seed must be identical for every player regardless of the order reveals arrive in.
func TestJointSeedIsOrderIndependent(t *testing.T) {
	reveals := []SeatReveal{
		{IdentityKey: "03cc", Nonce: nonce(t)},
		{IdentityKey: "02aa", Nonce: nonce(t)},
		{IdentityKey: "03bb", Nonce: nonce(t)},
	}
	first, err := JointSeed(reveals)
	if err != nil {
		t.Fatal(err)
	}

	shuffled := []SeatReveal{reveals[2], reveals[0], reveals[1]}
	second, err := JointSeed(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the joint seed depends on the order reveals arrived in")
	}
}

// The seed must depend on every nonce, so no single player controls it.
func TestJointSeedDependsOnEveryNonce(t *testing.T) {
	base := []SeatReveal{
		{IdentityKey: "02aa", Nonce: nonce(t)},
		{IdentityKey: "03bb", Nonce: nonce(t)},
	}
	first, err := JointSeed(base)
	if err != nil {
		t.Fatal(err)
	}

	for i := range base {
		altered := append([]SeatReveal{}, base...)
		altered[i] = SeatReveal{IdentityKey: base[i].IdentityKey, Nonce: nonce(t)}
		got, err := JointSeed(altered)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(first, got) {
			t.Fatalf("changing player %d's nonce did not change the seed", i)
		}
	}
}

// A duplicated identity would let one player contribute twice and weight the seed.
func TestJointSeedRejectsADuplicateIdentity(t *testing.T) {
	n := nonce(t)
	_, err := JointSeed([]SeatReveal{
		{IdentityKey: "02aa", Nonce: n},
		{IdentityKey: "02aa", Nonce: nonce(t)},
	})
	if err == nil {
		t.Fatal("a duplicated identity was accepted")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("unclear rejection: %v", err)
	}
}

func TestJointSeedValidation(t *testing.T) {
	if _, err := JointSeed(nil); err == nil {
		t.Error("derived a seed from no reveals")
	}
	if _, err := JointSeed([]SeatReveal{{Nonce: nonce(t)}}); err == nil {
		t.Error("accepted a reveal with no identity")
	}
	if _, err := JointSeed([]SeatReveal{{IdentityKey: "02aa", Nonce: []byte{1}}}); err == nil {
		t.Error("accepted a short nonce")
	}
}

// Every player must compute the same seat order from the same seed.
func TestSeatAssignmentIsDeterministic(t *testing.T) {
	keys := []string{"02aa", "03bb", "03cc", "02dd"}
	seed, err := JointSeed([]SeatReveal{
		{IdentityKey: "02aa", Nonce: nonce(t)},
		{IdentityKey: "03bb", Nonce: nonce(t)},
		{IdentityKey: "03cc", Nonce: nonce(t)},
		{IdentityKey: "02dd", Nonce: nonce(t)},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := AssignSeats(keys, seed)
	if err != nil {
		t.Fatal(err)
	}
	// Input order must not matter.
	reordered := []string{"03cc", "02dd", "02aa", "03bb"}
	second, err := AssignSeats(reordered, seed)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("seat %d differs between runs: %s vs %s", i, first[i], second[i])
		}
	}
	// Every player is seated exactly once.
	if len(first) != len(keys) {
		t.Fatalf("seated %d players, want %d", len(first), len(keys))
	}
	seen := map[string]bool{}
	for _, k := range first {
		if seen[k] {
			t.Fatalf("%s was seated twice", k)
		}
		seen[k] = true
	}
}

// The order must change when the seed does, or it could be predicted before the seed exists.
func TestSeatAssignmentDependsOnTheSeed(t *testing.T) {
	keys := []string{"02aa", "03bb", "03cc", "02dd", "03ee"}

	seedA, err := JointSeed([]SeatReveal{{IdentityKey: "02aa", Nonce: nonce(t)}})
	if err != nil {
		t.Fatal(err)
	}
	orderA, err := AssignSeats(keys, seedA)
	if err != nil {
		t.Fatal(err)
	}

	// Try several seeds: with five players a collision is possible but not repeatable.
	differed := false
	for i := 0; i < 10; i++ {
		seedB, err := JointSeed([]SeatReveal{{IdentityKey: "02aa", Nonce: nonce(t)}})
		if err != nil {
			t.Fatal(err)
		}
		orderB, err := AssignSeats(keys, seedB)
		if err != nil {
			t.Fatal(err)
		}
		for j := range orderA {
			if orderA[j] != orderB[j] {
				differed = true
			}
		}
		if differed {
			break
		}
	}
	if !differed {
		t.Fatal("the seat order did not change across ten different seeds")
	}
}

// Sorting by public key would let a player grind keys for a position; sorting by H(seed‖key)
// cannot be ground because the seed does not exist when keys are chosen.
func TestSeatOrderIsNotSortedByIdentity(t *testing.T) {
	keys := []string{"02aa", "02bb", "02cc", "02dd", "02ee", "02ff"}
	sortedByKey := true

	for i := 0; i < 20; i++ {
		seed, err := JointSeed([]SeatReveal{{IdentityKey: "02aa", Nonce: nonce(t)}})
		if err != nil {
			t.Fatal(err)
		}
		order, err := AssignSeats(keys, seed)
		if err != nil {
			t.Fatal(err)
		}
		for j := 1; j < len(order); j++ {
			if order[j] < order[j-1] {
				sortedByKey = false
			}
		}
		if !sortedByKey {
			break
		}
	}
	if sortedByKey {
		t.Fatal("seat order tracked identity-key order across twenty seeds; it would be grindable")
	}
}

func TestAssignSeatsValidation(t *testing.T) {
	seed, err := JointSeed([]SeatReveal{{IdentityKey: "02aa", Nonce: nonce(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssignSeats(nil, seed); err == nil {
		t.Error("seated an empty table")
	}
	if _, err := AssignSeats([]string{"02aa"}, []byte{1, 2, 3}); err == nil {
		t.Error("accepted a wrong-length seed")
	}
	if _, err := AssignSeats([]string{"02aa", ""}, seed); err == nil {
		t.Error("accepted a player with no identity")
	}
	if _, err := AssignSeats([]string{"02aa", "02aa"}, seed); err == nil {
		t.Error("accepted a duplicated player")
	}
}

// The whole protocol end to end: commit, reveal, verify, derive, assign.
func TestSeatOrderProtocolEndToEnd(t *testing.T) {
	keys := []string{"02aa", "03bb", "03cc"}
	nonces := make(map[string][]byte, len(keys))
	commitments := make(map[string][]byte, len(keys))

	// Everyone commits before the order is decided.
	for _, k := range keys {
		n := nonce(t)
		c, err := CommitSeatNonce(n)
		if err != nil {
			t.Fatal(err)
		}
		nonces[k], commitments[k] = n, c
	}

	// Then reveals, each checked against its commitment.
	var reveals []SeatReveal
	for _, k := range keys {
		if err := VerifySeatNonce(commitments[k], nonces[k]); err != nil {
			t.Fatalf("%s's reveal did not verify: %v", k, err)
		}
		reveals = append(reveals, SeatReveal{IdentityKey: k, Nonce: nonces[k]})
	}

	seed, err := JointSeed(reveals)
	if err != nil {
		t.Fatal(err)
	}
	order, err := AssignSeats(keys, seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != len(keys) {
		t.Fatalf("seated %d of %d players", len(order), len(keys))
	}
	t.Logf("seat order: %v", order)
}

// A full deal with proofs: every pass is committed, then verified against its opening.
func TestDealWithProofsVerifies(t *testing.T) {
	const seats = 3
	const n = cards.DeckSize

	type player struct {
		global      mentalpoker.Scalar
		perm        mentalpoker.Permutation
		perPosition []mentalpoker.Scalar
		shuffleC    []byte
		remaskC     []byte
	}

	players := make([]*player, seats)
	for i := range players {
		p := &player{global: mustScalar(t), perm: mustPerm(t, n)}
		pp, err := mentalpoker.NewScalars(n)
		if err != nil {
			t.Fatal(err)
		}
		p.perPosition = pp
		if p.shuffleC, err = CommitShuffle(p.global, p.perm); err != nil {
			t.Fatal(err)
		}
		if p.remaskC, err = CommitRemask(p.global, p.perPosition); err != nil {
			t.Fatal(err)
		}
		players[i] = p
	}

	// Shuffle chain, verifying each pass against its commitment.
	deck := mustDeck(t, n)
	for i, p := range players {
		out, err := deck.ShuffleStep(p.global, p.perm)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyShuffle(deck, out, p.global, p.perm, p.shuffleC); err != nil {
			t.Fatalf("seat %d's shuffle did not verify: %v", i, err)
		}
		deck = out
	}

	// Remask chain, likewise.
	for i, p := range players {
		out, err := deck.RemaskStep(p.global, p.perPosition)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyRemask(deck, out, p.global, p.perPosition, p.remaskC); err != nil {
			t.Fatalf("seat %d's remask did not verify: %v", i, err)
		}
		deck = out
	}

	if deck.Size() != n {
		t.Fatalf("the final deck has %d positions, want %d", deck.Size(), n)
	}
}
