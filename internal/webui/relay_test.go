package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// A player's wallet listens on their own loopback, so the table cannot dial it. This proves a
// complete dealerless deal still runs when every seat is reachable only through its browser --
// which is the deployed case, and the one that failed with "connection refused".
func TestDealRunsEntirelyThroughBrowserRelays(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 3, tableKey)

	relay := NewRelay()
	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	coord.UseRelay(relay)

	// Every seat registers the sentinel, so the coordinator must relay rather than dial. If it
	// tried to dial this value the deal would fail, which is what makes the test meaningful.
	endpoints := make([]AgentEndpoint, 0, len(ags))
	for _, a := range ags {
		endpoints = append(endpoints, AgentEndpoint{
			Seat: a.seat, IdentityKey: a.key.PubKey().ToDERHex(), URL: RelayURL,
		})
	}

	// One goroutine per seat, standing in for that player's browser: poll, forward to the
	// wallet, post the reply back. This is exactly what app.js does.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, a := range ags {
		wg.Add(1)
		go func(seatKey string, walletURL string) {
			defer wg.Done()
			for ctx.Err() == nil {
				for _, item := range relay.Collect(seatKey) {
					resp, err := http.Post(walletURL, "application/json", bytes.NewReader(item.Body))
					if err != nil {
						_ = relay.Deliver(seatKey, item.Nonce, nil, err.Error())
						continue
					}
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if readErr != nil {
						_ = relay.Deliver(seatKey, item.Nonce, nil, readErr.Error())
						continue
					}
					_ = relay.Deliver(seatKey, item.Nonce, body, "")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(a.key.PubKey().ToDERHex(), a.url)
	}

	dealt, err := coord.Deal(ctx, "relay-hand", endpoints, 2)
	cancel()
	wg.Wait()
	if err != nil {
		t.Fatalf("the relayed deal failed: %v", err)
	}

	// The same guarantees as a directly-dialled deal: every seat gets two cards, five on the
	// board, all eleven distinct. Relaying must not weaken the deal, only move the bytes.
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
	}
	if len(dealt.Board) != 5 {
		t.Fatalf("board has %d cards, want 5", len(dealt.Board))
	}
	for _, c := range dealt.Board {
		if _, dup := seen[c.String()]; dup {
			t.Fatalf("board card %s was also dealt to a seat", c)
		}
		seen[c.String()] = -1
	}
	if len(seen) != 11 {
		t.Fatalf("recovered %d distinct cards, want 11", len(seen))
	}
	if p := relay.Pending(); p != 0 {
		t.Fatalf("%d requests left in flight after the deal", p)
	}
}

// A seat whose browser never answers must fail the hand with something an operator can act on,
// not hang and not a bare context error.
func TestRelayTimesOutWhenNoBrowserAnswers(t *testing.T) {
	relay := NewRelay()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := relay.Do(ctx, "02aa", "nonce-1", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("want an error when no browser relays the request")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("did not relay")) {
		t.Fatalf("error does not explain the stall: %v", err)
	}
	if p := relay.Pending(); p != 0 {
		t.Fatalf("a timed-out request stayed in flight: %d", p)
	}
}

// One player must not be able to answer for another: a response carries the wallet's signature,
// and accepting it under the wrong seat would let a player substitute their own wallet's reply.
func TestRelayRefusesAReplyFromTheWrongSeat(t *testing.T) {
	relay := NewRelay()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = relay.Do(ctx, "02seatA", "nonce-2", json.RawMessage(`{}`))
	}()

	// Wait for it to be queued.
	for i := 0; i < 100 && relay.Pending() == 0; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	if err := relay.Deliver("02seatB", "nonce-2", json.RawMessage(`{}`), ""); err == nil {
		t.Fatal("seat B was allowed to answer a request addressed to seat A")
	}
	cancel()
	<-done
}

// A request is handed out once. Re-delivering it would present the same nonce to the wallet
// twice, which its replay cache rejects anyway.
func TestRelayCollectsEachRequestOnce(t *testing.T) {
	relay := NewRelay()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _, _ = relay.Do(ctx, "02seat", "nonce-3", json.RawMessage(`{"a":1}`)) }()
	for i := 0; i < 100 && relay.Pending() == 0; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	first := relay.Collect("02seat")
	if len(first) != 1 {
		t.Fatalf("collected %d requests, want 1", len(first))
	}
	if second := relay.Collect("02seat"); len(second) != 0 {
		t.Fatalf("the same request was handed out twice")
	}
}

// Two hands in a row must not deal the same cards.
//
// This is the bug a player found: the coordinator was passed the table ID instead of a per-hand
// ID, and a wallet deliberately returns its existing secrets when asked to commit twice for the
// same hand -- which is what stops a seat re-rolling after seeing another seat's contribution. A
// constant ID therefore dealt the identical hand every time.
func TestConsecutiveHandsDealDifferentCards(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 2, tableKey)
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

	fingerprint := func(handID string) string {
		dealt, err := coord.Deal(context.Background(), handID, endpoints, 2)
		if err != nil {
			t.Fatalf("deal %s: %v", handID, err)
		}
		out := ""
		for seat := 0; seat < len(dealt.Hole); seat++ {
			for _, c := range dealt.Hole[seat] {
				out += c.String() + ","
			}
		}
		for _, c := range dealt.Board {
			out += c.String() + ","
		}
		return out
	}

	first := fingerprint("table-h0")
	second := fingerprint("table-h1")
	if first == second {
		t.Fatalf("two hands dealt identical cards: %s", first)
	}

	// And the guarantee that makes a distinct ID necessary: asking twice for the SAME hand must
	// reproduce it, because a seat must not be able to re-roll its secrets mid-hand.
	if again := fingerprint("table-h0"); again != first {
		t.Fatalf("re-running one hand changed the cards:\n  %s\n  %s", first, again)
	}
}
