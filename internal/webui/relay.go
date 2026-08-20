package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Relay carries substrate traffic to a wallet the table cannot dial.
//
// A player's wallet listens on their own loopback, so the table -- which runs elsewhere -- can
// never reach it. Their browser can. So the table parks a request here, the page collects it,
// forwards it to the wallet, and posts the reply back.
//
// The browser is a pipe and nothing more. Requests are signed by the table and addressed to one
// seat's identity key, and responses are signed by the wallet; both are verified at the ends. A
// page that altered, replayed or fabricated a message would fail that verification, so relaying
// through untrusted software costs nothing in security terms.
type Relay struct {
	mu      sync.Mutex
	pending map[string]*relayCall // by request nonce
	queued  map[string][]string   // seat identity key -> nonces awaiting collection
	now     func() time.Time
}

// relayCall is one in-flight request.
type relayCall struct {
	// IdentityKey is the seat whose wallet must answer.
	IdentityKey string
	// Body is the signed substrate request, verbatim. It is never re-encoded: the signature
	// covers the transmitted bytes, so re-marshalling would invalidate it.
	Body json.RawMessage
	// reply delivers the wallet's response, or an error the page reports.
	reply chan relayReply
	// Deadline bounds how long a request may sit uncollected.
	Deadline time.Time
}

type relayReply struct {
	body json.RawMessage
	err  error
}

// RelayItem is one request handed to a browser for forwarding.
type RelayItem struct {
	Nonce string          `json:"nonce"`
	Body  json.RawMessage `json:"body"`
}

// RelayURL is the address a seat registers when its wallet is only reachable through its own
// browser. It is a sentinel rather than a real URL, because there is no address the table could
// dial: the wallet listens on the player's loopback, which is theirs and not ours.
const RelayURL = "relay:browser"

// NewRelay builds a relay.
func NewRelay() *Relay {
	return &Relay{
		pending: make(map[string]*relayCall),
		queued:  make(map[string][]string),
		now:     time.Now,
	}
}

// ErrRelayTimeout means no browser collected or answered the request in time.
var ErrRelayTimeout = errors.New("webui: the seat's browser did not relay the request in time")

// Do parks a request for a seat and waits for its wallet's reply.
//
// The nonce is the correlation key. It already exists in the signed request and is unique per
// request, so no second identifier is needed.
func (r *Relay) Do(ctx context.Context, identityKey, nonce string, body json.RawMessage) (json.RawMessage, error) {
	if identityKey == "" || nonce == "" {
		return nil, errors.New("webui: a relayed request needs a seat and a nonce")
	}

	call := &relayCall{
		IdentityKey: identityKey,
		Body:        body,
		reply:       make(chan relayReply, 1),
	}

	r.mu.Lock()
	if _, exists := r.pending[nonce]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("webui: nonce %s is already in flight", nonce)
	}
	if deadline, ok := ctx.Deadline(); ok {
		call.Deadline = deadline
	} else {
		call.Deadline = r.now().Add(60 * time.Second)
	}
	r.pending[nonce] = call
	r.queued[identityKey] = append(r.queued[identityKey], nonce)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.pending, nonce)
		r.mu.Unlock()
	}()

	select {
	case reply := <-call.reply:
		return reply.body, reply.err
	case <-ctx.Done():
		// A hand that stalls here must say so in terms an operator can act on, rather than
		// surfacing a bare context error.
		return nil, fmt.Errorf("%w: %s", ErrRelayTimeout, identityKey)
	}
}

// Collect hands a seat's browser the requests waiting for its wallet.
//
// Returning them removes them from the queue: a request is delivered once, and if the browser
// drops it the request times out rather than being handed to a later poll. Re-delivery would risk
// the wallet seeing the same nonce twice, which its replay cache would reject anyway.
func (r *Relay) Collect(identityKey string) []RelayItem {
	r.mu.Lock()
	defer r.mu.Unlock()

	nonces := r.queued[identityKey]
	if len(nonces) == 0 {
		return nil
	}
	delete(r.queued, identityKey)

	out := make([]RelayItem, 0, len(nonces))
	for _, nonce := range nonces {
		call, ok := r.pending[nonce]
		if !ok {
			continue // already answered or abandoned
		}
		out = append(out, RelayItem{Nonce: nonce, Body: call.Body})
	}
	return out
}

// Deliver returns a wallet's response to whoever is waiting for it.
//
// The identity key is checked against the request's, so one seat's browser cannot answer for
// another. That matters: a response carries the wallet's signature, and accepting it under the
// wrong seat would let a player supply their own wallet's answer in place of someone else's.
func (r *Relay) Deliver(identityKey, nonce string, body json.RawMessage, relayErr string) error {
	r.mu.Lock()
	call, ok := r.pending[nonce]
	if ok && call.IdentityKey != identityKey {
		r.mu.Unlock()
		return errors.New("webui: this seat is not the one that request was addressed to")
	}
	r.mu.Unlock()

	if !ok {
		// The waiter gave up, or the nonce was never issued. Not an error worth failing the
		// browser over; there is simply nothing to deliver to.
		return nil
	}

	reply := relayReply{body: body}
	if relayErr != "" {
		reply.err = fmt.Errorf("webui: the seat's wallet could not be reached: %s", relayErr)
	}
	select {
	case call.reply <- reply:
	default:
		// Buffered channel, so this only happens if a reply already landed. First answer wins.
	}
	return nil
}

// Pending reports how many requests are in flight, for diagnostics.
func (r *Relay) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
