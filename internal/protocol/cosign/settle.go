package cosign

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"
)

// Payout describes a BRC-29 payment to one recipient.
//
// The derivation material travels with the payout because the recipient's wallet needs it to
// re-derive the locking script and record the coin as its own. Paying a plain P2PKH to the
// recipient's identity key looks correct on-chain and is permanently unspendable by them —
// the failure the co-signing spike produced with real coins before this was understood.
type Payout struct {
	// RecipientKey is the payee's identity public key.
	RecipientKey *ec.PublicKey
	// Satoshis is the amount.
	Satoshis uint64
	// Prefix and Suffix are the raw derivation bytes. The KeyID uses their BASE64
	// encodings, while InternalizeAction carries the raw bytes — the two describe the same
	// values, and mixing them up yields a script the recipient cannot recognise.
	Prefix []byte
	Suffix []byte

	// Script is filled in by DerivePayout.
	Script *script.Script
}

// DerivePayout computes the BRC-29 locking script for a payout.
//
// The sender derives with LockForCounterparty; the recipient's wallet re-derives the mirror
// image with LockForSelf when it internalizes. LockForSelf cannot be used here because it
// needs the recipient's private key.
func DerivePayout(sender *ec.PrivateKey, p *Payout) error {
	if sender == nil {
		return errors.New("cosign: a sender key is required to derive a payout")
	}
	if p == nil || p.RecipientKey == nil {
		return errors.New("cosign: a payout needs a recipient")
	}
	if p.Satoshis == 0 {
		return errors.New("cosign: a payout of nothing is not a payout")
	}
	if len(p.Prefix) == 0 || len(p.Suffix) == 0 {
		return errors.New("cosign: a payout needs derivation material")
	}

	keyID := brc29.KeyID{
		DerivationPrefix: base64.StdEncoding.EncodeToString(p.Prefix),
		DerivationSuffix: base64.StdEncoding.EncodeToString(p.Suffix),
	}
	if err := keyID.Validate(); err != nil {
		return fmt.Errorf("cosign: invalid derivation material: %w", err)
	}
	lock, err := brc29.LockForCounterparty(sender, keyID, p.RecipientKey)
	if err != nil {
		return fmt.Errorf("cosign: deriving the payout script: %w", err)
	}
	p.Script = lock
	return nil
}

// Remittance returns the InternalizeAction arguments a recipient needs.
//
// Raw bytes here, not base64: the wallet base64-encodes them internally before validating,
// so passing the encoded form would derive a different key.
func (p Payout) Remittance(sender *ec.PrivateKey) *sdk.Payment {
	return &sdk.Payment{
		DerivationPrefix:  p.Prefix,
		DerivationSuffix:  p.Suffix,
		SenderIdentityKey: sender.PubKey(),
	}
}

// PotFunder is the subset of a wallet needed to fund and settle a pot.
type PotFunder interface {
	CreateAction(ctx context.Context, args sdk.CreateActionArgs, originator string) (*sdk.CreateActionResult, error)
	SignAction(ctx context.Context, args sdk.SignActionArgs, originator string) (*sdk.SignActionResult, error)
}

// FundPotArgs parameterises funding a pot.
type FundPotArgs struct {
	Wallet     PotFunder
	Originator string
	// Seats are the identity keys that must all authorise a spend, in seat order. The
	// order is part of the script and fixes the signature order.
	Seats []*ec.PublicKey
	// Satoshis is the pot value.
	Satoshis uint64
	// Basket keeps pot coins away from fee-paying coins. The funder has no exclusion
	// list, so a pot coin sharing the funding basket can be selected again to pay a fee
	// and rejected as a duplicate input.
	Basket string
	// Description is recorded on the action. At least five bytes, which the toolbox
	// enforces.
	Description string
}

// FundedPot is a funded pot output.
type FundedPot struct {
	Txid string
	Vout uint32
	// Script is the pot's locking script, needed later for the settlement sighash. The
	// wallet does not keep it for us.
	Script   *script.Script
	Satoshis uint64
	// BEEF is the funding transaction, needed as InputBEEF when spending the pot.
	BEEF []byte
}

