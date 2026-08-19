package substrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// MaxBodyBytes bounds a request body. Wallet calls are small; a large body is either a bug
// or an attempt to exhaust memory.
const MaxBodyBytes = 1 << 20

// SigningRequest is what a player is asked to approve.
//
// The fields exist so the player can check the request against what they believe the hand
// decided. A consent prompt that only says "sign this?" is not consent — it is a rubber
// stamp, and it would make the substrate as dangerous as a custodial server.
type SigningRequest struct {
	// HandID ties the request to a hand.
	HandID string `json:"handId"`
	// Purpose is a short description, e.g. "pot settlement" or "refund".
	Purpose string `json:"purpose"`
	// PotOutpoint is the pot being spent.
	PotOutpoint string `json:"potOutpoint"`
	// PotSatoshis is the pot's value.
	PotSatoshis uint64 `json:"potSatoshis"`
	// Outputs is every output the transaction commits to, in order. The player must see
	// all of them, since an undeclared extra output is how a pot gets skimmed.
	Outputs []SigningOutput `json:"outputs"`
	// FeeSatoshis is what the transaction consumes in fees.
	FeeSatoshis uint64 `json:"feeSatoshis"`
	// RawTxHex is the full transaction, so an advanced player can verify independently.
	RawTxHex string `json:"rawTxHex"`
}

// SigningOutput is one output in a signing request.
type SigningOutput struct {
	Satoshis      uint64 `json:"satoshis"`
	LockingScript string `json:"lockingScript"`
	// Description says who this pays in terms the player recognises, e.g. "you" or
	// "seat 3".
	Description string `json:"description"`
}

// Approver decides whether to sign.
//
// Returning an error declines. An implementation may prompt a human, apply a policy, or in
// tests approve automatically — but the decision is always the player's side to make, never
// the caller's.
type Approver interface {
	Approve(req SigningRequest) error
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(SigningRequest) error

// Approve implements Approver.
func (f ApproverFunc) Approve(req SigningRequest) error { return f(req) }

// Handler serves one method. Params are the raw method arguments; the returned value is
// JSON-encoded into the response.
type Handler func(caller *ec.PublicKey, params json.RawMessage) (any, error)

// Server serves BRC-100 calls over HTTP for one wallet.
type Server struct {
	logger *slog.Logger

	// wallet signs responses and is the audience for requests.
	wallet     *ec.PrivateKey
	audience   string
	nonces     *NonceCache
	limiter    *rateLimiter
	approver   Approver
	requireTLS bool
	origins    map[string]struct{}

	mu       sync.RWMutex
	grants   map[string]Grants
	handlers map[Method]Handler
}

// Config parameterises a Server.
type Config struct {
	// Wallet is the key this substrate serves. It never leaves the process.
	Wallet *ec.PrivateKey
	// Approver gates every signing request. Required: without it the substrate would
	// sign whatever it is asked to.
	Approver Approver
	// RequireTLS refuses to serve a plaintext request. Off only for local development.
	RequireTLS bool
	// RequestsPerMinute bounds one caller's rate. Zero applies a default.
	RequestsPerMinute int
	// AllowedOrigins are the web origins permitted to call this substrate from a browser.
	//
	// Required for a browser client, and deliberately an explicit list rather than "*":
	// this endpoint signs transactions, so any page being able to reach it would let a
	// hostile site enumerate and prompt a player's wallet. Authentication still applies —
	// CORS only decides which pages may ask.
	AllowedOrigins []string

	Logger *slog.Logger
}

// NewServer builds a substrate server.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Wallet == nil {
		return nil, errors.New("substrate: a wallet key is required")
	}
	if cfg.Approver == nil {
		// Defaulting to "approve everything" would be a silent downgrade from
		// non-custodial to custodial, so this is refused instead.
		return nil, errors.New("substrate: an approver is required; without one the wallet would sign anything it is asked to")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	rpm := cfg.RequestsPerMinute
	if rpm <= 0 {
		rpm = 120
	}

	origins := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		origins[strings.ToLower(strings.TrimSpace(o))] = struct{}{}
	}

	return &Server{
		logger:     logger,
		origins:    origins,
		wallet:     cfg.Wallet,
		audience:   cfg.Wallet.PubKey().ToDERHex(),
		nonces:     NewNonceCache(),
		limiter:    newRateLimiter(rpm, time.Minute),
		approver:   cfg.Approver,
		requireTLS: cfg.RequireTLS,
		grants:     make(map[string]Grants),
		handlers:   make(map[Method]Handler),
	}, nil
}

// Audience returns the wallet's identity key, which callers must address requests to.
func (s *Server) Audience() string { return s.audience }

