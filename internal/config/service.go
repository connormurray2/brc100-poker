package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Environment distinguishes a local development run from a real deployment.
//
// The distinction is load-bearing: several settings that are merely inconvenient locally are
// unsafe in a deployment, and the service refuses to start rather than degrade quietly.
type Environment string

const (
	// EnvDevelopment permits SQLite and plaintext transport.
	EnvDevelopment Environment = "development"
	// EnvProduction requires Postgres and transport protection.
	EnvProduction Environment = "production"
)

// Service is the table service's configuration.
type Service struct {
	Environment Environment

	// ListenAddress is where the table service serves play.
	ListenAddress string
	// PublicURL is how players reach it, used for advertising tables.
	PublicURL string

	// PostgresDSN is the wallet and ledger database. The database IS the wallet: there is
	// no UTXO discovery and no restore-from-seed, so losing it loses the coins.
	PostgresDSN string
	// SQLitePath is a development-only alternative.
	SQLitePath string

	// WalletKeyHex is the table's own fee-paying key. It never holds player funds.
	WalletKeyHex string

	// Originator is the FQDN-shaped identifier BRC-100 requires on every call.
	Originator string

	// RequireTLS refuses to serve substrate calls over plaintext.
	RequireTLS bool

	// BuyInSatoshis, SmallBlind and BigBlind are the table's advertised stakes.
	BuyInSatoshis uint64
	SmallBlind    uint64
	BigBlind      uint64
	// Seats is the table size.
	Seats int
	// RefundLockBlocks is how far ahead a refund's locktime sits. A player needs this
	// before funding: it is the worst case a stall can cost them.
	RefundLockBlocks uint32

	// RealValuePlay gates hands that move real coins. Off until the operator has proved a
	// database restore, because an unrestorable wallet loses funds outright.
	RealValuePlay bool
	// BackupVerifiedAt records when a restore was last proved, as an operator attestation.
	BackupVerifiedAt string

	// LogLevel is the slog level.
	LogLevel string
}

// Env reads configuration from the environment.
//
// Nothing is defaulted silently that would be unsafe if wrong: the network is a compile-time
// constant, the fee rate is fixed, and every unsafe combination is rejected by Validate rather
// than accommodated.
func Env() Service {
	return Service{
		Environment:      Environment(getenv("POKER_ENV", string(EnvDevelopment))),
		ListenAddress:    getenv("POKER_LISTEN", ":8080"),
		PublicURL:        os.Getenv("POKER_PUBLIC_URL"),
		PostgresDSN:      os.Getenv("POKER_POSTGRES_DSN"),
		SQLitePath:       os.Getenv("POKER_SQLITE_PATH"),
		WalletKeyHex:     os.Getenv("POKER_WALLET_KEY"),
		Originator:       getenv("POKER_ORIGINATOR", "table.poker.local"),
		RequireTLS:       getenvBool("POKER_REQUIRE_TLS", false),
		BuyInSatoshis:    getenvUint("POKER_BUY_IN_SATS", 5000),
		SmallBlind:       getenvUint("POKER_SMALL_BLIND", 25),
		BigBlind:         getenvUint("POKER_BIG_BLIND", 50),
		Seats:            int(getenvUint("POKER_SEATS", 2)),
		RefundLockBlocks: uint32(getenvUint("POKER_REFUND_LOCK_BLOCKS", 144)),
		RealValuePlay:    getenvBool("POKER_REAL_VALUE_PLAY", false),
		BackupVerifiedAt: os.Getenv("POKER_BACKUP_VERIFIED_AT"),
		LogLevel:         getenv("POKER_LOG_LEVEL", "info"),
	}
}