// FundPot creates the shared pot output.
func FundPot(ctx context.Context, args FundPotArgs) (FundedPot, error) {
	if args.Wallet == nil {
		return FundedPot{}, errors.New("cosign: a wallet is required to fund a pot")
	}
	if args.Satoshis == 0 {
		return FundedPot{}, errors.New("cosign: a pot needs a value")
	}
	if len(args.Description) < 5 {
		// The toolbox enforces a five-byte minimum and reports it from deep inside
		// validation; catching it here names the field.
		return FundedPot{}, errors.New("cosign: the pot description must be at least 5 bytes")
	}
	potScript, err := PotScript(args.Seats)
	if err != nil {
		return FundedPot{}, err
	}

	res, err := args.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: args.Description,
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:     *potScript,
			Satoshis:          args.Satoshis,
			OutputDescription: "shared pot output requiring every seat",
			Basket:            args.Basket,
		}},
		Options: &sdk.CreateActionOptions{
			SignAndProcess: boolPtr(true),
			// Defaults to TRUE. Shuffling outputs would move the pot's vout after we
			// recorded it and break any signature committing to the output set.
			RandomizeOutputs: boolPtr(false),
		},
	}, args.Originator)
	if err != nil {
		return FundedPot{}, fmt.Errorf("cosign: funding the pot: %w", err)
	}

	// Randomisation is off, so the pot is at vout 0 — but verify rather than assume, so a
	// future change to that default cannot silently move the money.
	tx, err := transaction.NewTransactionFromBEEF(res.Tx)
	if err != nil {
		return FundedPot{}, fmt.Errorf("cosign: parsing the funding transaction: %w", err)
	}
	vout := -1
	for i, o := range tx.Outputs {
		if o.LockingScript != nil && o.LockingScript.String() == potScript.String() && o.Satoshis == args.Satoshis {
			vout = i
			break
		}
	}
	if vout < 0 {
		return FundedPot{}, errors.New("cosign: the funding transaction does not contain the pot output")
	}

	return FundedPot{
		Txid:     res.Txid.String(),
		Vout:     uint32(vout),
		Script:   potScript,
		Satoshis: args.Satoshis,
		BEEF:     res.Tx,
	}, nil
}

// SettleArgs parameterises building a settlement.
type SettleArgs struct {
	Wallet     PotFunder
	Originator string
	Pot        FundedPot
	// Payouts are the recipients, already derived.
	Payouts []Payout
	// Seats is the number of seats whose signatures the pot requires.
	Seats int
	// Description is recorded on the action.
	Description string
}

// Settlement is an unsigned settlement awaiting signatures.
type Settlement struct {
	Tx *transaction.Transaction
	// PotInput is the index of the input spending the pot. Discovered, not assumed: the
	// funder may prepend its own inputs to pay the fee.
	PotInput int
	// Reference is the wallet's signable-transaction reference, needed by SignAction.
	Reference []byte
}

// BuildSettlement creates an unsigned settlement spending the pot.
//
// It uses the two-step path — CreateAction with SignAndProcess false — because the pot's
// unlocking script is caller-supplied and the wallet cannot produce it.
func BuildSettlement(ctx context.Context, args SettleArgs) (Settlement, error) {
	if args.Wallet == nil {
		return Settlement{}, errors.New("cosign: a wallet is required to build a settlement")
	}
	if len(args.Payouts) == 0 {
		return Settlement{}, errors.New("cosign: a settlement with no payouts pays nobody")
	}
	if len(args.Description) < 5 {
		return Settlement{}, errors.New("cosign: the settlement description must be at least 5 bytes")
	}
	if args.Pot.Script == nil {
		return Settlement{}, errors.New("cosign: the pot's locking script is required to spend it")
	}

	outputs := make([]sdk.CreateActionOutput, 0, len(args.Payouts))
	var total uint64
	for i, p := range args.Payouts {
		if p.Script == nil {
			return Settlement{}, fmt.Errorf("cosign: payout %d has no derived script", i)
		}
		total += p.Satoshis
		outputs = append(outputs, sdk.CreateActionOutput{
			LockingScript:     *p.Script,
			Satoshis:          p.Satoshis,
			OutputDescription: "pot payout to a winning seat",
		})
	}
	if total > args.Pot.Satoshis {
		return Settlement{}, fmt.Errorf("cosign: payouts total %d but the pot holds %d", total, args.Pot.Satoshis)
	}

	txid, err := chainHashFromHex(args.Pot.Txid)
	if err != nil {
		return Settlement{}, err
	}

	res, err := args.Wallet.CreateAction(ctx, sdk.CreateActionArgs{
		Description: args.Description,
		InputBEEF:   args.Pot.BEEF,
		Inputs: []sdk.CreateActionInput{{
			Outpoint:         transaction.Outpoint{Txid: *txid, Index: args.Pot.Vout},
			InputDescription: "the shared pot requiring every seat",
			// Fee sizing only; the real script is stripped before storage. Over-declare,
			// because under-declaring underpays the fee and earns an unretryable 4xx.
			UnlockingScriptLength: UnlockingScriptLength(args.Seats),
		}},
		Outputs: outputs,
		Options: &sdk.CreateActionOptions{
			// The whole point: return something signable so signatures can be
			// gathered from independent wallets.
			SignAndProcess:   boolPtr(false),
			RandomizeOutputs: boolPtr(false),
		},
	}, args.Originator)
	if err != nil {
		return Settlement{}, fmt.Errorf("cosign: building the settlement: %w", err)
	}
	if res.SignableTransaction == nil {
		return Settlement{}, errors.New("cosign: the wallet returned no signable transaction")
	}

	tx, err := transaction.NewTransactionFromBEEF(res.SignableTransaction.Tx)
	if err != nil {
		return Settlement{}, fmt.Errorf("cosign: parsing the signable transaction: %w", err)
	}
	potInput, err := FindPotInput(tx, args.Pot.Txid, args.Pot.Vout)
	if err != nil {
		return Settlement{}, err
	}
	// The sighash preimage commits to the input's value and script, so they must be
	// attached before anyone signs.
	tx.Inputs[potInput].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      args.Pot.Satoshis,
		LockingScript: args.Pot.Script,
	})

	return Settlement{Tx: tx, PotInput: potInput, Reference: res.SignableTransaction.Reference}, nil
}

