// Package potledger records pot outputs and settlement attempts.
//
// It exists because the wallet will not do it for us. A custom-script output comes back
// `Spendable=false` and is never minted into the wallet's UTXO store, and a caller-provided
// input is never reserved or spend-checked — both confirmed empirically by the co-signing
// spike. The application owns the lifecycle of its own pot coins.
//
// The write-ahead discipline matters more than the schema. A crash between signing and
// broadcasting must be recoverable: on restart the service has to be able to say "this pot
// may already be spent, go and find out" rather than either double-spending it or
// abandoning it.
package potledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PotState is where a pot output is in its lifecycle.
type PotState string

const (
	// PotFunding means the funding transaction has been built but not confirmed.
	PotFunding PotState = "funding"
	// PotFunded means the pot output exists and is playable.
	PotFunded PotState = "funded"
	// PotSettling means a settlement has been signed or broadcast. The pot may or may
	// not still be spendable, and that is exactly why this state exists.
	PotSettling PotState = "settling"
	// PotSettled means a settlement is confirmed.
	PotSettled PotState = "settled"
	// PotRefunded means the pot was returned to its funders.
	PotRefunded PotState = "refunded"
	// PotAbandoned means the pot could not be funded and no funds are at risk.
	PotAbandoned PotState = "abandoned"
)

// Valid reports whether the state is one this package recognises.
func (s PotState) Valid() bool {
	switch s {
	case PotFunding, PotFunded, PotSettling, PotSettled, PotRefunded, PotAbandoned:
		return true
	default:
		return false
	}
}

// AttemptState tracks one sign-and-broadcast attempt.
type AttemptState string

const (
	// AttemptIntended is written BEFORE signing. Its presence after a restart means a
	// transaction may exist on the network even though nothing confirmed it.
	AttemptIntended AttemptState = "intended"
	// AttemptBroadcast means the transaction was handed to the network.
	AttemptBroadcast AttemptState = "broadcast"
	// AttemptConfirmed means the network confirmed it.
	AttemptConfirmed AttemptState = "confirmed"
	// AttemptRejected means the network refused it. Final: never retry a rejection.
	AttemptRejected AttemptState = "rejected"
	// AttemptUnknown means the outcome could not be determined and must be reconciled.
	AttemptUnknown AttemptState = "unknown"
)

