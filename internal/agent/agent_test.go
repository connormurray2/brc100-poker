package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newKey(t *testing.T) *ec.PrivateKey {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// approveAll is only appropriate in a test: a real deployment asks the player.
func approveAll() substrate.Approver {
	return substrate.ApproverFunc(func(substrate.SigningRequest) error { return nil })
}

type env struct {
	agent  *Agent
	player *ec.PrivateKey
	seats  []*ec.PrivateKey
	pot    cosign.FundedPot
}

func newEnv(t *testing.T, approver substrate.Approver) env {
	t.Helper()
	ctx := context.Background()

	player := newKey(t)
	other := newKey(t)
	seats := []*ec.PrivateKey{player, other}

	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendSQLite,
		SQLitePath:    filepath.Join(t.TempDir(), "agent.db"),
		StorageName:   "agent-test",
		PrivateKeyHex: hex.EncodeToString(player.Serialize()),
		Logger:        quiet(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })

	a, err := New(Config{
		PrivateKeyHex: hex.EncodeToString(player.Serialize()),
		Wallet:        w,
		Approver:      approver,
		Originator:    "agent.poker.local",
		Logger:        quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}

	potScript, err := cosign.PotScript([]*ec.PublicKey{player.PubKey(), other.PubKey()})
	if err != nil {
		t.Fatal(err)
	}
	var txid chainhash.Hash
	txid[0] = 0x33
	pot := cosign.FundedPot{Txid: txid.String(), Vout: 0, Script: potScript, Satoshis: 5000}

	return env{agent: a, player: player, seats: seats, pot: pot}
}

// payTo builds a settlement paying `to` from the pot.
func (e env) settlement(t *testing.T, to *script.Script, amount uint64) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	h, err := chainhash.NewHashFromHex(e.pot.Txid)
	if err != nil {
		t.Fatal(err)
	}
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       h,
		SourceTxOutIndex: e.pot.Vout,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	})
	tx.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis: e.pot.Satoshis, LockingScript: e.pot.Script,
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: amount, LockingScript: to})
	return tx
}

