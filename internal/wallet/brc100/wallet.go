// Package brc100 wires a BRC-100 wallet against the configured network.
//
// The wiring here is deliberately opinionated. Several toolbox defaults are wrong for
// this application in ways that fail silently rather than loudly, so this package sets
// them explicitly and refuses to build a wallet that would run with an unsafe default.
// See design.md D5 for the full list and why each one matters.
package brc100

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/arcade"
	"github.com/galt-tr/go-arcade-toolbox/pkg/defs"
	"github.com/galt-tr/go-arcade-toolbox/pkg/headers"
	"github.com/galt-tr/go-arcade-toolbox/pkg/monitor"
	"github.com/galt-tr/go-arcade-toolbox/pkg/services"
	"github.com/galt-tr/go-arcade-toolbox/pkg/storage"
	"github.com/galt-tr/go-arcade-toolbox/pkg/storage/perfprovider"
	"github.com/galt-tr/go-arcade-toolbox/pkg/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/wdk"

	"github.com/cmurray/brc100-poker/internal/config"
)

// Backend selects the storage backend.
type Backend string

const (
	// BackendSQLite is for local development and tests only.
	BackendSQLite Backend = "sqlite"
	// BackendPostgres is required for any deployment holding real value.
	BackendPostgres Backend = "postgres"
)

// PotBasket holds outputs this application controls but the wallet cannot sign for.
//
// These must never share a basket with fee-paying coins. The funder has no exclusion
// list, so a pot coin sitting in the funding basket can be selected a second time to
// pay a fee, producing a duplicate input that the network rejects.
const PotBasket = "poker-pot"

// Options parameterises wallet construction.
type Options struct {
	Backend     Backend
	SQLitePath  string
	PostgresDSN string
	StorageName string

	// PrivateKeyHex is the wallet's key. It stays in this process and is never
	// written to storage, logged, or sent over any wire.
	PrivateKeyHex string

	// MaxDBConns must account for the monitor daemon as well as request workers.
	MaxDBConns int

	Logger *slog.Logger
}

func (o Options) validate() error {
	var errs []error
	switch o.Backend {
	case BackendSQLite:
		if o.SQLitePath == "" {
			errs = append(errs, errors.New("SQLitePath is required for the sqlite backend"))
		}
	case BackendPostgres:
		if o.PostgresDSN == "" {
			errs = append(errs, errors.New("PostgresDSN is required for the postgres backend"))
		}
	case "":
		errs = append(errs, errors.New("Backend is required and must be set explicitly"))
	default:
		errs = append(errs, fmt.Errorf("unknown backend %q", o.Backend))
	}
	if o.StorageName == "" {
		errs = append(errs, errors.New("StorageName is required"))
	}
	if o.PrivateKeyHex == "" {
		errs = append(errs, errors.New("PrivateKeyHex is required"))
	}
	return errors.Join(errs...)
}

// Wallet is a constructed BRC-100 wallet plus the subsystems it depends on.
//
// The monitor daemon is part of this struct rather than an optional extra because
// without it transactions never receive a status at all.
type Wallet struct {
	Wallet   *wallet.Wallet
	Provider *storage.Provider
	Oracle   arcade.TxOracle
	Headers  headers.Headers
	Monitor  *monitor.Daemon

	// IdentityKeyHex is the wallet's identity public key in DER hex.
	IdentityKeyHex string

	closeProvider perfprovider.CloseFunc
	logger        *slog.Logger
}

// StatusObserver receives applied transaction status batches.
//
// The contract is strict and violating it degrades the whole pipeline: it runs inline on
// the applier goroutine, so it must not block; there is no recover on that path, so it
// must not panic; delivery is at-least-once, so it must be idempotent; and the slice must
// not be retained or mutated.
type StatusObserver func(recs []arcade.TxRecord)

