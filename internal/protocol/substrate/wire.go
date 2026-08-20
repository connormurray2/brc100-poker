// Package substrate carries BRC-100 wallet calls over the network.
//
// It exists because go-arcade-toolbox is a BRC-100 wallet *library*: a key is passed to
// wallet.New and never leaves that process. The old JSON-RPC transport was removed in the
// rewrite, and upstream states the limitation plainly — BSV Desktop can pay an application
// but "cannot drive a toolbox wallet". Without a transport, player-held keys can only fund;
// they cannot sign a pot settlement, which is what a non-custodial pot requires.
//
// The toolbox's own storage REST API is deliberately NOT the model. Its default
// authenticator "performs NO cryptographic verification — any caller can claim any identity
// by setting the header", and it serves no TLS. Copying that shape would make every network
// boundary a custody boundary. Here identity is proven, never asserted.
package substrate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Protocol version. A mismatch is refused rather than negotiated: this carries signing
// authority, so silently falling back to an older contract is not acceptable.
const Version = "brc100-substrate/1"

// Method is a BRC-100 method name.
//
// Only the methods the game needs are named. Generalising the substrate to the full BRC-100
// surface is a later, separate concern — a smaller grant set is a smaller blast radius.
type Method string

const (
	// MethodGetPublicKey resolves a seat's identity or a derived key.
	MethodGetPublicKey Method = "getPublicKey"
	// MethodGetNetwork reports the network, translated to a valid BRC-100 value.
	MethodGetNetwork Method = "getNetwork"
	// MethodCreateAction builds a transaction.
	MethodCreateAction Method = "createAction"
	// MethodSignAction completes a previously created transaction.
	MethodSignAction Method = "signAction"
	// MethodInternalizeAction records an incoming payment.
	MethodInternalizeAction Method = "internalizeAction"
	// MethodListOutputs enumerates a wallet's outputs. Sensitive: a table has no
	// business enumerating a player's coins.
	MethodListOutputs Method = "listOutputs"
	// MethodListActions enumerates a wallet's transaction history. Also sensitive.
	MethodListActions Method = "listActions"
	// MethodDealCommit publishes a seat's shuffle and remask commitments, binding it to the
	// transformation it will apply before the deal begins.
	MethodDealCommit Method = "dealCommit"
	// MethodDealShuffle applies a seat's committed shuffle to a deck.
	MethodDealShuffle Method = "dealShuffle"
	// MethodDealRemask applies a seat's committed remask.
	MethodDealRemask Method = "dealRemask"
	// MethodDealReveal discloses a seat's per-position scalars for named positions. This is
	// the method that deals a card, and the reason the deal is dealerless: the scalars never
	// leave the agent, so nothing else can read a card the seat was not given.
	MethodDealReveal Method = "dealReveal"
	// MethodDealFinal records the completed deck with a seat, since an agent only sees the
	// deck as it was when its own pass ran and reading a card needs the final one.
	MethodDealFinal Method = "dealFinal"
	// MethodDealCard asks a seat's own agent to identify a card it can read.
	MethodDealCard Method = "dealCard"

	// MethodRecordStake tells a wallet which pot its stake went into, so it can verify a
	// later settlement against its own record rather than the table's word.
	//
	// The wallet derives the expected payout scripts itself from the sender's public key and
	// its own private key, so the caller supplies amounts and derivation material but never
	// the scripts. A caller that could name the scripts could name itself as the payee.
	MethodRecordStake Method = "recordStake"

	// MethodSignPot signs one input of a pot transaction. Not a BRC-100 method: it is
	// this application's co-signing primitive, and it is the only method that produces a
	// signature over money the caller proposed.
	MethodSignPot Method = "signPot"
)

