// Package proof closes two cheating vectors in the deal that the deal protocol alone leaves open.
//
// The mental-poker deal keeps cards private, but privacy is not honesty: a player who applies a
// transformation other than the one they claim can bias the deck, and a player who can predict
// the seat order can grind keys until they get the position they want. Both are addressed the
// same way — commit before, reveal after — so cheating is detectable and attributable rather
// than merely possible to worry about.
package proof

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/cmurray/brc100-poker/internal/game/mentalpoker"
)

// Domain separators. Distinct prefixes stop a commitment made for one purpose from being
// replayed as a commitment for another.
var (
	domainShuffle   = []byte("brc100-poker/proof/shuffle/v1")
	domainRemask    = []byte("brc100-poker/proof/remask/v1")
	domainSeatNonce = []byte("brc100-poker/proof/seat-nonce/v1")
	domainSeatSeed  = []byte("brc100-poker/proof/seat-seed/v1")
	domainSeatOrder = []byte("brc100-poker/proof/seat-order/v1")
)

// CommitmentSize is the byte length of a commitment.
const CommitmentSize = sha256.Size

// hasher accumulates length-prefixed fields.
//
// Length-prefixing matters: concatenating raw fields lets two different inputs produce the same
// byte stream, so a commitment to one transformation could open as a commitment to another. The
// upstream implementation concatenated without prefixes; this does not.
type hasher struct {
	h [][]byte
}

func (w *hasher) field(b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	w.h = append(w.h, n[:], b)
}

func (w *hasher) sum() []byte {
	h := sha256.New()
	for _, part := range w.h {
		_, _ = h.Write(part)
	}
	return h.Sum(nil)
}

// CommitShuffle commits to a shuffle pass: one global scalar and a permutation.
//
// Published before the deal begins, so the player is bound to the transformation they will apply
// before they can see anyone else's contribution.
func CommitShuffle(global mentalpoker.Scalar, perm mentalpoker.Permutation) ([]byte, error) {
	if !global.Valid() {
		return nil, errors.New("proof: cannot commit to an invalid scalar")
	}
	if len(perm) == 0 {
		return nil, errors.New("proof: cannot commit to an empty permutation")
	}
	w := &hasher{}
	w.field(domainShuffle)
	w.field(global.Bytes())
	w.field(encodePerm(perm))
	return w.sum(), nil
}

// CommitRemask commits to a remask pass: the global scalar being removed and the per-position
// scalars replacing it.
func CommitRemask(global mentalpoker.Scalar, perPosition []mentalpoker.Scalar) ([]byte, error) {
	if !global.Valid() {
		return nil, errors.New("proof: cannot commit to an invalid scalar")
	}
	if len(perPosition) == 0 {
		return nil, errors.New("proof: cannot commit to an empty scalar set")
	}
	w := &hasher{}
	w.field(domainRemask)
	w.field(global.Bytes())
	for i, s := range perPosition {
		if !s.Valid() {
			return nil, fmt.Errorf("proof: per-position scalar %d is invalid", i)
		}
		w.field(s.Bytes())
	}
	return w.sum(), nil
}

// VerifyShuffle checks a revealed shuffle pass against its commitment.
//
// Four things must hold: the opening matches the commitment, the permutation is a genuine
// bijection, recomputing the transformation on the known input reproduces the claimed output
// exactly, and the output is a well-formed deck. Together these mean a player cannot claim one
// transformation and apply another.
func VerifyShuffle(input, claimed mentalpoker.Deck, global mentalpoker.Scalar, perm mentalpoker.Permutation, commitment []byte) error {
	want, err := CommitShuffle(global, perm)
	if err != nil {
		return fmt.Errorf("proof: the opening is not well-formed: %w", err)
	}
	// Constant time, so a mismatch cannot be located byte by byte.
	if subtle.ConstantTimeCompare(want, commitment) != 1 {
		return errors.New("proof: the revealed shuffle does not match its commitment")
	}
	if err := mentalpoker.ValidatePermutation(perm, input.Size()); err != nil {
		return fmt.Errorf("proof: the revealed permutation is not valid: %w", err)
	}

	recomputed, err := input.ShuffleStep(global, perm)
	if err != nil {
		return fmt.Errorf("proof: recomputing the shuffle: %w", err)
	}
	if !recomputed.Equal(claimed) {
		return errors.New("proof: the claimed shuffle output does not match the committed transformation")
	}
	return nil
}

// VerifyRemask checks a revealed remask pass against its commitment.
func VerifyRemask(input, claimed mentalpoker.Deck, global mentalpoker.Scalar, perPosition []mentalpoker.Scalar, commitment []byte) error {
	want, err := CommitRemask(global, perPosition)
	if err != nil {
		return fmt.Errorf("proof: the opening is not well-formed: %w", err)
	}
	if subtle.ConstantTimeCompare(want, commitment) != 1 {
		return errors.New("proof: the revealed remask does not match its commitment")
	}
	if len(perPosition) != input.Size() {
		return fmt.Errorf("proof: %d per-position scalars for a %d-card deck", len(perPosition), input.Size())
	}

	recomputed, err := input.RemaskStep(global, perPosition)
	if err != nil {
		return fmt.Errorf("proof: recomputing the remask: %w", err)
	}
	if !recomputed.Equal(claimed) {
		return errors.New("proof: the claimed remask output does not match the committed transformation")
	}
	return nil
}

