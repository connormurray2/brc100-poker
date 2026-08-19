// Package cosign builds and co-signs the shared pot.
//
// Everything here generalises what the co-signing spike proved with real coins on
// teratestnet: an n-of-n output that no single party can move, spent by a transaction
// assembled from signatures produced independently on each seat's own machine.
//
// The sharp edges are documented at each call site rather than in one list, because each one
// fails silently and in a different way.
package cosign

import (
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
)

// MaxSeats bounds a pot. CHECKMULTISIG is limited well above this, but a table is 2..6.
const MaxSeats = 6

// SigHashFlag is the sighash type every pot signature uses.
//
// AllForkID commits to every output, which is what makes a signature a commitment to the
// hand's outcome rather than a blank cheque. It is also why output order must be pinned:
// shuffling outputs after signing invalidates every signature collected.
const SigHashFlag = sighash.AllForkID

// PotScript builds an n-of-n bare multisig: OP_n <pub1> … <pubN> OP_n OP_CHECKMULTISIG.
//
// Bare multisig rather than P2SH because the spike proved this exact shape spends cleanly
// through the toolbox's two-step path, and because it keeps the seats visible in the output
// so any observer can verify the pot needs every one of them.
//
// The key order is significant and is preserved: CHECKMULTISIG requires signatures in the
// same relative order as the keys.
func PotScript(seats []*ec.PublicKey) (*script.Script, error) {
	if len(seats) < 2 {
		return nil, fmt.Errorf("cosign: a pot needs at least 2 seats, got %d", len(seats))
	}
	if len(seats) > MaxSeats {
		return nil, fmt.Errorf("cosign: a pot supports at most %d seats, got %d", MaxSeats, len(seats))
	}

	seen := make(map[string]struct{}, len(seats))
	s := &script.Script{}
	n, err := smallInt(len(seats))
	if err != nil {
		return nil, err
	}
	if err := s.AppendOpcodes(n); err != nil {
		return nil, fmt.Errorf("cosign: appending the seat count: %w", err)
	}
	for i, pub := range seats {
		if pub == nil {
			return nil, fmt.Errorf("cosign: seat %d has no public key", i)
		}
		key := pub.Compressed()
		// A duplicated key would let one seat satisfy two slots, so the pot would
		// no longer require every seat.
		if _, dup := seen[string(key)]; dup {
			return nil, fmt.Errorf("cosign: seat %d repeats an earlier seat's key", i)
		}
		seen[string(key)] = struct{}{}
		if err := s.AppendPushData(key); err != nil {
			return nil, fmt.Errorf("cosign: appending seat %d's key: %w", i, err)
		}
	}
	if err := s.AppendOpcodes(n, script.OpCHECKMULTISIG); err != nil {
		return nil, fmt.Errorf("cosign: appending OP_CHECKMULTISIG: %w", err)
	}
	return s, nil
}

func smallInt(n int) (byte, error) {
	switch n {
	case 2:
		return script.Op2, nil
	case 3:
		return script.Op3, nil
	case 4:
		return script.Op4, nil
	case 5:
		return script.Op5, nil
	case 6:
		return script.Op6, nil
	default:
		return 0, fmt.Errorf("cosign: no small-int opcode for %d", n)
	}
}

// UnlockingScriptLength estimates the assembled unlocking script size for n seats.
//
// Over-estimating costs a little fee accuracy; under-estimating underpays the fee and earns
// a 4xx rejection that cannot be retried. So this deliberately rounds up: a DER signature
// plus its sighash byte is at most 73 bytes, each needs a push opcode, and the leading OP_0
// dummy adds one.
func UnlockingScriptLength(seats int) uint32 {
	const maxSigWithHashType = 73
	const pushPrefix = 1
	const dummy = 1
	const margin = 8
	return uint32(dummy + seats*(pushPrefix+maxSigWithHashType) + margin)
}

