package cosign

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"
)

// fakeWallet records what it was asked to do, so the abandon behaviour can be asserted.
type fakeWallet struct {
	mu       sync.Mutex
	aborted  [][]byte
	signErr  error
	signCall int
}

func (f *fakeWallet) CreateAction(context.Context, sdk.CreateActionArgs, string) (*sdk.CreateActionResult, error) {
	return nil, errors.New("not used")
}

func (f *fakeWallet) SignAction(context.Context, sdk.SignActionArgs, string) (*sdk.SignActionResult, error) {
	f.mu.Lock()
	f.signCall++
	err := f.signErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &sdk.SignActionResult{}, nil
}

func (f *fakeWallet) AbortAction(_ context.Context, args sdk.AbortActionArgs, _ string) (*sdk.AbortActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, args.Reference)
	return &sdk.AbortActionResult{Aborted: true}, nil
}

func (f *fakeWallet) abortCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.aborted)
}

func settlementFixture(t *testing.T, seats int) (Settlement, FundedPot, []*ec.PrivateKey) {
	t.Helper()
	privs := keys(t, seats)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	const sats = 5000
	tx := buildSpend(t, lock, sats)

	var src chainhash.Hash
	src[0] = 0x11
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: sats}
	return Settlement{Tx: tx, PotInput: 0, Reference: []byte("ref-1")}, pot, privs
}

// The hazard this exists for: a built settlement that is walked away from must be aborted, or
// its provisional change becomes a phantom zero-txid coin that disables the funder.
func TestIncompleteSignatureSetAbandonsTheSettlement(t *testing.T) {
	s, pot, privs := settlementFixture(t, 3)
	w := &fakeWallet{}

	// Only two of three seats sign.
	var sigs []Signature
	for i := 0; i < 2; i++ {
		sig, err := SignInput(s.Tx, 0, i, privs[i])
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, sig)
	}

	if _, err := Complete(context.Background(), w, "poker.local", s, pot, sigs, 3); err == nil {
		t.Fatal("an incomplete signature set completed")
	}
	if w.abortCount() != 1 {
		t.Fatalf("abort called %d times; an abandoned settlement must be released", w.abortCount())
	}
	if string(w.aborted[0]) != "ref-1" {
		t.Errorf("aborted reference = %q, want the settlement's own", w.aborted[0])
	}
}

func TestFailedScriptVerificationAbandonsTheSettlement(t *testing.T) {
	s, pot, privs := settlementFixture(t, 2)
	w := &fakeWallet{}

	// Sign, then corrupt the transaction so verification fails.
	var sigs []Signature
	for i, p := range privs {
		sig, err := SignInput(s.Tx, 0, i, p)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, sig)
	}
	s.Tx.Outputs[0].Satoshis = 1 // the signatures no longer commit to this

	if _, err := Complete(context.Background(), w, "poker.local", s, pot, sigs, 2); err == nil {
		t.Fatal("a settlement with invalid signatures completed")
	}
	if w.abortCount() != 1 {
		t.Fatalf("abort called %d times after a verification failure", w.abortCount())
	}
}

func TestSignFailureAbandonsTheSettlement(t *testing.T) {
	s, pot, privs := settlementFixture(t, 2)
	w := &fakeWallet{signErr: errors.New("wallet unavailable")}

	var sigs []Signature
	for i, p := range privs {
		sig, err := SignInput(s.Tx, 0, i, p)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, sig)
	}

	if _, err := Complete(context.Background(), w, "poker.local", s, pot, sigs, 2); err == nil {
		t.Fatal("a failed SignAction reported success")
	}
	if w.abortCount() != 1 {
		t.Fatalf("abort called %d times after SignAction failed", w.abortCount())
	}
}

// A successful settlement must NOT be aborted: aborting a completed action would be an
// attempt to unwind money that has already moved.
func TestSuccessfulSettlementIsNotAbandoned(t *testing.T) {
	s, pot, privs := settlementFixture(t, 2)
	w := &fakeWallet{}

	var sigs []Signature
	for i, p := range privs {
		sig, err := SignInput(s.Tx, 0, i, p)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, sig)
	}

	if _, err := Complete(context.Background(), w, "poker.local", s, pot, sigs, 2); err != nil {
		t.Fatalf("an honest settlement failed: %v", err)
	}
	if w.abortCount() != 0 {
		t.Fatal("a completed settlement was aborted")
	}
}

