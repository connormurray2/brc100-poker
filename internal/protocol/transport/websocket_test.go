package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startHub runs a real HTTP server so the test exercises the actual socket path.
func startHub(t *testing.T) (*Hub, string) {
	t.Helper()
	hub := NewHub(quietLogger())
	srv := httptest.NewServer(hub)
	t.Cleanup(func() {
		srv.Close()
		_ = hub.Close()
	})
	return hub, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url, table string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, quietLogger(), url, table)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The property the seating handshake depends on: a seat's own publish comes back to it.
func TestWebSocketPublisherReceivesItsOwnMessage(t *testing.T) {
	_, url := startHub(t)
	c := dial(t, url, "t1")

	got := make(chan Message, 4)
	unsub, err := c.Subscribe("t1", func(m Message) { got <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if _, err := c.Publish(context.Background(), "t1", []byte("hello"), "msg-1"); err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-got:
		if string(m.Payload) != "hello" {
			t.Errorf("payload = %q, want %q", m.Payload, "hello")
		}
		if m.ID != "msg-1" {
			t.Errorf("id = %q, want msg-1", m.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the publisher never received its own message")
	}
}

// Six seats on one table all receive every message: the multiplayer case.
func TestWebSocketFansOutToAllSeats(t *testing.T) {
	_, url := startHub(t)

	const seats = 6
	var wg sync.WaitGroup
	wg.Add(seats)
	for i := 0; i < seats; i++ {
		c := dial(t, url, "t1")
		var once sync.Once
		unsub, err := c.Subscribe("t1", func(Message) { once.Do(wg.Done) })
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()
	}

	// Give the hub a moment to register every connection.
	time.Sleep(300 * time.Millisecond)

	sender := dial(t, url, "t1")
	if _, err := sender.Publish(context.Background(), "t1", []byte("deal"), "deal-1"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("not every seat received the message")
	}
}

// Different tables must not see each other's traffic.
func TestWebSocketTablesAreIsolated(t *testing.T) {
	_, url := startHub(t)

	a := dial(t, url, "table-a")
	b := dial(t, url, "table-b")

	aGot := make(chan Message, 2)
	bGot := make(chan Message, 2)
	ua, err := a.Subscribe("table-a", func(m Message) { aGot <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer ua()
	ub, err := b.Subscribe("table-b", func(m Message) { bGot <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer ub()

	time.Sleep(200 * time.Millisecond)
	if _, err := a.Publish(context.Background(), "table-a", []byte("x"), "x1"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-aGot:
	case <-time.After(5 * time.Second):
		t.Fatal("table-a did not receive its own message")
	}
	select {
	case m := <-bGot:
		t.Fatalf("table-b received table-a's message: %q", m.Payload)
	case <-time.After(500 * time.Millisecond):
	}
}

// The hub's own in-process handler must see traffic, so the table service can observe the
// tables it hosts without dialling itself.
func TestHubLocalSubscriberReceivesRemotePublish(t *testing.T) {
	hub, url := startHub(t)

	got := make(chan Message, 2)
	unsub, err := hub.Subscribe("t1", func(m Message) { got <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	c := dial(t, url, "t1")
	if _, err := c.Publish(context.Background(), "t1", []byte("from a seat"), "s1"); err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-got:
		if string(m.Payload) != "from a seat" {
			t.Errorf("payload = %q", m.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hub's local subscriber did not receive a remote publish")
	}
}

// A hub publish must reach connected seats.
func TestHubPublishReachesSeats(t *testing.T) {
	hub, url := startHub(t)
	c := dial(t, url, "t1")

	got := make(chan Message, 2)
	unsub, err := c.Subscribe("t1", func(m Message) { got <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	time.Sleep(200 * time.Millisecond)
	if _, err := hub.Publish(context.Background(), "t1", []byte("from the table"), "tbl-1"); err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-got:
		if string(m.Payload) != "from the table" {
			t.Errorf("payload = %q", m.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the seat did not receive the hub's publish")
	}
}

// A seat must not be able to inject messages into a table it did not join.
func TestSeatCannotPublishToAnotherTable(t *testing.T) {
	_, url := startHub(t)
	c := dial(t, url, "table-a")

	if _, err := c.Publish(context.Background(), "table-b", []byte("intrude"), "i1"); err == nil {
		t.Fatal("a client published to a table it had not joined")
	}
	if _, err := c.Subscribe("table-b", func(Message) {}); err == nil {
		t.Fatal("a client subscribed to a table it had not joined")
	}
}

func TestWebSocketRequiresTableParameter(t *testing.T) {
	hub := NewHub(quietLogger())
	t.Cleanup(func() { _ = hub.Close() })
	srv := httptest.NewServer(hub)
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // test request
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without a table parameter", resp.StatusCode)
	}
}

// Oversized payloads must be refused rather than buffered.
func TestWebSocketRejectsOversizedPayload(t *testing.T) {
	_, url := startHub(t)
	c := dial(t, url, "t1")

	big := make([]byte, maxFrameBytes+1)
	if _, err := c.Publish(context.Background(), "t1", big, ""); err == nil {
		t.Fatal("an oversized payload was accepted")
	}
	hub := NewHub(quietLogger())
	t.Cleanup(func() { _ = hub.Close() })
	if _, err := hub.Publish(context.Background(), "t1", big, ""); err == nil {
		t.Fatal("the hub accepted an oversized payload")
	}
}

func TestClosedClientAndHubRejectOperations(t *testing.T) {
	hub, url := startHub(t)
	c := dial(t, url, "t1")
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Publish(context.Background(), "t1", []byte("x"), ""); err == nil {
		t.Error("a closed client published")
	}
	if _, err := c.Subscribe("t1", func(Message) {}); err == nil {
		t.Error("a closed client subscribed")
	}
	// Closing twice is harmless.
	if err := c.Close(); err != nil {
		t.Errorf("second Close returned %v", err)
	}

	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Subscribe("t1", func(Message) {}); err == nil {
		t.Error("a closed hub accepted a subscriber")
	}
	if _, err := hub.Publish(context.Background(), "t1", []byte("x"), ""); err == nil {
		t.Error("a closed hub published")
	}
}

// Both implementations must satisfy the same interface, so the game can run over either.
func TestBothImplementationsSatisfyTransport(t *testing.T) {
	var _ Transport = NewMemory()
	var _ Transport = NewHub(quietLogger())
	var _ Transport = (*Client)(nil)
}

// A seat that reconnects re-subscribes and receives subsequent traffic.
func TestReconnectingSeatResumesReceiving(t *testing.T) {
	_, url := startHub(t)

	first := dial(t, url, "t1")
	if _, err := first.Subscribe("t1", func(Message) {}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	again := dial(t, url, "t1")
	got := make(chan Message, 2)
	unsub, err := again.Subscribe("t1", func(m Message) { got <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	time.Sleep(200 * time.Millisecond)
	if _, err := again.Publish(context.Background(), "t1", []byte("after reconnect"), "r1"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if string(m.Payload) != "after reconnect" {
			t.Errorf("payload = %q", m.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a reconnected seat did not receive traffic")
	}
}
