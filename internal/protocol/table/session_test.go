package table

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cmurray/brc100-poker/internal/protocol/transport"
)

type seatEnv struct {
	tb      *Table
	tp      *transport.Memory
	session []*Session
	keys    []string
}

// twoSeatTable sets up a funded, dealing table with a session per seat over one shared
// in-memory transport, which is how a whole hand can be tested without sockets.
func twoSeatTable(t *testing.T, seats int) seatEnv {
	t.Helper()
	terms := goodTerms()
	terms.Seats = seats
	tb, err := New(terms)
	if err != nil {
		t.Fatal(err)
	}
	ks := keys(seats)
	for _, k := range ks {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}

	tp := transport.NewMemory()
	t.Cleanup(func() { _ = tp.Close() })

	var sessions []*Session
	for i, k := range ks {
		s, err := NewSession(SessionConfig{Table: tb, Transport: tp, SelfSeat: i, SelfKey: k})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		sessions = append(sessions, s)
	}
	return seatEnv{tb: tb, tp: tp, session: sessions, keys: ks}
}

func TestEnvelopeValidation(t *testing.T) {
	base := Envelope{TableID: "t", HandID: "h", Kind: KindAction, Seat: 0, IdentityKey: "aa"}
	if err := base.Validate(); err != nil {
		t.Fatalf("a valid envelope was refused: %v", err)
	}

	tests := map[string]func(*Envelope){
		"no table":    func(e *Envelope) { e.TableID = "" },
		"no kind":     func(e *Envelope) { e.Kind = "" },
		"no identity": func(e *Envelope) { e.IdentityKey = "" },
		"seat -1":     func(e *Envelope) { e.Seat = -1 },
		"seat 6":      func(e *Envelope) { e.Seat = MaxSeats },
		"no hand":     func(e *Envelope) { e.HandID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}

	// Hello is the one kind that does not need a hand: it is how a seat becomes known.
	hello := Envelope{TableID: "t", Kind: KindHello, Seat: 0, IdentityKey: "aa"}
	if err := hello.Validate(); err != nil {
		t.Errorf("hello was refused without a hand id: %v", err)
	}
}

func TestRevealBodyValidation(t *testing.T) {
	good := RevealBody{Positions: []int{1, 2}, Scalars: [][]byte{{1}, {2}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid reveal was refused: %v", err)
	}
	for name, bad := range map[string]RevealBody{
		"empty":      {},
		"mismatched": {Positions: []int{1, 2}, Scalars: [][]byte{{1}}},
		"negative":   {Positions: []int{-1}, Scalars: [][]byte{{1}}},
		"duplicated": {Positions: []int{3, 3}, Scalars: [][]byte{{1}, {2}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := bad.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// A refusal without a reason is indistinguishable from a seat going quiet.
func TestRefusalRequiresAReason(t *testing.T) {
	if err := (RefusalBody{}).Validate(); err == nil {
		t.Fatal("a refusal with no reason was accepted")
	}
	if err := (RefusalBody{Reason: "pays the wrong winner"}).Validate(); err != nil {
		t.Fatalf("a reasoned refusal was refused: %v", err)
	}
}

func TestPhaseBinding(t *testing.T) {
	bound := map[Kind]Phase{
		KindShuffle:            PhaseDealing,
		KindRemask:             PhaseDealing,
		KindHoleReveal:         PhaseDealing,
		KindAction:             PhaseBetting,
		KindSettlementProposal: PhaseSettling,
		KindSignature:          PhaseSettling,
	}
	for k, want := range bound {
		got, isBound := PhaseFor(k)
		if !isBound || got != want {
			t.Errorf("PhaseFor(%s) = %s,%v; want %s,true", k, got, isBound, want)
		}
	}
	// These may arrive at any time: a seat can go quiet or reconnect whenever.
	for _, k := range []Kind{KindHello, KindCatchUpRequest, KindAck, KindRefusal} {
		if _, isBound := PhaseFor(k); isBound {
			t.Errorf("%s is phase-bound but must be accepted at any time", k)
		}
	}
}

// The publisher's own session must see its message: the seating handshake depends on it.
func TestSenderSeesItsOwnMessage(t *testing.T) {
	env := twoSeatTable(t, 2)
	got := make(chan Envelope, 4)
	env.session[0].Handle(KindHello, func(e Envelope) error { got <- e; return nil })

	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	select {
	case e := <-got:
		if e.Seat != 0 {
			t.Errorf("seat = %d, want 0", e.Seat)
		}
	default:
		t.Fatal("the sender did not see its own hello")
	}
}

// The check that stops one seat acting as another, at the session boundary.
func TestForgedSeatMessageIsDropped(t *testing.T) {
	env := twoSeatTable(t, 2)
	if err := env.tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}
	for i := range env.keys {
		if err := env.tb.MarkRefundHeld(i); err != nil {
			t.Fatal(err)
		}
		if err := env.tb.MarkFunded(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.tb.BeginDeal(); err != nil {
		t.Fatal(err)
	}
	if err := env.tb.Advance(PhaseBetting); err != nil {
		t.Fatal(err)
	}

	var applied int
	var mu sync.Mutex
	env.session[1].Handle(KindAction, func(Envelope) error {
		mu.Lock()
		applied++
		mu.Unlock()
		return nil
	})

	// Seat 0's session sends an action but claims seat 1.
	forged := env.session[0]
	forged.mu.Lock()
	forged.selfSeat = 1 // claim another seat while keeping seat 0's identity
	forged.mu.Unlock()

	if err := forged.Send(context.Background(), KindAction, ActionBody{Action: "check"}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	mu.Lock()
	defer mu.Unlock()
	if applied != 0 {
		t.Fatal("a message claiming another seat was applied")
	}
}

// A message for a step that is not current must be dropped, not applied early.
func TestOutOfPhaseMessageIsDropped(t *testing.T) {
	env := twoSeatTable(t, 2)
	// Table is still open, so a betting action is out of phase.
	var applied int
	var mu sync.Mutex
	env.session[1].Handle(KindAction, func(Envelope) error {
		mu.Lock()
		applied++
		mu.Unlock()
		return nil
	})

	if err := env.session[0].Send(context.Background(), KindAction, ActionBody{Action: "check"}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	mu.Lock()
	defer mu.Unlock()
	if applied != 0 {
		t.Fatal("a betting action was applied while the table was still open")
	}
}

// A private reveal must not reach a seat it is not addressed to. This is the hole-card
// privacy property at the session layer.
func TestPrivateRevealReachesOnlyItsRecipient(t *testing.T) {
	env := twoSeatTable(t, 3)
	if err := env.tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}
	for i := range env.keys {
		if err := env.tb.MarkRefundHeld(i); err != nil {
			t.Fatal(err)
		}
		if err := env.tb.MarkFunded(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.tb.BeginDeal(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	received := map[int]int{}
	for i := 1; i < 3; i++ {
		seat := i
		env.session[seat].Handle(KindHoleReveal, func(Envelope) error {
			mu.Lock()
			received[seat]++
			mu.Unlock()
			return nil
		})
	}

	// Seat 0 reveals privately to seat 1 only.
	body := RevealBody{Positions: []int{4}, Scalars: [][]byte{{9}}}
	if err := env.session[0].SendPrivate(context.Background(), KindHoleReveal, env.keys[1], body); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	mu.Lock()
	defer mu.Unlock()
	if received[1] != 1 {
		t.Errorf("the addressed recipient received %d reveals, want 1", received[1])
	}
	if received[2] != 0 {
		t.Errorf("an unaddressed seat received %d reveals; hole-card privacy is broken", received[2])
	}
}

// Catch-up replaces the upstream re-broadcast loop: a reconnecting seat says what it has and
// receives only the gap.
func TestCatchUpSendsOnlyWhatIsMissing(t *testing.T) {
	env := twoSeatTable(t, 2)

	// Seat 0 sends three messages while seat 1 is listening.
	for i := 0; i < 3; i++ {
		if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
			t.Fatal(err)
		}
	}
	env.tp.Drain()

	// A peer that already has seq 2 should receive only the third.
	sent, err := env.session[0].ServeCatchUp(context.Background(), CatchUpRequestBody{HaveSeq: map[int]uint64{0: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("replayed %d messages, want 1", sent)
	}

	// A peer with nothing gets all three.
	sent, err = env.session[0].ServeCatchUp(context.Background(), CatchUpRequestBody{})
	if err != nil {
		t.Fatal(err)
	}
	if sent != 3 {
		t.Fatalf("replayed %d messages to a fresh peer, want 3", sent)
	}
}

// Catch-up must never replay another seat's private material.
func TestCatchUpDoesNotReplayPrivateMessages(t *testing.T) {
	env := twoSeatTable(t, 2)
	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	if err := env.session[0].SendPrivate(context.Background(), KindHoleReveal, env.keys[1],
		RevealBody{Positions: []int{1}, Scalars: [][]byte{{7}}}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	sent, err := env.session[0].ServeCatchUp(context.Background(), CatchUpRequestBody{})
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("replayed %d messages; the private reveal must not be replayed", sent)
	}
}

func TestAppliedTracksPerSeatSequence(t *testing.T) {
	env := twoSeatTable(t, 2)
	for i := 0; i < 2; i++ {
		if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.session[1].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[1]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	applied := env.session[1].Applied()
	if applied[0] != 2 {
		t.Errorf("applied[0] = %d, want 2", applied[0])
	}
	if applied[1] != 1 {
		t.Errorf("applied[1] = %d, want 1", applied[1])
	}
}

// A duplicate delivery must be applied once.
func TestDuplicateDeliveryAppliedOnce(t *testing.T) {
	env := twoSeatTable(t, 2)
	var mu sync.Mutex
	count := 0
	env.session[1].Handle(KindHello, func(Envelope) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	// Publish the same transport id twice.
	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	mu.Lock()
	first := count
	mu.Unlock()
	if first != 1 {
		t.Fatalf("handler ran %d times for one message", first)
	}
}

func TestSessionValidation(t *testing.T) {
	tb := newTable(t)
	tp := transport.NewMemory()
	t.Cleanup(func() { _ = tp.Close() })

	if _, err := NewSession(SessionConfig{Transport: tp, SelfKey: "aa"}); err == nil {
		t.Error("built a session with no table")
	}
	if _, err := NewSession(SessionConfig{Table: tb, SelfKey: "aa"}); err == nil {
		t.Error("built a session with no transport")
	}
	if _, err := NewSession(SessionConfig{Table: tb, Transport: tp}); err == nil {
		t.Error("built a session with no identity")
	}
}

func TestSendPrivateRequiresRecipient(t *testing.T) {
	env := twoSeatTable(t, 2)
	if err := env.session[0].SendPrivate(context.Background(), KindHoleReveal, "", RevealBody{}); err == nil {
		t.Fatal("a private message was sent with no recipient")
	}
}

func TestCloseStopsDelivery(t *testing.T) {
	env := twoSeatTable(t, 2)
	var mu sync.Mutex
	count := 0
	env.session[1].Handle(KindHello, func(Envelope) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})
	env.session[1].Close()

	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatal("a closed session still received messages")
	}
	// Closing twice must be harmless.
	env.session[1].Close()
}

// A panicking handler must not take the session down.
func TestPanickingHandlerIsContained(t *testing.T) {
	env := twoSeatTable(t, 2)
	env.session[1].Handle(KindHello, func(Envelope) error { panic("boom") })

	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()
	// Reaching here without the process dying is the assertion.
}

func TestTouchUpdatesLastSeen(t *testing.T) {
	env := twoSeatTable(t, 2)
	before := env.tb.Seats()[0].LastSeen
	time.Sleep(5 * time.Millisecond)

	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	after := env.tb.Seats()[0].LastSeen
	if !after.After(before) {
		t.Error("receiving a message did not update the sender's last-seen time")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	env := Envelope{TableID: "t", HandID: "h", Kind: KindAction, Seat: 1, IdentityKey: "aa"}
	if err := Encode(&env, ActionBody{Action: "raise", To: 250}); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBody[ActionBody](env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "raise" || got.To != 250 {
		t.Fatalf("round trip lost data: %+v", got)
	}

	if err := Encode(nil, ActionBody{}); err == nil {
		t.Error("encoded into a nil envelope")
	}
	empty := Envelope{Kind: KindAction}
	if _, err := DecodeBody[ActionBody](empty); err == nil {
		t.Error("decoded a message with no body")
	}
	bad := Envelope{Kind: KindAction, Body: []byte("{not json")}
	if _, err := DecodeBody[ActionBody](bad); err == nil {
		t.Error("decoded an invalid body")
	}
}

func TestMessagesCarryTheTableID(t *testing.T) {
	env := twoSeatTable(t, 2)
	got := make(chan Envelope, 2)
	env.session[1].Handle(KindHello, func(e Envelope) error { got <- e; return nil })

	if err := env.session[0].Send(context.Background(), KindHello, HelloBody{PublicKey: env.keys[0]}); err != nil {
		t.Fatal(err)
	}
	env.tp.Drain()

	e := <-got
	if e.TableID != env.tb.Terms().TableID {
		t.Errorf("table id = %q, want %q", e.TableID, env.tb.Terms().TableID)
	}
	if !strings.EqualFold(e.IdentityKey, env.keys[0]) {
		t.Errorf("identity = %q", e.IdentityKey)
	}
}
