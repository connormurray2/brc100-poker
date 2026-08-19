// Command agent runs one player's side of the game.
//
// It holds the player's key and never gives it up. A table can ask it to sign, and it signs only
// when the transaction matches the player's own record of the hand AND the player approves it.
// That is what makes the design non-custodial in practice: the table proposes, the player decides.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cmurray/brc100-poker/internal/agent"
	"github.com/cmurray/brc100-poker/internal/config"
	"github.com/cmurray/brc100-poker/internal/health"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	keyPath := flag.String("key", "", "path to the player's private key (required)")
	dbPath := flag.String("db", "player.db", "path to the player's wallet database (this IS the wallet: back it up)")
	listen := flag.String("listen", "127.0.0.1:8091", "address to serve the substrate on")
	originator := flag.String("originator", "agent.poker.local", "FQDN-shaped originator for BRC-100 calls")
	table := flag.String("table", "", "identity key of the table service to authorise")
	autoApprove := flag.Bool("auto-approve", false, "approve every signing request without asking (development only)")
	requireTLS := flag.Bool("require-tls", false, "refuse substrate calls over plaintext")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	flag.Parse()

	if *keyPath == "" {
		flag.Usage()
		return errors.New("-key is required")
	}

	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	raw, err := os.ReadFile(*keyPath) //nolint:gosec // operator-supplied path by design
	if err != nil {
		return fmt.Errorf("reading the player key: %w", err)
	}
	keyHex := strings.TrimSpace(string(raw))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendSQLite,
		SQLitePath:    *dbPath,
		StorageName:   "poker-player",
		PrivateKeyHex: keyHex,
		MaxDBConns:    8,
		Logger:        logger,
	}, nil)
	if err != nil {
		return fmt.Errorf("building the player wallet: %w", err)
	}
	defer func() {
		if cerr := w.Close(context.WithoutCancel(ctx)); cerr != nil {
			logger.Error("closing the wallet", "error", cerr)
		}
	}()
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("starting status tracking: %w", err)
	}

	// Auto-approval is a development convenience and a custodial posture in disguise, so it
	// is opt-in, loud, and never the default.
	approver := interactiveApprover(logger)
	if *autoApprove {
		logger.Warn("auto-approving every signing request: this is development behaviour and gives away the protection the agent exists to provide")
		approver = substrate.ApproverFunc(func(substrate.SigningRequest) error { return nil })
	}

	a, err := agent.New(agent.Config{
		PrivateKeyHex: keyHex,
		Wallet:        w,
		Approver:      approver,
		RequireTLS:    *requireTLS,
		Originator:    *originator,
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	if *table != "" {
		if err := a.GrantTable(*table); err != nil {
			return fmt.Errorf("authorising the table: %w", err)
		}
		logger.Info("authorised a table service", "table", *table)
	} else {
		logger.Warn("no table authorised: pass -table with the table's identity key before joining a hand")
	}

	balance, err := w.Wallet.Balance(ctx)
	if err != nil {
		return fmt.Errorf("reading the balance: %w", err)
	}

	fmt.Printf("agent version:   %s\n", version)
	fmt.Printf("player identity: %s\n", a.Identity())
	fmt.Printf("balance:         %d sat\n", balance)
	fmt.Printf("substrate:       http://%s/  (audience %s)\n", *listen, a.Server().Audience())

	reporter := health.New(false, false)
	for _, dep := range []string{health.DepDatabase, health.DepStatusTracking} {
		reporter.Set(dep, health.StateUp, "")
	}
	// The agent does not settle on its own behalf, so chain reachability is informational
	// rather than a precondition for it to be useful.
	reporter.SetOptional(health.DepOracle, health.StateUnknown, "not checked by the agent")
	reporter.SetOptional(health.DepHeaders, health.StateUnknown, "not checked by the agent")

	mux := http.NewServeMux()
	mux.Handle("/", a.Server())
	mux.HandleFunc("/livez", reporter.LivenessHandler())

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
		fmt.Println("\nshutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// interactiveApprover asks the player on the terminal.
//
// The prompt shows the material terms rather than asking "sign this?", because a prompt that
// does not say what is being signed is a rubber stamp — and an undeclared output is exactly how
// a pot gets skimmed. Anything other than an explicit yes declines: a mistyped answer must not
// move money.
func interactiveApprover(logger *slog.Logger) substrate.Approver {
	reader := bufio.NewReader(os.Stdin)

	return substrate.ApproverFunc(func(req substrate.SigningRequest) error {
		fmt.Println()
		fmt.Println("─── signing request ────────────────────────────────────")
		fmt.Printf("  hand:    %s\n", req.HandID)
		fmt.Printf("  purpose: %s\n", req.Purpose)
		fmt.Printf("  pot:     %s (%d sat)\n", req.PotOutpoint, req.PotSatoshis)
		fmt.Println("  outputs:")
		for _, o := range req.Outputs {
			fmt.Printf("    %10d sat  %-24s  %s\n", o.Satoshis, o.Description, truncate(o.LockingScript, 24))
		}
		if req.FeeSatoshis > 0 {
			fmt.Printf("  fee:     %d sat\n", req.FeeSatoshis)
		}
		fmt.Println("────────────────────────────────────────────────────────")
		fmt.Print("  sign this? [y/N] ")

		answer, err := reader.ReadString('\n')
		if err != nil {
			// A closed or unreadable stdin must decline, not approve: failing open here
			// would be the custodial behaviour the agent exists to avoid.
			logger.Warn("could not read an approval, declining", "error", err)
			return &substrate.Error{Code: substrate.CodeDeclined, Message: "no approval could be read"}
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return nil
		default:
			return &substrate.Error{Code: substrate.CodeDeclined, Message: "the player declined"}
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
