package brc100

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// The faucet returns derivation material as strings; the internalize call needs raw bytes.
// Accept either encoding so a change on either side cannot silently yield wrong bytes.
func TestDecodeDerivationAcceptsBase64AndHex(t *testing.T) {
	want := []byte("invoice-2026-08-19")

	b64 := base64.StdEncoding.EncodeToString(want)
	got, err := decodeDerivation(b64)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("base64 decoded to %q, want %q", got, want)
	}

	// Hex of bytes that are not valid base64 padding-wise, to exercise the fallback.
	raw := []byte{0xde, 0xad, 0xbe, 0xef, 0x01}
	got, err = decodeDerivation(hex.EncodeToString(raw))
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("hex decoded to %x, want %x", got, raw)
	}
}

func TestDecodeDerivationRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "not!valid!!", "zzzz zzzz"} {
		if _, err := decodeDerivation(s); err == nil {
			t.Errorf("decodeDerivation(%q) succeeded, want an error", s)
		}
	}
}

func TestFaucetURLIsTheLiveHost(t *testing.T) {
	// The faucet's own docs show faucet.teratestnet.org, which does not resolve.
	if FaucetURL != "https://faucet-ttn.bsvblockchain.tech" {
		t.Errorf("FaucetURL = %q; the live host is faucet-ttn.bsvblockchain.tech", FaucetURL)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate long = %q", got)
	}
}
