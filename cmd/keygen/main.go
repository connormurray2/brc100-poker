// Command keygen creates a development key for this project and prints the values needed
// to fund it on teratestnet.
//
// The private key is written to a file, never to stdout, so it cannot end up in a
// terminal transcript or CI log. Teratestnet coins are worthless, but the habit matters:
// the same tool shape should not be safe on ttn and dangerous on mainnet.
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
)

func main() {
	out := flag.String("out", "", "path to write the private key to (required)")
	force := flag.Bool("force", false, "overwrite an existing key file")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "keygen: -out is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*out, *force); err != nil {
		fmt.Fprintf(os.Stderr, "keygen: %v\n", err)
		os.Exit(1)
	}
}

func run(path string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; refusing to overwrite a key without -force", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", path, err)
	}

	priv, err := ec.NewPrivateKey()
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	// 0600: readable only by the owner.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(priv.Serialize())+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing key: %w", err)
	}

	pub := priv.PubKey()

	// false => testnet version byte. Teratestnet is testnet-based, so addresses and key
	// parameters are the testnet ones.
	addr, err := script.NewAddressFromPublicKey(pub, false)
	if err != nil {
		return fmt.Errorf("deriving address: %w", err)
	}

	fmt.Printf("key file:      %s (mode 0600, not printed)\n", path)
	fmt.Printf("identity key:  %s\n", hex.EncodeToString(pub.Compressed()))
	fmt.Printf("address (ttn): %s\n", addr.AddressString)
	return nil
}
