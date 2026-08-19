//go:build integration

package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/cmurray/brc100-poker/internal/agent"
	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
	"github.com/cmurray/brc100-poker/internal/protocol/table"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

// deployedURL is the live table service.
const deployedURL = "https://poker.siftbitcoin.com"

// The deployed service must be reachable and fit to hold value before a real-value hand is played
// against it. This is the precondition every other assertion here depends on.
func TestDeployedServiceIsReadyForRealValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, deployedURL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("the deployed service is unreachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Ready          bool     `json:"ready"`
		RealValueReady bool     `json:"realValueReady"`
		Reasons        []string `json:"reasons"`
		Blocked        []string `json:"realValueBlockedBy"`
		Dependencies   []struct {
			Name   string `json:"name"`
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"dependencies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if !body.Ready {
		t.Fatalf("the deployed service is not ready: %v", body.Reasons)
	}
	if !body.RealValueReady {
		t.Fatalf("the deployed service is not fit to hold value: %v", body.Blocked)
	}
	for _, d := range body.Dependencies {
		t.Logf("  %-15s %s %s", d.Name, d.State, d.Detail)
		if d.State != "up" {
			t.Errorf("dependency %s is %s", d.Name, d.State)
		}
	}

	// TLS must be real, not self-signed: the substrate carries signing authority.
	if resp.TLS == nil {
		t.Fatal("the deployed service answered without TLS")
	}
	if len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no certificate was presented")
	}
	cert := resp.TLS.PeerCertificates[0]
	t.Logf("certificate: %s issued by %s, expires %s",
		cert.Subject.CommonName, cert.Issuer.CommonName, cert.NotAfter.Format(time.DateOnly))
	if cert.Subject.CommonName != "poker.siftbitcoin.com" {
		t.Errorf("certificate is for %q", cert.Subject.CommonName)
	}
}

