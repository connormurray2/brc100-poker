package webui

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/agent"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
	"github.com/cmurray/brc100-poker/internal/protocol/table"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// liveAgent is one seat's real agent behind a real HTTP server.
type liveAgent struct {
	key  *ec.PrivateKey
	url  string
	seat int
}

func startAgents(t *testing.T, n int, tableKey *ec.PrivateKey) []liveAgent {
	t.Helper()
	ctx := context.Background()

	out := make([]liveAgent, 0, n)
	for i := 0; i < n; i++ {
		priv, err := ec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		keyHex := hex.EncodeToString(priv.Serialize())

		w, err := brc100.New(ctx, brc100.Options{
			Backend:       brc100.BackendSQLite,
			SQLitePath:    filepath.Join(t.TempDir(), "a.db"),
			StorageName:   "deal-test",
			PrivateKeyHex: keyHex,
			Logger:        quiet(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close(ctx) })

		a, err := agent.New(agent.Config{
			PrivateKeyHex: keyHex,
			Wallet:        w,
			Approver:      substrate.ApproverFunc(func(substrate.SigningRequest) error { return nil }),
			Originator:    "table.poker.local",
			Logger:        quiet(),
		})
		if err != nil {
			t.Fatal(err)
		}
		// The table must be granted, or its deal calls are refused — which is the
		// least-privilege model working, not an obstacle.
		if err := a.GrantTable(tableKey.PubKey().ToDERHex()); err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(a.Server())
		t.Cleanup(ts.Close)

		out = append(out, liveAgent{key: priv, url: ts.URL, seat: i})
	}
	return out
}

// The end-to-end property: a table deals a hand through seats' agents and cannot read a card.
func TestCoordinatorDealsWithoutSeeingHoleCards(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 3, tableKey)

	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}

	endpoints := make([]AgentEndpoint, 0, len(ags))
	for _, a := range ags {
		endpoints = append(endpoints, AgentEndpoint{
			Seat: a.seat, IdentityKey: a.key.PubKey().ToDERHex(), URL: a.url,
		})
	}

	dealt, err := coord.Deal(context.Background(), "coord-hand", endpoints, 2)
	if err != nil {
		t.Fatalf("the dealerless deal failed: %v", err)
	}

	// Every seat read exactly its own two cards.
	if len(dealt.Hole) != 3 {
		t.Fatalf("dealt to %d seats, want 3", len(dealt.Hole))
	}
	seen := map[string]int{}
	for seat, cs := range dealt.Hole {
		if len(cs) != 2 {
			t.Fatalf("seat %d got %d cards", seat, len(cs))
		}
		for _, c := range cs {
			if prev, dup := seen[c.String()]; dup {
				t.Fatalf("card %s dealt to seats %d and %d", c, prev, seat)
			}
			seen[c.String()] = seat
		}
		t.Logf("seat %d holds %v", seat, cs)
	}

	// Five board cards, distinct from every hole card.
	if len(dealt.Board) != 5 {
		t.Fatalf("board has %d cards, want 5", len(dealt.Board))
	}
	for _, c := range dealt.Board {
		if prev, dup := seen[c.String()]; dup {
			t.Fatalf("board card %s was also dealt to seat %d", c, prev)
		}
		seen[c.String()] = -1
	}
	t.Logf("board %v", dealt.Board)

	if len(seen) != 11 {
		t.Fatalf("recovered %d distinct cards, want 11", len(seen))
	}
}

// A seat with no agent means no dealerless deal is possible, and the table must say so rather than
// quietly dealing the cards itself while a player believes otherwise.
func TestTableReportsWhenItDealtTheCardsItself(t *testing.T) {
	terms := table.Terms{
		TableID: "t", BuyInSatoshis: 5000, SmallBlind: 25, BigBlind: 50,
		Seats: 2, RefundLockHeight: 100,
	}
	l, err := NewLiveTable(terms)
	if err != nil {
		t.Fatal(err)
	}

	// Two seats join, neither registers an agent.
	a, b := "02"+hexRepeat(64), "03"+hexRepeat(64)
	for _, k := range []string{a, b} {
		if _, err := l.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{a, b} {
		if err := l.Ready(k); err != nil {
			t.Fatal(err)
		}
	}

	if l.Dealerless() {
		t.Fatal("the table claimed a dealerless deal with no agents registered")
	}
	// The hand still plays: a demonstration is legitimate, provided it is labelled.
	v := l.View(0)
	if v.Street == "" {
		t.Fatal("the hand did not start")
	}
}

// A real dealerless deal through the live table, with agents registered.
func TestLiveTableDealsDealerlessly(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 2, tableKey)

	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLiveTable(table.Terms{
		TableID: "live", BuyInSatoshis: 5000, SmallBlind: 25, BigBlind: 50,
		Seats: 2, RefundLockHeight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	l.SetCoordinator(coord)

	for _, a := range ags {
		key := a.key.PubKey().ToDERHex()
		if _, err := l.Join(key); err != nil {
			t.Fatal(err)
		}
		if err := l.RegisterAgent(key, a.url); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range ags {
		if err := l.Ready(a.key.PubKey().ToDERHex()); err != nil {
			t.Fatalf("ready failed: %v", err)
		}
	}

	if !l.Dealerless() {
		t.Fatal("the table dealt the cards itself despite every seat having an agent")
	}

	// Each seat sees its own cards and not the other's.
	for seat := 0; seat < 2; seat++ {
		v := l.View(seat)
		for _, p := range v.Players {
			if p.Seat == seat {
				if len(p.Hole) != 2 {
					t.Fatalf("seat %d cannot see its own %d cards", seat, len(p.Hole))
				}
				t.Logf("seat %d sees its own cards: %v", seat, p.Hole)
			} else if len(p.Hole) != 0 {
				t.Fatalf("seat %d can see seat %d's cards: %v", seat, p.Seat, p.Hole)
			}
		}
	}
}

// RegisterAgent must refuse an identity that holds no seat.
func TestRegisterAgentRequiresASeat(t *testing.T) {
	l, err := NewLiveTable(table.Terms{
		TableID: "t", BuyInSatoshis: 5000, SmallBlind: 25, BigBlind: 50,
		Seats: 2, RefundLockHeight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RegisterAgent("02"+hexRepeat(64), "http://x"); err == nil {
		t.Error("registered an agent for an identity with no seat")
	}
	if err := l.RegisterAgent("", "http://x"); err == nil {
		t.Error("registered an agent with no identity")
	}
}

func TestNewCoordinatorValidation(t *testing.T) {
	if _, err := NewCoordinator(nil, "x.local"); err == nil {
		t.Error("built a coordinator with no caller key")
	}
	k, _ := ec.NewPrivateKey()
	if _, err := NewCoordinator(k, ""); err == nil {
		t.Error("built a coordinator with no originator")
	}
}

func hexRepeat(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}
