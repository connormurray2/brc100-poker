// Command fund claims teratestnet coins from the faucet into a local wallet.
//
// The faucet pays BRC-29: it derives a key from the wallet's identity key and returns the
// derivation material, which the wallet must internalize as a wallet payment for the coin
// to be spendable. This command does both halves.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

func main() {
	keyPath := flag.String("key", "", "path to the wallet private key (hex)")
	dbPath := flag.String("db", "wallet.db", "path to the wallet database (this IS the wallet: back it up)")
	originator := flag.String("originator", "poker.local", "FQDN-shaped originator for BRC-100 calls")
	captcha := flag.String("captcha", "", "Turnstile captcha token from the faucet page")
	bearer := flag.String("bearer", "", "faucet API bearer token (skips the captcha)")
	balanceOnly := flag.Bool("balance", false, "report the balance without claiming")
	flag.Parse()

	if *keyPath == "" {
		fmt.Fprintln(os.Stderr, "fund: -key is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*keyPath, *dbPath, *originator, *captcha, *bearer, *balanceOnly); err != nil {
		fmt.Fprintf(os.Stderr, "fund: %v\n", err)
		os.Exit(1)
	}
}

func run(keyPath, dbPath, originator, captcha, bearer string, balanceOnly bool) error {
	// The key path is supplied by the operator running this command; reading it is the
	// whole point of the flag.
	raw, err := os.ReadFile(keyPath) //nolint:gosec // operator-supplied path by design
	if err != nil {
		return fmt.Errorf("reading key: %w", err)
	}
	keyHex := strings.TrimSpace(string(raw))
	if _, err := hex.DecodeString(keyHex); err != nil {
		return fmt.Errorf("key file does not contain hex: %w", err)
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendSQLite,
		SQLitePath:    dbPath,
		StorageName:   "poker-fund",
		PrivateKeyHex: keyHex,
		MaxDBConns:    8,
		Logger:        logger,
	}, nil)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := w.Close(ctx); cerr != nil {
			fmt.Fprintf(os.Stderr, "fund: closing wallet: %v\n", cerr)
		}
	}()

	// Status tracking must run for a broadcast transaction to ever get a status.
	if err := w.Start(ctx); err != nil {
		return err
	}

	identity, err := w.IdentityKey(ctx, originator)
	if err != nil {
		return err
	}
	fmt.Printf("identity key: %s\n", identity)

	before, err := w.Wallet.Balance(ctx)
	if err != nil {
		return fmt.Errorf("reading balance: %w", err)
	}
	fmt.Printf("balance before: %d sat\n", before)

	if balanceOnly {
		return nil
	}

	claim, err := w.ClaimFromFaucet(ctx, originator, captcha, bearer)
	if err != nil {
		return err
	}
	fmt.Printf("claimed %d sat in tx %s\n", claim.Amount, claim.TxID)

	after, err := w.Wallet.Balance(ctx)
	if err != nil {
		return fmt.Errorf("reading balance: %w", err)
	}
	fmt.Printf("balance after: %d sat\n", after)
	if after <= before {
		return fmt.Errorf("balance did not increase (%d -> %d); the payment was not credited as spendable", before, after)
	}
	return nil
}