// Pot is a shared output the application controls but the wallet cannot sign for.
type Pot struct {
	// HandID ties the pot to a hand.
	HandID string
	// Txid and Vout locate the output. Vout is recorded rather than assumed, because
	// output randomisation is disabled but a future change must not silently move it.
	Txid string
	Vout uint32
	// Satoshis is the pot value.
	Satoshis uint64
	// LockingScript is the pot's script, hex-encoded. Stored because the sighash
	// preimage for a settlement needs it and the wallet does not keep it for us.
	LockingScript string
	// Seats lists the identity keys that must all authorise a spend.
	Seats []string
	State PotState

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Attempt is one sign-and-broadcast attempt against a pot.
type Attempt struct {
	ID     int64
	HandID string
	// Purpose distinguishes a settlement from a refund.
	Purpose string
	// Txid is the transaction this attempt concerns. It is known before broadcast
	// because the transaction is fully formed by then.
	Txid  string
	State AttemptState
	// Reason carries a rejection reason or a reconciliation note.
	Reason string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ledger records pots and attempts.
type Ledger struct {
	db *sql.DB
}

// New returns a ledger over an already-open database.
//
// The caller owns the handle: in deployment this is the same Postgres instance the wallet
// uses, so the pot ledger and the wallet's own state are backed up together. Backing up one
// without the other would leave a pot the service cannot locate.
func New(db *sql.DB) (*Ledger, error) {
	if db == nil {
		return nil, errors.New("potledger: a database handle is required")
	}
	return &Ledger{db: db}, nil
}

// Migrate creates the schema. Idempotent.
//
// Schema changes must be additive and forward-only: restoring an older wallet snapshot to
// undo a migration can permanently lose knowledge of coins.
func (l *Ledger) Migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS pots (
	hand_id        TEXT PRIMARY KEY,
	txid           TEXT NOT NULL,
	vout           INTEGER NOT NULL,
	satoshis       INTEGER NOT NULL,
	locking_script TEXT NOT NULL,
	seats          TEXT NOT NULL,
	state          TEXT NOT NULL,
	created_at     TIMESTAMP NOT NULL,
	updated_at     TIMESTAMP NOT NULL,
	UNIQUE (txid, vout)
);

CREATE TABLE IF NOT EXISTS attempts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	hand_id    TEXT NOT NULL,
	purpose    TEXT NOT NULL,
	txid       TEXT NOT NULL,
	state      TEXT NOT NULL,
	reason     TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS attempts_hand_idx ON attempts (hand_id);
CREATE INDEX IF NOT EXISTS pots_state_idx ON pots (state);
`
	if _, err := l.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("potledger: creating schema: %w", err)
	}
	return nil
}

// RecordPot stores a pot output.
func (l *Ledger) RecordPot(ctx context.Context, p Pot) error {
	if p.HandID == "" {
		return errors.New("potledger: hand id is required")
	}
	if p.Txid == "" {
		return errors.New("potledger: txid is required")
	}
	if p.Satoshis == 0 {
		return errors.New("potledger: a pot with no value is not a pot")
	}
	if p.LockingScript == "" {
		return errors.New("potledger: locking script is required to spend the pot later")
	}
	if len(p.Seats) == 0 {
		return errors.New("potledger: a pot must record the seats that authorise it")
	}
	if !p.State.Valid() {
		return fmt.Errorf("potledger: unknown pot state %q", p.State)
	}

	now := time.Now().UTC()
	_, err := l.db.ExecContext(ctx, `
INSERT INTO pots (hand_id, txid, vout, satoshis, locking_script, seats, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.HandID, p.Txid, p.Vout, p.Satoshis, p.LockingScript, encodeSeats(p.Seats), string(p.State), now, now)
	if err != nil {
		return fmt.Errorf("potledger: recording pot for hand %s: %w", p.HandID, err)
	}
	return nil
}

// Pot returns a pot by hand id.
func (l *Ledger) Pot(ctx context.Context, handID string) (Pot, error) {
	row := l.db.QueryRowContext(ctx, `
SELECT hand_id, txid, vout, satoshis, locking_script, seats, state, created_at, updated_at
FROM pots WHERE hand_id = ?`, handID)

	var p Pot
	var seats, state string
	err := row.Scan(&p.HandID, &p.Txid, &p.Vout, &p.Satoshis, &p.LockingScript, &seats, &state, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Pot{}, fmt.Errorf("potledger: no pot for hand %s: %w", handID, ErrNotFound)
	}
	if err != nil {
		return Pot{}, fmt.Errorf("potledger: reading pot for hand %s: %w", handID, err)
	}
	p.Seats = decodeSeats(seats)
	p.State = PotState(state)
	return p, nil
}

// ErrNotFound reports a missing record.
var ErrNotFound = errors.New("potledger: not found")

// SetPotState advances a pot's state.
func (l *Ledger) SetPotState(ctx context.Context, handID string, state PotState) error {
	if !state.Valid() {
		return fmt.Errorf("potledger: unknown pot state %q", state)
	}
	res, err := l.db.ExecContext(ctx,
		`UPDATE pots SET state = ?, updated_at = ? WHERE hand_id = ?`,
		string(state), time.Now().UTC(), handID)
	if err != nil {
		return fmt.Errorf("potledger: updating pot state for hand %s: %w", handID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("potledger: checking the pot update for hand %s: %w", handID, err)
	}
	if n == 0 {
		return fmt.Errorf("potledger: no pot for hand %s: %w", handID, ErrNotFound)
	}
	return nil
}

// PotsInState lists pots in a given state.
//
// This is the restart query: pots left in PotSettling are the ones whose fate is unknown
// and which must be reconciled against the network before anything else happens to them.
func (l *Ledger) PotsInState(ctx context.Context, state PotState) ([]Pot, error) {
	rows, err := l.db.QueryContext(ctx, `
SELECT hand_id, txid, vout, satoshis, locking_script, seats, state, created_at, updated_at
FROM pots WHERE state = ? ORDER BY created_at`, string(state))
	if err != nil {
		return nil, fmt.Errorf("potledger: listing pots in state %s: %w", state, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Pot
	for rows.Next() {
		var p Pot
		var seats, st string
		if err := rows.Scan(&p.HandID, &p.Txid, &p.Vout, &p.Satoshis, &p.LockingScript, &seats, &st, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("potledger: scanning a pot: %w", err)
		}
		p.Seats = decodeSeats(seats)
		p.State = PotState(st)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("potledger: iterating pots: %w", err)
	}
	return out, nil
}

// RecordIntent writes an attempt BEFORE the transaction is signed or broadcast.
//
// This is the write-ahead record. If the process dies immediately after this call, the
// restart path sees an intended attempt and knows a transaction may exist on the network
// even though nothing ever confirmed it.
func (l *Ledger) RecordIntent(ctx context.Context, handID, purpose, txid string) (int64, error) {
	if handID == "" || purpose == "" || txid == "" {
		return 0, errors.New("potledger: hand id, purpose and txid are all required for an intent")
	}
	now := time.Now().UTC()
	res, err := l.db.ExecContext(ctx, `
INSERT INTO attempts (hand_id, purpose, txid, state, reason, created_at, updated_at)
VALUES (?, ?, ?, ?, '', ?, ?)`,
		handID, purpose, txid, string(AttemptIntended), now, now)
	if err != nil {
		return 0, fmt.Errorf("potledger: recording intent for hand %s: %w", handID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("potledger: reading the intent id: %w", err)
	}
	return id, nil
}

// SetAttemptState records an attempt's outcome.
func (l *Ledger) SetAttemptState(ctx context.Context, id int64, state AttemptState, reason string) error {
	res, err := l.db.ExecContext(ctx,
		`UPDATE attempts SET state = ?, reason = ?, updated_at = ? WHERE id = ?`,
		string(state), reason, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("potledger: updating attempt %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("potledger: checking the attempt update: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("potledger: no attempt %d: %w", id, ErrNotFound)
	}
	return nil
}

// Attempts lists a hand's attempts oldest first.
func (l *Ledger) Attempts(ctx context.Context, handID string) ([]Attempt, error) {
	rows, err := l.db.QueryContext(ctx, `
SELECT id, hand_id, purpose, txid, state, reason, created_at, updated_at
FROM attempts WHERE hand_id = ? ORDER BY id`, handID)
	if err != nil {
		return nil, fmt.Errorf("potledger: listing attempts for hand %s: %w", handID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Attempt
	for rows.Next() {
		var a Attempt
		var st string
		if err := rows.Scan(&a.ID, &a.HandID, &a.Purpose, &a.Txid, &st, &a.Reason, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("potledger: scanning an attempt: %w", err)
		}
		a.State = AttemptState(st)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("potledger: iterating attempts: %w", err)
	}
	return out, nil
}

// Unresolved lists attempts whose outcome is not final.
//
// Called on startup. Every attempt this returns needs its fate established against the
// network before the pot it concerns is touched again.
func (l *Ledger) Unresolved(ctx context.Context) ([]Attempt, error) {
	rows, err := l.db.QueryContext(ctx, `
SELECT id, hand_id, purpose, txid, state, reason, created_at, updated_at
FROM attempts WHERE state IN (?, ?, ?) ORDER BY id`,
		string(AttemptIntended), string(AttemptBroadcast), string(AttemptUnknown))
	if err != nil {
		return nil, fmt.Errorf("potledger: listing unresolved attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Attempt
	for rows.Next() {
		var a Attempt
		var st string
		if err := rows.Scan(&a.ID, &a.HandID, &a.Purpose, &a.Txid, &st, &a.Reason, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("potledger: scanning an attempt: %w", err)
		}
		a.State = AttemptState(st)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("potledger: iterating attempts: %w", err)
	}
	return out, nil
}

// encodeSeats joins seat keys with a newline. Identity keys are hex, so they cannot
// contain the separator.
func encodeSeats(seats []string) string {
	out := ""
	for i, s := range seats {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

func decodeSeats(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
