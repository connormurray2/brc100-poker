package agent

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/galt-tr/go-arcade-toolbox/pkg/brc29"

	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

// The whole point of recordStake is that the wallet derives what it expects to be paid rather than
// accepting it. These assert that, because a wallet that took a script from its caller would sign
// away the pot to whoever asked.

func stakeParams(t *testing.T, handID string, sender *ec.PublicKey, recipient string, sats uint64) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(recordStakeParams{
		HandID:            handID,
		PotTxid:           "aa" + strings.Repeat("00", 31),
		PotVout:           0,
		PotSatoshis:       10000,
		PotScriptHex:      "51",
		Seat:              0,
		SenderIdentityKey: sender.ToDERHex(),
		Payouts: []recordStakePayout{{
			RecipientKey: recipient,
			Satoshis:     sats,
			Prefix:       base64.StdEncoding.EncodeToString([]byte("hand-1")),
			Suffix:       base64.StdEncoding.EncodeToString([]byte("seat-0")),
		}},
		MaxFee:      500,
		RefundTxHex: "0100000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The script the wallet records for its own payout must be the one it derives with LockForSelf --
// the mirror of the sender's LockForCounterparty. If these disagree the payout is unspendable.
func TestRecordedOwnPayoutScriptMatchesTheSendersDerivation(t *testing.T) {
	senderPriv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t)
	own := a.priv.PubKey().ToDERHex()

	res, err := a.handleRecordStake(nil, stakeParams(t, "hand-own", senderPriv.PubKey(), own, 6000))
	if err != nil {
		t.Fatalf("recording the stake: %v", err)
	}
	out, ok := res.(recordStakeResult)
	if !ok || !out.Recorded {
		t.Fatalf("the stake was not recorded: %#v", res)
	}

	// Independently compute what the SENDER would have paid to, and require a match.
	keyID := brc29.KeyID{
		DerivationPrefix: base64.StdEncoding.EncodeToString([]byte("hand-1")),
		DerivationSuffix: base64.StdEncoding.EncodeToString([]byte("seat-0")),
	}
	want, err := brc29.LockForCounterparty(senderPriv, keyID, a.priv.PubKey())
	if err != nil {
		t.Fatal(err)
	}
	wantHex := hex.EncodeToString(*want)
	if _, present := out.Payouts[wantHex]; !present {
		t.Fatalf("the wallet expects a different script than the sender pays to:\n  derived: %v\n  sender:  %s",
			keysOf(out.Payouts), wantHex)
	}
	if out.Payouts[wantHex] != 6000 {
		t.Fatalf("the recorded amount is %d, want 6000", out.Payouts[wantHex])
	}
}

// A stake with no refund must be refused: the refund is what makes committing safe.
func TestRecordStakeRefusesWithoutARefund(t *testing.T) {
	senderPriv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t)

	var p recordStakeParams
	if err := json.Unmarshal(stakeParams(t, "h", senderPriv.PubKey(), a.priv.PubKey().ToDERHex(), 6000), &p); err != nil {
		t.Fatal(err)
	}
	p.RefundTxHex = ""
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.handleRecordStake(nil, body); err == nil {
		t.Fatal("a stake with no refund was recorded")
	}
}

// A stake naming no payouts would accept a settlement that pays nobody.
func TestRecordStakeRefusesWithNoPayouts(t *testing.T) {
	senderPriv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t)

	var p recordStakeParams
	if err := json.Unmarshal(stakeParams(t, "h", senderPriv.PubKey(), a.priv.PubKey().ToDERHex(), 6000), &p); err != nil {
		t.Fatal(err)
	}
	p.Payouts = nil
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.handleRecordStake(nil, body); err == nil {
		t.Fatal("a stake with no payouts was recorded")
	}
}

func keysOf(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// newTestAgent builds an agent with a real wallet, matching how the other tests here do it.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	ctx := context.Background()
	player, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendSQLite,
		SQLitePath:    filepath.Join(t.TempDir(), "stake.db"),
		StorageName:   "stake-test",
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
		Approver:      approveAll(),
		Originator:    "agent.poker.local",
		Logger:        quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
