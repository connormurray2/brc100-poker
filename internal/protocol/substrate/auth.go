package substrate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// NewNonce returns a fresh request nonce.
func NewNonce() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("substrate: reading random source: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SignRequest fills in the caller's identity and signature.
//
// The audience is the wallet's identity key, so a request captured from one wallet cannot be
// replayed against a different one that happens to grant the same caller.
func SignRequest(r *Request, caller *ec.PrivateKey, audience string) error {
	if r == nil {
		return errors.New("substrate: no request to sign")
	}
	if caller == nil {
		return errors.New("substrate: no key to sign with")
	}
	if audience == "" {
		return errors.New("substrate: an audience is required so a request cannot be replayed elsewhere")
	}

	r.Version = Version
	r.IdentityKey = caller.PubKey().ToDERHex()
	r.Audience = audience
	if r.Nonce == "" {
		n, err := NewNonce()
		if err != nil {
			return err
		}
		r.Nonce = n
	}
	if r.TimestampUnix == 0 {
		r.TimestampUnix = time.Now().Unix()
	}

	sig, err := caller.Sign(requestDigest(*r))
	if err != nil {
		return fmt.Errorf("substrate: signing the request: %w", err)
	}
	r.Signature = hex.EncodeToString(sig.Serialize())
	return nil
}

// SignResponse fills in the wallet's identity and signature.
func SignResponse(r *Response, wallet *ec.PrivateKey) error {
	if r == nil {
		return errors.New("substrate: no response to sign")
	}
	if wallet == nil {
		return errors.New("substrate: no key to sign with")
	}
	r.Version = Version
	r.IdentityKey = wallet.PubKey().ToDERHex()

	sig, err := wallet.Sign(responseDigest(*r))
	if err != nil {
		return fmt.Errorf("substrate: signing the response: %w", err)
	}
	r.Signature = hex.EncodeToString(sig.Serialize())
	return nil
}

// VerifyRequest proves the caller controls the identity it claims.
//
// This is the check that separates this substrate from the toolbox's storage API, where any
// caller can claim any identity by setting a header.
func VerifyRequest(r Request, now time.Time, audience string) (*ec.PublicKey, error) {
	if r.Version != Version {
		return nil, &Error{Code: CodeBadRequest, Message: fmt.Sprintf("unsupported version %q, this wallet speaks %q", r.Version, Version)}
	}
	if !r.Method.Known() {
		return nil, &Error{Code: CodeUnknownMethod, Message: fmt.Sprintf("method %q is not served", r.Method)}
	}
	if err := ValidateOriginator(r.Originator); err != nil {
		return nil, &Error{Code: CodeBadRequest, Message: err.Error()}
	}
	if r.Nonce == "" {
		return nil, &Error{Code: CodeBadRequest, Message: "a nonce is required"}
	}
	if r.IdentityKey == "" {
		return nil, &Error{Code: CodeUnauthenticated, Message: "no identity key was presented"}
	}
	if r.Signature == "" {
		// An identity without a proof is exactly the failure mode this substrate
		// exists to prevent, so it is called out specifically.
		return nil, &Error{Code: CodeUnauthenticated, Message: "identity was asserted but not proven: no signature"}
	}
	if r.Audience == "" {
		return nil, &Error{Code: CodeBadRequest, Message: "an audience is required"}
	}
	if audience != "" && r.Audience != audience {
		return nil, &Error{Code: CodeUnauthenticated, Message: "the request is addressed to a different wallet"}
	}

	skew := now.Sub(time.Unix(r.TimestampUnix, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return nil, &Error{Code: CodeExpired, Message: fmt.Sprintf("timestamp is %s from now, outside the %s window", skew.Round(time.Second), MaxClockSkew)}
	}

	pub, err := ec.PublicKeyFromString(r.IdentityKey)
	if err != nil {
		return nil, &Error{Code: CodeUnauthenticated, Message: fmt.Sprintf("identity key is not a valid public key: %v", err)}
	}
	raw, err := hex.DecodeString(r.Signature)
	if err != nil {
		return nil, &Error{Code: CodeUnauthenticated, Message: "signature is not valid hex"}
	}
	sig, err := ec.ParseDERSignature(raw)
	if err != nil {
		return nil, &Error{Code: CodeUnauthenticated, Message: fmt.Sprintf("signature is not parseable: %v", err)}
	}
	if !sig.Verify(requestDigest(r), pub) {
		// Covers both an outright forgery and any tampering in transit, since the
		// digest covers every field.
		return nil, &Error{Code: CodeUnauthenticated, Message: "signature does not verify: the request was forged or altered"}
	}
	return pub, nil
}

// VerifyResponse proves the response came from the wallet the caller intended.
//
// Without this a caller cannot distinguish the real wallet from a substituted endpoint, and
// TLS alone would not tell it which *key* answered.
func VerifyResponse(r Response, expectWallet string, requestNonce string) error {
	if r.Version != Version {
		return fmt.Errorf("substrate: unsupported response version %q", r.Version)
	}
	if r.RequestNonce != requestNonce {
		return fmt.Errorf("substrate: response is for nonce %q, expected %q", r.RequestNonce, requestNonce)
	}
	if expectWallet != "" && r.IdentityKey != expectWallet {
		return fmt.Errorf("substrate: response came from %s, expected %s", r.IdentityKey, expectWallet)
	}
	if r.Signature == "" {
		return errors.New("substrate: response carries no signature; the endpoint cannot be authenticated")
	}

	pub, err := ec.PublicKeyFromString(r.IdentityKey)
	if err != nil {
		return fmt.Errorf("substrate: response identity key is invalid: %w", err)
	}
	raw, err := hex.DecodeString(r.Signature)
	if err != nil {
		return errors.New("substrate: response signature is not valid hex")
	}
	sig, err := ec.ParseDERSignature(raw)
	if err != nil {
		return fmt.Errorf("substrate: response signature is not parseable: %w", err)
	}
	if !sig.Verify(responseDigest(r), pub) {
		return errors.New("substrate: response signature does not verify")
	}
	return nil
}

// NonceCache rejects replayed requests.
//
// The signature makes a request unforgeable but perfectly replayable — it stays valid
// forever. This is what makes a captured request single-use. Entries are retained for the
// skew window plus a margin, since a request older than that is refused on its timestamp
// anyway.
type NonceCache struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	ttl   time.Duration
	limit int
}

// NewNonceCache returns a cache retaining nonces for the skew window.
func NewNonceCache() *NonceCache {
	return &NonceCache{
		seen:  make(map[string]time.Time),
		ttl:   MaxClockSkew * 2,
		limit: 100_000,
	}
}

// Use records a nonce, reporting false if it has been seen before.
func (c *NonceCache) Use(nonce string, now time.Time) bool {
	if nonce == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Sweep expired entries. Cheap because the map only ever holds one skew window of
	// traffic, and it keeps the cache from growing without bound.
	if len(c.seen) > 0 {
		for k, t := range c.seen {
			if now.Sub(t) > c.ttl {
				delete(c.seen, k)
			}
		}
	}
	if _, dup := c.seen[nonce]; dup {
		return false
	}
	// A hard cap in case the sweep cannot keep up under a flood. Refusing is safe:
	// the caller retries with a fresh nonce.
	if len(c.seen) >= c.limit {
		return false
	}
	c.seen[nonce] = now
	return true
}

// Len reports how many nonces are retained.
func (c *NonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