func TestAbandonIsSafeWithNoReference(t *testing.T) {
	w := &fakeWallet{}
	// Nothing was reserved, so there is nothing to release and no call to make.
	if err := Abandon(context.Background(), w, "poker.local", Settlement{}); err != nil {
		t.Fatalf("abandoning an unreserved settlement failed: %v", err)
	}
	if w.abortCount() != 0 {
		t.Error("abort was called for a settlement that reserved nothing")
	}
	if err := Abandon(context.Background(), nil, "poker.local", Settlement{Reference: []byte("r")}); err == nil {
		t.Error("abandoned with no wallet")
	}
}

// --- payouts ---------------------------------------------------------------

// The derivation must match what the recipient's wallet computes, or the payment lands
// on-chain and is permanently unspendable.
func TestDerivePayoutMatchesTheRecipientsOwnDerivation(t *testing.T) {
	sender, recipient := keys(t, 1)[0], keys(t, 1)[0]

	p := Payout{
		RecipientKey: recipient.PubKey(),
		Satoshis:     4800,
		Prefix:       []byte("hand-0001"),
		Suffix:       []byte("winner"),
	}
	if err := DerivePayout(sender, &p); err != nil {
		t.Fatal(err)
	}

	// What the recipient's wallet computes when it internalizes: LockForSelf with the
	// BASE64 key id, mirroring the sender's LockForCounterparty.
	keyID := brc29.KeyID{
		DerivationPrefix: base64.StdEncoding.EncodeToString(p.Prefix),
		DerivationSuffix: base64.StdEncoding.EncodeToString(p.Suffix),
	}
	expect, err := brc29.LockForSelf(brc29.PubHex(sender.PubKey().ToDERHex()), keyID, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(p.Script.String(), expect.String()) {
		t.Fatalf("sender derived %s but the recipient expects %s; the payout would be unspendable",
			p.Script.String(), expect.String())
	}
}

// Raw derivation bytes must go into the remittance, not their base64 form: the wallet
// base64-encodes them itself before validating.
func TestRemittanceCarriesRawBytes(t *testing.T) {
	sender := keys(t, 1)[0]
	p := Payout{Prefix: []byte("hand-0001"), Suffix: []byte("winner")}
	r := p.Remittance(sender)
	if string(r.DerivationPrefix) != "hand-0001" {
		t.Errorf("prefix = %q; the remittance must carry raw bytes", r.DerivationPrefix)
	}
	if string(r.DerivationSuffix) != "winner" {
		t.Errorf("suffix = %q", r.DerivationSuffix)
	}
	if r.SenderIdentityKey == nil || r.SenderIdentityKey.ToDERHex() != sender.PubKey().ToDERHex() {
		t.Error("the remittance does not name the sender")
	}
}

func TestDerivePayoutValidation(t *testing.T) {
	sender, recipient := keys(t, 1)[0], keys(t, 1)[0]
	good := func() *Payout {
		return &Payout{RecipientKey: recipient.PubKey(), Satoshis: 100, Prefix: []byte("p"), Suffix: []byte("s")}
	}

	if err := DerivePayout(nil, good()); err == nil {
		t.Error("derived with no sender")
	}
	if err := DerivePayout(sender, nil); err == nil {
		t.Error("derived a nil payout")
	}
	noRecipient := good()
	noRecipient.RecipientKey = nil
	if err := DerivePayout(sender, noRecipient); err == nil {
		t.Error("derived with no recipient")
	}
	zero := good()
	zero.Satoshis = 0
	if err := DerivePayout(sender, zero); err == nil {
		t.Error("derived a payout of nothing")
	}
	noMaterial := good()
	noMaterial.Prefix = nil
	if err := DerivePayout(sender, noMaterial); err == nil {
		t.Error("derived with no derivation material")
	}
}

func TestPayoutVout(t *testing.T) {
	sender, recipient := keys(t, 1)[0], keys(t, 1)[0]
	p := Payout{RecipientKey: recipient.PubKey(), Satoshis: 100, Prefix: []byte("p"), Suffix: []byte("s")}
	if err := DerivePayout(sender, &p); err != nil {
		t.Fatal(err)
	}

	tx := transaction.NewTransaction()
	other, err := PotScript(pubs(keys(t, 2)))
	if err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: other})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 100, LockingScript: p.Script})

	vout, err := PayoutVout(tx, p)
	if err != nil {
		t.Fatal(err)
	}
	if vout != 1 {
		t.Fatalf("vout = %d, want 1", vout)
	}
	if _, err := PayoutVout(nil, p); err == nil {
		t.Error("searched a nil transaction")
	}
	if _, err := PayoutVout(tx, Payout{}); err == nil {
		t.Error("searched for a payout with no script")
	}
}

