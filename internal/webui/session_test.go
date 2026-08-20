package webui

import (
	"testing"

	"github.com/cmurray/brc100-poker/internal/protocol/table"
)

// A table plays a session, not a single hand. These assert what "keep dealing" has to mean:
// chips carry, the button moves, and the table stops when a player leaves.

func sessionTable(t *testing.T) (*LiveTable, []string) {
	t.Helper()
	l, err := NewLiveTable(table.Terms{
		TableID: "s", BuyInSatoshis: 5000, SmallBlind: 25, BigBlind: 50,
		Seats: 2, RefundLockHeight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"02" + "11111111111111111111111111111111111111111111111111111111111111",
		"03" + "22222222222222222222222222222222222222222222222222222222222222",
	}
	for _, k := range keys {
		if _, err := l.Join(k); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	// Ready both seats: the second triggers the first deal.
	for _, k := range keys {
		if err := l.Ready(k); err != nil {
			t.Fatalf("ready: %v", err)
		}
	}
	return l, keys
}

// Playing to the end of a hand and asking for another must produce a genuinely new hand, with the
// button moved and the chips from the previous one.
func TestSessionDealsAnotherHandWithCarriedStacksAndAMovedButton(t *testing.T) {
	l, keys := sessionTable(t)

	if l.st == nil {
		t.Fatal("the first hand did not start")
	}
	buttonFirst := l.button

	// Fold to end the hand immediately: whoever is to act gives it up.
	toAct := l.st.ToAct
	var folder string
	for k, seat := range l.seatOf {
		if seat == toAct {
			folder = k
		}
	}
	if err := l.Act(folder, "fold", 0); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if !l.HandOver() {
		t.Fatal("the hand did not end on a fold")
	}

	// Stacks must not be equal to the buy-in any more: someone won the blinds.
	stacksAfter := append([]int64(nil), l.stacks...)
	if stacksAfter[0] == 5000 && stacksAfter[1] == 5000 {
		t.Fatalf("stacks did not change after a hand: %v", stacksAfter)
	}
	total := stacksAfter[0] + stacksAfter[1]
	if total != 10000 {
		t.Fatalf("chips were created or destroyed: total %d, want 10000", total)
	}

	dealt, err := l.NextHand()
	if err != nil {
		t.Fatalf("NextHand: %v", err)
	}
	if !dealt {
		t.Fatal("the table refused to deal a second hand")
	}
	if l.button == buttonFirst {
		t.Fatalf("the button did not move: still %d", l.button)
	}
	if l.st == nil || l.st.Done {
		t.Fatal("the second hand is not in play")
	}

	// The new hand must start from the carried stacks, not a fresh buy-in.
	var started int64
	for _, s := range l.st.Seats {
		started += s.Stack
	}
	// Blinds are posted, so the stacks plus the pot must equal what was carried.
	if started+l.st.Pot() != total {
		t.Fatalf("the second hand started with %d + pot %d, want %d carried",
			started, l.st.Pot(), total)
	}
	_ = keys
}

// Asking for another hand mid-hand must be refused, or a caller could cut a hand short.
func TestNextHandRefusesWhileAHandIsLive(t *testing.T) {
	l, _ := sessionTable(t)
	if l.st == nil {
		t.Fatal("the first hand did not start")
	}
	dealt, err := l.NextHand()
	if err != nil {
		t.Fatalf("NextHand: %v", err)
	}
	if dealt {
		t.Fatal("a new hand was dealt while one was still in play")
	}
}

// A player getting up is what ends a session.
func TestSessionStopsWhenAPlayerSitsOut(t *testing.T) {
	l, keys := sessionTable(t)

	toAct := l.st.ToAct
	var folder string
	for k, seat := range l.seatOf {
		if seat == toAct {
			folder = k
		}
	}
	if err := l.Act(folder, "fold", 0); err != nil {
		t.Fatalf("fold: %v", err)
	}

	if err := l.SitOut(keys[0]); err != nil {
		t.Fatalf("SitOut: %v", err)
	}
	dealt, err := l.NextHand()
	if err != nil {
		t.Fatalf("NextHand: %v", err)
	}
	if dealt {
		t.Fatal("the table dealt another hand after a player left")
	}
}

// A seat with no chips cannot post a blind, so the session ends rather than dealing a hand
// nobody can bet in.
func TestSessionStopsWhenASeatCannotCoverTheBigBlind(t *testing.T) {
	l, _ := sessionTable(t)
	toAct := l.st.ToAct
	var folder string
	for k, seat := range l.seatOf {
		if seat == toAct {
			folder = k
		}
	}
	if err := l.Act(folder, "fold", 0); err != nil {
		t.Fatalf("fold: %v", err)
	}
	// Simulate a busted seat.
	l.mu.Lock()
	l.stacks[0] = 10
	l.mu.Unlock()

	dealt, err := l.NextHand()
	if err != nil {
		t.Fatalf("NextHand: %v", err)
	}
	if dealt {
		t.Fatal("the table dealt a hand a seat could not post a blind for")
	}
}

// The regression a player actually hit: consecutive hands dealt identical cards.
//
// This goes through LiveTable rather than calling the coordinator directly, because the bug was
// in what LiveTable passed as the hand ID. A test that calls Deal itself cannot catch it.
func TestSessionHandIDChangesEveryHand(t *testing.T) {
	l, _ := sessionTable(t)

	seen := map[string]int{}
	for hand := 0; hand < 3; hand++ {
		id := func() string {
			l.mu.Lock()
			defer l.mu.Unlock()
			return l.handIDLocked()
		}()
		if prev, dup := seen[id]; dup {
			t.Fatalf("hand %d reused the ID from hand %d: %q -- every seat's wallet would "+
				"return its cached secrets and deal the identical hand", hand, prev, id)
		}
		seen[id] = hand

		// Fold to finish the hand.
		l.mu.Lock()
		toAct := l.st.ToAct
		l.mu.Unlock()
		var folder string
		for k, seat := range l.seatOf {
			if seat == toAct {
				folder = k
			}
		}
		if err := l.Act(folder, "fold", 0); err != nil {
			t.Fatalf("hand %d fold: %v", hand, err)
		}
		if hand < 2 {
			if dealt, err := l.NextHand(); err != nil || !dealt {
				t.Fatalf("hand %d did not lead to another: dealt=%v err=%v", hand, dealt, err)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("three hands produced %d distinct IDs", len(seen))
	}
}
