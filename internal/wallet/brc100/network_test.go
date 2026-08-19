package brc100

import (
	"context"
	"path/filepath"
	"testing"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/defs"
)

func TestTranslateNetwork(t *testing.T) {
	tests := map[defs.BSVNetwork]string{
		defs.NetworkMainnet: BRC100Mainnet,
		defs.NetworkTestnet: BRC100Testnet,
		defs.NetworkTTN:     BRC100Testnet,
		defs.NetworkTSTN:    BRC100Testnet,
	}
	for in, want := range tests {
		got, err := TranslateNetwork(in)
		if err != nil {
			t.Errorf("TranslateNetwork(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("TranslateNetwork(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := TranslateNetwork("nonsense"); err == nil {
		t.Error("expected an error for an unknown network")
	}
}

func TestTranslateSDKNetworkAcceptsInternalNames(t *testing.T) {
	for _, in := range []string{"main", "test", "ttn", "tstn", "mainnet", "testnet"} {
		if _, err := TranslateSDKNetwork(sdk.Network(in)); err != nil {
			t.Errorf("TranslateSDKNetwork(%q): %v", in, err)
		}
	}
	if _, err := TranslateSDKNetwork(sdk.Network("regtest")); err == nil {
		t.Error("expected an error for an untranslatable network")
	}
}

// Documents the upstream bug this translation exists to contain: on teratestnet the
// wallet's own GetNetwork returns a value that is not valid under BRC-100.
func TestWalletGetNetworkNeedsTranslation(t *testing.T) {
	ctx := context.Background()
	w, err := New(ctx, Options{
		Backend:       BackendSQLite,
		SQLitePath:    filepath.Join(t.TempDir(), "wallet.db"),
		StorageName:   "net-test",
		PrivateKeyHex: "0000000000000000000000000000000000000000000000000000000000000001",
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })

	got, err := w.Wallet.GetNetwork(ctx, nil, "poker.test")
	if err != nil {
		t.Fatalf("GetNetwork: %v", err)
	}
	raw := string(got.Network)
	t.Logf("wallet GetNetwork returned %q", raw)

	if raw == BRC100Mainnet || raw == BRC100Testnet {
		t.Logf("upstream now returns a valid BRC-100 value; translation is a no-op")
	}

	translated, err := TranslateSDKNetwork(got.Network)
	if err != nil {
		t.Fatalf("TranslateSDKNetwork(%q): %v", raw, err)
	}
	if translated != BRC100Testnet {
		t.Errorf("translated network = %q, want %q", translated, BRC100Testnet)
	}
}
