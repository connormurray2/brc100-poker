package brc100

import (
	"encoding/hex"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// identityKeyFromPrivateHex derives the identity public key (DER hex) from a private key.
//
// The identity key scopes the arcade SSE stream and names the storage user, so it must be
// derived the same way on every start: a token derived from a different key silently
// subscribes to the wrong stream.
func identityKeyFromPrivateHex(privHex string) (string, error) {
	raw, err := hex.DecodeString(privHex)
	if err != nil {
		return "", fmt.Errorf("private key is not valid hex: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("private key must be 32 bytes, got %d", len(raw))
	}
	// Validate the scalar ourselves. PrivateKeyFromBytes accepts a zero key and
	// silently reduces out-of-range values, either of which would give us a wallet
	// whose identity is not the one the caller thinks it configured.
	d := new(big.Int).SetBytes(raw)
	if d.Sign() == 0 {
		return "", fmt.Errorf("private key is zero, which is not a valid secp256k1 scalar")
	}
	if d.Cmp(ec.S256().N) >= 0 {
		return "", fmt.Errorf("private key is not less than the curve order")
	}

	priv, _ := ec.PrivateKeyFromBytes(raw)
	if priv == nil {
		return "", fmt.Errorf("private key is not a valid secp256k1 scalar")
	}
	return hex.EncodeToString(priv.PubKey().Compressed()), nil
}
