// Command spendprobe proves a wallet can actually spend, not merely report a balance.
//
// This is step 4 of the restore drill. A restored wallet that lists outputs but cannot sign is
// worthless, and the difference is invisible until something tries to move a coin — so the drill
// tries.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

func main() {
	keyPath := flag.String("key", "", "path to the wallet key")
	dsn := flag.String("dsn", "", "Postgres DSN for the wallet database")
	storage := flag.String("storage", "poker-table", "storage name the wallet was created with")
	originator := flag.String("originator", "poker.local", "FQDN-shaped originator")
	amount := flag.Uint64("amount", 1000, "satoshis to move")
	flag.Parse()

	if *keyPath == "" || *dsn == "" {
		fmt.Fprintln(os.Stderr, "spendprobe: -key and -dsn are required")
		os.Exit(2)
	}
	if err := run(*keyPath, *dsn, *storage, *originator, *amount); err != nil {
		fmt.Fprintf(os.Stderr, "spendprobe: %v\n", err)
		os.Exit(1)
	}
}

func run(keyPath, dsn, storage, originator string, amount uint64) error {
	raw, err := os.ReadFile(keyPath) //nolint:gosec // operator-supplied path by design
	if err != nil {
		return err
	}
	keyHex := strings.TrimSpace(string(raw))
	kb, err := hex.DecodeString(keyHex)
	if err != nil {
		return fmt.Errorf("the key is not hex: %w", err)
	}
	priv, _ := ec.PrivateKeyFromBytes(kb)
	if priv == nil {
		return fmt.Errorf("the key is not a valid scalar")
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendPostgres,
		PostgresDSN:   dsn,
		StorageName:   storage,
		PrivateKeyHex: keyHex,
		MaxDBConns:    8,
		Logger:        logger,
	}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close(ctx) }()
	if err := w.Start(ctx); err != nil {
		return err
	}

	before, err := w.Wallet.Balance(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("balance before: %d sat\n", before)

	// Pay to the wallet's own address: the point is that signing and broadcast work, not where
	// the coin ends up.
	addr, err := script.NewAddressFromPublicKey(priv.PubKey(), false) // false => testnet
	if err != nil {
		return err
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return err
	}

	res, err := w.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: "restore drill spend probe",
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:     *lock,
			Satoshis:          amount,
			OutputDescription: "spend probe output value",
		}},
		Options: &sdk.CreateActionOptions{SignAndProcess: boolPtr(true), RandomizeOutputs: boolPtr(false)},
	}, originator)
	if err != nil {
		// This is the failure the drill exists to catch: the wallet has coins on paper and
		// cannot move them.
		return fmt.Errorf("the restored wallet could not spend: %w", err)
	}

	fmt.Printf("SPENT: the restored wallet signed and broadcast %s\n", res.Txid.String())
	after, err := w.Wallet.Balance(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("balance after:  %d sat\n", after)
	return nil
}

func boolPtr(b bool) *bool { return &b }
