// Package broadcast is the single path from this application to the transaction oracle.
//
// It exists because arcade's broadcast contract has a trap that is easy to get wrong and
// expensive to get wrong: a 4xx tx-level rejection returns a result with Rejected set and
// **err == nil**. Code that branches only on `err != nil` reads a final rejection as a
// success and carries on as though the money moved.
//
// Routing every broadcast through Classify means that mistake can only be made once,
// here, where it is tested.
package broadcast

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/galt-tr/go-arcade-toolbox/pkg/arcade"
)

// Outcome is the classified result of a broadcast attempt.
type Outcome int

const (
	// OutcomeAccepted means arcade took the transaction into its pipeline (HTTP 202).
	// It is NOT a settlement verdict: the authoritative outcome arrives later via
	// status. Do not treat a coin as settled on the strength of this.
	OutcomeAccepted Outcome = iota

	// OutcomeRejected is a final, durable tx-level verdict (HTTP 4xx). Never retry it.
	// Resubmitting the same bytes will be rejected again, and the reason is in Reason.
	OutcomeRejected

	// OutcomeBackpressure means arcade was over capacity (HTTP 503) and the
	// transaction was never queued. Retrying the same bytes is safe and expected.
	OutcomeBackpressure

	// OutcomeIndeterminate means the transaction's fate is unknown (>=500 or a
	// transport failure). It may or may not have been queued. Reconcile by querying
	// the transaction's status; do not blind-retry.
	OutcomeIndeterminate
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAccepted:
		return "accepted"
	case OutcomeRejected:
		return "rejected"
	case OutcomeBackpressure:
		return "backpressure"
	case OutcomeIndeterminate:
		return "indeterminate"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// Retryable reports whether resubmitting the identical bytes is safe.
//
// Only backpressure qualifies. An indeterminate outcome is deliberately excluded: the
// transaction may already be in flight, so it must be reconciled rather than resent.
func (o Outcome) Retryable() bool { return o == OutcomeBackpressure }

// Result is a classified broadcast outcome.
type Result struct {
	Outcome Outcome
	TxID    string
	Status  arcade.Status

	// Reason carries arcade's rejection reason when Outcome is OutcomeRejected.
	Reason string

	// RetryAfter is the minimum wait before retrying a backpressured broadcast.
	RetryAfter time.Duration

	// Err is the underlying error for an indeterminate outcome. It is nil for the
	// other outcomes, including a rejection — which is exactly the trap this
	// package exists to contain.
	Err error
}

// Rejected reports a final rejection. Prefer this over checking an error.
func (r Result) Rejected() bool { return r.Outcome == OutcomeRejected }

// Accepted reports that arcade took the transaction, with no claim about settlement.
func (r Result) Accepted() bool { return r.Outcome == OutcomeAccepted }

// Classify maps a raw oracle Broadcast return into an Outcome.
//
// This is the whole point of the package, and the ordering matters: the rejection flag is
// checked before the nil-error path, because a rejection arrives with a nil error.
func Classify(res *arcade.BroadcastResult, err error) Result {
	// Backpressure first: it is a typed error and the only safely retryable case.
	var bp *arcade.BackpressureError
	if errors.As(err, &bp) {
		retry := bp.RetryAfter
		if retry <= 0 {
			retry = time.Second
		}
		return Result{Outcome: OutcomeBackpressure, RetryAfter: retry, Err: err}
	}

	// A tx-level rejection can arrive with err == nil. Check the flag, not the error.
	if res != nil && res.Rejected {
		return Result{
			Outcome: OutcomeRejected,
			TxID:    res.TxID,
			Status:  res.Status,
			Reason:  res.ExtraInfo,
		}
	}

	if err != nil {
		return Result{Outcome: OutcomeIndeterminate, Err: err}
	}

	if res == nil {
		return Result{
			Outcome: OutcomeIndeterminate,
			Err:     errors.New("broadcast returned neither a result nor an error"),
		}
	}

	// Defensive: a REJECTED status without the flag set is still a rejection.
	if res.Status == arcade.StatusRejected {
		return Result{
			Outcome: OutcomeRejected,
			TxID:    res.TxID,
			Status:  res.Status,
			Reason:  res.ExtraInfo,
		}
	}

	return Result{Outcome: OutcomeAccepted, TxID: res.TxID, Status: res.Status}
}

// ErrRejected is returned by Send when arcade issues a final rejection.
type ErrRejected struct {
	TxID   string
	Reason string
}

func (e *ErrRejected) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("broadcast rejected (txid %s)", e.TxID)
	}
	return fmt.Sprintf("broadcast rejected (txid %s): %s", e.TxID, e.Reason)
}

// Broadcaster is the subset of the oracle this package needs.
type Broadcaster interface {
	Broadcast(ctx context.Context, txHex string) (*arcade.BroadcastResult, error)
}

// Policy bounds the retry behaviour for backpressure.
type Policy struct {
	// MaxBackpressureRetries caps retries of a 503. Zero means do not retry.
	MaxBackpressureRetries int
	// MaxBackoff clamps the advised Retry-After.
	MaxBackoff time.Duration
}

// DefaultPolicy retries backpressure a few times with arcade's advised delay.
func DefaultPolicy() Policy {
	return Policy{MaxBackpressureRetries: 5, MaxBackoff: 10 * time.Second}
}

// Send broadcasts once, retrying only backpressure, and never retrying a rejection.
//
// The returned Result is always populated. A rejection is reported both in the Result and
// as an *ErrRejected, so a caller that only checks the error still cannot mistake it for
// success.
func Send(ctx context.Context, b Broadcaster, txHex string, pol Policy) (Result, error) {
	var last Result
	for attempt := 0; ; attempt++ {
		res, err := b.Broadcast(ctx, txHex)
		last = Classify(res, err)

		switch last.Outcome {
		case OutcomeAccepted:
			return last, nil

		case OutcomeRejected:
			return last, &ErrRejected{TxID: last.TxID, Reason: last.Reason}

		case OutcomeBackpressure:
			if attempt >= pol.MaxBackpressureRetries {
				return last, fmt.Errorf("broadcast: backpressure persisted after %d attempts: %w", attempt+1, last.Err)
			}
			wait := last.RetryAfter
			if pol.MaxBackoff > 0 && wait > pol.MaxBackoff {
				wait = pol.MaxBackoff
			}
			select {
			case <-ctx.Done():
				return last, ctx.Err()
			case <-time.After(wait):
			}

		case OutcomeIndeterminate:
			// Do not resend. The caller must reconcile via the oracle's status.
			return last, fmt.Errorf("broadcast: outcome indeterminate, reconcile by status: %w", last.Err)
		}
	}
}