func winnerScript(t *testing.T, tag byte) *script.Script {
	t.Helper()
	s := &script.Script{}
	if err := s.AppendPushData([]byte{tag, tag, tag}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOpcodes(script.OpDROP, script.OpTRUE); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e env) recordStake(t *testing.T, handID string, payouts map[string]uint64) {
	t.Helper()
	if err := e.agent.RecordStake(Stake{
		HandID:      handID,
		PotTxid:     e.pot.Txid,
		PotVout:     e.pot.Vout,
		PotSatoshis: e.pot.Satoshis,
		Seat:        0,
		RefundHeld:  true,
		Expectation: cosign.Expectation{
			PotTxid:      e.pot.Txid,
			PotVout:      e.pot.Vout,
			PotSatoshis:  e.pot.Satoshis,
			Payouts:      payouts,
			MaxFee:       500,
			PotScriptHex: e.pot.Script.String(),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func (e env) signPot(t *testing.T, handID string, tx *transaction.Transaction) (any, error) {
	t.Helper()
	params, err := json.Marshal(signPotParams{
		HandID:   handID,
		RawTxHex: tx.Hex(),
		PotInput: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e.agent.handleSignPot(newKey(t).PubKey(), params)
}

func TestAgentSignsAnHonestSettlement(t *testing.T) {
	e := newEnv(t, approveAll())
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-1", map[string]uint64{win.String(): 4800})

	res, err := e.signPot(t, "hand-1", e.settlement(t, win, 4800))
	if err != nil {
		t.Fatalf("an honest settlement was refused: %v", err)
	}
	sig, ok := res.(signPotResult)
	if !ok {
		t.Fatalf("result = %T, want signPotResult", res)
	}
	if sig.Seat != 0 {
		t.Errorf("seat = %d, want 0", sig.Seat)
	}
	if sig.DER == "" {
		t.Error("no signature was returned")
	}
	// The signature must actually verify against this player's key.
	raw, err := hex.DecodeString(sig.DER)
	if err != nil {
		t.Fatal(err)
	}
	tx := e.settlement(t, win, 4800)
	if err := cosign.VerifySignature(tx, 0, cosign.Signature{
		Seat: 0, IdentityKey: e.player.PubKey().ToDERHex(), DER: raw,
	}, e.player.PubKey()); err != nil {
		t.Fatalf("the returned signature does not verify: %v", err)
	}
}

// The agent must not sign for a hand it has no record of: that is what stops a table inventing
// one.
func TestAgentRefusesAnUnknownHand(t *testing.T) {
	e := newEnv(t, approveAll())
	win := winnerScript(t, 0xaa)

	_, err := e.signPot(t, "a-hand-that-never-happened", e.settlement(t, win, 4800))
	if err == nil {
		t.Fatal("the agent signed for a hand it holds no stake in")
	}
	var se *substrate.Error
	if !asSubstrateError(err, &se) || se.Code != substrate.CodeForbidden {
		t.Fatalf("error = %v, want forbidden", err)
	}
	if !strings.Contains(se.Message, "holds no stake") {
		t.Errorf("the message does not explain the refusal: %q", se.Message)
	}
}

// The first gate: a settlement that does not match the agent's own record is refused BEFORE the
// player is ever asked, so the prompt cannot be used to launder a bad transaction.
func TestAgentRefusesTheWrongWinnerWithoutAskingThePlayer(t *testing.T) {
	var asked atomic.Int64
	counting := substrate.ApproverFunc(func(substrate.SigningRequest) error {
		asked.Add(1)
		return nil
	})
	e := newEnv(t, counting)

	honest := winnerScript(t, 0xaa)
	thief := winnerScript(t, 0xbb)
	e.recordStake(t, "hand-1", map[string]uint64{honest.String(): 4800})

	_, err := e.signPot(t, "hand-1", e.settlement(t, thief, 4800))
	if err == nil {
		t.Fatal("a settlement paying the wrong recipient was signed")
	}
	if asked.Load() != 0 {
		t.Fatal("the player was asked to approve a settlement the agent could already tell was wrong")
	}
}

func TestAgentRefusesAnAlteredAmount(t *testing.T) {
	e := newEnv(t, approveAll())
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-1", map[string]uint64{win.String(): 4800})

	if _, err := e.signPot(t, "hand-1", e.settlement(t, win, 100)); err == nil {
		t.Fatal("a settlement with the wrong payout amount was signed")
	}
}

func TestAgentRefusesAnUnexpectedOutput(t *testing.T) {
	e := newEnv(t, approveAll())
	win := winnerScript(t, 0xaa)
	skim := winnerScript(t, 0xcc)
	e.recordStake(t, "hand-1", map[string]uint64{win.String(): 4000})

	tx := e.settlement(t, win, 4000)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 800, LockingScript: skim})

	if _, err := e.signPot(t, "hand-1", tx); err == nil {
		t.Fatal("a settlement with an undeclared output was signed")
	}
}

// The second gate: even a correct settlement is not signed without the player's approval.
func TestAgentDoesNotSignWhenThePlayerDeclines(t *testing.T) {
	declining := substrate.ApproverFunc(func(substrate.SigningRequest) error {
		return &substrate.Error{Code: substrate.CodeDeclined, Message: "the player declined"}
	})
	e := newEnv(t, declining)
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-1", map[string]uint64{win.String(): 4800})

	_, err := e.signPot(t, "hand-1", e.settlement(t, win, 4800))
	if err == nil {
		t.Fatal("the agent signed despite the player declining")
	}
	var se *substrate.Error
	if !asSubstrateError(err, &se) || se.Code != substrate.CodeDeclined {
		t.Fatalf("error = %v, want declined", err)
	}
}

// The approver must see the material terms, or the prompt is a rubber stamp.
func TestApproverSeesTheMaterialTerms(t *testing.T) {
	var seen substrate.SigningRequest
	capturing := substrate.ApproverFunc(func(r substrate.SigningRequest) error {
		seen = r
		return nil
	})
	e := newEnv(t, capturing)
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-42", map[string]uint64{win.String(): 4800})

	if _, err := e.signPot(t, "hand-42", e.settlement(t, win, 4800)); err != nil {
		t.Fatal(err)
	}

	if seen.HandID != "hand-42" {
		t.Errorf("handId = %q", seen.HandID)
	}
	if seen.PotSatoshis != e.pot.Satoshis {
		t.Errorf("potSatoshis = %d, want %d", seen.PotSatoshis, e.pot.Satoshis)
	}
	if len(seen.Outputs) != 1 || seen.Outputs[0].Satoshis != 4800 {
		t.Errorf("the approver did not see the outputs: %+v", seen.Outputs)
	}
	// The fee is what the pot loses beyond the payouts, and the player must see it.
	if seen.FeeSatoshis != 200 {
		t.Errorf("fee = %d, want 200", seen.FeeSatoshis)
	}
	if !strings.Contains(seen.PotOutpoint, e.pot.Txid) {
		t.Errorf("the approver did not see which pot: %q", seen.PotOutpoint)
	}
	if seen.RawTxHex == "" {
		t.Error("the approver cannot verify independently without the raw transaction")
	}
}

// A stake with no refund must be refused: recording one would let a stall trap it.
func TestRecordStakeRequiresARefund(t *testing.T) {
	e := newEnv(t, approveAll())
	err := e.agent.RecordStake(Stake{HandID: "h", PotTxid: e.pot.Txid, RefundHeld: false})
	if err == nil {
		t.Fatal("a stake with no refund was recorded")
	}
	if !strings.Contains(err.Error(), "no refund held") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

func TestRecordStakeValidation(t *testing.T) {
	e := newEnv(t, approveAll())
	if err := e.agent.RecordStake(Stake{PotTxid: "aa", RefundHeld: true}); err == nil {
		t.Error("recorded a stake with no hand id")
	}
	if err := e.agent.RecordStake(Stake{HandID: "h", RefundHeld: true}); err == nil {
		t.Error("recorded a stake with no pot")
	}
}

func TestStakeLookup(t *testing.T) {
	e := newEnv(t, approveAll())
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-1", map[string]uint64{win.String(): 4800})

	got, ok := e.agent.Stake("hand-1")
	if !ok {
		t.Fatal("the recorded stake was not found")
	}
	if got.PotTxid != e.pot.Txid || got.Seat != 0 {
		t.Errorf("stake = %+v", got)
	}
	if _, ok := e.agent.Stake("absent"); ok {
		t.Error("an unrecorded hand was found")
	}
}

// A table is granted only what it needs.
func TestGrantedTableCannotEnumerateTheWallet(t *testing.T) {
	e := newEnv(t, approveAll())
	tableKey := newKey(t).PubKey().ToDERHex()
	if err := e.agent.GrantTable(tableKey); err != nil {
		t.Fatal(err)
	}

	g := substrate.TableGrants()
	for _, m := range []substrate.Method{substrate.MethodListOutputs, substrate.MethodListActions} {
		if g.Allows(m) {
			t.Errorf("a table is granted %q", m)
		}
	}
	if !g.Allows(substrate.MethodSignPot) {
		t.Error("a table cannot request a signature, which it needs")
	}

	e.agent.RevokeTable(tableKey)
}

func TestGetNetworkIsTranslated(t *testing.T) {
	e := newEnv(t, approveAll())
	res, err := e.agent.handleGetNetwork(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]string)
	if !ok {
		t.Fatalf("result = %T", res)
	}
	// The wallet emits the invalid "ttn"; the agent must not leak it.
	if m["network"] != brc100.BRC100Testnet {
		t.Errorf("network = %q, want %q", m["network"], brc100.BRC100Testnet)
	}
}

func TestGetPublicKeyReturnsTheIdentity(t *testing.T) {
	e := newEnv(t, approveAll())
	res, err := e.agent.handleGetPublicKey(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]string)
	if !ok {
		t.Fatalf("result = %T, want a map", res)
	}
	if m["publicKey"] != e.player.PubKey().ToDERHex() {
		t.Errorf("publicKey = %q", m["publicKey"])
	}
	if e.agent.Identity() != e.player.PubKey().ToDERHex() {
		t.Error("Identity does not match the player's key")
	}
}

func TestSignPotRejectsMalformedInput(t *testing.T) {
	e := newEnv(t, approveAll())
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-1", map[string]uint64{win.String(): 4800})

	if _, err := e.agent.handleSignPot(newKey(t).PubKey(), json.RawMessage("{not json")); err == nil {
		t.Error("accepted unparseable params")
	}
	empty, _ := json.Marshal(signPotParams{})
	if _, err := e.agent.handleSignPot(newKey(t).PubKey(), empty); err == nil {
		t.Error("accepted params with no hand id or transaction")
	}
	badTx, _ := json.Marshal(signPotParams{HandID: "hand-1", RawTxHex: "nothex"})
	if _, err := e.agent.handleSignPot(newKey(t).PubKey(), badTx); err == nil {
		t.Error("accepted an unparseable transaction")
	}
	// An out-of-range pot input must be refused rather than panicking.
	tx := e.settlement(t, win, 4800)
	oob, _ := json.Marshal(signPotParams{HandID: "hand-1", RawTxHex: tx.Hex(), PotInput: 9})
	if _, err := e.agent.handleSignPot(newKey(t).PubKey(), oob); err == nil {
		t.Error("accepted an out-of-range pot input")
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	player := newKey(t)
	w, err := brc100.New(ctx, brc100.Options{
		Backend: brc100.BackendSQLite, SQLitePath: filepath.Join(t.TempDir(), "v.db"),
		StorageName: "v", PrivateKeyHex: hex.EncodeToString(player.Serialize()), Logger: quiet(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })

	keyHex := hex.EncodeToString(player.Serialize())
	if _, err := New(Config{PrivateKeyHex: keyHex, Approver: approveAll(), Originator: "a.local"}); err == nil {
		t.Error("built an agent with no wallet")
	}
	// The important one: no approver must be refused, not defaulted to approve-all.
	if _, err := New(Config{PrivateKeyHex: keyHex, Wallet: w, Originator: "a.local"}); err == nil {
		t.Error("built an agent with no approver; it would sign anything it is asked to")
	}
	if _, err := New(Config{PrivateKeyHex: keyHex, Wallet: w, Approver: approveAll()}); err == nil {
		t.Error("built an agent with no originator")
	}
	if _, err := New(Config{PrivateKeyHex: "nothex", Wallet: w, Approver: approveAll(), Originator: "a.local"}); err == nil {
		t.Error("built an agent with a non-hex key")
	}
	zero := strings.Repeat("00", 32)
	if _, err := New(Config{PrivateKeyHex: zero, Wallet: w, Approver: approveAll(), Originator: "a.local"}); err == nil {
		t.Error("built an agent with a zero key")
	}
}

// asSubstrateError uses errors.As rather than a type assertion so it keeps working if these
// errors are ever wrapped on the way out.
func asSubstrateError(err error, target **substrate.Error) bool {
	return errors.As(err, target)
}
