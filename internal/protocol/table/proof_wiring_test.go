package table

import (
	"context"
	"strings"
	"testing"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
)

// A seat's commitments must be stable: it is bound to one transformation before the deal begins.
func TestCommitmentsAreStableAndBinding(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]

	s1, r1, err := p.Commitments()
	if err != nil {
		t.Fatal(err)
	}
	s2, r2, err := p.Commitments()
	if err != nil {
		t.Fatal(err)
	}
	if string(s1) != string(s2) || string(r1) != string(r2) {
		t.Fatal("a seat's commitments changed between calls; it would be bound to nothing")
	}
	if len(s1) == 0 || len(r1) == 0 {
		t.Fatal("a commitment is empty")
	}
	if string(s1) == string(r1) {
		t.Fatal("the shuffle and remask commitments are identical")
	}
}

// The shuffle a seat actually applies must be the one it committed to. Generating a fresh
// permutation at shuffle time would make the commitment bind nothing at all.
func TestAppliedShuffleMatchesTheCommitment(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]

	shuffleCommit, remaskCommit, err := p.Commitments()
	if err != nil {
		t.Fatal(err)
	}
	// A seat records its own published commitment alongside its peers', so the same
	// verification path covers every seat including itself.
	if err := p.RecordPeerCommitments(0, shuffleCommit, remaskCommit); err != nil {
		t.Fatal(err)
	}

	// Record what seat 0 is handed, then let it contribute.
	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordInput(0, base)

	if err := p.StartShuffle(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	// Open the seat's own pass and verify it against the commitment it published.
	global, perm := p.openShuffle()
	if err := p.VerifyPeerShuffle(0, global, perm, p.Deck()); err != nil {
		// Seat 0 verifying itself runs exactly the check any peer would run.
		t.Fatalf("a seat's own shuffle did not verify against its commitment: %v", err)
	}
}

// A pass from a seat that never committed cannot be verified, so it must be refused rather than
// trusted — that is what makes the proof binding during play instead of optional.
func TestUncommittedSeatsPassIsRefused(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]

	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordInput(1, base)

	global, perm := env.players[1].openShuffle()
	err = p.VerifyPeerShuffle(1, global, perm, base)
	if err == nil {
		t.Fatal("a pass from a seat with no commitment was accepted")
	}
	if !strings.Contains(err.Error(), "no shuffle commitment") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// A seat must not be able to replace its commitment after seeing others' contributions.
func TestCommitmentCannotBeChanged(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]

	first, firstRemask, err := env.players[1].Commitments()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RecordPeerCommitments(1, first, firstRemask); err != nil {
		t.Fatal(err)
	}
	// Re-recording the same commitments is idempotent.
	if err := p.RecordPeerCommitments(1, first, firstRemask); err != nil {
		t.Fatalf("re-recording identical commitments failed: %v", err)
	}

	// A different one must be refused.
	altered := append([]byte{}, first...)
	altered[0] ^= 0x01
	err = p.RecordPeerCommitments(1, altered, firstRemask)
	if err == nil {
		t.Fatal("a seat changed its shuffle commitment")
	}
	if !strings.Contains(err.Error(), "change its shuffle commitment") {
		t.Errorf("unclear refusal: %v", err)
	}
}

// A dishonest shuffler is caught: the seat commits to one transformation and applies another.
func TestDishonestShufflerIsCaughtAndAttributed(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	verifier := env.players[0]
	cheat := env.players[1]

	commit, remaskCommit, err := cheat.Commitments()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.RecordPeerCommitments(1, commit, remaskCommit); err != nil {
		t.Fatal(err)
	}

	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	verifier.RecordInput(1, base)

	// The cheat applies a scalar other than the committed one.
	otherGlobal, err := mentalpoker.NewScalar()
	if err != nil {
		t.Fatal(err)
	}
	_, perm := cheat.openShuffle()
	cheated, err := base.ShuffleStep(otherGlobal, perm)
	if err != nil {
		t.Fatal(err)
	}

	committedGlobal, _ := cheat.openShuffle()
	err = verifier.VerifyPeerShuffle(1, committedGlobal, perm, cheated)
	if err == nil {
		t.Fatal("a shuffle that used a different scalar than committed was accepted")
	}
	// The failure must name the seat, so a stall or a dispute is attributable.
	if !strings.Contains(err.Error(), "seat 1") {
		t.Errorf("the failure does not attribute the cheat: %v", err)
	}
}

func TestRemaskProofWiring(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]

	_, remaskCommit, err := p.Commitments()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RecordPeerCommitments(0, mustShuffleCommit(t, p), remaskCommit); err != nil {
		t.Fatal(err)
	}

	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordInput(0, base)

	global, perPosition := p.openRemask()
	out, err := base.RemaskStep(global, perPosition)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyPeerRemask(0, global, perPosition, out); err != nil {
		t.Fatalf("an honest remask did not verify: %v", err)
	}

	// Alter one position's scalar: a single changed card is enough to bias a deal.
	tampered := append([]mentalpoker.Scalar{}, perPosition...)
	tampered[3], err = mentalpoker.NewScalar()
	if err != nil {
		t.Fatal(err)
	}
	cheated, err := base.RemaskStep(global, tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyPeerRemask(0, global, perPosition, cheated); err == nil {
		t.Fatal("a remask that altered one position was accepted")
	}
}

func TestCommittedSeatsTracking(t *testing.T) {
	env := newHandEnv(t, 3, cards.DeckSize)
	p := env.players[0]

	if len(p.CommittedSeats()) != 0 {
		t.Fatal("seats were reported as committed before any commitment")
	}
	for _, seat := range []int{2, 1} {
		s, r, err := env.players[seat].Commitments()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.RecordPeerCommitments(seat, s, r); err != nil {
			t.Fatal(err)
		}
	}
	got := p.CommittedSeats()
	// Sorted, so a deal can compare against the seat list without normalising.
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("committed seats = %v, want [1 2]", got)
	}
}

func TestRecordPeerCommitmentsValidation(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]
	s, r, err := env.players[1].Commitments()
	if err != nil {
		t.Fatal(err)
	}

	if err := p.RecordPeerCommitments(-1, s, r); err == nil {
		t.Error("accepted a negative seat")
	}
	if err := p.RecordPeerCommitments(9, s, r); err == nil {
		t.Error("accepted a seat outside the table")
	}
	if err := p.RecordPeerCommitments(1, []byte{1, 2}, r); err == nil {
		t.Error("accepted a malformed shuffle commitment")
	}
	if err := p.RecordPeerCommitments(1, s, nil); err == nil {
		t.Error("accepted a missing remask commitment")
	}
}

// Verifying a pass needs the deck that seat was handed, not whatever the deck looks like now.
func TestVerifyRequiresTheRecordedInput(t *testing.T) {
	env := newHandEnv(t, 2, cards.DeckSize)
	p := env.players[0]
	s, r, err := env.players[1].Commitments()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RecordPeerCommitments(1, s, r); err != nil {
		t.Fatal(err)
	}

	global, perm := env.players[1].openShuffle()
	base, err := mentalpoker.BaseDeck(cards.DeckSize)
	if err != nil {
		t.Fatal(err)
	}
	err = p.VerifyPeerShuffle(1, global, perm, base)
	if err == nil {
		t.Fatal("verified a pass with no record of the seat's input")
	}
	if !strings.Contains(err.Error(), "no record of the deck") {
		t.Errorf("unclear refusal: %v", err)
	}
}

func mustShuffleCommit(t *testing.T, p *HandPlayer) []byte {
	t.Helper()
	s, _, err := p.Commitments()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