// Known reports whether the method is one this substrate serves.
func (m Method) Known() bool {
	switch m {
	case MethodGetPublicKey, MethodGetNetwork, MethodCreateAction, MethodSignAction,
		MethodInternalizeAction, MethodListOutputs, MethodListActions, MethodSignPot,
		MethodRecordStake,
		MethodDealCommit, MethodDealShuffle, MethodDealRemask, MethodDealReveal,
		MethodDealFinal, MethodDealCard:
		return true
	default:
		return false
	}
}

// Request is a substrate call.
//
// Every field except Signature is covered by the signature, so none of them can be altered
// in transit without detection.
type Request struct {
	Version string `json:"version"`
	Method  Method `json:"method"`
	// Originator is the FQDN-shaped caller identifier BRC-100 requires.
	Originator string `json:"originator"`
	// Params is the method's arguments, opaque at this layer.
	Params json.RawMessage `json:"params,omitempty"`

	// IdentityKey is the caller's public key in DER hex. It is a claim until the
	// signature proves it.
	IdentityKey string `json:"identityKey"`
	// Nonce makes each request unique, so a captured request cannot be replayed.
	Nonce string `json:"nonce"`
	// TimestampUnix bounds how long a captured request stays usable at all.
	TimestampUnix int64 `json:"timestampUnix"`
	// Audience is the identity key of the wallet this request is for, so a request
	// captured from one wallet cannot be replayed against another.
	Audience string `json:"audience"`

	// Signature proves control of IdentityKey over everything above.
	Signature string `json:"signature"`
}

