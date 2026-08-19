package brc100

import (
	"fmt"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/defs"
)

// BRC-100 defines exactly two network values. Anything else is invalid on the wire.
const (
	BRC100Mainnet = "mainnet"
	BRC100Testnet = "testnet"
)

// TranslateNetwork maps a toolbox-internal network name to a valid BRC-100 value.
//
// This exists because the wallet's GetNetwork casts its internal chain name straight to
// an sdk.Network without mapping it. That returns "main"/"test" on the standard networks
// and the outright invalid "ttn" on teratestnet — a value go-sdk's own NetworkFromString
// rejects. The library's conformance test asserts the unmapped behaviour, so nothing
// upstream will catch it for us.
//
// Teratestnet is testnet-based, so it presents as "testnet".
func TranslateNetwork(internal defs.BSVNetwork) (string, error) {
	switch internal {
	case defs.NetworkMainnet:
		return BRC100Mainnet, nil
	case defs.NetworkTestnet, defs.NetworkTTN, defs.NetworkTSTN:
		return BRC100Testnet, nil
	default:
		return "", fmt.Errorf("brc100: unknown internal network %q", internal)
	}
}

// TranslateSDKNetwork maps whatever GetNetwork returned into a valid BRC-100 value.
//
// Accepts both the internal names the library actually emits and the correct BRC-100
// values, so it is safe to apply to the result of a GetNetwork call unconditionally.
func TranslateSDKNetwork(n sdk.Network) (string, error) {
	switch string(n) {
	case BRC100Mainnet, string(defs.NetworkMainnet):
		return BRC100Mainnet, nil
	case BRC100Testnet, string(defs.NetworkTestnet), string(defs.NetworkTTN), string(defs.NetworkTSTN):
		return BRC100Testnet, nil
	default:
		return "", fmt.Errorf("brc100: cannot translate network %q to a BRC-100 value", n)
	}
}
