// Package transport carries game messages between the seats at a table.
//
// This is the only coupling between the game protocol and how bytes move, which is what
// lets the same engine run over an in-memory bus in tests and a WebSocket in production
// with no game-logic change.
//
// Two properties are load-bearing and easy to get wrong:
//
//   - **Publishes echo back to the publisher.** The seating handshake depends on a seat
//     seeing its own announcement, so a fan-out that excludes the sender breaks seating
//     permanently rather than visibly.
//   - **Delivery is asynchronous.** Handlers never run inline on the publisher's goroutine,
//     so a slow or blocking handler cannot deadlock the publisher.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// Message is one game message on a table.
type Message struct {
	// TableID identifies the table this message belongs to.
	TableID string
	// ID is the de-duplication key. The same ID delivered twice is applied once.
	ID string
	// Payload is the opaque game message.
	Payload []byte
}

// Handler receives messages for a table. It must not panic; a panic in a handler is
// contained but the message is lost.
type Handler func(Message)

// Transport carries messages for tables.
type Transport interface {
	// Subscribe registers a handler for a table and returns a function that
	// unsubscribes it. The handler is called asynchronously, never inline on a
	// publisher's goroutine.
	Subscribe(tableID string, h Handler) (unsubscribe func(), err error)

	// Publish sends a payload to every subscriber of the table, including the
	// publisher's own subscribers. An empty id means "assign a fresh one".
	//
	// It returns the number of handlers the message was delivered to.
	Publish(ctx context.Context, tableID string, payload []byte, id string) (int, error)

	// Close releases the transport. Subsequent operations fail.
	Close() error
}

// NewID returns a fresh de-duplication key.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("transport: reading random source: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrClosed is returned once a transport has been closed.
var ErrClosed = errors.New("transport: closed")

// Dedup tracks which message ids have already been applied.
//
// De-duplication belongs on the receiving side because a sender may legitimately re-publish
// the same id so a reconnecting seat can catch up. Re-publication must reach the peer that
// missed it without being applied twice by peers that already have it.
type Dedup struct {
	mu   sync.Mutex
	seen map[string]struct{}
	// order preserves insertion order so the oldest entries can be evicted.
	order []string
	limit int
}

// NewDedup returns a de-duplicator retaining at most limit ids.
//
// The bound matters: a table that runs for a long time would otherwise grow this map
// without limit. Evicting the oldest ids is safe because catch-up concerns recent traffic.
func NewDedup(limit int) *Dedup {
	if limit <= 0 {
		limit = 4096
	}
	return &Dedup{seen: make(map[string]struct{}, limit), limit: limit}
}

// FirstSeen reports whether this id has not been seen before, recording it if so.
func (d *Dedup) FirstSeen(id string) bool {
	if id == "" {
		// An empty id carries no de-duplication information, so it cannot be
		// suppressed. Treat it as new rather than silently dropping it.
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.seen[id]; dup {
		return false
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.limit {
		evict := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, evict)
	}
	return true
}

// Len reports how many ids are currently retained.
func (d *Dedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