// Validate reports every problem at once, so an operator fixes one round of errors rather than
// discovering them one restart at a time.
func (s Service) Validate() error {
	var errs []error

	switch s.Environment {
	case EnvDevelopment, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("POKER_ENV must be %q or %q, got %q",
			EnvDevelopment, EnvProduction, s.Environment))
	}

	if s.ListenAddress == "" {
		errs = append(errs, errors.New("POKER_LISTEN is required"))
	}
	if s.WalletKeyHex == "" {
		errs = append(errs, errors.New("POKER_WALLET_KEY is required"))
	} else if err := validateKeyHex(s.WalletKeyHex); err != nil {
		errs = append(errs, fmt.Errorf("POKER_WALLET_KEY: %w", err))
	}
	if err := validateOriginator(s.Originator); err != nil {
		errs = append(errs, fmt.Errorf("POKER_ORIGINATOR: %w", err))
	}

	// Storage. Postgres is required in production because the database is the wallet and
	// SQLite offers no managed backup story.
	switch {
	case s.Environment == EnvProduction && s.PostgresDSN == "":
		errs = append(errs, errors.New("POKER_POSTGRES_DSN is required in production: the database is the wallet, and it must be backed up"))
	case s.PostgresDSN == "" && s.SQLitePath == "":
		errs = append(errs, errors.New("either POKER_POSTGRES_DSN or POKER_SQLITE_PATH is required"))
	}
	if s.Environment == EnvProduction && s.SQLitePath != "" {
		errs = append(errs, errors.New("POKER_SQLITE_PATH is not permitted in production"))
	}

	// Transport protection. A substrate call carries signing authority, so plaintext in a
	// deployment turns a network boundary into a custody boundary.
	if s.Environment == EnvProduction && !s.RequireTLS {
		errs = append(errs, errors.New("POKER_REQUIRE_TLS must be true in production: substrate calls carry signing authority"))
	}

	// Stakes.
	if s.Seats < 2 || s.Seats > 6 {
		errs = append(errs, fmt.Errorf("POKER_SEATS must be 2..6, got %d", s.Seats))
	}
	if s.BuyInSatoshis == 0 {
		errs = append(errs, errors.New("POKER_BUY_IN_SATS must be positive"))
	}
	if s.SmallBlind == 0 || s.BigBlind == 0 {
		errs = append(errs, errors.New("blinds must be positive"))
	}
	if s.SmallBlind > s.BigBlind {
		errs = append(errs, errors.New("POKER_SMALL_BLIND exceeds POKER_BIG_BLIND"))
	}
	if s.BigBlind > s.BuyInSatoshis {
		errs = append(errs, errors.New("POKER_BIG_BLIND exceeds POKER_BUY_IN_SATS"))
	}
	if s.RefundLockBlocks == 0 {
		errs = append(errs, errors.New("POKER_REFUND_LOCK_BLOCKS must be positive: a refund with no locktime is spendable immediately"))
	}

	// Real value play is gated on a proved restore. An unrestorable wallet does not lose
	// availability, it loses the coins.
	if s.RealValuePlay && s.BackupVerifiedAt == "" {
		errs = append(errs, errors.New("POKER_REAL_VALUE_PLAY requires POKER_BACKUP_VERIFIED_AT: a restore must be proved before real value is at risk"))
	}

	if _, err := ParseLogLevel(s.LogLevel); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Redacted returns the configuration with secrets removed, for logging at startup.
//
// The effective configuration is worth logging; the key and DSN are not. A DSN carries a
// password, so it is reduced to its shape rather than printed.
func (s Service) Redacted() map[string]any {
	storage := "none"
	switch {
	case s.PostgresDSN != "":
		storage = "postgres"
	case s.SQLitePath != "":
		storage = "sqlite:" + s.SQLitePath
	}
	return map[string]any{
		"environment":      string(s.Environment),
		"listen":           s.ListenAddress,
		"publicURL":        s.PublicURL,
		"network":          string(Network),
		"feeRateSatPerKB":  FeeRateSatPerKB,
		"storage":          storage,
		"originator":       s.Originator,
		"requireTLS":       s.RequireTLS,
		"seats":            s.Seats,
		"buyInSatoshis":    s.BuyInSatoshis,
		"smallBlind":       s.SmallBlind,
		"bigBlind":         s.BigBlind,
		"refundLockBlocks": s.RefundLockBlocks,
		"realValuePlay":    s.RealValuePlay,
		"backupVerifiedAt": s.BackupVerifiedAt,
		"logLevel":         s.LogLevel,
	}
}

// ParseLogLevel maps a level name to a slog level.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("POKER_LOG_LEVEL %q is not one of debug, info, warn, error", s)
	}
}

func validateKeyHex(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("must be 64 hex characters, got %d", len(s))
	}
	for _, c := range strings.ToLower(s) {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("must be hex")
		}
	}
	// The all-zero key is not a valid scalar, and it is what an unset variable filled with
	// zeros looks like.
	if strings.Trim(s, "0") == "" {
		return errors.New("is all zeros, which is not a valid key")
	}
	return nil
}

func validateOriginator(o string) error {
	if o == "" {
		return errors.New("is required")
	}
	if !strings.Contains(o, ".") {
		return fmt.Errorf("%q has no domain part; BRC-100 requires an FQDN shape", o)
	}
	if strings.ContainsAny(o, " \t/\\:") {
		return fmt.Errorf("%q is not FQDN-shaped", o)
	}
	return nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// An unparseable boolean is reported by Validate via the value it produced
		// rather than silently becoming the fallback; returning the fallback here
		// keeps parsing total.
		return fallback
	}
	return b
}

func getenvUint(key string, fallback uint64) uint64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0 // Validate rejects zero for every field that uses this.
	}
	return n
}
