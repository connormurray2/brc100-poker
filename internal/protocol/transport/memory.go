package transport

import (
	"context"
	"fmt"
	"sync"
)

// Memory is an in-process Transport.
//
// It is the reference implementation of the interface's contract, and the one the game
// tests run against: a full hand can be played deterministically with no sockets, so a test
// failure is a game-logic failure rather than a networking flake.
type Memory struct {
	mu     sync.RWMutex
	closed bool
	nextID uint64
	tables map[string]map[uint64]Handler

	// wg tracks in-flight deliveries so Close and Drain can wait for them.
	wg sync.WaitGroup
}

// NewMemory returns an in-process transport.
func NewMemory() *Memory {
	return &Memory{tables: make(map[string]map[uint64]Handler)}
}

// Subscribe registers a handler for a table.
func (m *Memory) Subscribe(tableID string, h Handler) (func(), error) {
	if tableID == "" {
		return nil, fmt.Errorf("transport: table id is required")
	}
	if h == nil {
		return nil, fmt.Errorf("transport: handler is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}

	subs, ok := m.tables[tableID]
	if !ok {
		subs = make(map[uint64]Handler)
		m.tables[tableID] = subs
	}
	m.nextID++
	id := m.nextID
	subs[id] = h

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if subs, ok := m.tables[tableID]; ok {
				delete(subs, id)
				if len(subs) == 0 {
					delete(m.tables, tableID)
				}
			}
		})
	}, nil
}

// Publish delivers to every subscriber of the table, including the publisher's own.
func (m *Memory) Publish(ctx context.Context, tableID string, payload []byte, id string) (int, error) {
	if tableID == "" {
		return 0, fmt.Errorf("transport: table id is required")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return 0, err
		}
	}

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return 0, ErrClosed
	}
	handlers := make([]Handler, 0, len(m.tables[tableID]))
	for _, h := range m.tables[tableID] {
		handlers = append(handlers, h)
	}
	m.mu.RUnlock()

	// Copy the payload so a caller reusing its buffer cannot mutate what subscribers
	// observe, and so handlers running later still see the bytes as published.
	buf := make([]byte, len(payload))
	copy(buf, payload)

	for _, h := range handlers {
		msg := Message{TableID: tableID, ID: id, Payload: buf}
		m.wg.Add(1)
		// Asynchronous by contract: a handler must never run inline on the
		// publisher's goroutine, or a blocking handler would deadlock the publisher.
		go func(h Handler) {
			defer m.wg.Done()
			defer func() {
				// A panicking handler must not take the process down. The
				// message is lost, which is preferable to a crashed table.
				_ = recover()
			}()
			h(msg)
		}(h)
	}
	return len(handlers), nil
}

// Drain waits for in-flight deliveries to complete.
//
// Tests need this: delivery is asynchronous by contract, so asserting on state immediately
// after Publish would race the handlers.
func (m *Memory) Drain() { m.wg.Wait() }

// Close releases the transport after in-flight deliveries finish.
func (m *Memory) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.tables = make(map[string]map[uint64]Handler)
	m.mu.Unlock()

	m.wg.Wait()
	return nil
}
