// Command table runs the poker table service.
//
// The service coordinates play and pays its own fees. It holds no player key and no pot key,
// so it can stall a hand and can never move a player's money — every settlement it proposes is
// verified independently by each seat before that seat signs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/galt-tr/go-arcade-toolbox/pkg/arcade"

	"github.com/cmurray/brc100-poker/internal/config"
	"github.com/cmurray/brc100-poker/internal/health"
	"github.com/cmurray/brc100-poker/internal/protocol/transport"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

// version is stamped at build time.
var version = "dev"

func main() {
	if err := run(); err != nil {
		// Configuration errors are the common case and are already fully explained, so
		// print them plainly rather than wrapping them in a stack of context.
		fmt.Fprintf(os.Stderr, "table: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Env()
	if err := cfg.Validate(); err != nil {
		// Refuse to start rather than degrade quietly. Validate reports every problem at
		// once so the operator fixes one round instead of one restart at a time.
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	level, err := config.ParseLogLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// The effective configuration is worth logging; the wallet key and the DSN password are
	// not, so Redacted omits them.
	logger.Info("starting the table service",
		"version", version,
		"config", cfg.Redacted())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reporter := health.New(cfg.RealValuePlay, cfg.BackupVerifiedAt != "")

	// ---- wallet -----------------------------------------------------------
	//
	// The table's own fee-paying wallet. Storage is the same instance the pot ledger uses,
	// so the wallet's coins and our record of the pots are backed up together — backing up
	// one without the other would leave a pot the service cannot locate.
	backend := brc100.BackendPostgres
	if cfg.PostgresDSN == "" {
		backend = brc100.BackendSQLite
	}

	w, err := brc100.New(ctx, brc100.Options{
		Backend:       backend,
		PostgresDSN:   cfg.PostgresDSN,
		SQLitePath:    cfg.SQLitePath,
		StorageName:   "poker-table",
		PrivateKeyHex: cfg.WalletKeyHex,
		MaxDBConns:    24,
		Logger:        logger,
	}, statusObserver(logger))
	if err != nil {
		reporter.Set(health.DepDatabase, health.StateDown, err.Error())
		return fmt.Errorf("building the wallet: %w", err)
	}
	defer func() {
		if cerr := w.Close(context.WithoutCancel(ctx)); cerr != nil {
			logger.Error("closing the wallet", "error", cerr)
		}
	}()
	reporter.Set(health.DepDatabase, health.StateUp, "")

	// Status tracking is not optional: without the monitor daemon, transactions never
	// receive a status at all.
	if err := w.Start(ctx); err != nil {
		reporter.Set(health.DepStatusTracking, health.StateDown, err.Error())
		return fmt.Errorf("starting status tracking: %w", err)
	}
	reporter.Set(health.DepStatusTracking, health.StateUp, "")

	identity, err := w.IdentityKey(ctx, cfg.Originator)
	if err != nil {
		return fmt.Errorf("resolving the table's identity key: %w", err)
	}
	logger.Info("table wallet ready", "identityKey", identity)

	// ---- chain dependencies ------------------------------------------------
	//
	// Checked at startup and then periodically: a hand cannot settle while the oracle is
	// unreachable, and that must be visible as a dependency outage rather than as a game
	// fault.
	checkChain(ctx, logger, w, cfg.Originator, reporter)
	go pollChain(ctx, logger, w, cfg.Originator, reporter)

	// ---- HTTP -------------------------------------------------------------
	hub := transport.NewHub(logger)
	defer func() {
		if cerr := hub.Close(); cerr != nil {
			logger.Error("closing the transport hub", "error", cerr)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", reporter.LivenessHandler())
	mux.HandleFunc("/readyz", reporter.ReadinessHandler())
	mux.Handle("/table", hub)
	// A root index, so opening the URL in a browser explains what this is and where the
	// endpoints are rather than returning a bare 404.
	mux.HandleFunc("/", indexHandler(cfg, identity, version))

	srv := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", cfg.ListenAddress)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	// Drain in-flight requests rather than cutting them off mid-hand.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// indexHandler describes the service at its root.
//
// Anything other than the exact root path is a genuine 404: a wrong path should say so rather than
// silently serving the index and letting a client believe it reached something.
func indexHandler(cfg config.Service, identityKey, version string) http.HandlerFunc {
	type endpoint struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	type index struct {
		Service     string     `json:"service"`
		Version     string     `json:"version"`
		Network     string     `json:"network"`
		IdentityKey string     `json:"identityKey"`
		Seats       int        `json:"seats"`
		BuyInSats   uint64     `json:"buyInSatoshis"`
		SmallBlind  uint64     `json:"smallBlind"`
		BigBlind    uint64     `json:"bigBlind"`
		Custody     string     `json:"custody"`
		Endpoints   []endpoint `json:"endpoints"`
	}

	// The identity key is public and a player needs it: it is what their agent authorises.
	body := index{
		Service:     "brc100-poker table service",
		Version:     version,
		Network:     "teratestnet",
		IdentityKey: identityKey,
		Seats:       cfg.Seats,
		BuyInSats:   cfg.BuyInSatoshis,
		SmallBlind:  cfg.SmallBlind,
		BigBlind:    cfg.BigBlind,
		Custody:     "non-custodial: this service holds no player key and cannot move a pot alone",
		Endpoints: []endpoint{
			{Path: "/livez", Description: "liveness — is the process running"},
			{Path: "/readyz", Description: "readiness — can it serve play, and is it fit to hold value"},
			{Path: "/table?table=<id>", Description: "game transport (WebSocket upgrade)"},
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(body); err != nil {
			// The response is already partly written by this point, so there is nothing
			// useful to send; record it and move on.
			_ = err
		}
	}
}

// statusObserver applies transaction status updates.
//
// The contract is strict and violating it degrades the whole pipeline: it runs inline on the
// applier goroutine so it must not block, delivery is at-least-once so it must be idempotent,
// and the slice must not be retained or mutated. Logging satisfies all three.
func statusObserver(logger *slog.Logger) brc100.StatusObserver {
	return func(recs []arcade.TxRecord) {
		for _, rec := range recs {
			logger.Debug("transaction status applied",
				"txid", rec.TxID, "status", string(rec.Status), "height", rec.BlockHeight)
		}
	}
}

// checkChain probes the chain services once.
func checkChain(ctx context.Context, logger *slog.Logger, w *brc100.Wallet, originator string, r *health.Reporter) {
	h, err := w.Wallet.GetHeight(ctx, nil, originator)
	if err != nil {
		r.Set(health.DepOracle, health.StateDown, err.Error())
		r.Set(health.DepHeaders, health.StateDown, err.Error())
		logger.Warn("chain services are unreachable", "error", err)
		return
	}
	r.Set(health.DepOracle, health.StateUp, "")
	r.Set(health.DepHeaders, health.StateUp, fmt.Sprintf("tip at height %d", h.Height))
}

// pollChain re-checks the chain services periodically.
//
// A dependency that was up at startup and is now down must stop reading as up, or readiness
// becomes a claim about the past.
func pollChain(ctx context.Context, logger *slog.Logger, w *brc100.Wallet, originator string, r *health.Reporter) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkChain(ctx, logger, w, originator, r)
		}
	}
}
