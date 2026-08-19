package table

import (
	"strings"
	"testing"
	"time"
)

func goodTerms() Terms {
	return Terms{
		TableID:          "t-1",
		BuyInSatoshis:    5000,
		SmallBlind:       25,
		BigBlind:         50,
		Seats:            3,
		RefundLockHeight: 30000,
	}
}

func newTable(t *testing.T) *Table {
	t.Helper()
	tb, err := New(goodTerms())
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// keys returns distinct fake identity keys. Real keys are proven upstream; the table only
// records the binding.
func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat(string(rune('a'+i)), 66)
	}
	return out
}

func TestTermsValidation(t *testing.T) {
	tests := map[string]func(*Terms){
		"no id":            func(x *Terms) { x.TableID = "" },
		"one seat":         func(x *Terms) { x.Seats = 1 },
		"seven seats":      func(x *Terms) { x.Seats = 7 },
		"no buy-in":        func(x *Terms) { x.BuyInSatoshis = 0 },
		"no blinds":        func(x *Terms) { x.BigBlind = 0 },
		"sb over bb":       func(x *Terms) { x.SmallBlind = 100; x.BigBlind = 50 },
		"bb over buy-in":   func(x *Terms) { x.BigBlind = 9000 },
		"no refund height": func(x *Terms) { x.RefundLockHeight = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			terms := goodTerms()
			mutate(&terms)
			if err := terms.Validate(); err == nil {
				t.Fatal("expected an error")
			}
			if _, err := New(terms); err == nil {
				t.Fatal("New accepted invalid terms")
			}
		})
	}
}

// A player must see the terms, including how long a stall can cost them, before committing.
func TestTermsAreVisibleBeforeFunding(t *testing.T) {
	tb := newTable(t)
	got := tb.Terms()
	if got.BuyInSatoshis != 5000 || got.RefundLockHeight != 30000 {
		t.Fatalf("terms not advertised: %+v", got)
	}
	if tb.Phase() != PhaseOpen {
		t.Fatalf("phase = %s, want open", tb.Phase())
	}
}

func TestJoinAssignsSeatsInOrder(t *testing.T) {
	tb := newTable(t)
	ks := keys(3)
	for i, k := range ks {
		idx, err := tb.Join(k)
		if err != nil {
			t.Fatal(err)
		}
		if idx != i {
			t.Fatalf("seat = %d, want %d", idx, i)
		}
	}
	if len(tb.Seats()) != 3 {
		t.Fatalf("seated %d players", len(tb.Seats()))
	}
}

// One identity must not hold two seats: it would let a player see two hands and collude with
// themselves.
func TestIdentityCannotTakeTwoSeats(t *testing.T) {
	tb := newTable(t)
	ks := keys(2)
	if _, err := tb.Join(ks[0]); err != nil {
		t.Fatal(err)
	}
	_, err := tb.Join(ks[0])
	if err == nil {
		t.Fatal("one identity took two seats")
	}
	if !strings.Contains(err.Error(), "already holds a seat") {
		t.Errorf("unclear error: %v", err)
	}
	// Case must not be a way around it.
	if _, err := tb.Join(strings.ToUpper(ks[0])); err == nil {
		t.Fatal("case variation bypassed the one-seat-per-identity rule")
	}
}

func TestFullTableRefusesJoins(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(3) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tb.Join(keys(4)[3]); err == nil {
		t.Fatal("a full table accepted another player")
	}
}

func TestJoinRequiresAnIdentity(t *testing.T) {
	tb := newTable(t)
	if _, err := tb.Join(""); err == nil {
		t.Fatal("a seat was taken with no identity")
	}
	if _, err := tb.Join("   "); err == nil {
		t.Fatal("a seat was taken with a blank identity")
	}
}

