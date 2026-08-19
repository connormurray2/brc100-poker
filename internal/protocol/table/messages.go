package table

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Kind identifies a game message.
//
// The set is deliberately small and ordered: each kind belongs to exactly one phase, which is
// what lets a receiver reject a message that arrives for a step that is not current rather
// than applying it early.
type Kind string

const (
	// KindHello announces a seat. The transport echoes it back to the sender, which is
	// how a seat learns its own seat index — and why a fan-out that excludes the sender
	// breaks seating permanently.
	KindHello Kind = "hello"

	// KindShuffle carries one seat's shuffle contribution.
	KindShuffle Kind = "shuffle"
	// KindRemask carries one seat's per-position re-masking.
	KindRemask Kind = "remask"
	// KindShuffleProof carries the commitment that constrains a shuffler during play,
	// rather than only auditing them afterwards.
	KindShuffleProof Kind = "shuffleProof"

	// KindHoleReveal privately discloses one position's scalar to one recipient.
	KindHoleReveal Kind = "holeReveal"
	// KindBoardReveal publicly discloses one position's scalar to everyone.
	KindBoardReveal Kind = "boardReveal"
	// KindShowdownReveal discloses a seat's hole scalars at showdown.
	KindShowdownReveal Kind = "showdownReveal"

	// KindAction is a betting action.
	KindAction Kind = "action"

	// KindSettlementProposal asks the seats to sign a settlement.
	KindSettlementProposal Kind = "settlementProposal"
	// KindSignature carries one seat's signature.
	KindSignature Kind = "signature"
	// KindRefusal declines to sign, with a reason. A refusal without a reason is
	// indistinguishable from a seat that has simply gone quiet.
	KindRefusal Kind = "refusal"

	// KindCatchUpRequest asks for the messages a reconnecting seat missed.
	KindCatchUpRequest Kind = "catchUpRequest"
	// KindAck acknowledges a message, replacing the upstream design's re-broadcast loop.
	KindAck Kind = "ack"
)

// PhaseFor reports the phase a message kind belongs to, and whether it is phase-bound.
//
// Hello, catch-up, acks and refusals are not phase-bound: a seat may go quiet or reconnect at
// any point, and refusing to sign is meaningful whenever a signature was requested.
func PhaseFor(k Kind) (Phase, bool) {
	switch k {
	case KindShuffle, KindRemask, KindShuffleProof, KindHoleReveal:
		return PhaseDealing, true
	case KindAction:
		return PhaseBetting, true
	case KindBoardReveal, KindShowdownReveal:
		// Board reveals happen between betting streets and at showdown, so they are
		// valid in either of those phases; treated as betting-phase here.
		return PhaseBetting, true
	case KindSettlementProposal, KindSignature:
		return PhaseSettling, true
	default:
		return "", false
	}
}

// Envelope wraps every game message.
//
// The seat index and identity key are both carried so a receiver can check that the claimed
// seat really belongs to the claimed identity, and the sequence lets a reconnecting seat work
// out exactly what it missed.
type Envelope struct {
	TableID string `json:"tableId"`
	HandID  string `json:"handId"`
	Kind    Kind   `json:"kind"`
	// Seat is the seat this message acts for.
	Seat int `json:"seat"`
	// IdentityKey is the sender's proven identity, hex-encoded.
	IdentityKey string `json:"identityKey"`
	// Seq is the sender's own monotonic counter, so gaps are detectable per seat.
	Seq uint64 `json:"seq"`
	// Recipient scopes a private message to one seat. Empty means everyone.
	Recipient string `json:"recipient,omitempty"`
	// Body is the kind-specific payload.
	Body json.RawMessage `json:"body,omitempty"`
}

// Validate checks an envelope's shape before any of its meaning is trusted.
func (e Envelope) Validate() error {
	var errs []error
	if e.TableID == "" {
		errs = append(errs, errors.New("table id is required"))
	}
	if e.Kind == "" {
		errs = append(errs, errors.New("kind is required"))
	}
	if e.Seat < 0 || e.Seat >= MaxSeats {
		errs = append(errs, fmt.Errorf("seat %d is outside 0..%d", e.Seat, MaxSeats-1))
	}
	if e.IdentityKey == "" {
		errs = append(errs, errors.New("identity key is required"))
	}
	// Every kind except hello belongs to a hand: a message with no hand cannot be
	// checked against any game state.
	if e.Kind != KindHello && e.HandID == "" {
		errs = append(errs, fmt.Errorf("%s requires a hand id", e.Kind))
	}
	return errors.Join(errs...)
}