// Signature is one seat's signature over one input.
type Signature struct {
	// Seat is the index of the seat in the pot script's key order. Assembly depends on
	// it, since CHECKMULTISIG requires key-order signatures.
	Seat int
	// IdentityKey is the seat's public key, hex-encoded, for attribution.
	IdentityKey string
	// DER is the signature with the sighash-type byte appended.
	DER []byte
}

// SignInput produces one seat's signature over a transaction input.
//
// The input's source output must already be set on the transaction: the sighash preimage
// commits to the input's value and locking script, so signing without them produces a
// signature over the wrong message.
func SignInput(tx *transaction.Transaction, inputIndex int, seat int, priv *ec.PrivateKey) (Signature, error) {
	if tx == nil {
		return Signature{}, errors.New("cosign: no transaction to sign")
	}
	if inputIndex < 0 || inputIndex >= len(tx.Inputs) {
		return Signature{}, fmt.Errorf("cosign: input %d out of range for %d inputs", inputIndex, len(tx.Inputs))
	}
	if priv == nil {
		return Signature{}, errors.New("cosign: no private key to sign with")
	}
	in := tx.Inputs[inputIndex]
	if in.SourceTxOutput() == nil {
		return Signature{}, fmt.Errorf("cosign: input %d has no source output; the sighash would commit to the wrong value", inputIndex)
	}

	hash, err := tx.CalcInputSignatureHash(uint32(inputIndex), SigHashFlag)
	if err != nil {
		return Signature{}, fmt.Errorf("cosign: computing the signature hash for input %d: %w", inputIndex, err)
	}
	sig, err := priv.Sign(hash)
	if err != nil {
		return Signature{}, fmt.Errorf("cosign: signing input %d: %w", inputIndex, err)
	}
	return Signature{
		Seat:        seat,
		IdentityKey: priv.PubKey().ToDERHex(),
		DER:         append(sig.Serialize(), byte(SigHashFlag)),
	}, nil
}

// VerifySignature checks one seat's signature against its claimed key.
//
// Verifying before assembly means an invalid signature is attributed to the seat that sent
// it, rather than surfacing later as an opaque script failure on the whole transaction.
func VerifySignature(tx *transaction.Transaction, inputIndex int, sig Signature, pub *ec.PublicKey) error {
	if pub == nil {
		return errors.New("cosign: no public key to verify against")
	}
	if len(sig.DER) < 2 {
		return fmt.Errorf("cosign: seat %d sent a %d-byte signature", sig.Seat, len(sig.DER))
	}
	if got := sig.DER[len(sig.DER)-1]; got != byte(SigHashFlag) {
		return fmt.Errorf("cosign: seat %d used sighash type 0x%02x, want 0x%02x", sig.Seat, got, byte(SigHashFlag))
	}
	if sig.IdentityKey != "" && sig.IdentityKey != pub.ToDERHex() {
		return fmt.Errorf("cosign: seat %d's signature claims key %s but the pot expects %s",
			sig.Seat, sig.IdentityKey, pub.ToDERHex())
	}

	hash, err := tx.CalcInputSignatureHash(uint32(inputIndex), SigHashFlag)
	if err != nil {
		return fmt.Errorf("cosign: computing the signature hash: %w", err)
	}
	parsed, err := ec.ParseDERSignature(sig.DER[:len(sig.DER)-1])
	if err != nil {
		return fmt.Errorf("cosign: seat %d sent an unparseable signature: %w", sig.Seat, err)
	}
	if !parsed.Verify(hash, pub) {
		return fmt.Errorf("cosign: seat %d's signature does not verify", sig.Seat)
	}
	return nil
}

