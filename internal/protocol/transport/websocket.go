package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wireFrame is the JSON envelope on the wire.
//
// A framed envelope rather than raw bytes because a reconnecting seat needs the message id
// to de-duplicate what it already applied, and the table id lets one connection serve more
// than one table.
type wireFrame struct {
	TableID string `json:"tableId"`
	ID      string `json:"id"`
	Payload []byte `json:"payload"`
}

// maxFrameBytes bounds a single frame. Game messages are small; anything larger is either a
// bug or an attempt to exhaust memory.
const maxFrameBytes = 1 << 20

// writeTimeout bounds a single write so one stalled peer cannot hold a table's hub.
const writeTimeout = 10 * time.Second

// Hub is the server side of the WebSocket transport.
//
// It fans a published message out to every connection subscribed to the table, including
// the one that published it. That echo is required: the seating handshake depends on a seat
// seeing its own announcement.
type Hub struct {
	logger *slog.Logger

	mu     sync.RWMutex
	closed bool
	nextID uint64
	// conns maps a table id to the connections subscribed to it.
	conns map[string]map[uint64]*conn
	// local holds in-process handlers, so the table service can observe its own
	// tables without dialling itself.
	localNext uint64
	local     map[string]map[uint64]Handler

	wg sync.WaitGroup
}

type conn struct {
	ws *websocket.Conn
	// writeMu serialises writes: the library allows only one concurrent writer.
	writeMu sync.Mutex
}

// NewHub returns a server-side transport hub.
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		logger: logger,
		conns:  make(map[string]map[uint64]*conn),
		local:  make(map[string]map[uint64]Handler),
	}
}

// Subscribe registers an in-process handler, so the table service can observe the tables it
// hosts without opening a socket to itself.
func (h *Hub) Subscribe(tableID string, fn Handler) (func(), error) {
	if tableID == "" {
		return nil, errors.New("transport: table id is required")
	}
	if fn == nil {
		return nil, errors.New("transport: handler is required")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	subs, ok := h.local[tableID]
	if !ok {
		subs = make(map[uint64]Handler)
		h.local[tableID] = subs
	}
	h.localNext++
	id := h.localNext
	subs[id] = fn

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if subs, ok := h.local[tableID]; ok {
				delete(subs, id)
				if len(subs) == 0 {
					delete(h.local, tableID)
				}
			}
		})
	}, nil
}

// Publish fans a message out to every subscriber of the table.
func (h *Hub) Publish(ctx context.Context, tableID string, payload []byte, id string) (int, error) {
	if tableID == "" {
		return 0, errors.New("transport: table id is required")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(payload) > maxFrameBytes {
		return 0, fmt.Errorf("transport: payload is %d bytes, over the %d limit", len(payload), maxFrameBytes)
	}
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return 0, err
		}
	}

	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return 0, ErrClosed
	}
	remote := make([]*conn, 0, len(h.conns[tableID]))
	for _, c := range h.conns[tableID] {
		remote = append(remote, c)
	}
	locals := make([]Handler, 0, len(h.local[tableID]))
	for _, fn := range h.local[tableID] {
		locals = append(locals, fn)
	}
	h.mu.RUnlock()

	buf := make([]byte, len(payload))
	copy(buf, payload)

	frame, err := json.Marshal(wireFrame{TableID: tableID, ID: id, Payload: buf})
	if err != nil {
		return 0, fmt.Errorf("transport: encoding frame: %w", err)
	}

	// Deliver to local handlers asynchronously, matching the interface contract.
	for _, fn := range locals {
		msg := Message{TableID: tableID, ID: id, Payload: buf}
		h.wg.Add(1)
		go func(fn Handler) {
			defer h.wg.Done()
			defer func() { _ = recover() }()
			fn(msg)
		}(fn)
	}

	delivered := len(locals)
	for _, c := range remote {
		if err := c.write(ctx, frame); err != nil {
			// One broken peer must not fail the publish for everyone else. The
			// read loop will notice the closed socket and unregister it.
			h.logger.Debug("dropping a frame for a failed connection", "table", tableID, "error", err)
			continue
		}
		delivered++
	}
	return delivered, nil
}

func (c *conn) write(ctx context.Context, frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, frame)
}

// Drain waits for in-flight local deliveries.
func (h *Hub) Drain() { h.wg.Wait() }

// Close shuts the hub and every connection down.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	var all []*conn
	for _, subs := range h.conns {
		for _, c := range subs {
			all = append(all, c)
		}
	}
	h.conns = make(map[string]map[uint64]*conn)
	h.local = make(map[string]map[uint64]Handler)
	h.mu.Unlock()

	for _, c := range all {
		_ = c.ws.Close(websocket.StatusNormalClosure, "hub closing")
	}
	h.wg.Wait()
	return nil
}