// Private reports whether the message is addressed to one seat.
//
// A private message must never be fanned out in the clear: a hole-card reveal is exactly the
// material that must not reach anyone else.
func (e Envelope) Private() bool { return e.Recipient != "" }

// HelloBody announces a seat.
type HelloBody struct {
	// PublicKey is the seat's identity key, repeated inside the signed body so it cannot
	// be swapped in the envelope alone.
	PublicKey string `json:"publicKey"`
}

// ShuffleBody carries a shuffled deck.
type ShuffleBody struct {
	// Deck is the deck after this seat's contribution, as compressed points.
	Deck [][]byte `json:"deck"`
}

// RemaskBody carries a re-masked deck.
type RemaskBody struct {
	Deck [][]byte `json:"deck"`
}

// ShuffleProofBody commits to a shuffle so a dishonest shuffler is constrained during play.
type ShuffleProofBody struct {
	// Commitment binds the seat's permutation and scalars without revealing them.
	Commitment []byte `json:"commitment"`
}

// RevealBody discloses scalars for deck positions.
type RevealBody struct {
	// Positions and Scalars are index-aligned.
	Positions []int    `json:"positions"`
	Scalars   [][]byte `json:"scalars"`
}

// Validate checks the reveal is well-formed.
func (r RevealBody) Validate() error {
	if len(r.Positions) == 0 {
		return errors.New("a reveal must name at least one position")
	}
	if len(r.Positions) != len(r.Scalars) {
		return fmt.Errorf("reveal has %d positions and %d scalars", len(r.Positions), len(r.Scalars))
	}
	seen := make(map[int]struct{}, len(r.Positions))
	for _, p := range r.Positions {
		if p < 0 {
			return fmt.Errorf("reveal names negative position %d", p)
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("reveal names position %d twice", p)
		}
		seen[p] = struct{}{}
	}
	return nil
}

// ActionBody is a betting action.
type ActionBody struct {
	// Action is the action name, matching the engine's vocabulary.
	Action string `json:"action"`
	// To is the total street commitment a bet or raise targets. A total rather than an
	// increment, so every independent replayer resolves it identically.
	To int64 `json:"to,omitempty"`
}

// SettlementProposalBody asks the seats to sign.
type SettlementProposalBody struct {
	// RawTxHex is the unsigned settlement.
	RawTxHex string `json:"rawTxHex"`
	// PotInput is the index of the input spending the pot.
	PotInput int `json:"potInput"`
	// PotTxid, PotVout and PotSatoshis let a seat confirm which pot is being spent.
	PotTxid     string `json:"potTxid"`
	PotVout     uint32 `json:"potVout"`
	PotSatoshis uint64 `json:"potSatoshis"`
	// Reference is the wallet's signable-transaction reference.
	Reference []byte `json:"reference,omitempty"`
}

// SignatureBody carries one seat's signature.
type SignatureBody struct {
	// DER is the signature with its sighash-type byte appended.
	DER []byte `json:"der"`
}

// RefusalBody declines to sign.
type RefusalBody struct {
	// Reason explains the refusal. Required: an unexplained refusal cannot be
	// distinguished from a seat that has gone quiet, and the other seats need to know
	// which it is before falling back on refunds.
	Reason string `json:"reason"`
}

// Validate requires a reason.
func (r RefusalBody) Validate() error {
	if r.Reason == "" {
		return errors.New("a refusal must give a reason")
	}
	return nil
}

// CatchUpRequestBody asks for missed messages.
type CatchUpRequestBody struct {
	// HaveSeq is the highest sequence the requester has applied, per seat index.
	HaveSeq map[int]uint64 `json:"haveSeq"`
}

// AckBody acknowledges a message by id.
type AckBody struct {
	// MessageID is the transport de-duplication key being acknowledged.
	MessageID string `json:"messageId"`
}

// Encode marshals a body into an envelope.
func Encode(e *Envelope, body any) error {
	if e == nil {
		return errors.New("table: no envelope to encode into")
	}
	if body == nil {
		e.Body = nil
		return nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("table: encoding a %s body: %w", e.Kind, err)
	}
	e.Body = raw
	return nil
}

// DecodeBody unmarshals an envelope's body.
func DecodeBody[T any](e Envelope) (T, error) {
	var out T
	if len(e.Body) == 0 {
		return out, fmt.Errorf("table: %s message has no body", e.Kind)
	}
	if err := json.Unmarshal(e.Body, &out); err != nil {
		return out, fmt.Errorf("table: decoding a %s body: %w", e.Kind, err)
	}
	return out, nil
}
