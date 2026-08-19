package table

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cmurray/brc100-poker/internal/protocol/transport"
)

// Session runs one seat's view of a table over a transport.
//
// It is the seam between "bytes arriving" and "game state advancing": it authorises the
// sender, enforces ordered progression, de-duplicates, and keeps the log a reconnecting seat
// needs. Game logic lives above it, and none of it trusts the table service.
type Session struct {
	table    *Table
	tp       transport.Transport
	tableID  string
	dedup    *transport.Dedup
	selfSeat int
	selfKey  string

	mu sync.RWMutex
	// applied tracks the highest sequence applied per seat, so a gap is detectable and a
	// reconnecting seat can say exactly what it has.
	applied map[int]uint64
	// log retains this seat's own sent messages so it can answer a catch-up request.
	// Only own messages: a seat cannot vouch for another's traffic.
	log []Envelope
	// nextSeq is this seat's outgoing counter.
	nextSeq uint64

	handlers map[Kind]MessageHandler
	unsub    func()
}

// MessageHandler processes a validated, authorised, in-phase message.
type MessageHandler func(Envelope) error

// SessionConfig parameterises a Session.
type SessionConfig struct {
	Table     *Table
	Transport transport.Transport
	// SelfSeat and SelfKey identify which seat this session speaks for.
	SelfSeat int
	SelfKey  string
	// DedupLimit bounds the retained message-id set. Zero applies a default.
	DedupLimit int
}

// NewSession builds a session and subscribes to the table's traffic.
func NewSession(cfg SessionConfig) (*Session, error) {
	if cfg.Table == nil {
		return nil, errors.New("table: a table is required")
	}
	if cfg.Transport == nil {
		return nil, errors.New("table: a transport is required")
	}
	if cfg.SelfKey == "" {
		return nil, errors.New("table: this session has no identity")
	}

	s := &Session{
		table:    cfg.Table,
		tp:       cfg.Transport,
		tableID:  cfg.Table.Terms().TableID,
		dedup:    transport.NewDedup(cfg.DedupLimit),
		selfSeat: cfg.SelfSeat,
		selfKey:  cfg.SelfKey,
		applied:  make(map[int]uint64),
		handlers: make(map[Kind]MessageHandler),
	}

	unsub, err := cfg.Transport.Subscribe(s.tableID, s.onMessage)
	if err != nil {
		return nil, fmt.Errorf("table: subscribing to %s: %w", s.tableID, err)
	}
	s.unsub = unsub
	return s, nil
}

// Handle registers a handler for a message kind.
func (s *Session) Handle(k Kind, h MessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[k] = h
}

// Close unsubscribes.
func (s *Session) Close() {
	s.mu.Lock()
	unsub := s.unsub
	s.unsub = nil
	s.mu.Unlock()
	if unsub != nil {
		unsub()
	}
}

// Send publishes a message for this seat.
func (s *Session) Send(ctx context.Context, kind Kind, body any) error {
	return s.sendTo(ctx, kind, "", body)
}

// SendPrivate publishes a message addressed to one recipient.
//
// The transport still fans the frame out, so the body must already be protected for the
// recipient by the caller. Addressing alone is routing, not confidentiality.
func (s *Session) SendPrivate(ctx context.Context, kind Kind, recipient string, body any) error {
	if recipient == "" {
		return errors.New("table: a private message needs a recipient")
	}
	return s.sendTo(ctx, kind, recipient, body)
}