// New builds a wallet, its storage, and its monitor daemon.
//
// The caller must call Close. The monitor is created but not started; call Start so the
// caller controls the lifetime of the status stream.
func New(ctx context.Context, opts Options, observe StatusObserver) (*Wallet, error) {
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("brc100: invalid options: %w", err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// The identity key scopes the SSE stream. Derive it before anything else: the
	// callback token depends on it, and the library will not derive that for us.
	identityKeyHex, err := identityKeyFromPrivateHex(opts.PrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("brc100: deriving identity key: %w", err)
	}

	cfg := defs.DefaultServicesConfig(config.Network)

	// Without this, arcade drops our events and transactions strand with no status.
	cfg.Arcade.CallbackToken = wdk.DeriveArcadeCallbackToken(identityKeyHex)
	if cfg.Arcade.CallbackToken == "" {
		return nil, errors.New("brc100: derived an empty arcade callback token")
	}

	oracle := arcade.New(logger, nil, cfg.Arcade)

	// CacheDepth(0) lets the many proofs in one block share a single header fetch.
	hdrs, err := headers.New(logger, cfg.ChainTracks, headers.WithCacheDepth(0))
	if err != nil {
		return nil, fmt.Errorf("brc100: building headers service: %w", err)
	}

	provider, closeProvider, err := perfprovider.New(ctx, logger, perfprovider.Config{
		Backend:     backendFor(opts.Backend),
		SQLitePath:  opts.SQLitePath,
		PostgresDSN: opts.PostgresDSN,

		// Set explicitly: storage.Provider defaults to mainnet and perfprovider
		// defaults to testnet, so neither default is the network we want.
		Network:     config.Network,
		StorageName: opts.StorageName,
		MaxDBConns:  opts.MaxDBConns,
		FeeModel:    config.FeeModel(),

		ExtraOptions: []storage.Option{
			// Turn fee underpayment into a local error instead of an
			// unretryable remote rejection.
			storage.WithMinBroadcastFeeRate(config.MinBroadcastFeeRateSatPerKB),

			// Sub-dust change is otherwise silently dropped, which changes the
			// output count after signatures already committed to it.
			storage.WithRequiredChangeOutput(),

			// Keep ancestry O(inputs) rather than O(chain length).
			storage.WithDirectInputBEEF(),
		},
	}, oracle, hdrs)
	if err != nil {
		return nil, fmt.Errorf("brc100: building storage provider: %w", err)
	}

	if _, err := provider.Migrate(ctx, opts.StorageName, identityKeyHex); err != nil {
		closeQuietly(ctx, logger, closeProvider)
		return nil, fmt.Errorf("brc100: migrating storage: %w", err)
	}

	w, err := wallet.New(config.Network, opts.PrivateKeyHex, provider,
		wallet.WithServices(services.New(logger, oracle, hdrs, cfg)),
		wallet.WithLogger(logger),
	)
	if err != nil {
		closeQuietly(ctx, logger, closeProvider)
		return nil, fmt.Errorf("brc100: building wallet: %w", err)
	}

	monCfg := defs.DefaultMonitorConfig()
	monOpts := []monitor.Option{
		// The default of 8 is not enough past a few hundred TPS.
		monitor.WithApplyConcurrency(32),
	}
	if observe != nil {
		monOpts = append(monOpts, monitor.WithStatusObserver(safeObserver(logger, observe)))
	}
	mon, err := monitor.NewDaemon(logger, provider, hdrs, oracle, monCfg, monOpts...)
	if err != nil {
		closeQuietly(ctx, logger, closeProvider)
		return nil, fmt.Errorf("brc100: building monitor daemon: %w", err)
	}

	return &Wallet{
		Wallet:         w,
		Provider:       provider,
		Oracle:         oracle,
		Headers:        hdrs,
		Monitor:        mon,
		IdentityKeyHex: identityKeyHex,
		closeProvider:  closeProvider,
		logger:         logger,
	}, nil
}

// safeObserver enforces the parts of the observer contract we can enforce for the caller.
//
// A panic in an observer would otherwise take the process down: there is no recover on
// the applier path. Recovering here keeps a buggy observer from killing the pipeline,
// and the log line makes the bug visible rather than silent.
func safeObserver(logger *slog.Logger, observe StatusObserver) func([]arcade.TxRecord) {
	return func(recs []arcade.TxRecord) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("status observer panicked", "panic", r, "records", len(recs))
			}
		}()
		observe(recs)
	}
}

// Start begins status tracking. Real-value play must not proceed without it.
func (w *Wallet) Start(ctx context.Context) error {
	cfg := defs.DefaultMonitorConfig()
	if err := w.Monitor.Start(ctx, cfg.Tasks.EnabledTasks()); err != nil {
		return fmt.Errorf("brc100: starting monitor daemon: %w", err)
	}
	return nil
}

// Close stops the monitor and releases storage, reporting every failure.
//
// Both steps always run: a monitor that will not stop must not leave the database
// connection pool open behind it.
func (w *Wallet) Close(ctx context.Context) error {
	var errs []error
	if w.Monitor != nil {
		if err := w.Monitor.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stopping monitor daemon: %w", err))
		}
	}
	if w.closeProvider != nil {
		if err := w.closeProvider(ctx); err != nil {
			errs = append(errs, fmt.Errorf("closing storage provider: %w", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("brc100: closing wallet: %w", errors.Join(errs...))
	}
	return nil
}

// closeQuietly releases storage on a construction failure.
//
// The construction error is the one the caller needs, so a cleanup failure is logged
// rather than returned — but it is never silently discarded.
func closeQuietly(ctx context.Context, logger *slog.Logger, close perfprovider.CloseFunc) {
	if err := close(ctx); err != nil {
		logger.Error("closing storage provider after a failed wallet construction", "error", err)
	}
}

// IdentityKey returns the wallet's identity public key, binding storage on first call.
func (w *Wallet) IdentityKey(ctx context.Context, originator string) (string, error) {
	pub, err := w.Wallet.GetPublicKey(ctx, sdk.GetPublicKeyArgs{IdentityKey: true}, originator)
	if err != nil {
		return "", fmt.Errorf("brc100: getting identity key: %w", err)
	}
	return pub.PublicKey.ToDERHex(), nil
}

func backendFor(b Backend) perfprovider.Backend {
	if b == BackendPostgres {
		return perfprovider.BackendPostgres
	}
	return perfprovider.BackendSQLite
}
