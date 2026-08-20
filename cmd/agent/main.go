// Command agent runs one player's side of the game.
//
// It holds the player's key and never gives it up. A table can ask it to sign, and it signs only
// when the transaction matches the player's own record of the hand AND the player approves it.
// That is what makes the design non-custodial in practice: the table proposes, the player decides.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	origins := flag.String("origin", "", "comma-separated web origins allowed to call this agent from a browser")
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

	var allowed []string
	for _, o := range strings.Split(*origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed = append(allowed, o)
		}
	}

	a, err := agent.New(agent.Config{
		PrivateKeyHex:  keyHex,
		Wallet:         w,
		Approver:       approver,
		RequireTLS:     *requireTLS,
		Originator:     *originator,
		AllowedOrigins: allowed,
		Logger:         logger,
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
	// /identity lets a browser client discover which player this agent speaks for, and doubles
	// as the connection test. It returns only public values: the identity key and the audience
	// a caller must address requests to.
	mux.HandleFunc("/identity", identityHandler(a, w, allowed))
	// Funding from the page, so a player never has to stop the agent to claim coins.
	mux.HandleFunc("/faucet", faucetHandler(w, *originator, allowed, logger))
	// Owner-only methods, reachable from an allowed origin without a substrate signature: the
	// page is the player's own client and cannot sign as them. Origin is the trust boundary
	// here, the same one the faucet button relies on, and only loopback is served by default.
	mux.HandleFunc("/owner/recordStake", ownerHandler(a, allowed, logger))

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

// guardOrigin applies the browser origin allowlist and handles the preflight.
//
// Returns false when the request has already been answered and the caller must stop. Shared by
// every browser-facing endpoint so one of them cannot accidentally be left open.
func guardOrigin(allowed []string, methods string) func(http.ResponseWriter, *http.Request) bool {
	permitted := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		permitted[strings.ToLower(o)] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := permitted[strings.ToLower(origin)]; !ok {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return false
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			w.Header().Set("Access-Control-Allow-Methods", methods)
			w.WriteHeader(http.StatusNoContent)
			return false
		}
		return true
	}
}

// identityHandler serves the agent's public identity and balance to a browser client.
//
// Deliberately unauthenticated: everything it returns is public, and a client needs it before it
// can authenticate anything. It is still origin-checked, so an unlisted page cannot discover which
// wallet is running here.
//
// The balance is included because the page's first job is telling a player whether they can afford
// to sit down, and asking them to read it off a terminal is a worse answer than showing it.
func identityHandler(a *agent.Agent, w2 *brc100.Wallet, allowed []string) http.HandlerFunc {
	guard := guardOrigin(allowed, "GET, OPTIONS")

	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		out := map[string]any{
			"identityKey": a.Identity(),
			"audience":    a.Server().Audience(),
		}
		// A balance read failure must not make the endpoint useless: the identity is what a
		// client needs to proceed, so report the balance as unknown and carry on.
		if bal, err := w2.Wallet.Balance(r.Context()); err == nil {
			out["balanceSatoshis"] = bal
		} else {
			out["balanceError"] = err.Error()
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// faucetHandler claims teratestnet coins into this wallet.
//
// It exists so a player can fund without stopping the agent. The wallet database is held open by
// this process, so running cmd/fund alongside it would contend for the same SQLite file; doing the
// claim in-process avoids that entirely.
//
// POST-only, because it changes state and a GET would be triggerable by any page that can make the
// browser issue one.
func faucetHandler(w2 *brc100.Wallet, originator string, allowed []string, logger *slog.Logger) http.HandlerFunc {
	guard := guardOrigin(allowed, "POST, OPTIONS")

	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()

		w.Header().Set("content-type", "application/json")
		claim, err := w2.ClaimFromFaucet(ctx, originator, "", "")
		if err != nil {
			logger.Warn("faucet claim failed", "error", err)
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		bal, balErr := w2.Wallet.Balance(ctx)
		out := map[string]any{"satoshis": claim.Amount, "txid": claim.TxID}
		if balErr == nil {
			out["balanceSatoshis"] = bal
		}
		logger.Info("claimed from the faucet", "satoshis", claim.Amount, "txid", claim.TxID)
		_ = json.NewEncoder(w).Encode(out)
	}
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

// ownerHandler serves an owner-only method to the player's own page.
//
// The page is the player's client, not a third party, and it cannot produce a substrate signature
// as the owner. Origin is therefore the boundary, and the agent listens on loopback by default so
// nothing off-machine can reach it at all.
//
// Only recordStake is exposed this way, and deliberately: it arms the wallet with the player's own
// expectation of a settlement. It moves no money and grants nothing -- a wrong expectation makes
// the wallet refuse to sign, never sign something worse.
func ownerHandler(a *agent.Agent, allowed []string, logger *slog.Logger) http.HandlerFunc {
	permitted := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		permitted[strings.ToLower(o)] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := permitted[strings.ToLower(origin)]; !ok {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "could not read the body", http.StatusBadRequest)
			return
		}
		w.Header().Set("content-type", "application/json")
		res, err := a.RecordStakeJSON(body)
		if err != nil {
			logger.Warn("recordStake refused", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	}
}
