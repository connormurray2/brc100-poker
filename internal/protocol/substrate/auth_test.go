package substrate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func newKey(t *testing.T) *ec.PrivateKey {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signedRequest(t *testing.T, caller *ec.PrivateKey, audience string) Request {
	t.Helper()
	r := Request{
		Method:     MethodGetPublicKey,
		Originator: "table.poker.local",
		Params:     json.RawMessage(`{"identityKey":true}`),
	}
	if err := SignRequest(&r, caller, audience); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSignedRequestVerifies(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()
	r := signedRequest(t, caller, audience)

	pub, err := VerifyRequest(r, time.Now(), audience)
	if err != nil {
		t.Fatalf("a correctly signed request was refused: %v", err)
	}
	if pub.ToDERHex() != caller.PubKey().ToDERHex() {
		t.Error("verification returned the wrong caller key")
	}
}

// The failure this substrate exists to prevent: an identity claimed without proof. The
// toolbox's own storage API accepts exactly this.
func TestAssertedIdentityIsRejected(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()

	r := Request{
		Version:       Version,
		Method:        MethodGetPublicKey,
		Originator:    "table.poker.local",
		IdentityKey:   caller.PubKey().ToDERHex(), // claimed
		Nonce:         "abc123",
		TimestampUnix: time.Now().Unix(),
		Audience:      audience,
		// No signature: identity is asserted only.
	}
	_, err := VerifyRequest(r, time.Now(), audience)
	if err == nil {
		t.Fatal("an unsigned request was accepted; any caller could claim any identity")
	}
	var se *Error
	if !asError(err, &se) || se.Code != CodeUnauthenticated {
		t.Fatalf("code = %v, want unauthenticated", err)
	}
	if !strings.Contains(se.Message, "not proven") {
		t.Errorf("message does not explain the failure: %q", se.Message)
	}
}

// Signing one identity's request with another key must not authenticate the claimed one.
func TestImpersonationIsRejected(t *testing.T) {
	victim, imposter, wallet := newKey(t), newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()

	r := signedRequest(t, imposter, audience)
	// Claim the victim's identity while keeping the imposter's signature.
	r.IdentityKey = victim.PubKey().ToDERHex()

	if _, err := VerifyRequest(r, time.Now(), audience); err == nil {
		t.Fatal("a request claiming another identity was accepted")
	}
}

// Every field is covered by the digest, so any tampering must be detected.
func TestTamperedRequestIsRejected(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()

	tests := map[string]func(r *Request){
		"method":     func(r *Request) { r.Method = MethodSignPot },
		"originator": func(r *Request) { r.Originator = "evil.example.com" },
		"params":     func(r *Request) { r.Params = json.RawMessage(`{"identityKey":false}`) },
		"nonce":      func(r *Request) { r.Nonce = "tampered" },
		"timestamp":  func(r *Request) { r.TimestampUnix += 1 },
		"audience":   func(r *Request) { r.Audience = newKey(t).PubKey().ToDERHex() },
	}
	for name, tamper := range tests {
		t.Run(name, func(t *testing.T) {
			r := signedRequest(t, caller, audience)
			tamper(&r)
			if _, err := VerifyRequest(r, time.Now(), audience); err == nil {
				t.Fatalf("tampering with %s was not detected", name)
			}
		})
	}
}

// A request captured from one wallet must not work against another.
func TestRequestCannotBeReplayedAgainstADifferentWallet(t *testing.T) {
	caller := newKey(t)
	walletA, walletB := newKey(t), newKey(t)

	r := signedRequest(t, caller, walletA.PubKey().ToDERHex())
	if _, err := VerifyRequest(r, time.Now(), walletB.PubKey().ToDERHex()); err == nil {
		t.Fatal("a request addressed to one wallet was accepted by another")
	}
}

// A signature stays valid forever, so the nonce cache is what makes a request single-use.
func TestReplayedNonceIsRejected(t *testing.T) {
	c := NewNonceCache()
	now := time.Now()
	if !c.Use("nonce-1", now) {
		t.Fatal("the first use of a nonce was refused")
	}
	if c.Use("nonce-1", now) {
		t.Fatal("a replayed nonce was accepted")
	}
	if !c.Use("nonce-2", now) {
		t.Fatal("a distinct nonce was refused")
	}
	if c.Use("", now) {
		t.Fatal("an empty nonce was accepted")
	}
}

// A whole captured request, replayed verbatim, must be refused the second time.
func TestFullRequestReplayIsRejected(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()
	r := signedRequest(t, caller, audience)
	cache := NewNonceCache()
	now := time.Now()

	if _, err := VerifyRequest(r, now, audience); err != nil {
		t.Fatal(err)
	}
	if !cache.Use(r.Nonce, now) {
		t.Fatal("the first use was refused")
	}
	// The signature still verifies — that is the point — so only the cache stops it.
	if _, err := VerifyRequest(r, now, audience); err != nil {
		t.Fatalf("the captured request stopped verifying: %v", err)
	}
	if cache.Use(r.Nonce, now) {
		t.Fatal("the replayed request was accepted")
	}
}

func TestNonceCacheExpiresOldEntries(t *testing.T) {
	c := NewNonceCache()
	start := time.Now()
	if !c.Use("old", start) {
		t.Fatal("first use refused")
	}
	// Well past the retention window: the entry is swept, and the request would be
	// refused on its timestamp anyway.
	later := start.Add(MaxClockSkew * 3)
	if !c.Use("fresh", later) {
		t.Fatal("a fresh nonce was refused")
	}
	if c.Len() > 1 {
		t.Errorf("cache retains %d entries; expired ones were not swept", c.Len())
	}
}

func TestStaleTimestampIsRejected(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()
	r := signedRequest(t, caller, audience)

	for _, skew := range []time.Duration{MaxClockSkew + time.Minute, -(MaxClockSkew + time.Minute)} {
		_, err := VerifyRequest(r, time.Unix(r.TimestampUnix, 0).Add(skew), audience)
		if err == nil {
			t.Fatalf("a request %s out of date was accepted", skew)
		}
		var se *Error
		if !asError(err, &se) || se.Code != CodeExpired {
			t.Errorf("code = %v, want expired", err)
		}
	}
}

func TestUnknownMethodIsRejected(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()
	r := Request{Method: Method("drainWallet"), Originator: "x.local"}
	if err := SignRequest(&r, caller, audience); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyRequest(r, time.Now(), audience)
	if err == nil {
		t.Fatal("an unknown method was accepted")
	}
	var se *Error
	if !asError(err, &se) || se.Code != CodeUnknownMethod {
		t.Errorf("code = %v, want unknown_method", err)
	}
}

func TestVersionMismatchIsRejected(t *testing.T) {
	caller, wallet := newKey(t), newKey(t)
	audience := wallet.PubKey().ToDERHex()
	r := signedRequest(t, caller, audience)
	r.Version = "brc100-substrate/0"
	if _, err := VerifyRequest(r, time.Now(), audience); err == nil {
		t.Fatal("a mismatched protocol version was accepted; signing authority must not silently downgrade")
	}
}

func TestOriginatorValidation(t *testing.T) {
	good := []string{"table.poker.local", "poker.example.com", "a.b.c.d"}
	for _, o := range good {
		if err := ValidateOriginator(o); err != nil {
			t.Errorf("ValidateOriginator(%q) = %v", o, err)
		}
	}
	bad := []string{"", "nodomain", "has space.com", "sl/ash.com", "co:lon.com", strings.Repeat("a", 300) + ".com"}
	for _, o := range bad {
		if err := ValidateOriginator(o); err == nil {
			t.Errorf("ValidateOriginator(%q) accepted an invalid originator", o)
		}
	}
}

// --- responses -------------------------------------------------------------

func TestSignedResponseVerifies(t *testing.T) {
	wallet := newKey(t)
	resp := Response{RequestNonce: "n1", Result: json.RawMessage(`{"ok":true}`)}
	if err := SignResponse(&resp, wallet); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(resp, wallet.PubKey().ToDERHex(), "n1"); err != nil {
		t.Fatalf("a correctly signed response was refused: %v", err)
	}
}

// A caller must be able to detect a substituted endpoint, which TLS alone would not reveal.
func TestSubstitutedEndpointIsDetected(t *testing.T) {
	real, imposter := newKey(t), newKey(t)
	resp := Response{RequestNonce: "n1", Result: json.RawMessage(`{"ok":true}`)}
	if err := SignResponse(&resp, imposter); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(resp, real.PubKey().ToDERHex(), "n1"); err == nil {
		t.Fatal("a response from a different wallet was accepted")
	}
}

func TestTamperedResponseIsRejected(t *testing.T) {
	wallet := newKey(t)
	key := wallet.PubKey().ToDERHex()

	tests := map[string]func(r *Response){
		"result": func(r *Response) { r.Result = json.RawMessage(`{"ok":false}`) },
		"error":  func(r *Response) { r.Error = &Error{Code: CodeInternal, Message: "injected"} },
		"nonce":  func(r *Response) { r.RequestNonce = "n2" },
	}
	for name, tamper := range tests {
		t.Run(name, func(t *testing.T) {
			resp := Response{RequestNonce: "n1", Result: json.RawMessage(`{"ok":true}`)}
			if err := SignResponse(&resp, wallet); err != nil {
				t.Fatal(err)
			}
			tamper(&resp)
			nonce := "n1"
			if name == "nonce" {
				nonce = "n2"
			}
			if err := VerifyResponse(resp, key, nonce); err == nil {
				t.Fatalf("tampering with the response %s was not detected", name)
			}
		})
	}
}

// A response must be tied to exactly one request, so an answer cannot be shifted onto a
// different call.
func TestResponseIsBoundToItsRequest(t *testing.T) {
	wallet := newKey(t)
	resp := Response{RequestNonce: "n1", Result: json.RawMessage(`{"ok":true}`)}
	if err := SignResponse(&resp, wallet); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(resp, wallet.PubKey().ToDERHex(), "different-nonce"); err == nil {
		t.Fatal("a response was accepted for the wrong request")
	}
}

func TestUnsignedResponseIsRejected(t *testing.T) {
	wallet := newKey(t)
	resp := Response{Version: Version, RequestNonce: "n1", IdentityKey: wallet.PubKey().ToDERHex()}
	if err := VerifyResponse(resp, wallet.PubKey().ToDERHex(), "n1"); err == nil {
		t.Fatal("an unsigned response was accepted")
	}
}

// --- grants ----------------------------------------------------------------

// A table must not be able to enumerate a player's wallet.
func TestTableCannotEnumerateAPlayersWallet(t *testing.T) {
	g := TableGrants()
	for _, m := range []Method{MethodListOutputs, MethodListActions} {
		if g.Allows(m) {
			t.Errorf("a table is granted %q; it has no business enumerating a player's coins", m)
		}
	}
	// It also must not be able to make the wallet spend on its own.
	for _, m := range []Method{MethodCreateAction, MethodSignAction} {
		if g.Allows(m) {
			t.Errorf("a table is granted %q", m)
		}
	}
	// What it does need.
	for _, m := range []Method{MethodGetPublicKey, MethodSignPot} {
		if !g.Allows(m) {
			t.Errorf("a table is not granted %q, which it needs", m)
		}
	}
}

func TestOwnerHasFullAccess(t *testing.T) {
	g := OwnerGrants()
	for _, m := range []Method{
		MethodGetPublicKey, MethodGetNetwork, MethodCreateAction, MethodSignAction,
		MethodInternalizeAction, MethodListOutputs, MethodListActions, MethodSignPot,
	} {
		if !g.Allows(m) {
			t.Errorf("the owner is not granted %q", m)
		}
	}
}

func TestGrantsRejectUnknownMethods(t *testing.T) {
	if _, err := NewGrants(Method("drainWallet")); err == nil {
		t.Fatal("an unknown method was granted")
	}
	var zero Grants
	if zero.Allows(MethodGetPublicKey) {
		t.Error("a zero-value grant set allows a method; it must deny by default")
	}
}

func TestGrantsMethodsAreStablyOrdered(t *testing.T) {
	g := OwnerGrants()
	first := g.Methods()
	for i := 0; i < 5; i++ {
		again := g.Methods()
		if len(again) != len(first) {
			t.Fatal("method count varies between calls")
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatal("method order varies between calls")
			}
		}
	}
}

func TestSignValidation(t *testing.T) {
	k := newKey(t)
	if err := SignRequest(nil, k, "aud"); err == nil {
		t.Error("signed a nil request")
	}
	r := Request{Method: MethodGetPublicKey, Originator: "x.local"}
	if err := SignRequest(&r, nil, "aud"); err == nil {
		t.Error("signed with a nil key")
	}
	if err := SignRequest(&r, k, ""); err == nil {
		t.Error("signed without an audience")
	}
	if err := SignResponse(nil, k); err == nil {
		t.Error("signed a nil response")
	}
	resp := Response{}
	if err := SignResponse(&resp, nil); err == nil {
		t.Error("signed a response with a nil key")
	}
}

// asError is errors.As specialised to *Error, kept local so the test reads plainly. It uses
// errors.As rather than a type assertion so it keeps working if these errors are ever
// wrapped on the way out.
func asError(err error, target **Error) bool {
	return errors.As(err, target)
}