// --- refunds ---------------------------------------------------------------

func TestBuildRefundBindsItsLocktime(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	var src chainhash.Hash
	src[0] = 0x22
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}

	tx, err := BuildRefund(RefundArgs{
		Pot: pot, Recipient: privs[0].PubKey(), Satoshis: 4700, LockHeight: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.LockTime != 30000 {
		t.Errorf("locktime = %d, want 30000", tx.LockTime)
	}
	// A final sequence would make the locktime decorative.
	if tx.Inputs[0].SequenceNumber == transaction.DefaultSequenceNumber {
		t.Error("the refund input is final, so its locktime does not bind")
	}
	// The source output must be attached, or signing would commit to the wrong value.
	if tx.Inputs[0].SourceTxOutput() == nil {
		t.Error("the refund input has no source output")
	}

	// And it must actually be spendable by the seats.
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
	if err := VerifyScript(tx, 0, lock, pot.Satoshis); err != nil {
		t.Fatalf("the refund does not satisfy the pot script: %v", err)
	}
}

func TestBuildRefundValidation(t *testing.T) {
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	var src chainhash.Hash
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}

	tests := map[string]RefundArgs{
		"no recipient":   {Pot: pot, Satoshis: 100, LockHeight: 1},
		"zero amount":    {Pot: pot, Recipient: privs[0].PubKey(), LockHeight: 1},
		"over the pot":   {Pot: pot, Recipient: privs[0].PubKey(), Satoshis: 6000, LockHeight: 1},
		"no lock height": {Pot: pot, Recipient: privs[0].PubKey(), Satoshis: 100},
		"no pot script":  {Pot: FundedPot{Txid: src.String(), Satoshis: 5000}, Recipient: privs[0].PubKey(), Satoshis: 100, LockHeight: 1},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildRefund(args); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestFundPotValidation(t *testing.T) {
	ctx := context.Background()
	privs := keys(t, 2)
	base := FundPotArgs{
		Wallet: &fakeWallet{}, Seats: pubs(privs), Satoshis: 5000,
		Description: "a valid description",
	}

	noWallet := base
	noWallet.Wallet = nil
	if _, err := FundPot(ctx, noWallet); err == nil {
		t.Error("funded with no wallet")
	}
	noValue := base
	noValue.Satoshis = 0
	if _, err := FundPot(ctx, noValue); err == nil {
		t.Error("funded a pot with no value")
	}
	// The toolbox enforces a five-byte minimum from deep inside validation; catching it
	// here names the field.
	shortDesc := base
	shortDesc.Description = "hi"
	if _, err := FundPot(ctx, shortDesc); err == nil {
		t.Error("funded with a description under 5 bytes")
	}
}

func TestBuildSettlementValidation(t *testing.T) {
	ctx := context.Background()
	privs := keys(t, 2)
	lock, err := PotScript(pubs(privs))
	if err != nil {
		t.Fatal(err)
	}
	var src chainhash.Hash
	pot := FundedPot{Txid: src.String(), Vout: 0, Script: lock, Satoshis: 5000}
	payout := Payout{Satoshis: 4800, Script: lock}

	base := SettleArgs{
		Wallet: &fakeWallet{}, Pot: pot, Payouts: []Payout{payout},
		Seats: 2, Description: "a valid description",
	}

	noWallet := base
	noWallet.Wallet = nil
	if _, err := BuildSettlement(ctx, noWallet); err == nil {
		t.Error("built with no wallet")
	}
	noPayouts := base
	noPayouts.Payouts = nil
	if _, err := BuildSettlement(ctx, noPayouts); err == nil {
		t.Error("built a settlement that pays nobody")
	}
	shortDesc := base
	shortDesc.Description = "hi"
	if _, err := BuildSettlement(ctx, shortDesc); err == nil {
		t.Error("built with a short description")
	}
	noScript := base
	noScript.Pot = FundedPot{Txid: src.String(), Satoshis: 5000}
	if _, err := BuildSettlement(ctx, noScript); err == nil {
		t.Error("built without the pot's locking script")
	}
	// Paying out more than the pot holds must be refused before any wallet call.
	overspend := base
	overspend.Payouts = []Payout{{Satoshis: 9000, Script: lock}}
	if _, err := BuildSettlement(ctx, overspend); err == nil {
		t.Error("built a settlement paying out more than the pot holds")
	}
}