// The check that stops one player acting as another.
func TestAuthoriseRejectsAForgedSeatClaim(t *testing.T) {
	tb := newTable(t)
	ks := keys(2)
	for _, k := range ks {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}

	if err := tb.Authorise(0, ks[0]); err != nil {
		t.Fatalf("the real occupant was refused: %v", err)
	}
	// Seat 1's identity claiming seat 0.
	err := tb.Authorise(0, ks[1])
	if err == nil {
		t.Fatal("one seat's identity was authorised for another seat")
	}
	if !strings.Contains(err.Error(), "is not seat 0") {
		t.Errorf("unclear error: %v", err)
	}
	// An unseated identity.
	if err := tb.Authorise(0, keys(3)[2]); err == nil {
		t.Fatal("an unseated identity was authorised")
	}
	if err := tb.Authorise(9, ks[0]); err == nil {
		t.Fatal("a nonexistent seat was authorised")
	}
}

func TestSeatOf(t *testing.T) {
	tb := newTable(t)
	ks := keys(2)
	for _, k := range ks {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if idx, err := tb.SeatOf(ks[1]); err != nil || idx != 1 {
		t.Fatalf("SeatOf = %d, %v", idx, err)
	}
	if _, err := tb.SeatOf("unknown"); err == nil {
		t.Fatal("an unseated identity resolved to a seat")
	}
}

// The precondition the whole non-custodial design rests on: no stake before a refund.
func TestFundingRefusedBeforeRefundIsHeld(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(2) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}

	err := tb.MarkFunded(0)
	if err == nil {
		t.Fatal("a seat funded before holding a refund")
	}
	if !strings.Contains(err.Error(), "no refund yet") {
		t.Errorf("unclear error: %v", err)
	}

	if err := tb.MarkRefundHeld(0); err != nil {
		t.Fatal(err)
	}
	if err := tb.MarkFunded(0); err != nil {
		t.Fatalf("funding was refused after the refund was held: %v", err)
	}
}

// A hand must not start on a partial pot.
func TestDealRefusedOnAPartialPot(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(3) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}
	// Only two of three fund.
	for _, i := range []int{0, 1} {
		if err := tb.MarkRefundHeld(i); err != nil {
			t.Fatal(err)
		}
		if err := tb.MarkFunded(i); err != nil {
			t.Fatal(err)
		}
	}

	if tb.FullyFunded() {
		t.Fatal("a partially funded table reported as fully funded")
	}
	if err := tb.BeginDeal(); err == nil {
		t.Fatal("dealing began on a partial pot")
	}
	// The unfunded seat must be attributable.
	unfunded := tb.UnfundedSeats()
	if len(unfunded) != 1 || unfunded[0] != 2 {
		t.Fatalf("unfunded = %v, want [2]", unfunded)
	}

	if err := tb.MarkRefundHeld(2); err != nil {
		t.Fatal(err)
	}
	if err := tb.MarkFunded(2); err != nil {
		t.Fatal(err)
	}
	if !tb.FullyFunded() {
		t.Fatal("a fully funded table did not report as such")
	}
	if err := tb.BeginDeal(); err != nil {
		t.Fatalf("dealing was refused on a full pot: %v", err)
	}
	if tb.Phase() != PhaseDealing {
		t.Fatalf("phase = %s", tb.Phase())
	}
}

// Terms are frozen once the roster closes, so a table cannot advertise one buy-in and charge
// another.
func TestSeatsCannotBeTakenAfterRosterCloses(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(2) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Join(keys(3)[2]); err == nil {
		t.Fatal("a seat was taken after the roster closed")
	}
}

func TestRosterNeedsTwoSeats(t *testing.T) {
	tb := newTable(t)
	if _, err := tb.Join(keys(1)[0]); err != nil {
		t.Fatal(err)
	}
	if err := tb.CloseRoster(); err == nil {
		t.Fatal("a one-seat roster was closed")
	}
}

// Out-of-order progression must be refused: it is what keeps every seat's view in step.
func TestPhaseProgressionIsOrdered(t *testing.T) {
	tb := newTable(t)
	// Betting cannot be reached from open.
	if err := tb.Advance(PhaseBetting); err == nil {
		t.Fatal("jumped from open straight to betting")
	}
	if err := tb.Advance(PhaseFunding); err != nil {
		t.Fatal(err)
	}
	if err := tb.Advance(PhaseSettling); err == nil {
		t.Fatal("jumped from funding straight to settling")
	}
	if err := tb.Advance(PhaseDealing); err != nil {
		t.Fatal(err)
	}
	if err := tb.Advance(PhaseBetting); err != nil {
		t.Fatal(err)
	}
	if err := tb.Advance(PhaseSettling); err != nil {
		t.Fatal(err)
	}
	if err := tb.Advance(PhaseClosed); err != nil {
		t.Fatal(err)
	}
	// Nothing follows closed.
	if err := tb.Advance(PhaseOpen); err == nil {
		t.Fatal("a closed table was reopened")
	}
}

