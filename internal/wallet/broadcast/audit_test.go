package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/galt-tr/go-arcade-toolbox/pkg/arcade"
)

// The reconstruction property: given a hand, the records say what happened to its money.
func TestHandMoneyHistoryIsReconstructable(t *testing.T) {
	ctx := context.Background()
	rec := NewRecorder()

	// A hand that funds, settles, and pays out.
	for _, e := range []Event{
		{HandID: "hand-1", Purpose: PurposeFundPot, TxID: "aa", Satoshis: 5000, Outcome: OutcomeAccepted},
		{HandID: "hand-1", Purpose: PurposeSettlement, TxID: "bb", Satoshis: 4800, Outcome: OutcomeAccepted},
		{HandID: "hand-1", Purpose: PurposePayout, TxID: "bb", Satoshis: 4800, Outcome: OutcomeAccepted},
		// An unrelated hand, to prove the query is scoped.
		{HandID: "hand-2", Purpose: PurposeFundPot, TxID: "cc", Satoshis: 5000, Outcome: OutcomeAccepted},
	} {
		rec.Record(ctx, e)
	}

	history := rec.ForHand("hand-1")
	if len(history) != 3 {
		t.Fatalf("hand-1 history has %d events, want 3", len(history))
	}
	// Oldest first, so a reader follows the money in order.
	if history[0].Purpose != PurposeFundPot || history[2].Purpose != PurposePayout {
		t.Errorf("history is not in order: %v", history)
	}
	for _, e := range history {
		if e.HandID != "hand-1" {
			t.Errorf("the query leaked an event from %s", e.HandID)
		}
		if e.At.IsZero() {
			t.Error("an event has no timestamp")
		}
	}
	if len(rec.ForHand("hand-3")) != 0 {
		t.Error("an unknown hand returned events")
	}
}

// A rejection reason must be retained: it is the record of why money did not move, and a
// rejection without it is a dead end for whoever investigates later.
func TestRejectionReasonIsRecorded(t *testing.T) {
	ctx := context.Background()
	rec := NewRecorder()
	o := oracleReturning(pair{res: &arcade.BroadcastResult{
		TxID: "dd", Rejected: true, ExtraInfo: "bad-txns-inputs-duplicate",
	}})

	_, err := SendAudited(ctx, o, rec, DefaultPolicy(),
		Event{HandID: "hand-9", Purpose: PurposeSettlement, Satoshis: 4800}, "00")
	if err == nil {
		t.Fatal("a rejected broadcast reported success")
	}

	events := rec.ForHand("hand-9")
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	e := events[0]
	if e.Outcome != OutcomeRejected {
		t.Errorf("outcome = %v, want rejected", e.Outcome)
	}
	if e.Reason != "bad-txns-inputs-duplicate" {
		t.Errorf("reason = %q, want the network's rejection reason", e.Reason)
	}
	// The txid comes from the result when the caller did not know it.
	if e.TxID != "dd" {
		t.Errorf("txid = %q", e.TxID)
	}
}

// An indeterminate outcome must be recorded as such, not as a failure or a success: the
// service genuinely does not know whether the money moved.
func TestIndeterminateOutcomeIsRecorded(t *testing.T) {
	ctx := context.Background()
	rec := NewRecorder()
	o := oracleReturning(pair{err: errUnavailable{}})

	if _, err := SendAudited(ctx, o, rec, DefaultPolicy(),
		Event{HandID: "hand-x", Purpose: PurposeRefund}, "00"); err == nil {
		t.Fatal("an indeterminate broadcast reported success")
	}
	events := rec.ForHand("hand-x")
	if len(events) != 1 || events[0].Outcome != OutcomeIndeterminate {
		t.Fatalf("events = %+v, want one indeterminate", events)
	}
	if events[0].Reason == "" {
		t.Error("an indeterminate outcome was recorded with no reason")
	}
}

func TestAcceptedBroadcastIsRecorded(t *testing.T) {
	ctx := context.Background()
	rec := NewRecorder()
	o := oracleReturning(pair{res: &arcade.BroadcastResult{TxID: "ee", Status: arcade.StatusReceived}})

	if _, err := SendAudited(ctx, o, rec, DefaultPolicy(),
		Event{HandID: "hand-ok", Purpose: PurposeFundPot, Satoshis: 5000}, "00"); err != nil {
		t.Fatal(err)
	}
	events := rec.Events()
	if len(events) != 1 || events[0].Outcome != OutcomeAccepted {
		t.Fatalf("events = %+v", events)
	}
}

// A rejection must not be filtered out by a level threshold set to keep routine traffic quiet.
func TestRejectionLogsAtErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	// Error level only: a rejection must still appear.
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewLogAuditor(logger)

	a.Record(context.Background(), Event{
		HandID: "hand-1", Purpose: PurposeSettlement, TxID: "ff",
		Satoshis: 4800, Outcome: OutcomeRejected, Reason: "insufficient-fee",
	})

	if buf.Len() == 0 {
		t.Fatal("a rejection was filtered out at error level")
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", entry["level"])
	}
	for _, want := range []string{"handId", "purpose", "txid", "reason"} {
		if _, ok := entry[want]; !ok {
			t.Errorf("the log entry omits %q: %v", want, entry)
		}
	}
}

// An accepted broadcast is routine and belongs at info, so it can be filtered.
func TestAcceptedLogsAtInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewLogAuditor(logger)

	a.Record(context.Background(), Event{HandID: "h", Purpose: PurposeFundPot, Outcome: OutcomeAccepted})
	if buf.Len() != 0 {
		t.Error("a routine acceptance was logged at error level")
	}
}

func TestIndeterminateLogsAtErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	NewLogAuditor(logger).Record(context.Background(), Event{
		HandID: "h", Purpose: PurposeSettlement, Outcome: OutcomeIndeterminate,
	})
	if buf.Len() == 0 {
		t.Fatal("an unknown outcome was filtered out; the service does not know if money moved")
	}
	if !strings.Contains(buf.String(), "unknown") {
		t.Errorf("the message does not convey uncertainty: %s", buf.String())
	}
}

func TestSendAuditedToleratesANilAuditor(t *testing.T) {
	o := oracleReturning(pair{res: &arcade.BroadcastResult{TxID: "gg", Status: arcade.StatusReceived}})
	if _, err := SendAudited(context.Background(), o, nil, DefaultPolicy(), Event{HandID: "h"}, "00"); err != nil {
		t.Fatalf("a nil auditor broke the broadcast: %v", err)
	}
}

func TestRecorderIsConcurrencySafe(t *testing.T) {
	rec := NewRecorder()
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			rec.Record(context.Background(), Event{HandID: "h", Purpose: PurposeFundPot})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if len(rec.Events()) != 20 {
		t.Fatalf("recorded %d events, want 20", len(rec.Events()))
	}
}

func TestEventsReturnsACopy(t *testing.T) {
	rec := NewRecorder()
	rec.Record(context.Background(), Event{HandID: "h", Satoshis: 100})
	got := rec.Events()
	got[0].Satoshis = 999
	if rec.Events()[0].Satoshis != 100 {
		t.Fatal("Events returned a slice sharing the recorder's state")
	}
}

// errUnavailable is a transport-style failure with no rejection verdict.
type errUnavailable struct{}

func (errUnavailable) Error() string { return "dial tcp: connection refused" }
