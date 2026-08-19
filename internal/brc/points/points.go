// Package points is the reference implementation of the BRC point-operations proposal.
//
// It implements derivePoint and multiplyPoint: a wallet-side primitive letting an application
// perform scalar multiplication on a caller-supplied secp256k1 point using a derived key the wallet
// never discloses. See docs/proposals/brc-point-operations.md.
//
// The security argument rests on one thing: the key is derived from a protocol identifier and never
// signs a transaction. A wallet that implemented this over a spending key would have built a genuine
// static-ECDH oracle, so the derivation is mandatory rather than advisory.
package points

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// SecurityLevel mirrors BRC-100's protocol security levels.
type SecurityLevel int

const (
	// LevelSilent permits use without per-call user interaction.
	LevelSilent SecurityLevel = 0
	// LevelApp scopes a key to the calling application.
	LevelApp SecurityLevel = 1
	// LevelAppAndCounterparty scopes it to the application and counterparty both.
	LevelAppAndCounterparty SecurityLevel = 2
)

// Valid reports whether the level is one BRC-100 defines.
func (l SecurityLevel) Valid() bool { return l >= LevelSilent && l <= LevelAppAndCounterparty }

// Protocol identifies the protocol a key is derived for.
//
// This is the field that makes the primitive safe: a key derived for "mental-poker-deal" is not the
// key that signs transactions, so there is no signature equation for an oracle to attack.
type Protocol struct {
	Level SecurityLevel
	Name  string
}

// Validate checks the protocol identifier against BRC-100's bounds.
func (p Protocol) Validate() error {
	if !p.Level.Valid() {
		return fmt.Errorf("points: security level %d is not 0, 1 or 2", p.Level)
	}
	if len(p.Name) < 5 || len(p.Name) > 400 {
		return fmt.Errorf("points: protocol name must be 5..400 bytes, got %d", len(p.Name))
	}
	return nil
}

// PointSize is the byte length of a compressed secp256k1 point.
const PointSize = 33

// Wallet holds a root key and derives protocol keys from it.
//
// Deliberately separate from any spending wallet: this type cannot build or sign a transaction, so
// the separation the proposal requires is structural rather than a matter of discipline.
type Wallet struct {
	root *ec.PrivateKey
}

// NewWallet builds a point-operations wallet over a root key.
func NewWallet(root *ec.PrivateKey) (*Wallet, error) {
	if root == nil {
		return nil, errors.New("points: a root key is required")
	}
	return &Wallet{root: root}, nil
}

// DeriveArgs parameterises a derivation.
type DeriveArgs struct {
	Protocol Protocol
	KeyID    string
	// Counterparty scopes the derivation. Empty means "self".
	Counterparty string
}

func (a DeriveArgs) validate() error {
	if err := a.Protocol.Validate(); err != nil {
		return err
	}
	if a.KeyID == "" || len(a.KeyID) > 800 {
		return fmt.Errorf("points: key id must be 1..800 bytes, got %d", len(a.KeyID))
	}
	if a.Counterparty != "" {
		if _, err := ec.PublicKeyFromString(a.Counterparty); err != nil {
			return fmt.Errorf("points: counterparty is not a valid public key: %w", err)
		}
	}
	return nil
}

// DerivePoint returns the public point of the derived key.
//
// An application publishes this before a protocol run so counterparties know which key will be
// used, which is what lets a run be verified afterwards.
func (w *Wallet) DerivePoint(args DeriveArgs) (string, error) {
	d, err := w.scalar(args)
	if err != nil {
		return "", err
	}
	priv, _ := ec.PrivateKeyFromBytes(d.Bytes())
	if priv == nil {
		return "", errors.New("points: derivation produced an invalid key")
	}
	return priv.PubKey().ToDERHex(), nil
}

// MultiplyArgs parameterises a multiplication.
type MultiplyArgs struct {
	DeriveArgs
	// Point is the caller-supplied point, compressed hex.
	Point string
	// Invert multiplies by the modular inverse instead, which is how a mask is removed.
	Invert bool
}

