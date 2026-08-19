package cosign

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

func keys(t *testing.T, n int) []*ec.PrivateKey {
	t.Helper()
	out := make([]*ec.PrivateKey, n)
	for i := range out {
		k, err := ec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		out[i] = k
	}
	return out
}

func pubs(privs []*ec.PrivateKey) []*ec.PublicKey {
	out := make([]*ec.PublicKey, len(privs))
	for i, p := range privs {
		out[i] = p.PubKey()
	}
	return out
}

// buildSpend makes a transaction spending a pot output, with the source output attached so
// the sighash preimage commits to the right value and script.
func buildSpend(t *testing.T, lock *script.Script, sats uint64) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	var src chainhash.Hash
	src[0] = 0x11
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       &src,
		SourceTxOutIndex: 0,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	})
	tx.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      sats,
		LockingScript: lock,
	})

	payout := &script.Script{}
	if err := payout.AppendOpcodes(script.OpTRUE); err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats - 200, LockingScript: payout})
	return tx
}

// The core property, across every table size: a pot spends only when EVERY seat has signed.
func TestNOfNSpendsWithAllSignatures(t *testing.T) {
	for seats := 2; seats <= MaxSeats; seats++ {
		t.Run(strings.Repeat("seat", 1)+string(rune('0'+seats)), func(t *testing.T) {
			privs := keys(t, seats)
			lock, err := PotScript(pubs(privs))
			if err != nil {
				t.Fatal(err)
			}

			const sats = 5000
			tx := buildSpend(t, lock, sats)

			sigs := make([]Signature, 0, seats)
			for i, p := range privs {
				s, err := SignInput(tx, 0, i, p)
				if err != nil {
					t.Fatal(err)
				}
				if err := VerifySignature(tx, 0, s, p.PubKey()); err != nil {
					t.Fatalf("seat %d's own signature did not verify: %v", i, err)
				}
				sigs = append(sigs, s)
			}

			unlock, err := Assemble(sigs, seats)
			if err != nil {
				t.Fatal(err)
			}
			tx.Inputs[0].UnlockingScript = unlock

			if err := VerifyScript(tx, 0, lock, sats); err != nil {
				t.Fatalf("%d-of-%d pot did not spend: %v", seats, seats, err)
			}

			// The declared length must cover what was actually assembled, or the fee
			// is underpaid and the broadcast earns an unretryable rejection.
			if declared := UnlockingScriptLength(seats); int(declared) < len(*unlock) {
				t.Errorf("declared %d bytes but assembled %d", declared, len(*unlock))
			}
		})
	}
}

// One missing signature must not spend the pot: this is what makes it non-custodial.
func TestMissingSignatureCannotSpend(t *testing.T) {
	const seats = 4
	privs := keys(t, seats)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	const sats = 5000
	tx := buildSpend(t, lock, sats)

	// Every seat but the last signs.
	var sigs []Signature
	for i := 0; i < seats-1; i++ {
		s, err := SignInput(tx, 0, i, privs[i])
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}

	// Assembly must refuse an incomplete set outright.
	if _, err := Assemble(sigs, seats); err == nil {
		t.Fatal("an incomplete signature set was assembled")
	}
}

// A seat that signs the wrong thing is caught and attributed, not left to fail opaquely.
func TestWrongKeySignatureIsRejectedAndAttributed(t *testing.T) {
	const seats = 3
	privs := keys(t, seats)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	tx := buildSpend(t, lock, 5000)

	imposter := keys(t, 1)[0]
	sig, err := SignInput(tx, 0, 1, imposter)
	if err != nil {
		t.Fatal(err)
	}
	err = VerifySignature(tx, 0, sig, privs[1].PubKey())
	if err == nil {
		t.Fatal("an imposter's signature verified against another seat's key")
	}
	if !strings.Contains(err.Error(), "seat 1") {
		t.Errorf("error does not attribute the failure to seat 1: %v", err)
	}
}