// Response is a substrate reply.
//
// The wallet signs its responses too: a caller must be able to detect a substituted
// endpoint, not merely trust the channel.
type Response struct {
	Version string `json:"version"`
	// RequestNonce ties the response to exactly one request.
	RequestNonce string          `json:"requestNonce"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *Error          `json:"error,omitempty"`

	// IdentityKey is the responding wallet's key, and Signature proves it.
	IdentityKey string `json:"identityKey"`
	Signature   string `json:"signature"`
}

// Error is a structured failure.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("substrate: %s: %s", e.Code, e.Message) }

// Code classifies a failure. Codes are stable; messages are not.
type Code string

const (
	// CodeBadRequest is a malformed or unparseable request.
	CodeBadRequest Code = "bad_request"
	// CodeUnauthenticated means identity was not proven.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeForbidden means the caller is authenticated but not granted this method.
	CodeForbidden Code = "forbidden"
	// CodeUnknownMethod means the method is not served.
	CodeUnknownMethod Code = "unknown_method"
	// CodeReplayed means the nonce has been seen before.
	CodeReplayed Code = "replayed"
	// CodeExpired means the timestamp is outside the accepted window.
	CodeExpired Code = "expired"
	// CodeDeclined means the player refused to authorise the operation.
	CodeDeclined Code = "declined"
	// CodeRateLimited means the caller exceeded its allowance.
	CodeRateLimited Code = "rate_limited"
	// CodeTooLarge means the request body exceeded the maximum.
	CodeTooLarge Code = "too_large"
	// CodeInternal is a fault in the wallet or substrate.
	CodeInternal Code = "internal"
)

// MaxClockSkew bounds how far a request's timestamp may be from the server's clock.
//
// Deliberately tight. The nonce cache is what actually prevents replay; this only bounds how
// long the cache must remember a nonce, and how long a captured request is worth anything at
// all if the cache is ever lost.
const MaxClockSkew = 2 * time.Minute

// requestDigest is the message a request's signature covers.
//
// Length-prefixing every field is what makes the encoding unambiguous: without it, moving a
// character between two adjacent fields could produce the same byte stream and therefore the
// same valid signature.
func requestDigest(r Request) []byte {
	h := sha256.New()
	writeField(h, "brc100-substrate-request")
	writeField(h, r.Version)
	writeField(h, string(r.Method))
	writeField(h, r.Originator)
	writeField(h, string(r.Params))
	writeField(h, r.IdentityKey)
	writeField(h, r.Nonce)
	writeUint64(h, uint64(r.TimestampUnix))
	writeField(h, r.Audience)
	sum := h.Sum(nil)
	return sum
}

// responseDigest is the message a response's signature covers.
func responseDigest(r Response) []byte {
	h := sha256.New()
	writeField(h, "brc100-substrate-response")
	writeField(h, r.Version)
	writeField(h, r.RequestNonce)
	writeField(h, string(r.Result))
	if r.Error != nil {
		writeField(h, string(r.Error.Code))
		writeField(h, r.Error.Message)
	} else {
		writeField(h, "")
		writeField(h, "")
	}
	writeField(h, r.IdentityKey)
	return h.Sum(nil)
}

type byteWriter interface {
	Write(p []byte) (int, error)
}

func writeField(w byteWriter, s string) {
	writeUint64(w, uint64(len(s)))
	_, _ = w.Write([]byte(s))
}

func writeUint64(w byteWriter, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = w.Write(b[:])
}

// ValidateOriginator checks the FQDN-shaped originator BRC-100 requires.
//
// The toolbox validates this too and rejects a call without it, so checking here turns a
// deep library error into a clear boundary error.
func ValidateOriginator(o string) error {
	if o == "" {
		return errors.New("originator is required")
	}
	if len(o) > 255 {
		return fmt.Errorf("originator is %d characters, over the 255 limit", len(o))
	}
	if strings.ContainsAny(o, " \t\r\n/\\:") {
		return fmt.Errorf("originator %q is not FQDN-shaped", o)
	}
	if !strings.Contains(o, ".") {
		return fmt.Errorf("originator %q has no domain part", o)
	}
	return nil
}

// Grants is the set of methods one caller identity may invoke.
//
// Least privilege is the point: a table service needs to propose transactions and collect
// signatures, and has no business enumerating a player's outputs or history.
type Grants struct {
	methods map[Method]struct{}
}

// NewGrants builds a grant set.
func NewGrants(methods ...Method) (Grants, error) {
	g := Grants{methods: make(map[Method]struct{}, len(methods))}
	for _, m := range methods {
		if !m.Known() {
			return Grants{}, fmt.Errorf("substrate: cannot grant unknown method %q", m)
		}
		g.methods[m] = struct{}{}
	}
	return g, nil
}

// Allows reports whether the method is granted.
func (g Grants) Allows(m Method) bool {
	if g.methods == nil {
		return false
	}
	_, ok := g.methods[m]
	return ok
}

// Methods lists the granted methods in a stable order.
func (g Grants) Methods() []Method {
	out := make([]Method, 0, len(g.methods))
	for m := range g.methods {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TableGrants is what a table service may do to a player's wallet.
//
// It can ask for the seat's identity, propose a signature, and hand over a received payment.
// It cannot enumerate outputs or history, and it cannot make the wallet spend on its own.
func TableGrants() Grants {
	// The deal methods are granted because the table drives the chain: it asks each seat in
	// turn to apply its pass. The secrets themselves never leave the agent, so granting these
	// lets the table sequence a deal without ever being able to read a card.
	g, err := NewGrants(MethodGetPublicKey, MethodGetNetwork, MethodSignPot, MethodInternalizeAction,
		MethodDealCommit, MethodDealShuffle, MethodDealRemask, MethodDealReveal,
		MethodDealFinal, MethodDealCard)
	if err != nil {
		// The method list is a compile-time constant, so this cannot fail.
		panic(err)
	}
	return g
}

// OwnerGrants is what a player's own client may do to their wallet: everything served.
func OwnerGrants() Grants {
	g, err := NewGrants(MethodGetPublicKey, MethodGetNetwork, MethodCreateAction,
		MethodSignAction, MethodInternalizeAction, MethodListOutputs, MethodListActions, MethodSignPot,
		MethodRecordStake,
		MethodDealCommit, MethodDealShuffle, MethodDealRemask, MethodDealReveal,
		MethodDealFinal, MethodDealCard)
	if err != nil {
		panic(err)
	}
	return g
}
