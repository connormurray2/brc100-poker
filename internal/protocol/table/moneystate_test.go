package table

import (
	"strings"
	"testing"
)

func newTracker(t *testing.T) *MoneyTracker {
	t.Helper()
	tr, err := NewMoneyTracker(2, 2500, 5000, 30000)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestNewMoneyTrackerValidation(t *testing.T) {
	if _, err := NewMoneyTracker(1, 100, 200, 1); err == nil {
		t.Error("built a tracker for a one-seat table")
	}
	if _, err := NewMoneyTracker(7, 100, 200, 1); err == nil {
		t.Error("built a tracker for a seven-seat table")
	}
	// A player must be told when a stall ends, so a refund height is required.
	if _, err := NewMoneyTracker(2, 100, 200, 0); err == nil {
		t.Error("built a tracker with no refund height")
	}
}

func TestStartsUncommitted(t *testing.T) {
	tr := newTracker(t)
	s, err := tr.State(0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Phase != MoneyUncommitted {
		t.Fatalf("phase = %q, want uncommitted", s.Phase)
	}
	if s.RefundHeld {
		t.Error("a fresh seat claims a refund it does not have")
	}
	if s.AtRisk() {
		t.Error("a seat with nothing committed is reported at risk")
	}
	if s.StalledSeat != -1 {
		t.Errorf("stalledSeat = %d, want -1", s.StalledSeat)
	}
}

// A player must not be shown "committed" without a refund, because that is a state the system must
// never reach.
func TestCommittedRequiresARefund(t *testing.T) {
	tr := newTracker(t)
	err := tr.Committed(0)
	if err == nil {
		t.Fatal("a seat was shown as committed with no refund held")
	}
	if !strings.Contains(err.Error(), "no refund") {
		t.Errorf("unclear error: %v", err)
	}

	if err := tr.RefundHeld(0); err != nil {
		t.Fatal(err)
	}
	if err := tr.Committed(0); err != nil {
		t.Fatalf("committing after a refund was held failed: %v", err)
	}
	s, _ := tr.State(0)
	if s.Phase != MoneyCommitted || !s.AtRisk() {
		t.Errorf("state = %+v; a committed seat is at risk", s)
	}
}

func TestRefundHeldMovesToTheSafeState(t *testing.T) {
	tr := newTracker(t)
	if err := tr.RefundHeld(1); err != nil {
		t.Fatal(err)
	}
	s, _ := tr.State(1)
	if s.Phase != MoneyRefundHeld {
		t.Fatalf("phase = %q, want refund-held", s.Phase)
	}
	if !s.RefundHeld {
		t.Error("refundHeld is false after being recorded")
	}
	// Nothing is committed yet, so nothing is at risk.
	if s.AtRisk() {
		t.Error("holding a refund with nothing committed is reported at risk")
	}
}

// Settled and spendable are distinct: broadcasting is not receiving.
func TestSettledIsNotTheSameAsSpendable(t *testing.T) {
	tr := newTracker(t)
	for seat := 0; seat < 2; seat++ {
		if err := tr.RefundHeld(seat); err != nil {
			t.Fatal(err)
		}
		if err := tr.Committed(seat); err != nil {
			t.Fatal(err)
		}
	}
	tr.Settled("abc123", map[int]uint64{1: 4800})

	winner, _ := tr.State(1)
	if winner.Phase != MoneySettled {
		t.Fatalf("phase = %q", winner.Phase)
	}
	if winner.PayoutSatoshis != 4800 {
		t.Errorf("payout = %d, want 4800", winner.PayoutSatoshis)
	}
	if winner.PayoutSpendable {
		t.Error("a settled payout was reported spendable before it was received")
	}
	if !strings.Contains(winner.Summary(), "Broadcast and spendable") {
		t.Errorf("the summary does not warn that the payout is not yet spendable: %q", winner.Summary())
	}

	if err := tr.PayoutReceived(1); err != nil {
		t.Fatal(err)
	}
	winner, _ = tr.State(1)
	if !winner.PayoutSpendable {
		t.Error("payoutSpendable is false after being received")
	}
	if !strings.Contains(winner.Summary(), "received and spendable") {
		t.Errorf("summary = %q", winner.Summary())
	}

	// The loser is settled with no payout, which is an outcome rather than missing data.
	loser, _ := tr.State(0)
	if loser.Phase != MoneySettled {
		t.Errorf("loser phase = %q", loser.Phase)
	}
	if loser.PayoutSatoshis != 0 {
		t.Errorf("loser payout = %d, want 0", loser.PayoutSatoshis)
	}
	if !strings.Contains(loser.Summary(), "did not win") {
		t.Errorf("loser summary = %q", loser.Summary())
	}
}

// A stall must tell the player who is responsible and when they get their money back — the state a
// player most needs to understand.
func TestStallTellsThePlayerWhoAndWhen(t *testing.T) {
	tr := newTracker(t)
	tr.SetHeight(29900)
	for seat := 0; seat < 2; seat++ {
		if err := tr.RefundHeld(seat); err != nil {
			t.Fatal(err)
		}
		if err := tr.Committed(seat); err != nil {
			t.Fatal(err)
		}
	}

	if err := tr.Stalled(1, "did not disclose its board scalars"); err != nil {
		t.Fatal(err)
	}
	s, _ := tr.State(0)
	if s.Phase != MoneyStalled {
		t.Fatalf("phase = %q, want stalled", s.Phase)
	}
	if s.StalledSeat != 1 {
		t.Errorf("stalledSeat = %d, want 1", s.StalledSeat)
	}
	if !s.AtRisk() {
		t.Error("a stalled seat is not reported at risk")
	}

	summary := s.Summary()
	for _, want := range []string{"seat 1", "did not disclose", "30000", "100 blocks away"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary omits %q: %s", want, summary)
		}
	}

	// An unexplained stall is refused: a player cannot act on it.
	if err := tr.Stalled(0, ""); err == nil {
		t.Error("a stall with no reason was accepted")
	}
}

// A seat whose money already moved is not retroactively stalled.
func TestSettledSeatIsNotRetroactivelyStalled(t *testing.T) {
	tr := newTracker(t)
	for seat := 0; seat < 2; seat++ {
		if err := tr.RefundHeld(seat); err != nil {
			t.Fatal(err)
		}
		if err := tr.Committed(seat); err != nil {
			t.Fatal(err)
		}
	}
	tr.Settled("abc", map[int]uint64{0: 4800})
	if err := tr.Stalled(1, "went quiet afterwards"); err != nil {
		t.Fatal(err)
	}

	s, _ := tr.State(0)
	if s.Phase != MoneySettled {
		t.Fatalf("a settled seat was moved to %q; its money already moved", s.Phase)
	}
}

func TestRecoveredEndsAtRisk(t *testing.T) {
	tr := newTracker(t)
	if err := tr.RefundHeld(0); err != nil {
		t.Fatal(err)
	}
	if err := tr.Committed(0); err != nil {
		t.Fatal(err)
	}
	if err := tr.Stalled(1, "unresponsive"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Recovered(0); err != nil {
		t.Fatal(err)
	}

	s, _ := tr.State(0)
	if s.Phase != MoneyRecovered {
		t.Fatalf("phase = %q", s.Phase)
	}
	if s.AtRisk() {
		t.Error("a recovered seat is still reported at risk")
	}
	if !strings.Contains(s.Summary(), "recovered by refund") {
		t.Errorf("summary = %q", s.Summary())
	}
}

func TestSettlingIsNotFinal(t *testing.T) {
	tr := newTracker(t)
	for seat := 0; seat < 2; seat++ {
		if err := tr.RefundHeld(seat); err != nil {
			t.Fatal(err)
		}
		if err := tr.Committed(seat); err != nil {
			t.Fatal(err)
		}
	}
	tr.Settling("pending-tx")

	s, _ := tr.State(0)
	if s.Phase != MoneySettling {
		t.Fatalf("phase = %q, want settling", s.Phase)
	}
	if !s.AtRisk() {
		t.Error("a settling seat is not reported at risk; the outcome is not yet certain")
	}
	if !strings.Contains(s.Summary(), "Not final") {
		t.Errorf("the summary does not convey that settling is not final: %q", s.Summary())
	}
	if s.SettlementTxID != "pending-tx" {
		t.Errorf("settlementTxid = %q", s.SettlementTxID)
	}
}

// A player needs the refund height BEFORE committing, since it is the worst case a stall costs.
func TestRefundTimingIsVisibleBeforeCommitting(t *testing.T) {
	tr := newTracker(t)
	if err := tr.RefundHeld(0); err != nil {
		t.Fatal(err)
	}
	s, _ := tr.State(0)
	if s.RefundSpendableAtHeight != 30000 {
		t.Fatalf("refund height = %d, want 30000", s.RefundSpendableAtHeight)
	}
	summary := s.Summary()
	if !strings.Contains(summary, "30000") || !strings.Contains(summary, "Safe to commit") {
		t.Errorf("the summary does not tell the player it is safe to commit and until when: %q", summary)
	}
}

func TestAllReturnsSeatsInOrder(t *testing.T) {
	tr, err := NewMoneyTracker(4, 100, 400, 500)
	if err != nil {
		t.Fatal(err)
	}
	all := tr.All()
	if len(all) != 4 {
		t.Fatalf("got %d seats, want 4", len(all))
	}
	for i, s := range all {
		if s.Seat != i {
			t.Fatalf("position %d holds seat %d", i, s.Seat)
		}
	}
}

func TestSetHandAndHeight(t *testing.T) {
	tr := newTracker(t)
	tr.SetHand("hand-9")
	tr.SetHeight(12345)
	s, _ := tr.State(0)
	if s.HandID != "hand-9" {
		t.Errorf("handId = %q", s.HandID)
	}
	if s.CurrentHeight != 12345 {
		t.Errorf("currentHeight = %d", s.CurrentHeight)
	}
}

func TestUnknownSeatIsRejected(t *testing.T) {
	tr := newTracker(t)
	if _, err := tr.State(9); err == nil {
		t.Error("returned state for a seat that does not exist")
	}
	if err := tr.RefundHeld(9); err == nil {
		t.Error("recorded a refund for a seat that does not exist")
	}
	if err := tr.Committed(9); err == nil {
		t.Error("committed a seat that does not exist")
	}
	if err := tr.Recovered(9); err == nil {
		t.Error("recovered a seat that does not exist")
	}
	if err := tr.PayoutReceived(9); err == nil {
		t.Error("recorded a payout for a seat that does not exist")
	}
}

func TestSummaryCoversEveryPhase(t *testing.T) {
	// Every phase must produce something a player can read; an unhandled phase would show a
	// placeholder at exactly the moment a player is trying to understand their money.
	for _, phase := range []MoneyPhase{
		MoneyUncommitted, MoneyRefundHeld, MoneyCommitted,
		MoneySettling, MoneySettled, MoneyStalled, MoneyRecovered,
	} {
		s := MoneyState{Phase: phase, StakeSatoshis: 100, PotSatoshis: 200,
			RefundSpendableAtHeight: 500, StalledSeat: 1, StallReason: "went quiet"}
		got := s.Summary()
		if got == "" || strings.Contains(got, "Unknown money phase") {
			t.Errorf("phase %q has no readable summary: %q", phase, got)
		}
	}
	// An unrecognised phase is reported as such rather than silently rendering as fine.
	odd := MoneyState{Phase: MoneyPhase("nonsense")}
	if !strings.Contains(odd.Summary(), "Unknown") {
		t.Error("an unknown phase did not report itself as unknown")
	}
}