// Assemble builds the unlocking script from a complete signature set.
//
// Signatures are ordered by seat index because CHECKMULTISIG requires them in the same
// relative order as the keys in the locking script. The leading OP_0 satisfies the
// well-known off-by-one dummy pop.
func Assemble(sigs []Signature, seats int) (*script.Script, error) {
	if seats < 2 {
		return nil, fmt.Errorf("cosign: a pot needs at least 2 seats, got %d", seats)
	}
	if len(sigs) != seats {
		return nil, fmt.Errorf("cosign: have %d signatures, need all %d: an incomplete set must not be broadcast", len(sigs), seats)
	}

	ordered := make([]Signature, seats)
	for _, s := range sigs {
		if s.Seat < 0 || s.Seat >= seats {
			return nil, fmt.Errorf("cosign: signature claims seat %d, outside 0..%d", s.Seat, seats-1)
		}
		if ordered[s.Seat].DER != nil {
			return nil, fmt.Errorf("cosign: two signatures claim seat %d", s.Seat)
		}
		if len(s.DER) == 0 {
			return nil, fmt.Errorf("cosign: seat %d sent an empty signature", s.Seat)
		}
		ordered[s.Seat] = s
	}
	for i, s := range ordered {
		if s.DER == nil {
			return nil, fmt.Errorf("cosign: no signature from seat %d", i)
		}
	}

	out := &script.Script{}
	if err := out.AppendOpcodes(script.Op0); err != nil {
		return nil, fmt.Errorf("cosign: appending the CHECKMULTISIG dummy: %w", err)
	}
	for i, s := range ordered {
		if err := out.AppendPushData(s.DER); err != nil {
			return nil, fmt.Errorf("cosign: appending seat %d's signature: %w", i, err)
		}
	}
	return out, nil
}

// VerifyScript runs the real script interpreter over an assembled input.
//
// Storage would also verify before broadcast, but doing it here reports the failure against
// our own object — which hand, which pot — instead of "script verification failed for input
// N" several layers down. It also costs microseconds against a create path measured in
// hundreds of milliseconds.
func VerifyScript(tx *transaction.Transaction, inputIndex int, lock *script.Script, satoshis uint64) error {
	if tx == nil {
		return errors.New("cosign: no transaction to verify")
	}
	if inputIndex < 0 || inputIndex >= len(tx.Inputs) {
		return fmt.Errorf("cosign: input %d out of range for %d inputs", inputIndex, len(tx.Inputs))
	}
	if tx.Inputs[inputIndex].UnlockingScript == nil {
		return fmt.Errorf("cosign: input %d has no unlocking script to verify", inputIndex)
	}
	err := interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, inputIndex, &transaction.TransactionOutput{
			LockingScript: lock,
			Satoshis:      satoshis,
		}),
		interpreter.WithForkID(),
		interpreter.WithAfterGenesis(),
	)
	if err != nil {
		return fmt.Errorf("cosign: script verification failed for input %d: %w", inputIndex, err)
	}
	return nil
}

// FindPotInput locates the input spending a given outpoint.
//
// The index is not assumed to be zero: the funder may prepend its own inputs to pay the fee,
// so the pot's input position is discovered rather than guessed.
func FindPotInput(tx *transaction.Transaction, txid string, vout uint32) (int, error) {
	if tx == nil {
		return 0, errors.New("cosign: no transaction to search")
	}
	for i, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == txid && in.SourceTxOutIndex == vout {
			return i, nil
		}
	}
	return 0, fmt.Errorf("cosign: the transaction does not spend %s:%d", txid, vout)
}

// PotScriptFromHex parses a pot locking script.
//
// A seat stores the script rather than reconstructing it from seat keys, because reconstructing
// depends on getting the key order right and a mismatch would produce a signature over the
// wrong preimage — a failure that surfaces as an opaque script error rather than as a mistake
// about key order.
func PotScriptFromHex(s string) (*script.Script, error) {
	if s == "" {
		return nil, errors.New("cosign: no pot script")
	}
	out, err := script.NewFromHex(s)
	if err != nil {
		return nil, fmt.Errorf("cosign: parsing the pot script: %w", err)
	}
	return out, nil
}