// MultiplyPoint multiplies a caller-supplied point by the derived key.
//
// The scalar never leaves this function. That is the whole value of the primitive: an application
// gets the transformed point and cannot obtain the secret that produced it.
func (w *Wallet) MultiplyPoint(args MultiplyArgs) (string, error) {
	if err := args.validate(); err != nil {
		return "", err
	}

	// An off-curve or identity point must be refused rather than coerced. Accepting one is the
	// standard invalid-curve attack, and it would let a caller learn structure about the scalar.
	pub, err := ec.PublicKeyFromString(args.Point)
	if err != nil {
		return "", fmt.Errorf("points: the supplied point is not a valid public key: %w", err)
	}
	if pub.X == nil || pub.Y == nil {
		return "", errors.New("points: the supplied point has no coordinates")
	}
	curve := ec.S256()

	// The coordinates must be canonical field elements. This check is not redundant with
	// IsOnCurve: the go-sdk parser accepts an x-coordinate greater than the field prime and
	// reduces it silently, after which IsOnCurve reports true for a point that was never
	// validly encoded. Accepting a non-canonical encoding is the invalid-curve class this
	// primitive must refuse, so the bound is checked explicitly.
	fieldPrime := curve.Params().P
	if pub.X.Sign() < 0 || pub.X.Cmp(fieldPrime) >= 0 {
		return "", errors.New("points: the supplied point's x coordinate is not a canonical field element")
	}
	if pub.Y.Sign() < 0 || pub.Y.Cmp(fieldPrime) >= 0 {
		return "", errors.New("points: the supplied point's y coordinate is not a canonical field element")
	}
	if !curve.IsOnCurve(pub.X, pub.Y) {
		return "", errors.New("points: the supplied point is not on secp256k1")
	}
	// The identity is not a usable protocol point: multiplying it yields the identity
	// regardless of the scalar, which would silently produce a meaningless result.
	if pub.X.Sign() == 0 && pub.Y.Sign() == 0 {
		return "", errors.New("points: the supplied point is the identity")
	}

	d, err := w.scalar(args.DeriveArgs)
	if err != nil {
		return "", err
	}
	if args.Invert {
		inv := new(big.Int).ModInverse(d, curve.Params().N)
		if inv == nil {
			// Unreachable for a correct derivation, but returning a wrong point here
			// would corrupt a protocol run undetectably.
			return "", errors.New("points: the derived scalar has no inverse")
		}
		d = inv
	}

	x, y := curve.ScalarMult(pub.X, pub.Y, d.Bytes())
	if x == nil || y == nil || !curve.IsOnCurve(x, y) {
		return "", errors.New("points: multiplication produced an invalid point")
	}
	out := &ec.PublicKey{Curve: curve, X: x, Y: y}
	return out.ToDERHex(), nil
}

// scalar derives the protocol key.
//
// The derivation binds the root key, the protocol, the key id and the counterparty, each
// length-prefixed so two different inputs cannot produce the same scalar. A real wallet would use
// its BRC-42/43 path here; the property that matters is that the result is unrelated to any key
// that signs transactions.
func (w *Wallet) scalar(args DeriveArgs) (*big.Int, error) {
	if err := args.validate(); err != nil {
		return nil, err
	}

	counterparty := strings.ToLower(args.Counterparty)
	if counterparty == "" {
		counterparty = "self"
	}

	n := ec.S256().Params().N
	// Rejection-sample so the scalar is uniform in [1, n-1] rather than biased by reduction,
	// and never zero.
	for i := 0; i < 256; i++ {
		h := sha256.New()
		writeField(h, []byte("brc-point-operations/v1"))
		writeField(h, w.root.Serialize())
		writeField(h, []byte{byte(args.Protocol.Level)})
		writeField(h, []byte(args.Protocol.Name))
		writeField(h, []byte(args.KeyID))
		writeField(h, []byte(counterparty))
		var ctr [4]byte
		binary.BigEndian.PutUint32(ctr[:], uint32(i)) //nolint:gosec // bounded by the loop
		writeField(h, ctr[:])

		d := new(big.Int).SetBytes(h.Sum(nil))
		if d.Sign() != 0 && d.Cmp(n) < 0 {
			return d, nil
		}
	}
	// Astronomically unreachable: each attempt succeeds with probability ~1-2^-128.
	return nil, errors.New("points: could not derive a valid scalar")
}

func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}