// Grant authorises a caller identity for a set of methods.
func (s *Server) Grant(identityKeyHex string, g Grants) error {
	if identityKeyHex == "" {
		return errors.New("substrate: an identity key is required to grant methods")
	}
	if _, err := ec.PublicKeyFromString(identityKeyHex); err != nil {
		return fmt.Errorf("substrate: %q is not a valid identity key: %w", identityKeyHex, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[strings.ToLower(identityKeyHex)] = g
	return nil
}

// Revoke removes a caller's grants.
func (s *Server) Revoke(identityKeyHex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grants, strings.ToLower(identityKeyHex))
}

// HandleMethod registers a handler.
func (s *Server) HandleMethod(m Method, h Handler) error {
	if !m.Known() {
		return fmt.Errorf("substrate: cannot serve unknown method %q", m)
	}
	if h == nil {
		return errors.New("substrate: a handler is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[m] = h
	return nil
}

// Approve runs the approver, so a handler can gate a signature on the player's decision.
func (s *Server) Approve(req SigningRequest) error { return s.approver.Approve(req) }

// ServeHTTP serves one substrate call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS, so a browser client can reach a player's own agent. Only origins the player
	// explicitly allowed are echoed back: this endpoint signs transactions, and a wildcard
	// would let any page prompt the wallet.
	origin := strings.ToLower(r.Header.Get("Origin"))
	if origin != "" {
		if _, ok := s.origins[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
		} else {
			// An unlisted origin is refused before any wallet work happens.
			s.logger.Debug("refusing a request from an unlisted origin", "origin", origin)
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodOptions {
		// A preflight carries no request to authenticate; the headers above are the answer.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		s.fail(w, "", &Error{Code: CodeBadRequest, Message: "only POST is served"}, http.StatusMethodNotAllowed)
		return
	}
	// Refusing plaintext outside development keeps a network boundary from becoming a
	// custody boundary.
	if s.requireTLS && r.TLS == nil {
		s.fail(w, "", &Error{Code: CodeBadRequest, Message: "this endpoint requires TLS"}, http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		s.fail(w, "", &Error{Code: CodeBadRequest, Message: "could not read the request body"}, http.StatusBadRequest)
		return
	}
	if len(body) > MaxBodyBytes {
		s.fail(w, "", &Error{Code: CodeTooLarge, Message: fmt.Sprintf("request exceeds %d bytes", MaxBodyBytes)}, http.StatusRequestEntityTooLarge)
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.fail(w, "", &Error{Code: CodeBadRequest, Message: "request is not valid JSON"}, http.StatusBadRequest)
		return
	}

	now := time.Now()
	caller, verr := VerifyRequest(req, now, s.audience)
	if verr != nil {
		var se *Error
		if !errors.As(verr, &se) {
			se = &Error{Code: CodeUnauthenticated, Message: verr.Error()}
		}
		s.fail(w, req.Nonce, se, statusFor(se.Code))
		return
	}
	callerKey := strings.ToLower(caller.ToDERHex())

	// Rate limit after authentication so an unauthenticated flood cannot consume a
	// legitimate caller's allowance, but before any wallet work.
	if !s.limiter.allow(callerKey, now) {
		s.fail(w, req.Nonce, &Error{Code: CodeRateLimited, Message: "too many requests"}, http.StatusTooManyRequests)
		return
	}
	if !s.nonces.Use(req.Nonce, now) {
		s.fail(w, req.Nonce, &Error{Code: CodeReplayed, Message: "this request has already been served"}, http.StatusConflict)
		return
	}

	s.mu.RLock()
	grants, granted := s.grants[callerKey]
	handler, served := s.handlers[req.Method]
	s.mu.RUnlock()

	if !granted || !grants.Allows(req.Method) {
		// Deliberately the same message either way: telling an unknown caller which
		// methods exist is free reconnaissance.
		s.fail(w, req.Nonce, &Error{Code: CodeForbidden, Message: fmt.Sprintf("caller is not permitted to call %q", req.Method)}, http.StatusForbidden)
		return
	}
	if !served {
		s.fail(w, req.Nonce, &Error{Code: CodeUnknownMethod, Message: fmt.Sprintf("method %q has no handler", req.Method)}, http.StatusNotImplemented)
		return
	}

	result, err := handler(caller, req.Params)
	if err != nil {
		var se *Error
		if !errors.As(err, &se) {
			se = &Error{Code: CodeInternal, Message: err.Error()}
		}
		s.logger.Debug("method failed", "method", req.Method, "code", se.Code, "error", se.Message)
		s.fail(w, req.Nonce, se, statusFor(se.Code))
		return
	}

	var encoded json.RawMessage
	if result != nil {
		encoded, err = json.Marshal(result)
		if err != nil {
			s.fail(w, req.Nonce, &Error{Code: CodeInternal, Message: "could not encode the result"}, http.StatusInternalServerError)
			return
		}
	}
	s.respond(w, Response{RequestNonce: req.Nonce, Result: encoded}, http.StatusOK)
}

func (s *Server) fail(w http.ResponseWriter, nonce string, e *Error, status int) {
	s.respond(w, Response{RequestNonce: nonce, Error: e}, status)
}

func (s *Server) respond(w http.ResponseWriter, resp Response, status int) {
	if err := SignResponse(&resp, s.wallet); err != nil {
		// Without a signature the caller cannot authenticate the endpoint, so an
		// unsigned response is worse than none.
		s.logger.Error("could not sign the response", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("could not encode the response", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		s.logger.Debug("could not write the response", "error", err)
	}
}

func statusFor(c Code) int {
	switch c {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnauthenticated, CodeExpired:
		return http.StatusUnauthorized
	case CodeForbidden, CodeDeclined:
		return http.StatusForbidden
	case CodeUnknownMethod:
		return http.StatusNotImplemented
	case CodeReplayed:
		return http.StatusConflict
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusInternalServerError
	}
}

// rateLimiter bounds each caller independently, so one flooding caller cannot deny service
// to the others.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