// Complete assembles the signatures and hands the transaction to the wallet to broadcast.
//
// Local verification runs first: storage would catch a bad script too, but doing it here names
// which pot and which hand rather than reporting "script verification failed for input N"
// several layers down.
func Complete(ctx context.Context, w PotFunder, originator string, s Settlement, pot FundedPot, sigs []Signature, seats int) (*sdk.SignActionResult, error) {
	unlock, err := Assemble(sigs, seats)
	if err != nil {
		return nil, err
	}
	if declared := UnlockingScriptLength(seats); len(*unlock) > int(declared) {
		return nil, fmt.Errorf("cosign: the assembled script is %d bytes but %d were declared; the fee is underpaid",
			len(*unlock), declared)
	}

	s.Tx.Inputs[s.PotInput].UnlockingScript = unlock
	if err := VerifyScript(s.Tx, s.PotInput, pot.Script, pot.Satoshis); err != nil {
		return nil, err
	}

	res, err := w.SignAction(ctx, sdk.SignActionArgs{
		Reference: s.Reference,
		Spends: map[uint32]sdk.SignActionSpend{
			uint32(s.PotInput): {UnlockingScript: *unlock},
		},
	}, originator)
	if err != nil {
		return nil, fmt.Errorf("cosign: completing the settlement: %w", err)
	}
	return res, nil
}

// PayoutVout locates a payout's output in a settled transaction, so the recipient knows which
// output to internalize.
func PayoutVout(tx *transaction.Transaction, p Payout) (uint32, error) {
	if tx == nil {
		return 0, errors.New("cosign: no transaction to search")
	}
	if p.Script == nil {
		return 0, errors.New("cosign: the payout has no derived script")
	}
	for i, o := range tx.Outputs {
		if o.LockingScript != nil && o.LockingScript.String() == p.Script.String() {
			return uint32(i), nil
		}
	}
	return 0, errors.New("cosign: the transaction does not contain the payout output")
}

// RefundArgs parameterises a pre-signed refund.
type RefundArgs struct {
	Pot FundedPot
	// Recipient is the seat getting its stake back.
	Recipient *ec.PublicKey
	// Satoshis is the refunded amount, less the refund's own fee.
	Satoshis uint64
	// LockHeight is the height at which the refund becomes spendable.
	LockHeight uint32
}

// BuildRefund constructs an unsigned refund of the pot.
//
// The input carries a non-final sequence so the locktime actually binds: with a final
// sequence the transaction is spendable immediately and the timelock is decorative.
func BuildRefund(args RefundArgs) (*transaction.Transaction, error) {
	if args.Recipient == nil {
		return nil, errors.New("cosign: a refund needs a recipient")
	}
	if args.Satoshis == 0 || args.Satoshis > args.Pot.Satoshis {
		return nil, fmt.Errorf("cosign: a refund of %d is not payable from a pot of %d", args.Satoshis, args.Pot.Satoshis)
	}
	if args.LockHeight == 0 {
		return nil, errors.New("cosign: a refund needs a lock height, or it is spendable immediately")
	}
	if args.Pot.Script == nil {
		return nil, errors.New("cosign: the pot's locking script is required")
	}

	// false => testnet. Teratestnet is testnet-based, so address parameters are testnet.
	addr, err := script.NewAddressFromPublicKey(args.Recipient, false)
	if err != nil {
		return nil, fmt.Errorf("cosign: deriving the refund address: %w", err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, fmt.Errorf("cosign: building the refund script: %w", err)
	}

	txid, err := chainHashFromHex(args.Pot.Txid)
	if err != nil {
		return nil, err
	}

	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       txid,
		SourceTxOutIndex: args.Pot.Vout,
		// Non-final, so the locktime binds.
		SequenceNumber: transaction.DefaultSequenceNumber - 1,
	})
	tx.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      args.Pot.Satoshis,
		LockingScript: args.Pot.Script,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: args.Satoshis, LockingScript: lock})
	tx.LockTime = args.LockHeight
	return tx, nil
}

func boolPtr(b bool) *bool { return &b }

// chainHashFromHex parses a txid.
func chainHashFromHex(s string) (*chainhash.Hash, error) {
	h, err := chainhash.NewHashFromHex(s)
	if err != nil {
		return nil, fmt.Errorf("cosign: %q is not a valid txid: %w", s, err)
	}
	return h, nil
}
