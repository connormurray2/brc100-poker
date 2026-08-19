package substrate

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// approveAll is only ever appropriate in a test: a real deployment must ask the player.
func approveAll() Approver { return ApproverFunc(func(SigningRequest) error { return nil }) }

type testEnv struct {
	srv      *Server
	url      string
	wallet   *ec.PrivateKey
	audience string
}

func newEnv(t *testing.T, approver Approver, rpm int) testEnv {
	t.Helper()
	wallet := newKey(t)
	srv, err := NewServer(Config{
		Wallet:            wallet,
		Approver:          approver,
		RequestsPerMinute: rpm,
		Logger:            quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.HandleMethod(MethodGetPublicKey, func(caller *ec.PublicKey, _ json.RawMessage) (any, error) {
		return map[string]string{"publicKey": wallet.PubKey().ToDERHex()}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return testEnv{srv: srv, url: ts.URL, wallet: wallet, audience: srv.Audience()}
}

func call(t *testing.T, env testEnv, caller *ec.PrivateKey, method Method, params any) (*Response, int) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	req := Request{Method: method, Originator: "table.poker.local", Params: raw}
	if err := SignRequest(&req, caller, env.audience); err != nil {
		t.Fatal(err)
	}
	return post(t, env.url, req)
}

func post(t *testing.T, url string, req Request) (*Response, int) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	// The URL is this test's own httptest server.
	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // test-local URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		return nil, resp.StatusCode
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", resp.StatusCode, raw)
	}
	return &out, resp.StatusCode
}

func TestGrantedCallSucceedsAndResponseIsAuthenticated(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}

	req := Request{Method: MethodGetPublicKey, Originator: "owner.poker.local"}
	if err := SignRequest(&req, caller, env.audience); err != nil {
		t.Fatal(err)
	}
	resp, status := post(t, env.url, req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, error = %+v", status, resp.Error)
	}
	// The caller must be able to prove which wallet answered.
	if err := VerifyResponse(*resp, env.audience, req.Nonce); err != nil {
		t.Fatalf("the response did not authenticate: %v", err)
	}
}

