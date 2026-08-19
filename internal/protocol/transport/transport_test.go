package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The seating handshake depends on a publisher seeing its own message. A fan-out that
// excludes the sender breaks seating permanently rather than visibly, so this is asserted
// first and explicitly.
func TestPublisherReceivesItsOwnMessage(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	var got atomic.Int64
	unsub, err := m.Subscribe("t1", func(Message) { got.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	n, err := m.Publish(context.Background(), "t1", []byte("hello"), "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("delivered to %d handlers, want 1", n)
	}
	m.Drain()
	if got.Load() != 1 {
		t.Fatal("the publisher's own subscriber did not receive the message")
	}
}

func TestAllSubscribersReceive(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	const seats = 6
	var wg sync.WaitGroup
	wg.Add(seats)
	var count atomic.Int64
	for i := 0; i < seats; i++ {
		unsub, err := m.Subscribe("t1", func(Message) {
			count.Add(1)
			wg.Done()
		})
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()
	}

	n, err := m.Publish(context.Background(), "t1", []byte("deal"), "")
	if err != nil {
		t.Fatal(err)
	}
	if n != seats {
		t.Fatalf("delivered to %d handlers, want %d", n, seats)
	}
	wg.Wait()
	if count.Load() != seats {
		t.Fatalf("%d handlers ran, want %d", count.Load(), seats)
	}
}

// Delivery must not run inline on the publisher's goroutine: a blocking handler would
// otherwise deadlock the publisher.
func TestDeliveryIsAsynchronous(t *testing.T) {
	m := NewMemory()

	release := make(chan struct{})
	entered := make(chan struct{})
	unsub, err := m.Subscribe("t1", func(Message) {
		close(entered)
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	done := make(chan struct{})
	go func() {
		if _, err := m.Publish(context.Background(), "t1", []byte("x"), ""); err != nil {
			t.Error(err)
		}
		close(done)
	}()

	select {
	case <-done:
		// Publish returned without waiting for the blocked handler: correct.
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a handler; delivery is running inline")
	}

	<-entered
	close(release)
	_ = m.Close()
}

func TestTablesAreIsolated(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	var a, b atomic.Int64
	ua, err := m.Subscribe("table-a", func(Message) { a.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer ua()
	ub, err := m.Subscribe("table-b", func(Message) { b.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer ub()

	if _, err := m.Publish(context.Background(), "table-a", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	m.Drain()
	if a.Load() != 1 || b.Load() != 0 {
		t.Fatalf("table-a got %d, table-b got %d; tables are not isolated", a.Load(), b.Load())
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	var got atomic.Int64
	unsub, err := m.Subscribe("t1", func(Message) { got.Add(1) })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Publish(context.Background(), "t1", []byte("1"), ""); err != nil {
		t.Fatal(err)
	}
	m.Drain()
	unsub()

	n, err := m.Publish(context.Background(), "t1", []byte("2"), "")
	if err != nil {
		t.Fatal(err)
	}
	m.Drain()
	if n != 0 {
		t.Errorf("delivered to %d handlers after unsubscribe, want 0", n)
	}
	if got.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", got.Load())
	}

	// Unsubscribing twice must be harmless.
	unsub()
}

func TestPublishAssignsAnIDWhenEmpty(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	ids := make(chan string, 2)
	unsub, err := m.Subscribe("t1", func(msg Message) { ids <- msg.ID })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	for i := 0; i < 2; i++ {
		if _, err := m.Publish(context.Background(), "t1", []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}
	m.Drain()

	a, b := <-ids, <-ids
	if a == "" || b == "" {
		t.Fatal("an assigned id was empty")
	}
	if a == b {
		t.Fatal("two publishes were assigned the same id")
	}
}

func TestExplicitIDIsPreserved(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	got := make(chan string, 1)
	unsub, err := m.Subscribe("t1", func(msg Message) { got <- msg.ID })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if _, err := m.Publish(context.Background(), "t1", []byte("x"), "shuffle-step-2"); err != nil {
		t.Fatal(err)
	}
	m.Drain()
	if id := <-got; id != "shuffle-step-2" {
		t.Fatalf("id = %q, want the explicit id", id)
	}
}

// A caller reusing its buffer must not be able to mutate what subscribers observe.
func TestPayloadIsCopied(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	got := make(chan []byte, 1)
	unsub, err := m.Subscribe("t1", func(msg Message) { got <- msg.Payload })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	buf := []byte("original")
	if _, err := m.Publish(context.Background(), "t1", buf, ""); err != nil {
		t.Fatal(err)
	}
	copy(buf, "MUTATED!")
	m.Drain()

	if s := string(<-got); s != "original" {
		t.Fatalf("subscriber saw %q; the payload was not copied", s)
	}
}

// A panicking handler must not take the table down with it.
func TestPanickingHandlerIsContained(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })

	var good atomic.Int64
	u1, err := m.Subscribe("t1", func(Message) { panic("handler blew up") })
	if err != nil {
		t.Fatal(err)
	}
	defer u1()
	u2, err := m.Subscribe("t1", func(Message) { good.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer u2()

	if _, err := m.Publish(context.Background(), "t1", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	m.Drain()
	if good.Load() != 1 {
		t.Fatal("a panicking handler prevented another handler from running")
	}
}

func TestClosedTransportRejectsOperations(t *testing.T) {
	m := NewMemory()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Subscribe("t1", func(Message) {}); err == nil {
		t.Error("Subscribe succeeded on a closed transport")
	}
	if _, err := m.Publish(context.Background(), "t1", []byte("x"), ""); err == nil {
		t.Error("Publish succeeded on a closed transport")
	}
	// Closing twice must be harmless.
	if err := m.Close(); err != nil {
		t.Errorf("second Close returned %v", err)
	}
}

func TestPublishHonoursContextCancellation(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Publish(ctx, "t1", []byte("x"), ""); err == nil {
		t.Fatal("Publish ignored a cancelled context")
	}
}

func TestValidationRejectsEmptyInput(t *testing.T) {
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })
	if _, err := m.Subscribe("", func(Message) {}); err == nil {
		t.Error("Subscribe accepted an empty table id")
	}
	if _, err := m.Subscribe("t1", nil); err == nil {
		t.Error("Subscribe accepted a nil handler")
	}
	if _, err := m.Publish(context.Background(), "", []byte("x"), ""); err == nil {
		t.Error("Publish accepted an empty table id")
	}
}

func TestDedupAppliesOnce(t *testing.T) {
	d := NewDedup(16)
	if !d.FirstSeen("a") {
		t.Fatal("first sighting reported as duplicate")
	}
	if d.FirstSeen("a") {
		t.Fatal("second sighting reported as new")
	}
	if !d.FirstSeen("b") {
		t.Fatal("a different id reported as duplicate")
	}
}

// An empty id carries no de-duplication information, so it must not be suppressed.
func TestDedupTreatsEmptyIDAsNew(t *testing.T) {
	d := NewDedup(16)
	if !d.FirstSeen("") || !d.FirstSeen("") {
		t.Fatal("an empty id was suppressed as a duplicate")
	}
}

// The retained set must be bounded: a long-running table would otherwise grow it forever.
func TestDedupEvictsOldestBeyondLimit(t *testing.T) {
	d := NewDedup(3)
	for _, id := range []string{"1", "2", "3", "4"} {
		d.FirstSeen(id)
	}
	if d.Len() > 3 {
		t.Fatalf("retained %d ids, want at most 3", d.Len())
	}
	// "1" was evicted, so it reads as new again. That is the accepted trade-off:
	// catch-up only concerns recent traffic.
	if !d.FirstSeen("1") {
		t.Fatal("the oldest id was not evicted")
	}
	// The most recent entries are still suppressed.
	if d.FirstSeen("4") {
		t.Fatal("a recent id was evicted before an older one")
	}
}

func TestDedupIsConcurrencySafe(t *testing.T) {
	d := NewDedup(1024)
	var wg sync.WaitGroup
	var firsts atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.FirstSeen("contended") {
				firsts.Add(1)
			}
		}()
	}
	wg.Wait()
	if firsts.Load() != 1 {
		t.Fatalf("%d goroutines saw the same id as new, want exactly 1", firsts.Load())
	}
}
