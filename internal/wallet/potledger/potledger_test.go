package potledger

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return l
}

func samplePot() Pot {
	return Pot{
		HandID:        "hand-0001",
		Txid:          "d05c683aca4bd3436973eb74423d81e06349165f76534c00f69e98fabe9e5bc3",
		Vout:          0,
		Satoshis:      5000,
		LockingScript: "522103aa2102bb52ae",
		Seats:         []string{"03aa", "02bb"},
		State:         PotFunded,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	l := newLedger(t)
	// A second migration must not fail: deployments re-run it on every start.
	if err := l.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestRecordAndReadPot(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	want := samplePot()
	if err := l.RecordPot(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := l.Pot(ctx, want.HandID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Txid != want.Txid || got.Vout != want.Vout || got.Satoshis != want.Satoshis {
		t.Errorf("pot outpoint/value mismatch: %+v", got)
	}
	// The locking script must survive: the settlement sighash needs it and the wallet
	// does not keep it for us.
	if got.LockingScript != want.LockingScript {
		t.Errorf("locking script = %q, want %q", got.LockingScript, want.LockingScript)
	}
	if len(got.Seats) != 2 || got.Seats[0] != "03aa" || got.Seats[1] != "02bb" {
		t.Errorf("seats = %v, want [03aa 02bb]", got.Seats)
	}
	if got.State != PotFunded {
		t.Errorf("state = %q", got.State)
	}
}

func TestPotValidation(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)

	tests := map[string]func(p *Pot){
		"no hand id":        func(p *Pot) { p.HandID = "" },
		"no txid":           func(p *Pot) { p.Txid = "" },
		"no value":          func(p *Pot) { p.Satoshis = 0 },
		"no locking script": func(p *Pot) { p.LockingScript = "" },
		"no seats":          func(p *Pot) { p.Seats = nil },
		"bad state":         func(p *Pot) { p.State = PotState("nonsense") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := samplePot()
			mutate(&p)
			if err := l.RecordPot(ctx, p); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// The same output must not be recorded twice under different hands.
func TestDuplicateOutpointRejected(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	p := samplePot()
	if err := l.RecordPot(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.HandID = "hand-0002"
	if err := l.RecordPot(ctx, p); err == nil {
		t.Fatal("the same outpoint was recorded under two hands")
	}
}

func TestMissingPotReportsNotFound(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	if _, err := l.Pot(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := l.SetPotState(ctx, "absent", PotSettled); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPotState err = %v, want ErrNotFound", err)
	}
}

func TestSetPotStateAndQuery(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	if err := l.RecordPot(ctx, samplePot()); err != nil {
		t.Fatal(err)
	}
	if err := l.SetPotState(ctx, "hand-0001", PotSettling); err != nil {
		t.Fatal(err)
	}

	settling, err := l.PotsInState(ctx, PotSettling)
	if err != nil {
		t.Fatal(err)
	}
	if len(settling) != 1 || settling[0].HandID != "hand-0001" {
		t.Fatalf("PotsInState(settling) = %+v", settling)
	}
	funded, err := l.PotsInState(ctx, PotFunded)
	if err != nil {
		t.Fatal(err)
	}
	if len(funded) != 0 {
		t.Fatalf("pot still listed as funded: %+v", funded)
	}
	if err := l.SetPotState(ctx, "hand-0001", PotState("nonsense")); err == nil {
		t.Error("an invalid state was accepted")
	}
}

// The write-ahead property: an intent recorded before signing survives a restart, so the
// service knows a transaction may exist even though nothing confirmed it.
func TestIntentIsVisibleAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "pots.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordPot(ctx, samplePot()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordIntent(ctx, "hand-0001", "settlement", "aabbcc"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: close without ever recording an outcome.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	l2, err := New(db2)
	if err != nil {
		t.Fatal(err)
	}

	pending, err := l2.Unresolved(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("found %d unresolved attempts after restart, want 1", len(pending))
	}
	if pending[0].State != AttemptIntended {
		t.Errorf("state = %q, want %q", pending[0].State, AttemptIntended)
	}
	if pending[0].Txid != "aabbcc" {
		t.Errorf("txid = %q; without it the outcome cannot be reconciled", pending[0].Txid)
	}
}

func TestResolvedAttemptsAreNotUnresolved(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	if err := l.RecordPot(ctx, samplePot()); err != nil {
		t.Fatal(err)
	}

	confirmed, err := l.RecordIntent(ctx, "hand-0001", "settlement", "tx-confirmed")
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := l.RecordIntent(ctx, "hand-0001", "settlement", "tx-rejected")
	if err != nil {
		t.Fatal(err)
	}
	stillOpen, err := l.RecordIntent(ctx, "hand-0001", "refund", "tx-open")
	if err != nil {
		t.Fatal(err)
	}

	if err := l.SetAttemptState(ctx, confirmed, AttemptConfirmed, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAttemptState(ctx, rejected, AttemptRejected, "bad-txns-inputs-duplicate"); err != nil {
		t.Fatal(err)
	}

	pending, err := l.Unresolved(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != stillOpen {
		t.Fatalf("unresolved = %+v, want only the open attempt", pending)
	}

	all, err := l.Attempts(ctx, "hand-0001")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("attempts = %d, want 3", len(all))
	}
	// A rejection reason must be retained: it is the record of why money did not move.
	var foundReason bool
	for _, a := range all {
		if a.ID == rejected {
			if a.Reason != "bad-txns-inputs-duplicate" {
				t.Errorf("rejection reason = %q", a.Reason)
			}
			foundReason = true
		}
	}
	if !foundReason {
		t.Error("the rejected attempt was not returned")
	}
}

// A broadcast attempt is also unresolved: acceptance is not settlement.
func TestBroadcastCountsAsUnresolved(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	if err := l.RecordPot(ctx, samplePot()); err != nil {
		t.Fatal(err)
	}
	id, err := l.RecordIntent(ctx, "hand-0001", "settlement", "tx1")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetAttemptState(ctx, id, AttemptBroadcast, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := l.Unresolved(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatal("a broadcast attempt must remain unresolved until it confirms")
	}
}

func TestIntentValidation(t *testing.T) {
	ctx := context.Background()
	l := newLedger(t)
	for name, args := range map[string][3]string{
		"no hand":    {"", "settlement", "tx"},
		"no purpose": {"h", "", "tx"},
		"no txid":    {"h", "settlement", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := l.RecordIntent(ctx, args[0], args[1], args[2]); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if err := l.SetAttemptState(ctx, 99999, AttemptConfirmed, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestNewRequiresDB(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New accepted a nil database handle")
	}
}

func TestSeatEncodingRoundTrips(t *testing.T) {
	cases := [][]string{
		nil,
		{"03aa"},
		{"03aa", "02bb", "03cc", "02dd", "03ee", "02ff"},
	}
	for _, want := range cases {
		got := decodeSeats(encodeSeats(want))
		if len(want) == 0 {
			if len(got) != 0 {
				t.Errorf("empty seats decoded to %v", got)
			}
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("decoded %d seats, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("seat %d = %q, want %q", i, got[i], want[i])
			}
		}
	}
}
