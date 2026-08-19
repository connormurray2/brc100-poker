package brc100

import (
	"strings"
	"testing"

	"github.com/galt-tr/go-arcade-toolbox/pkg/wdk"
)

func TestIdentityKeyFromPrivateHex(t *testing.T) {
	// Private key 1 -> the secp256k1 generator point, compressed.
	const privOne = "0000000000000000000000000000000000000000000000000000000000000001"
	const wantG = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

	got, err := identityKeyFromPrivateHex(privOne)
	if err != nil {
		t.Fatalf("identityKeyFromPrivateHex: %v", err)
	}
	if got != wantG {
		t.Fatalf("identity key = %s, want %s", got, wantG)
	}
}

func TestIdentityKeyRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"empty":     "",
		"not hex":   "zzzz",
		"too short": "0001",
		"zero":      "0000000000000000000000000000000000000000000000000000000000000000",
		// The curve order N itself, and N+1: both out of range.
		"order":      "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141",
		"past order": "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364142",
		"too long":   "00000000000000000000000000000000000000000000000000000000000000000102",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := identityKeyFromPrivateHex(in); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// The callback token must be non-empty and stable: it scopes the SSE stream and its
// Last-Event-ID replay across restarts.
func TestCallbackTokenIsStableAndNonEmpty(t *testing.T) {
	key, err := identityKeyFromPrivateHex("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	a := wdk.DeriveArcadeCallbackToken(key)
	b := wdk.DeriveArcadeCallbackToken(key)
	if a == "" {
		t.Fatal("callback token is empty; arcade would drop our events")
	}
	if a != b {
		t.Fatalf("callback token is not deterministic: %q vs %q", a, b)
	}

	other, err := identityKeyFromPrivateHex("0000000000000000000000000000000000000000000000000000000000000002")
	if err != nil {
		t.Fatal(err)
	}
	if wdk.DeriveArcadeCallbackToken(other) == a {
		t.Fatal("different identity keys produced the same callback token")
	}
	if strings.Contains(a, key) {
		t.Fatal("callback token leaks the identity key verbatim")
	}
}