// Signatures must be assembled in key order, not arrival order.
func TestAssemblyIsKeyOrderedNotArrivalOrdered(t *testing.T) {
	const seats = 3
	privs := keys(t, seats)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	const sats = 5000
	tx := buildSpend(t, lock, sats)

	var sigs []Signature
	for i, p := range privs {
		s, err := SignInput(tx, 0, i, p)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	// Deliver them in reverse: the order signatures arrive in is not the order the
	// script requires.
	reversed := []Signature{sigs[2], sigs[0], sigs[1]}
	unlock, err := Assemble(reversed, seats)
	if err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].UnlockingScript = unlock
	if err := VerifyScript(tx, 0, lock, sats); err != nil {
		t.Fatalf("assembly did not reorder into key order: %v", err)
	}
}

// A signature is a commitment to the outputs. Changing an output after signing must
// invalidate the collected signatures.
func TestChangingAnOutputInvalidatesSignatures(t *testing.T) {
	const seats = 2
	privs := keys(t, seats)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	const sats = 5000
	tx := buildSpend(t, lock, sats)

	var sigs []Signature
	for i, p := range privs {
		s, err := SignInput(tx, 0, i, p)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	unlock, err := Assemble(sigs, seats)
	if err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].UnlockingScript = unlock
	if err := VerifyScript(tx, 0, lock, sats); err != nil {
		t.Fatalf("the honest transaction did not verify: %v", err)
	}

	// Now redirect the payout. The signatures committed to the old output set.
	tx.Outputs[0].Satoshis = 1
	if err := VerifyScript(tx, 0, lock, sats); err == nil {
		t.Fatal("signatures still verified after an output was changed")
	}
}

// Adding an unexpected output must also invalidate the signatures.
func TestAddingAnOutputInvalidatesSignatures(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	const sats = 5000
	tx := buildSpend(t, lock, sats)

	var sigs []Signature
	for i, p := range privs {
		s, err := SignInput(tx, 0, i, p)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	unlock, err := Assemble(sigs, 2)
	if err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].UnlockingScript = unlock

	extra := &script.Script{}
	if err := extra.AppendOpcodes(script.OpTRUE); err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 100, LockingScript: extra})
	if err := VerifyScript(tx, 0, lock, sats); err == nil {
		t.Fatal("signatures still verified after an output was added")
	}
}

func TestPotScriptValidation(t *testing.T) {
	privs := keys(t, 3)
	if _, err := PotScript(nil); err == nil {
		t.Error("accepted an empty seat list")
	}
	if _, err := PotScript(pubs(privs[:1])); err == nil {
		t.Error("accepted a one-seat pot")
	}
	tooMany := pubs(keys(t, MaxSeats+1))
	if _, err := PotScript(tooMany); err == nil {
		t.Errorf("accepted a %d-seat pot", MaxSeats+1)
	}
	withNil := pubs(privs)
	withNil[1] = nil
	if _, err := PotScript(withNil); err == nil {
		t.Error("accepted a nil seat key")
	}
	// A duplicated key would let one seat satisfy two slots.
	dup := pubs(privs)
	dup[2] = dup[0]
	if _, err := PotScript(dup); err == nil {
		t.Error("accepted a pot with a duplicated seat key")
	}
}

func TestAssembleValidation(t *testing.T) {
	if _, err := Assemble(nil, 1); err == nil {
		t.Error("accepted a one-seat pot")
	}
	if _, err := Assemble([]Signature{{Seat: 0, DER: []byte{1}}}, 2); err == nil {
		t.Error("accepted an incomplete set")
	}
	// Two signatures claiming the same seat leaves another seat unsigned.
	dup := []Signature{{Seat: 0, DER: []byte{1}}, {Seat: 0, DER: []byte{2}}}
	if _, err := Assemble(dup, 2); err == nil {
		t.Error("accepted two signatures for the same seat")
	}
	oob := []Signature{{Seat: 0, DER: []byte{1}}, {Seat: 7, DER: []byte{2}}}
	if _, err := Assemble(oob, 2); err == nil {
		t.Error("accepted an out-of-range seat index")
	}
	empty := []Signature{{Seat: 0, DER: []byte{1}}, {Seat: 1, DER: nil}}
	if _, err := Assemble(empty, 2); err == nil {
		t.Error("accepted an empty signature")
	}
}