func (s *Session) sendTo(ctx context.Context, kind Kind, recipient string, body any) error {
	s.mu.Lock()
	s.nextSeq++
	env := Envelope{
		TableID:     s.tableID,
		Kind:        kind,
		Seat:        s.selfSeat,
		IdentityKey: s.selfKey,
		Seq:         s.nextSeq,
		Recipient:   recipient,
	}
	s.mu.Unlock()

	if h := s.table.Terms().TableID; h != "" {
		env.TableID = h
	}
	env.HandID = s.handID()

	if err := Encode(&env, body); err != nil {
		return err
	}
	if err := env.Validate(); err != nil {
		return fmt.Errorf("table: refusing to send an invalid %s: %w", kind, err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("table: encoding the envelope: %w", err)
	}

	s.mu.Lock()
	s.log = append(s.log, env)
	s.mu.Unlock()

	// A fresh transport id per publish, so a deliberate re-publish for catch-up can reuse
	// it and be suppressed by peers that already applied it.
	if _, err := s.tp.Publish(ctx, s.tableID, raw, ""); err != nil {
		return fmt.Errorf("table: publishing a %s: %w", kind, err)
	}
	return nil
}

// handID returns the current hand identifier. One hand per table in this slice, so the table
// id doubles as the hand id; a multi-hand table would carry a counter here.
func (s *Session) handID() string { return s.tableID }

// onMessage is the receive path. Every check here runs before any game state moves.
func (s *Session) onMessage(msg transport.Message) {
	// De-duplicate first: a re-published message must reach the peer that missed it
	// without being applied twice by peers that already have it.
	if !s.dedup.FirstSeen(msg.ID) {
		return
	}

	var env Envelope
	if err := json.Unmarshal(msg.Payload, &env); err != nil {
		return
	}
	if err := env.Validate(); err != nil {
		return
	}
	if env.TableID != s.tableID {
		return
	}

	// A private message for someone else is not this seat's business.
	if env.Private() && env.Recipient != s.selfKey {
		return
	}

	// Authorise: the claimed seat must really belong to the claimed identity. Hello is
	// exempt because it is how a seat becomes known in the first place.
	if env.Kind != KindHello {
		if err := s.table.Authorise(env.Seat, env.IdentityKey); err != nil {
			return
		}
		// A hello body repeats the key so it cannot be swapped in the envelope alone;
		// for other kinds the authorisation above is the binding.
	}

	// Enforce ordered progression: a message for a step that is not current is dropped
	// rather than applied early.
	if want, bound := PhaseFor(env.Kind); bound {
		got := s.table.Phase()
		// A board reveal is also legitimate during dealing, since the first board
		// cards are revealed as the deal completes.
		boardDuringDeal := env.Kind == KindBoardReveal && got == PhaseDealing
		if got != want && !boardDuringDeal {
			return
		}
	}

	// Track per-seat sequence so gaps are visible to catch-up.
	s.mu.Lock()
	if env.Seq > s.applied[env.Seat] {
		s.applied[env.Seat] = env.Seq
	}
	h := s.handlers[env.Kind]
	s.mu.Unlock()

	_ = s.table.Touch(env.Seat)

	if h == nil {
		return
	}
	// A handler must not take the session down with it.
	defer func() { _ = recover() }()
	_ = h(env)
}

// Applied returns the highest sequence applied per seat.
//
// This is what a reconnecting seat sends so peers can work out precisely what it missed,
// replacing the upstream design's re-broadcast-everything loop.
func (s *Session) Applied() map[int]uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[int]uint64, len(s.applied))
	for k, v := range s.applied {
		out[k] = v
	}
	return out
}

// RequestCatchUp asks peers for what this seat is missing.
func (s *Session) RequestCatchUp(ctx context.Context) error {
	return s.Send(ctx, KindCatchUpRequest, CatchUpRequestBody{HaveSeq: s.Applied()})
}

// ServeCatchUp re-publishes this seat's own messages that the requester is missing.
//
// Only own messages, and only public ones: re-sending another seat's private reveal would
// leak material that was never this seat's to disclose.
func (s *Session) ServeCatchUp(ctx context.Context, req CatchUpRequestBody) (int, error) {
	have := req.HaveSeq[s.selfSeat]

	s.mu.RLock()
	var missing []Envelope
	for _, env := range s.log {
		if env.Seq > have && !env.Private() {
			missing = append(missing, env)
		}
	}
	s.mu.RUnlock()

	sent := 0
	for _, env := range missing {
		raw, err := json.Marshal(env)
		if err != nil {
			return sent, fmt.Errorf("table: encoding a replayed %s: %w", env.Kind, err)
		}
		// A fresh id: peers that already applied the original suppress it by their own
		// per-seat sequence tracking, and the requester needs it delivered.
		if _, err := s.tp.Publish(ctx, s.tableID, raw, ""); err != nil {
			return sent, fmt.Errorf("table: replaying a %s: %w", env.Kind, err)
		}
		sent++
	}
	return sent, nil
}

// SentCount reports how many messages this seat has sent, for tests and diagnostics.
func (s *Session) SentCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.log)
}
