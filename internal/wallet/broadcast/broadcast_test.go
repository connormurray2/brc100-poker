package broadcast

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/galt-tr/go-arcade-toolbox/pkg/arcade"
)

// The trap this package exists for: a 4xx rejection returns err == nil with Rejected set.
// Anything that branches on the error alone reads this as success.
func TestRejectionWithNilErrorIsNotSuccess(t *testing.T) {
	res := Classify(&arcade.BroadcastResult{
		TxID:      "abc",
		Status:    arcade.StatusRejected,
		Rejected:  true,
		ExtraInfo: "bad-txns-inputs-duplicate",
	}, nil)

	if res.Accepted() {
		t.Fatal("a rejection was classified as accepted")
	}
	if !res.Rejected() {
		t.Fatal("Rejected() is false for a rejected broadcast")
	}
	if res.Outcome.Retryable() {
		t.Fatal("a rejection must never be retryable")
	}
	if res.Reason != "bad-txns-inputs-duplicate" {
		t.Errorf("reason = %q, want the arcade rejection reason", res.Reason)
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]struct {
		res       *arcade.BroadcastResult
		err       error
		want      Outcome
		retryable bool
	}{
		"202 accepted": {
			res:  &arcade.BroadcastResult{TxID: "a", Status: arcade.StatusReceived},
			want: OutcomeAccepted,
		},
		"202 with an already-seen status is still accepted": {
			res:  &arcade.BroadcastResult{TxID: "a", Status: arcade.StatusSeenOnNetwork},
			want: OutcomeAccepted,
		},
		"4xx rejected, nil error": {
			res:  &arcade.BroadcastResult{TxID: "a", Rejected: true},
			want: OutcomeRejected,
		},
		"REJECTED status without the flag is still rejected": {
			res:  &arcade.BroadcastResult{TxID: "a", Status: arcade.StatusRejected},
			want: OutcomeRejected,
		},
		"503 backpressure": {
			err:       &arcade.BackpressureError{RetryAfter: time.Second},
			want:      OutcomeBackpressure,
			retryable: true,
		},
		"backpressure wrapped": {
			err:       fmt.Errorf("sending: %w", &arcade.BackpressureError{RetryAfter: 2 * time.Second}),
			want:      OutcomeBackpressure,
			retryable: true,
		},
		"500 indeterminate": {
			err:  errors.New("500 internal server error"),
			want: OutcomeIndeterminate,
		},
		"transport failure indeterminate": {
			err:  errors.New("dial tcp: connection refused"),
			want: OutcomeIndeterminate,
		},
		"nothing at all is indeterminate": {
			want: OutcomeIndeterminate,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := Classify(tc.res, tc.err)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", got.Outcome, tc.want)
			}
			if got.Outcome.Retryable() != tc.retryable {
				t.Errorf("Retryable() = %v, want %v", got.Outcome.Retryable(), tc.retryable)
			}
		})
	}
}

// Backpressure defaults to a positive delay even if arcade sends no Retry-After, so a
// retry loop cannot spin.
func TestBackpressureAlwaysHasPositiveDelay(t *testing.T) {
	got := Classify(nil, &arcade.BackpressureError{})
	if got.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want a positive default", got.RetryAfter)
	}
}

type fakeOracle struct {
	calls   int
	results []struct {
		res *arcade.BroadcastResult
		err error
	}
}

func (f *fakeOracle) Broadcast(_ context.Context, _ string) (*arcade.BroadcastResult, error) {
	i := f.calls
	f.calls++
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return f.results[i].res, f.results[i].err
}

func oracleReturning(pairs ...struct {
	res *arcade.BroadcastResult
	err error
}) *fakeOracle {
	return &fakeOracle{results: pairs}
}

type pair = struct {
	res *arcade.BroadcastResult
	err error
}

func TestSendNeverRetriesARejection(t *testing.T) {
	o := oracleReturning(pair{res: &arcade.BroadcastResult{TxID: "a", Rejected: true, ExtraInfo: "nope"}})

	res, err := Send(context.Background(), o, "00", DefaultPolicy())
	if o.calls != 1 {
		t.Fatalf("broadcast attempted %d times; a rejection must never be retried", o.calls)
	}
	if !res.Rejected() {
		t.Error("result does not report a rejection")
	}
	var rej *ErrRejected
	if !errors.As(err, &rej) {
		t.Fatalf("error %v is not *ErrRejected", err)
	}
	if rej.Reason != "nope" {
		t.Errorf("reason = %q, want %q", rej.Reason, "nope")
	}
}

func TestSendRetriesBackpressureThenSucceeds(t *testing.T) {
	o := oracleReturning(
		pair{err: &arcade.BackpressureError{RetryAfter: time.Millisecond}},
		pair{err: &arcade.BackpressureError{RetryAfter: time.Millisecond}},
		pair{res: &arcade.BroadcastResult{TxID: "a", Status: arcade.StatusReceived}},
	)

	res, err := Send(context.Background(), o, "00", Policy{MaxBackpressureRetries: 5, MaxBackoff: time.Second})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.Accepted() {
		t.Errorf("outcome = %v, want accepted", res.Outcome)
	}
	if o.calls != 3 {
		t.Errorf("attempts = %d, want 3", o.calls)
	}
}

func TestSendGivesUpOnPersistentBackpressure(t *testing.T) {
	o := oracleReturning(pair{err: &arcade.BackpressureError{RetryAfter: time.Millisecond}})

	_, err := Send(context.Background(), o, "00", Policy{MaxBackpressureRetries: 2, MaxBackoff: time.Second})
	if err == nil {
		t.Fatal("expected an error after retries were exhausted")
	}
	if o.calls != 3 {
		t.Errorf("attempts = %d, want 3 (initial + 2 retries)", o.calls)
	}
}

// An indeterminate outcome must not be resent: the transaction may already be in flight.
func TestSendDoesNotRetryIndeterminate(t *testing.T) {
	o := oracleReturning(pair{err: errors.New("500 boom")})

	res, err := Send(context.Background(), o, "00", DefaultPolicy())
	if err == nil {
		t.Fatal("expected an error")
	}
	if o.calls != 1 {
		t.Fatalf("attempts = %d; an indeterminate outcome must be reconciled, not resent", o.calls)
	}
	if res.Outcome != OutcomeIndeterminate {
		t.Errorf("outcome = %v, want indeterminate", res.Outcome)
	}
}

func TestSendHonoursContextCancellation(t *testing.T) {
	o := oracleReturning(pair{err: &arcade.BackpressureError{RetryAfter: time.Hour}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := Send(ctx, o, "00", DefaultPolicy()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
}