// Signing without the source output attached would sign the wrong message.
func TestSignRequiresSourceOutput(t *testing.T) {
	privs := keys(t, 2)
	tx := transaction.NewTransaction()
	var src chainhash.Hash
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &src, SourceTxOutIndex: 0})

	if _, err := SignInput(tx, 0, 0, privs[0]); err == nil {
		t.Fatal("signed an input with no source output; the sighash would be wrong")
	}
}

func TestSignValidation(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	tx := buildSpend(t, lock, 5000)

	if _, err := SignInput(nil, 0, 0, privs[0]); err == nil {
		t.Error("signed a nil transaction")
	}
	if _, err := SignInput(tx, 9, 0, privs[0]); err == nil {
		t.Error("signed an out-of-range input")
	}
	if _, err := SignInput(tx, 0, 0, nil); err == nil {
		t.Error("signed with a nil key")
	}
}

func TestVerifySignatureRejectsMalformed(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	tx := buildSpend(t, lock, 5000)
	good, err := SignInput(tx, 0, 0, privs[0])
	if err != nil {
		t.Fatal(err)
	}

	if err := VerifySignature(tx, 0, good, nil); err == nil {
		t.Error("verified against a nil key")
	}
	short := good
	short.DER = []byte{0x30}
	if err := VerifySignature(tx, 0, short, privs[0].PubKey()); err == nil {
		t.Error("accepted a truncated signature")
	}
	// The wrong sighash type must be refused: it would commit to a different message.
	badFlag := good
	badFlag.DER = append(append([]byte{}, good.DER[:len(good.DER)-1]...), 0x01)
	if err := VerifySignature(tx, 0, badFlag, privs[0].PubKey()); err == nil {
		t.Error("accepted a signature with the wrong sighash type")
	}
	// A claimed identity that does not match the pot's key must be refused.
	wrongClaim := good
	wrongClaim.IdentityKey = privs[1].PubKey().ToDERHex()
	if err := VerifySignature(tx, 0, wrongClaim, privs[0].PubKey()); err == nil {
		t.Error("accepted a signature whose claimed key differs from the pot's")
	}
	garbage := good
	garbage.DER = []byte{0x30, 0x02, 0xff, 0xff, byte(SigHashFlag)}
	if err := VerifySignature(tx, 0, garbage, privs[0].PubKey()); err == nil {
		t.Error("accepted an unparseable DER signature")
	}
}

func TestVerifyScriptValidation(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	tx := buildSpend(t, lock, 5000)

	if err := VerifyScript(nil, 0, lock, 5000); err == nil {
		t.Error("verified a nil transaction")
	}
	if err := VerifyScript(tx, 9, lock, 5000); err == nil {
		t.Error("verified an out-of-range input")
	}
	// No unlocking script set yet.
	if err := VerifyScript(tx, 0, lock, 5000); err == nil {
		t.Error("verified an input with no unlocking script")
	}
}

// The pot's input index is discovered, not assumed: the funder may prepend fee inputs.
func TestFindPotInputAmongOthers(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}

	tx := transaction.NewTransaction()
	var fee, pot chainhash.Hash
	fee[0] = 0xaa
	pot[0] = 0xbb
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &fee, SourceTxOutIndex: 3})
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &pot, SourceTxOutIndex: 1})
	tx.Inputs[1].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: 5000, LockingScript: lock})

	idx, err := FindPotInput(tx, pot.String(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("found the pot at input %d, want 1", idx)
	}
	if _, err := FindPotInput(tx, pot.String(), 9); err == nil {
		t.Error("found a pot at an outpoint the transaction does not spend")
	}
	if _, err := FindPotInput(nil, pot.String(), 1); err == nil {
		t.Error("searched a nil transaction")
	}
}

func TestUnlockingScriptLengthGrowsWithSeats(t *testing.T) {
	prev := uint32(0)
	for seats := 2; seats <= MaxSeats; seats++ {
		got := UnlockingScriptLength(seats)
		if got <= prev {
			t.Fatalf("length for %d seats (%d) did not exceed %d", seats, got, prev)
		}
		prev = got
	}
}