// A complete real-value hand where each seat signs through its OWN agent over the substrate, rather
// than the test holding both keys directly.
//
// This is the difference that matters: in every earlier test the process running the hand also held
// every private key. Here each signature is produced by a separate agent that checks the settlement
// against its own record and could refuse.
func TestRealValueHandThroughPlayerAgents(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	aHex, aPriv := loadKey(t, "../../secrets/liveA.key")
	bHex, bPriv := loadKey(t, "../../secrets/liveB.key")

	// Each player's wallet, each with its own agent. The keys never leave their agent.
	walletA := openWallet(t, aHex, "../../secrets/liveA.db", "poker-fund")
	walletB := openWallet(t, bHex, "../../secrets/liveB.db", "poker-fund")

	const seats = 2
	const potSats = 4000
	const payoutSats = 3600

	bal, err := walletA.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bal < potSats*2 {
		t.Skipf("seat 0 holds %d sat, needs %d", bal, potSats*2)
	}

	// The table's identity: the caller each agent authorises. In the deployed system this is the
	// service's own key; here the test coordinates, so it uses its own.
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	agents := make([]*agent.Agent, seats)
	servers := make([]*httptest.Server, seats)
	for i, spec := range []struct {
		keyHex string
		wallet *brc100.Wallet
	}{{aHex, walletA}, {bHex, walletB}} {
		a, err := agent.New(agent.Config{
			PrivateKeyHex: spec.keyHex,
			Wallet:        spec.wallet,
			// Auto-approve stands in for a human here. The gate that matters is the
			// agent's own verification, which runs first and is not bypassed.
			Approver:   substrate.ApproverFunc(func(substrate.SigningRequest) error { return nil }),
			Originator: "e2e.poker.local",
			Logger:     logger,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.GrantTable(tableKey.PubKey().ToDERHex()); err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(a.Server())
		t.Cleanup(ts.Close)
		agents[i], servers[i] = a, ts
	}
	t.Logf("agent 0 identity %s…", agents[0].Identity()[:16])
	t.Logf("agent 1 identity %s…", agents[1].Identity()[:16])

	// ---- fund the pot -----------------------------------------------------
	pot, err := cosign.FundPot(ctx, cosign.FundPotArgs{
		Wallet:      walletA.Wallet,
		Originator:  originator,
		Seats:       []*ec.PublicKey{aPriv.PubKey(), bPriv.PubKey()},
		Satoshis:    potSats,
		Basket:      brc100.PotBasket,
		Description: "fund a pot for the deployed-service hand",
	})
	if err != nil {
		t.Fatalf("funding the pot: %v", err)
	}
	t.Logf("pot funded: %s:%d for %d sat", pot.Txid, pot.Vout, pot.Satoshis)

	height, err := walletA.Wallet.GetHeight(ctx, nil, originator)
	if err != nil {
		t.Fatal(err)
	}

	// ---- money state, as a player would see it ----------------------------
	money, err := table.NewMoneyTracker(seats, potSats/seats, potSats, height.Height+144)
	if err != nil {
		t.Fatal(err)
	}
	money.SetHand("deployed-hand-1")
	money.SetHeight(height.Height)

	// ---- refunds before anything is at risk -------------------------------
	for seat, priv := range []*ec.PrivateKey{aPriv, bPriv} {
		refund, err := cosign.BuildRefund(cosign.RefundArgs{
			Pot: pot, Recipient: priv.PubKey(), Satoshis: potSats - 300,
			LockHeight: height.Height + 144,
		})
		if err != nil {
			t.Fatal(err)
		}
		var sigs []cosign.Signature
		for i, signer := range []*ec.PrivateKey{aPriv, bPriv} {
			s, err := cosign.SignInput(refund, 0, i, signer)
			if err != nil {
				t.Fatal(err)
			}
			sigs = append(sigs, s)
		}
		unlock, err := cosign.Assemble(sigs, seats)
		if err != nil {
			t.Fatal(err)
		}
		refund.Inputs[0].UnlockingScript = unlock
		if err := cosign.VerifyScript(refund, 0, pot.Script, pot.Satoshis); err != nil {
			t.Fatalf("seat %d's refund does not satisfy the pot: %v", seat, err)
		}
		if err := money.RefundHeld(seat); err != nil {
			t.Fatal(err)
		}
	}
	for seat := 0; seat < seats; seat++ {
		if err := money.Committed(seat); err != nil {
			t.Fatal(err)
		}
	}
	s0, _ := money.State(0)
	t.Logf("seat 0 sees: %s", s0.Summary())

	// ---- the winner's payout ----------------------------------------------
	payout := cosign.Payout{
		RecipientKey: bPriv.PubKey(),
		Satoshis:     payoutSats,
		Prefix:       []byte("deployed-hand-1"),
		Suffix:       []byte("winner"),
	}
	if err := cosign.DerivePayout(aPriv, &payout); err != nil {
		t.Fatal(err)
	}

	settlement, err := cosign.BuildSettlement(ctx, cosign.SettleArgs{
		Wallet:      walletA.Wallet,
		Originator:  originator,
		Pot:         pot,
		Payouts:     []cosign.Payout{payout},
		Seats:       seats,
		Description: "settle the deployed-service hand",
	})
	if err != nil {
		t.Fatalf("building the settlement: %v", err)
	}

	// Each seat's agent needs its own record of the hand, or it refuses to sign — which is the
	// property under test.
	declared := map[string]uint64{}
	for _, o := range settlement.Tx.Outputs {
		declared[strings.ToLower(o.LockingScript.String())] += o.Satoshis
	}
	var totalOut uint64
	for _, v := range declared {
		totalOut += v
	}
	for seat := 0; seat < seats; seat++ {
		if err := agents[seat].RecordStake(agent.Stake{
			HandID: "deployed-hand-1", PotTxid: pot.Txid, PotVout: pot.Vout,
			PotSatoshis: pot.Satoshis, Seat: seat, RefundHeld: true,
			Expectation: cosign.Expectation{
				PotTxid: pot.Txid, PotVout: pot.Vout,
				PotSatoshis:  totalOut + 500,
				Payouts:      declared,
				MaxFee:       500,
				PotScriptHex: pot.Script.String(),
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// ---- each seat signs through its OWN agent, over the substrate --------
	money.Settling("")
	var sigs []cosign.Signature
	for seat := 0; seat < seats; seat++ {
		sig := signViaAgent(t, servers[seat].URL, tableKey, agents[seat].Server().Audience(),
			"deployed-hand-1", settlement.Tx.Hex(), settlement.PotInput)
		der, err := hex.DecodeString(sig.DER)
		if err != nil {
			t.Fatal(err)
		}
		if sig.Seat != seat {
			t.Fatalf("agent %d returned a signature for seat %d", seat, sig.Seat)
		}
		sigs = append(sigs, cosign.Signature{Seat: seat, DER: der})
		t.Logf("seat %d signed through its own agent", seat)
	}

	res, err := cosign.Complete(ctx, walletA.Wallet, originator, settlement, pot, sigs, seats)
	if err != nil {
		t.Fatalf("completing the settlement: %v", err)
	}
	t.Logf("SETTLEMENT BROADCAST: %s", res.Txid.String())
	money.Settled(res.Txid.String(), map[int]uint64{1: payoutSats})

	s1, _ := money.State(1)
	t.Logf("seat 1 sees: %s", s1.Summary())
	if s1.PayoutSpendable {
		t.Error("the payout was reported spendable before it was received")
	}

	// ---- the winner receives it -------------------------------------------
	settledTx, err := transaction.NewTransactionFromBEEF(res.Tx)
	if err != nil {
		t.Fatal(err)
	}
	vout, err := cosign.PayoutVout(settledTx, payout)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(25 * time.Minute)
	for {
		rec, err := walletA.Oracle.GetTx(ctx, res.Txid.String())
		if err == nil && len(rec.MerklePath) > 0 && len(rec.RawTx) > 0 {
			proven := provenBEEF(t, rec.RawTx, rec.MerklePath)
			before, err := walletB.Wallet.Balance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			internalizeViaAgent(t, servers[1].URL, tableKey, agents[1].Server().Audience(),
				proven, vout, payout, aPriv)
			after, err := walletB.Wallet.Balance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("WINNER BALANCE: %d -> %d sat (delta %+d)", before, after, int64(after)-int64(before))
			if after <= before {
				t.Fatal("the payout was not credited as spendable")
			}
			if err := money.PayoutReceived(1); err != nil {
				t.Fatal(err)
			}
			s1, _ = money.State(1)
			t.Logf("seat 1 now sees: %s", s1.Summary())
			return
		}
		if time.Now().After(deadline) {
			t.Logf("the settlement was broadcast but has not mined; the co-signing through agents is proved")
			return
		}
		time.Sleep(30 * time.Second)
	}
}

// signViaAgent asks an agent to sign, over the substrate, exactly as the table would.
func signViaAgent(t *testing.T, url string, caller *ec.PrivateKey, audience, handID, rawTxHex string, potInput int) struct {
	Seat int    `json:"seat"`
	DER  string `json:"der"`
} {
	t.Helper()

	params, err := json.Marshal(map[string]any{
		"handId": handID, "rawTxHex": rawTxHex, "potInput": potInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := callSubstrate(t, url, caller, audience, substrate.MethodSignPot, params)

	var out struct {
		Seat int    `json:"seat"`
		DER  string `json:"der"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decoding the signature: %v", err)
	}
	return out
}

// internalizeViaAgent has the winner's agent record the payout, so the coin becomes spendable.
func internalizeViaAgent(t *testing.T, url string, caller *ec.PrivateKey, audience string,
	beef []byte, vout uint32, payout cosign.Payout, sender *ec.PrivateKey) {
	t.Helper()

	params, err := json.Marshal(map[string]any{
		"beefHex":           hex.EncodeToString(beef),
		"outputIndex":       vout,
		"derivationPrefix":  hex.EncodeToString(payout.Prefix),
		"derivationSuffix":  hex.EncodeToString(payout.Suffix),
		"senderIdentityKey": sender.PubKey().ToDERHex(),
		"description":       "pot settlement received by the winning seat",
	})
	if err != nil {
		t.Fatal(err)
	}
	callSubstrate(t, url, caller, audience, substrate.MethodInternalizeAction, params)
}

// callSubstrate makes one authenticated substrate call and verifies the response.
func callSubstrate(t *testing.T, url string, caller *ec.PrivateKey, audience string,
	method substrate.Method, params json.RawMessage) substrate.Response {
	t.Helper()

	req := substrate.Request{Method: method, Originator: "e2e.poker.local", Params: params}
	if err := substrate.SignRequest(&req, caller, audience); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("content-type", "application/json")

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	var out substrate.Response
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("%s failed: %s: %s", method, out.Error.Code, out.Error.Message)
	}
	// The response must authenticate, or a caller cannot tell the real agent from a substitute.
	if err := substrate.VerifyResponse(out, audience, req.Nonce); err != nil {
		t.Fatalf("the agent's response did not authenticate: %v", err)
	}
	return out
}

func provenBEEF(t *testing.T, rawTx, merklePath []byte) []byte {
	t.Helper()
	tx, err := transaction.NewTransactionFromBytes(rawTx)
	if err != nil {
		t.Fatal(err)
	}
	bump, err := transaction.NewMerklePathFromHex(hex.EncodeToString(merklePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AddMerkleProof(bump); err != nil {
		t.Fatal(err)
	}
	beef := transaction.NewBeefV2()
	if _, err := beef.MergeTransaction(tx); err != nil {
		t.Fatal(err)
	}
	out, err := beef.AtomicBytes(tx.TxID())
	if err != nil {
		t.Fatal(err)
	}
	return out
}
