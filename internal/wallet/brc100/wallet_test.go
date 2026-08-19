package brc100

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptionsValidation(t *testing.T) {
	const key = "0000000000000000000000000000000000000000000000000000000000000001"

	tests := map[string]struct {
		opts    Options
		wantErr string
	}{
		"backend must be explicit": {
			opts:    Options{StorageName: "t", PrivateKeyHex: key},
			wantErr: "Backend is required",
		},
		"sqlite needs a path": {
			opts:    Options{Backend: BackendSQLite, StorageName: "t", PrivateKeyHex: key},
			wantErr: "SQLitePath is required",
		},
		"postgres needs a dsn": {
			opts:    Options{Backend: BackendPostgres, StorageName: "t", PrivateKeyHex: key},
			wantErr: "PostgresDSN is required",
		},
		"storage name required": {
			opts:    Options{Backend: BackendSQLite, SQLitePath: "x.db", PrivateKeyHex: key},
			wantErr: "StorageName is required",
		},
		"key required": {
			opts:    Options{Backend: BackendSQLite, SQLitePath: "x.db", StorageName: "t"},
			wantErr: "PrivateKeyHex is required",
		},
		"unknown backend": {
			opts:    Options{Backend: "mysql", SQLitePath: "x.db", StorageName: "t", PrivateKeyHex: key},
			wantErr: "unknown backend",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.opts.validate()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// Builds the whole wiring end to end against SQLite: storage, migration, wallet, and the
// monitor daemon. A fresh wallet must report a zero balance, since there is no UTXO
// discovery and no restore-from-seed.
func TestNewBuildsWalletAndReportsZeroBalance(t *testing.T) {
	ctx := context.Background()
	w, err := New(ctx, Options{
		Backend:       BackendSQLite,
		SQLitePath:    filepath.Join(t.TempDir(), "wallet.db"),
		StorageName:   "poker-test",
		PrivateKeyHex: "0000000000000000000000000000000000000000000000000000000000000001",
		MaxDBConns:    8,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })

	if w.Monitor == nil {
		t.Error("monitor daemon is nil; transactions would never receive a status")
	}
	if w.IdentityKeyHex == "" {
		t.Fatal("identity key is empty")
	}

	const originator = "poker.test"
	got, err := w.IdentityKey(ctx, originator)
	if err != nil {
		t.Fatalf("IdentityKey: %v", err)
	}
	if got != w.IdentityKeyHex {
		t.Errorf("wallet identity key %s does not match locally derived %s", got, w.IdentityKeyHex)
	}

	bal, err := w.Wallet.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Errorf("fresh wallet balance = %d, want 0", bal)
	}
}
