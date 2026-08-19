package config

import (
	"strings"
	"testing"
)

func devService() Service {
	return Service{
		Environment:      EnvDevelopment,
		ListenAddress:    ":8080",
		SQLitePath:       "wallet.db",
		WalletKeyHex:     strings.Repeat("11", 32),
		Originator:       "table.poker.local",
		BuyInSatoshis:    5000,
		SmallBlind:       25,
		BigBlind:         50,
		Seats:            2,
		RefundLockBlocks: 144,
		LogLevel:         "info",
	}
}

func prodService() Service {
	s := devService()
	s.Environment = EnvProduction
	s.SQLitePath = ""
	s.PostgresDSN = "postgres://user:pass@host/db"
	s.RequireTLS = true
	return s
}

func TestValidDevelopmentAndProductionConfigs(t *testing.T) {
	if err := devService().Validate(); err != nil {
		t.Fatalf("a valid development config was refused: %v", err)
	}
	if err := prodService().Validate(); err != nil {
		t.Fatalf("a valid production config was refused: %v", err)
	}
}

// Production must not run on SQLite: the database is the wallet, and SQLite has no managed
// backup story.
func TestProductionRequiresPostgres(t *testing.T) {
	s := prodService()
	s.PostgresDSN = ""
	err := s.Validate()
	if err == nil {
		t.Fatal("production started with no Postgres DSN")
	}
	if !strings.Contains(err.Error(), "the database is the wallet") {
		t.Errorf("the error does not explain why: %v", err)
	}

	withSQLite := prodService()
	withSQLite.SQLitePath = "wallet.db"
	if err := withSQLite.Validate(); err == nil {
		t.Fatal("production accepted a SQLite path")
	}
}

// A substrate call carries signing authority, so plaintext in a deployment turns a network
// boundary into a custody boundary.
func TestProductionRequiresTLS(t *testing.T) {
	s := prodService()
	s.RequireTLS = false
	err := s.Validate()
	if err == nil {
		t.Fatal("production started without TLS required")
	}
	if !strings.Contains(err.Error(), "signing authority") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

// Real value must not be at risk before a restore has been proved.
func TestRealValuePlayRequiresAProvedRestore(t *testing.T) {
	s := prodService()
	s.RealValuePlay = true
	err := s.Validate()
	if err == nil {
		t.Fatal("real-value play was enabled with no proved restore")
	}
	if !strings.Contains(err.Error(), "restore must be proved") {
		t.Errorf("the error does not explain why: %v", err)
	}

	s.BackupVerifiedAt = "2026-08-19"
	if err := s.Validate(); err != nil {
		t.Fatalf("real-value play was refused after a restore was attested: %v", err)
	}
}

func TestKeyValidation(t *testing.T) {
	for name, key := range map[string]string{
		"empty":     "",
		"short":     "aabb",
		"not hex":   strings.Repeat("zz", 32),
		"all zeros": strings.Repeat("00", 32),
	} {
		t.Run(name, func(t *testing.T) {
			s := devService()
			s.WalletKeyHex = key
			if err := s.Validate(); err == nil {
				t.Fatal("an invalid wallet key was accepted")
			}
		})
	}
}

func TestOriginatorValidation(t *testing.T) {
	for name, o := range map[string]string{
		"empty":     "",
		"no domain": "localhost",
		"has space": "table poker.local",
		"has slash": "table/poker.local",
	} {
		t.Run(name, func(t *testing.T) {
			s := devService()
			s.Originator = o
			if err := s.Validate(); err == nil {
				t.Fatal("an invalid originator was accepted")
			}
		})
	}
}

func TestStakesValidation(t *testing.T) {
	tests := map[string]func(*Service){
		"one seat":       func(s *Service) { s.Seats = 1 },
		"seven seats":    func(s *Service) { s.Seats = 7 },
		"no buy-in":      func(s *Service) { s.BuyInSatoshis = 0 },
		"no blinds":      func(s *Service) { s.BigBlind = 0 },
		"sb over bb":     func(s *Service) { s.SmallBlind = 100; s.BigBlind = 50 },
		"bb over buy-in": func(s *Service) { s.BigBlind = 9000 },
		"no refund lock": func(s *Service) { s.RefundLockBlocks = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := devService()
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// Every problem is reported at once, so an operator fixes one round rather than discovering
// them one restart at a time.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	s := Service{Environment: EnvProduction, LogLevel: "nonsense"}
	err := s.Validate()
	if err == nil {
		t.Fatal("an empty production config was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"POKER_LISTEN", "POKER_WALLET_KEY", "POKER_POSTGRES_DSN", "POKER_REQUIRE_TLS", "POKER_LOG_LEVEL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %s: %v", want, msg)
		}
	}
}

// Secrets must never reach a log line.
func TestRedactedOmitsSecrets(t *testing.T) {
	s := prodService()
	s.WalletKeyHex = strings.Repeat("ab", 32)
	s.PostgresDSN = "postgres://user:supersecret@host/db"

	red := s.Redacted()
	flat := ""
	for k, v := range red {
		flat += k + "=" + toString(v) + ";"
	}
	if strings.Contains(flat, s.WalletKeyHex) {
		t.Error("the redacted config contains the wallet key")
	}
	if strings.Contains(flat, "supersecret") {
		t.Error("the redacted config contains the database password")
	}
	// It must still be useful: the effective configuration is worth logging.
	if red["storage"] != "postgres" {
		t.Errorf("storage = %v, want the shape without the DSN", red["storage"])
	}
	if red["network"] != string(Network) {
		t.Errorf("network = %v, want %v", red["network"], Network)
	}
	if red["requireTLS"] != true {
		t.Error("the redacted config lost requireTLS")
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, s := range []string{"debug", "info", "warn", "warning", "error", "", "INFO"} {
		if _, err := ParseLogLevel(s); err != nil {
			t.Errorf("ParseLogLevel(%q) = %v", s, err)
		}
	}
	if _, err := ParseLogLevel("chatty"); err == nil {
		t.Error("an invalid log level was accepted")
	}
}

func TestEnvironmentValidation(t *testing.T) {
	s := devService()
	s.Environment = "staging"
	if err := s.Validate(); err == nil {
		t.Fatal("an unknown environment was accepted")
	}
}