// A stall must name the seat responsible: the other seats need to know who to blame.
func TestStallIsAttributable(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(2) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}

	if err := tb.Stall(1, "did not send its shuffle contribution"); err != nil {
		t.Fatal(err)
	}
	if tb.Phase() != PhaseStalled {
		t.Fatalf("phase = %s, want stalled", tb.Phase())
	}
	seat, reason := tb.StallInfo()
	if seat != 1 {
		t.Errorf("blamed seat %d, want 1", seat)
	}
	if reason == "" {
		t.Error("no stall reason recorded")
	}

	// A stall without a reason is not explicable, so it is refused.
	tb2 := newTable(t)
	if err := tb2.Stall(0, ""); err == nil {
		t.Fatal("a stall with no reason was accepted")
	}
}

func TestStalledTableCanOnlyClose(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(2) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.Stall(0, "unresponsive"); err != nil {
		t.Fatal(err)
	}
	if err := tb.Advance(PhaseBetting); err == nil {
		t.Fatal("a stalled table resumed play")
	}
	if err := tb.Advance(PhaseClosed); err != nil {
		t.Fatalf("a stalled table could not close: %v", err)
	}
}

func TestTimedOutSeats(t *testing.T) {
	tb := newTable(t)
	base := time.Now()
	tb.now = func() time.Time { return base }

	for _, k := range keys(2) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	// Seat 0 speaks up later; seat 1 stays quiet.
	tb.now = func() time.Time { return base.Add(30 * time.Second) }
	if err := tb.Touch(0); err != nil {
		t.Fatal(err)
	}

	tb.now = func() time.Time { return base.Add(45 * time.Second) }
	out := tb.TimedOutSeats(20 * time.Second)
	if len(out) != 1 || out[0] != 1 {
		t.Fatalf("timed out = %v, want [1]", out)
	}
	if err := tb.Touch(9); err == nil {
		t.Error("touched a nonexistent seat")
	}
}

// The identity-key order is the pot script's key order, so it must be index-aligned and
// stable: a reordering would produce a different pot script and invalidate every signature.
func TestIdentityKeysAreSeatOrdered(t *testing.T) {
	tb := newTable(t)
	ks := keys(3)
	for _, k := range ks {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	got := tb.IdentityKeys()
	if len(got) != 3 {
		t.Fatalf("got %d keys", len(got))
	}
	for i, k := range ks {
		if got[i] != k {
			t.Errorf("key %d = %s…, want %s…", i, short(got[i]), short(k))
		}
	}
	// Stable across calls.
	for i := 0; i < 3; i++ {
		again := tb.IdentityKeys()
		for j := range got {
			if again[j] != got[j] {
				t.Fatal("identity key order varies between calls")
			}
		}
	}
}

func TestFundingRejectedOutsideFundingPhase(t *testing.T) {
	tb := newTable(t)
	for _, k := range keys(2) {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	// Still open, not funding.
	if err := tb.MarkRefundHeld(0); err != nil {
		t.Fatal(err)
	}
	if err := tb.MarkFunded(0); err == nil {
		t.Fatal("a buy-in was accepted while the table was still open")
	}
}

func TestSeatIndexBoundsChecked(t *testing.T) {
	tb := newTable(t)
	if _, err := tb.Join(keys(1)[0]); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{-1, 5} {
		if err := tb.MarkRefundHeld(i); err == nil {
			t.Errorf("MarkRefundHeld(%d) succeeded", i)
		}
		if err := tb.Touch(i); err == nil {
			t.Errorf("Touch(%d) succeeded", i)
		}
	}
}
