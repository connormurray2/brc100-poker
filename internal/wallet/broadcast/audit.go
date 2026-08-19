package broadcast

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Purpose describes why a transaction was broadcast.
//
// Recorded alongside the outcome because "which transaction failed" is rarely the useful
// question — "which hand's money did not move, and why" is.
type Purpose string

const (
	// PurposeFundPot is a buy-in entering the shared pot.
	PurposeFundPot Purpose = "fund-pot"
	// PurposeSettlement is a pot paying the winner.
	PurposeSettlement Purpose = "settlement"
	// PurposeRefund is a stake being recovered after a stall.
	PurposeRefund Purpose = "refund"
	// PurposePayout is a winner receiving their coin.
	PurposePayout Purpose = "payout"
)

// Event is one money-movement record.
//
// Together these must be sufficient to reconstruct what happened to a hand's funds without
// consulting the chain, because the most likely time to need them is when the chain view is
// the thing in doubt.
type Event struct {
	At      time.Time
	HandID  string
	Purpose Purpose
	TxID    string
	// Satoshis is the value the transaction moves.
	Satoshis uint64
	Outcome  Outcome
	// Reason carries a rejection reason or a reconciliation note. A rejection without its
	// reason is a dead end for whoever investigates later.
	Reason string
}

// Auditor records money movement.
//
// Deliberately an interface: the table service logs to slog, and tests assert on a recorder,
// but a deployment may want a durable sink without changing any call site.
type Auditor interface {
	Record(ctx context.Context, e Event)
}

// LogAuditor writes events to a structured logger.
type LogAuditor struct {
	logger *slog.Logger
}

// NewLogAuditor returns an auditor writing to the given logger.
func NewLogAuditor(logger *slog.Logger) *LogAuditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogAuditor{logger: logger}
}

// Record logs a money-movement event.
//
// A rejection is logged at error level rather than info: it is the record of money that was
// supposed to move and did not, and it should not be filtered out by a level threshold that
// was set to keep routine traffic quiet.
func (a *LogAuditor) Record(_ context.Context, e Event) {
	attrs := []any{
		"handId", e.HandID,
		"purpose", string(e.Purpose),
		"txid", e.TxID,
		"satoshis", e.Satoshis,
		"outcome", e.Outcome.String(),
	}
	if e.Reason != "" {
		attrs = append(attrs, "reason", e.Reason)
	}

	switch e.Outcome {
	case OutcomeRejected:
		a.logger.Error("money movement rejected", attrs...)
	case OutcomeIndeterminate:
		// Also error: an unresolved outcome means the service does not know whether the
		// money moved, which needs attention rather than a quiet info line.
		a.logger.Error("money movement outcome unknown", attrs...)
	case OutcomeBackpressure:
		a.logger.Warn("money movement backpressured", attrs...)
	default:
		a.logger.Info("money movement accepted", attrs...)
	}
}

// Recorder is an in-memory Auditor for tests and diagnostics.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// NewRecorder returns an in-memory auditor.
func NewRecorder() *Recorder { return &Recorder{} }

// Record stores an event.
func (r *Recorder) Record(_ context.Context, e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a copy of the recorded events.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// ForHand returns the events for one hand, oldest first.
//
// This is the reconstruction query: given a hand, what happened to its money.
func (r *Recorder) ForHand(handID string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.HandID == handID {
			out = append(out, e)
		}
	}
	return out
}

// SendAudited broadcasts and records the outcome.
//
// Every money-moving broadcast should go through here rather than calling Send directly, so
// that the audit record cannot be forgotten at one call site and present at the others.
func SendAudited(ctx context.Context, b Broadcaster, a Auditor, pol Policy, e Event, txHex string) (Result, error) {
	res, err := Send(ctx, b, txHex, pol)

	record := e
	record.At = time.Now().UTC()
	record.Outcome = res.Outcome
	if record.TxID == "" {
		record.TxID = res.TxID
	}
	// Prefer the rejection reason the network gave over anything the caller guessed.
	if res.Reason != "" {
		record.Reason = res.Reason
	} else if err != nil && res.Outcome != OutcomeAccepted {
		record.Reason = err.Error()
	}

	if a != nil {
		a.Record(ctx, record)
	}
	return res, err
}