// An ungranted caller is refused even though it authenticated correctly.
func TestUngrantedCallerIsForbidden(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	stranger := newKey(t)

	resp, status := call(t, env, stranger, MethodGetPublicKey, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if resp.Error == nil || resp.Error.Code != CodeForbidden {
		t.Fatalf("error = %+v, want forbidden", resp.Error)
	}
}

// The least-privilege property, end to end: a table cannot enumerate a player's wallet.
func TestTableCannotCallListOutputsOverTheWire(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	table := newKey(t)
	if err := env.srv.Grant(table.PubKey().ToDERHex(), TableGrants()); err != nil {
		t.Fatal(err)
	}
	if err := env.srv.HandleMethod(MethodListOutputs, func(*ec.PublicKey, json.RawMessage) (any, error) {
		t.Error("the listOutputs handler ran for a table caller")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	resp, status := call(t, env, table, MethodListOutputs, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if resp.Error.Code != CodeForbidden {
		t.Errorf("code = %q, want forbidden", resp.Error.Code)
	}
}

func TestUnsignedRequestIsRejectedOverTheWire(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}

	// A fully-formed request claiming a granted identity, but with no signature.
	req := Request{
		Version:       Version,
		Method:        MethodGetPublicKey,
		Originator:    "owner.poker.local",
		IdentityKey:   caller.PubKey().ToDERHex(),
		Nonce:         "n-unsigned",
		TimestampUnix: time.Now().Unix(),
		Audience:      env.audience,
	}
	resp, status := post(t, env.url, req)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if resp.Error.Code != CodeUnauthenticated {
		t.Errorf("code = %q, want unauthenticated", resp.Error.Code)
	}
}

func TestReplayedRequestIsRejectedOverTheWire(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}

	req := Request{Method: MethodGetPublicKey, Originator: "owner.poker.local"}
	if err := SignRequest(&req, caller, env.audience); err != nil {
		t.Fatal(err)
	}
	if _, status := post(t, env.url, req); status != http.StatusOK {
		t.Fatalf("first call status = %d", status)
	}
	resp, status := post(t, env.url, req)
	if status != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409", status)
	}
	if resp.Error.Code != CodeReplayed {
		t.Errorf("code = %q, want replayed", resp.Error.Code)
	}
}

// Consent: the wallet must not sign without the player's approval for that request.
func TestDeclinedSigningRequestProducesNoSignature(t *testing.T) {
	var signed atomic.Bool
	declining := ApproverFunc(func(SigningRequest) error {
		return &Error{Code: CodeDeclined, Message: "the player declined"}
	})
	env := newEnv(t, declining, 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}
	if err := env.srv.HandleMethod(MethodSignPot, func(*ec.PublicKey, json.RawMessage) (any, error) {
		if err := env.srv.Approve(SigningRequest{HandID: "h1", Purpose: "pot settlement"}); err != nil {
			return nil, err
		}
		signed.Store(true)
		return map[string]string{"signature": "deadbeef"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	resp, status := call(t, env, caller, MethodSignPot, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if resp.Error.Code != CodeDeclined {
		t.Errorf("code = %q, want declined", resp.Error.Code)
	}
	if signed.Load() {
		t.Fatal("a signature was produced despite the player declining")
	}
}

// The approver must see the material terms, or the prompt is a rubber stamp.
func TestApproverSeesTheMaterialTerms(t *testing.T) {
	var seen SigningRequest
	capturing := ApproverFunc(func(r SigningRequest) error {
		seen = r
		return nil
	})
	env := newEnv(t, capturing, 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}

	want := SigningRequest{
		HandID:      "hand-42",
		Purpose:     "pot settlement",
		PotOutpoint: "abcd:0",
		PotSatoshis: 5000,
		FeeSatoshis: 200,
		Outputs: []SigningOutput{
			{Satoshis: 4800, LockingScript: "76a914aa88ac", Description: "you"},
		},
	}
	if err := env.srv.HandleMethod(MethodSignPot, func(*ec.PublicKey, json.RawMessage) (any, error) {
		if err := env.srv.Approve(want); err != nil {
			return nil, err
		}
		return map[string]bool{"signed": true}, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, status := call(t, env, caller, MethodSignPot, nil); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if seen.HandID != want.HandID || seen.PotSatoshis != want.PotSatoshis || seen.FeeSatoshis != want.FeeSatoshis {
		t.Errorf("the approver did not see the material terms: %+v", seen)
	}
	if len(seen.Outputs) != 1 || seen.Outputs[0].Satoshis != 4800 {
		t.Errorf("the approver did not see the outputs: %+v", seen.Outputs)
	}
}

// One approval must not authorise a second signature: a replayed request is refused before
// it ever reaches the approver again.
func TestApprovalDoesNotAuthoriseASecondSignature(t *testing.T) {
	var approvals atomic.Int64
	counting := ApproverFunc(func(SigningRequest) error {
		approvals.Add(1)
		return nil
	})
	env := newEnv(t, counting, 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}
	if err := env.srv.HandleMethod(MethodSignPot, func(*ec.PublicKey, json.RawMessage) (any, error) {
		if err := env.srv.Approve(SigningRequest{HandID: "h1"}); err != nil {
			return nil, err
		}
		return map[string]bool{"signed": true}, nil
	}); err != nil {
		t.Fatal(err)
	}

	req := Request{Method: MethodSignPot, Originator: "owner.poker.local"}
	if err := SignRequest(&req, caller, env.audience); err != nil {
		t.Fatal(err)
	}
	if _, status := post(t, env.url, req); status != http.StatusOK {
		t.Fatal("the first signing call failed")
	}
	if _, status := post(t, env.url, req); status != http.StatusConflict {
		t.Fatal("the replayed signing call was not refused")
	}
	if approvals.Load() != 1 {
		t.Fatalf("the approver ran %d times for one approval", approvals.Load())
	}
}

// A flooding caller must not deny service to another.
func TestRateLimitIsPerCaller(t *testing.T) {
	env := newEnv(t, approveAll(), 3)
	flooder, quietCaller := newKey(t), newKey(t)
	for _, k := range []*ec.PrivateKey{flooder, quietCaller} {
		if err := env.srv.Grant(k.PubKey().ToDERHex(), OwnerGrants()); err != nil {
			t.Fatal(err)
		}
	}

	limited := false
	for i := 0; i < 6; i++ {
		_, status := call(t, env, flooder, MethodGetPublicKey, nil)
		if status == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("a flooding caller was never rate limited")
	}
	// The other caller still gets served.
	if _, status := call(t, env, quietCaller, MethodGetPublicKey, nil); status != http.StatusOK {
		t.Fatalf("a quiet caller was denied service: status %d", status)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	huge := bytes.Repeat([]byte("a"), MaxBodyBytes+100)
	resp, err := http.Post(env.url, "application/json", bytes.NewReader(huge)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestTLSIsRequiredWhenConfigured(t *testing.T) {
	wallet := newKey(t)
	srv, err := NewServer(Config{Wallet: wallet, Approver: approveAll(), RequireTLS: true, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv) // plaintext
	defer ts.Close()

	caller := newKey(t)
	if err := srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}
	req := Request{Method: MethodGetPublicKey, Originator: "owner.poker.local"}
	if err := SignRequest(&req, caller, srv.Audience()); err != nil {
		t.Fatal(err)
	}
	resp, status := post(t, ts.URL, req)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a plaintext request", status)
	}
	if !strings.Contains(resp.Error.Message, "TLS") {
		t.Errorf("message does not mention TLS: %q", resp.Error.Message)
	}
}

func TestNonPostIsRefused(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	resp, err := http.Get(env.url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRevokeStopsAccess(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	caller := newKey(t)
	key := caller.PubKey().ToDERHex()
	if err := env.srv.Grant(key, OwnerGrants()); err != nil {
		t.Fatal(err)
	}
	if _, status := call(t, env, caller, MethodGetPublicKey, nil); status != http.StatusOK {
		t.Fatal("a granted call failed")
	}
	env.srv.Revoke(key)
	if _, status := call(t, env, caller, MethodGetPublicKey, nil); status != http.StatusForbidden {
		t.Fatal("a revoked caller was still served")
	}
}

// A method with no handler must not look like a permission failure, and vice versa.
func TestGrantedButUnhandledMethod(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	caller := newKey(t)
	if err := env.srv.Grant(caller.PubKey().ToDERHex(), OwnerGrants()); err != nil {
		t.Fatal(err)
	}
	resp, status := call(t, env, caller, MethodListActions, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", status)
	}
	if resp.Error.Code != CodeUnknownMethod {
		t.Errorf("code = %q", resp.Error.Code)
	}
}

func TestServerConfigValidation(t *testing.T) {
	if _, err := NewServer(Config{Approver: approveAll()}); err == nil {
		t.Error("built a server with no wallet key")
	}
	// The important one: no approver must be refused, not defaulted to approve-all.
	if _, err := NewServer(Config{Wallet: newKey(t)}); err == nil {
		t.Error("built a server with no approver; it would sign anything it is asked to")
	}

	srv, err := NewServer(Config{Wallet: newKey(t), Approver: approveAll(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Grant("", OwnerGrants()); err == nil {
		t.Error("granted an empty identity key")
	}
	if err := srv.Grant("not-a-key", OwnerGrants()); err == nil {
		t.Error("granted an invalid identity key")
	}
	if err := srv.HandleMethod(Method("drainWallet"), func(*ec.PublicKey, json.RawMessage) (any, error) { return nil, nil }); err == nil {
		t.Error("registered a handler for an unknown method")
	}
	if err := srv.HandleMethod(MethodGetPublicKey, nil); err == nil {
		t.Error("registered a nil handler")
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	resp, err := http.Post(env.url, "application/json", strings.NewReader("{not json")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Even a failure response must be signed, or a caller cannot tell a real refusal from an
// injected one.
func TestErrorResponsesAreSigned(t *testing.T) {
	env := newEnv(t, approveAll(), 0)
	stranger := newKey(t)
	resp, status := call(t, env, stranger, MethodGetPublicKey, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d", status)
	}
	if resp.Signature == "" {
		t.Fatal("an error response carried no signature")
	}
	if err := VerifyResponse(*resp, env.audience, resp.RequestNonce); err != nil {
		t.Fatalf("the error response did not authenticate: %v", err)
	}
}