func encodePerm(p mentalpoker.Permutation) []byte {
	out := make([]byte, len(p)*4)
	for i, v := range p {
		binary.BigEndian.PutUint32(out[i*4:], uint32(v)) //nolint:gosec // permutation entries are bounded by deck size
	}
	return out
}

// --- seat ordering ---------------------------------------------------------

// NonceSize is the byte length of a seat-order nonce.
const NonceSize = 32

// CommitSeatNonce commits to a player's seat-order nonce.
//
// Seats are not assigned by sorted public key, which a player could grind by generating many
// keys until one sorts where they want. Instead every player commits to a random nonce before
// the order is decided; the order derives from all revealed nonces, so no player can predict or
// bias their own position.
func CommitSeatNonce(nonce []byte) ([]byte, error) {
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("proof: a seat nonce must be %d bytes, got %d", NonceSize, len(nonce))
	}
	w := &hasher{}
	w.field(domainSeatNonce)
	w.field(nonce)
	return w.sum(), nil
}

// VerifySeatNonce checks a revealed nonce against its commitment.
func VerifySeatNonce(commitment, nonce []byte) error {
	want, err := CommitSeatNonce(nonce)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(want, commitment) != 1 {
		return errors.New("proof: the revealed seat nonce does not match its commitment")
	}
	return nil
}

// SeatReveal is one player's opened seat-order commitment.
type SeatReveal struct {
	// IdentityKey is the player's public key, hex-encoded.
	IdentityKey string
	Nonce       []byte
}

// JointSeed derives the shared seed from every player's revealed nonce.
//
// Sorted by identity key so every player computes the same seed regardless of the order reveals
// arrived in, and dependent on every nonce so no single player controls it.
func JointSeed(reveals []SeatReveal) ([]byte, error) {
	if len(reveals) == 0 {
		return nil, errors.New("proof: no reveals to derive a seed from")
	}

	sorted := make([]SeatReveal, len(reveals))
	copy(sorted, reveals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].IdentityKey < sorted[j].IdentityKey })

	seen := make(map[string]struct{}, len(sorted))
	w := &hasher{}
	w.field(domainSeatSeed)
	for _, r := range sorted {
		if r.IdentityKey == "" {
			return nil, errors.New("proof: a reveal has no identity key")
		}
		if len(r.Nonce) != NonceSize {
			return nil, fmt.Errorf("proof: %s… revealed a %d-byte nonce", short(r.IdentityKey), len(r.Nonce))
		}
		// A duplicated identity would let one player contribute twice and so weight the
		// seed towards a value they control.
		if _, dup := seen[r.IdentityKey]; dup {
			return nil, fmt.Errorf("proof: %s… appears twice in the reveals", short(r.IdentityKey))
		}
		seen[r.IdentityKey] = struct{}{}
		w.field([]byte(r.IdentityKey))
		w.field(r.Nonce)
	}
	return w.sum(), nil
}

// AssignSeats orders players by H(seed ‖ identity), which is unpredictable before the seed
// exists and identical for everyone once it does.
//
// Returns the identity keys in seat order: index 0 is seat 0.
func AssignSeats(identityKeys []string, jointSeed []byte) ([]string, error) {
	if len(identityKeys) == 0 {
		return nil, errors.New("proof: no players to seat")
	}
	if len(jointSeed) != CommitmentSize {
		return nil, fmt.Errorf("proof: a joint seed must be %d bytes, got %d", CommitmentSize, len(jointSeed))
	}

	type scored struct {
		key   string
		score string
	}
	out := make([]scored, 0, len(identityKeys))
	seen := make(map[string]struct{}, len(identityKeys))
	for _, k := range identityKeys {
		if k == "" {
			return nil, errors.New("proof: a player has no identity key")
		}
		if _, dup := seen[k]; dup {
			return nil, fmt.Errorf("proof: %s… appears twice", short(k))
		}
		seen[k] = struct{}{}

		w := &hasher{}
		w.field(domainSeatOrder)
		w.field(jointSeed)
		w.field([]byte(k))
		out = append(out, scored{key: k, score: string(w.sum())})
	}

	// Ties are impossible in practice but the comparison stays total anyway, so the result
	// is deterministic rather than dependent on sort stability.
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].key < out[j].key
	})

	keys := make([]string, len(out))
	for i, s := range out {
		keys[i] = s.key
	}
	return keys, nil
}

func short(key string) string {
	const n = 12
	if len(key) <= n {
		return key
	}
	return key[:n]
}