// ServeHTTP upgrades a request and serves one seat's connection.
//
// The table is taken from the "table" query parameter. Authentication is deliberately not
// handled here: seat identity is proven in the game protocol, which is the layer that knows
// what a seat is.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tableID := r.URL.Query().Get("table")
	if tableID == "" {
		http.Error(w, "missing table parameter", http.StatusBadRequest)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The browser client is served from the same origin as this endpoint in
		// deployment; a permissive setting here would let any page drive a table.
		OriginPatterns: nil,
	})
	if err != nil {
		h.logger.Debug("websocket upgrade failed", "error", err)
		return
	}
	ws.SetReadLimit(maxFrameBytes)

	c := &conn{ws: ws}
	id, ok := h.register(tableID, c)
	if !ok {
		_ = ws.Close(websocket.StatusGoingAway, "hub closed")
		return
	}
	defer h.unregister(tableID, id)

	ctx := r.Context()
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			// A normal close is expected; anything else is worth a debug line.
			if websocket.CloseStatus(err) == -1 {
				h.logger.Debug("connection read ended", "table", tableID, "error", err)
			}
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var f wireFrame
		if err := json.Unmarshal(data, &f); err != nil {
			h.logger.Debug("dropping an unparseable frame", "table", tableID, "error", err)
			continue
		}
		// A connection may only publish to the table it joined, so one seat cannot
		// inject messages into another table over its own socket.
		if f.TableID != "" && f.TableID != tableID {
			h.logger.Debug("dropping a frame for a different table", "joined", tableID, "claimed", f.TableID)
			continue
		}
		if _, err := h.Publish(ctx, tableID, f.Payload, f.ID); err != nil {
			h.logger.Debug("republishing a received frame failed", "table", tableID, "error", err)
		}
	}
}

func (h *Hub) register(tableID string, c *conn) (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, false
	}
	subs, ok := h.conns[tableID]
	if !ok {
		subs = make(map[uint64]*conn)
		h.conns[tableID] = subs
	}
	h.nextID++
	subs[h.nextID] = c
	return h.nextID, true
}

func (h *Hub) unregister(tableID string, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs, ok := h.conns[tableID]; ok {
		delete(subs, id)
		if len(subs) == 0 {
			delete(h.conns, tableID)
		}
	}
}

// Client is the seat side of the WebSocket transport.
type Client struct {
	logger  *slog.Logger
	tableID string

	ws      *websocket.Conn
	writeMu sync.Mutex

	mu       sync.RWMutex
	closed   bool
	nextID   uint64
	handlers map[uint64]Handler

	cancel context.CancelFunc
	done   chan struct{}
}

// Dial connects to a hub for one table and starts reading.
func Dial(ctx context.Context, logger *slog.Logger, url, tableID string) (*Client, error) {
	if tableID == "" {
		return nil, errors.New("transport: table id is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	ws, resp, err := websocket.Dial(ctx, url+"?table="+tableID, nil)
	if resp != nil && resp.Body != nil {
		// The handshake response body carries nothing we need, but leaving it open
		// leaks a connection per dial.
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("transport: dialling %s: %w", url, err)
	}
	ws.SetReadLimit(maxFrameBytes)

	readCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Client{
		logger:   logger,
		tableID:  tableID,
		ws:       ws,
		handlers: make(map[uint64]Handler),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go c.readLoop(readCtx)
	return c, nil
}

func (c *Client) readLoop(ctx context.Context) {
	defer close(c.done)
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var f wireFrame
		if err := json.Unmarshal(data, &f); err != nil {
			c.logger.Debug("dropping an unparseable frame", "error", err)
			continue
		}

		c.mu.RLock()
		hs := make([]Handler, 0, len(c.handlers))
		for _, h := range c.handlers {
			hs = append(hs, h)
		}
		c.mu.RUnlock()

		msg := Message{TableID: c.tableID, ID: f.ID, Payload: f.Payload}
		for _, h := range hs {
			func(h Handler) {
				defer func() { _ = recover() }()
				h(msg)
			}(h)
		}
	}
}

// Subscribe registers a handler for the client's table.
func (c *Client) Subscribe(tableID string, h Handler) (func(), error) {
	if h == nil {
		return nil, errors.New("transport: handler is required")
	}
	if tableID != "" && tableID != c.tableID {
		return nil, fmt.Errorf("transport: client is joined to table %q, not %q", c.tableID, tableID)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	c.nextID++
	id := c.nextID
	c.handlers[id] = h

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.handlers, id)
		})
	}, nil
}

// Publish sends a message to the table.
//
// The hub echoes it back to every subscriber including this one, so a caller must not also
// apply the message locally or it would be applied twice.
func (c *Client) Publish(ctx context.Context, tableID string, payload []byte, id string) (int, error) {
	if tableID != "" && tableID != c.tableID {
		return 0, fmt.Errorf("transport: client is joined to table %q, not %q", c.tableID, tableID)
	}
	if len(payload) > maxFrameBytes {
		return 0, fmt.Errorf("transport: payload is %d bytes, over the %d limit", len(payload), maxFrameBytes)
	}

	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return 0, ErrClosed
	}
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return 0, err
		}
	}

	frame, err := json.Marshal(wireFrame{TableID: c.tableID, ID: id, Payload: payload})
	if err != nil {
		return 0, fmt.Errorf("transport: encoding frame: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := c.ws.Write(wctx, websocket.MessageText, frame); err != nil {
		return 0, fmt.Errorf("transport: writing frame: %w", err)
	}
	return 1, nil
}

// Close disconnects the client.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.handlers = make(map[uint64]Handler)
	c.mu.Unlock()

	err := c.ws.Close(websocket.StatusNormalClosure, "client closing")
	c.cancel()
	<-c.done
	if err != nil {
		return fmt.Errorf("transport: closing client: %w", err)
	}
	return nil
}
