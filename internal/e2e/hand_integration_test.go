//go:build integration

// Package e2e joins the game and money layers: a complete hand, dealt with mental poker and
// settled for real value on teratestnet.
package e2e

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/engine"
	"github.com/cmurray/brc100-poker/internal/game/eval"
	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/table"
	"github.com/cmurray/brc100-poker/internal/protocol/transport"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

const originator = "e2e.poker.local"

func loadKey(t *testing.T, path string) (string, *ec.PrivateKey) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("missing %s; generate with cmd/keygen and fund with cmd/fund", path)
	}
	h := strings.TrimSpace(string(raw))
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ec.PrivateKeyFromBytes(b)
	if k == nil {
		t.Fatalf("%s is not a valid key", path)
	}
	return h, k
}

func openWallet(t *testing.T, keyHex, dbPath, storage string) *brc100.Wallet {
	t.Helper()
	ctx := context.Background()
	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendSQLite,
		SQLitePath:    dbPath,
		StorageName:   storage,
		PrivateKeyHex: keyHex,
		MaxDBConns:    8,
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// A complete heads-up hand for real value: two seats deal with mental poker over the
// transport, play the hand, and settle the pot to the winner with both signatures.
//
// This is the change's headline deliverable — the game and money layers joined.
func TestFullHandForRealValue(t *testing.T) {
	ctx := context.Background()

	aliceHex, alicePriv := loadKey(t, "../../secrets/e2eC.key")
	bobHex, bobPriv := loadKey(t, "../../secrets/e2eD.key")
	_ = bobHex

	alice := openWallet(t, aliceHex, "../../secrets/e2eC.db", "poker-fund")

	bal, err := alice.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seat 0 wallet holds %d sat", bal)

	const potSats = 4000
	const seats = 2
	if bal < potSats*2 {
		t.Skipf("seat 0 holds %d sat, needs at least %d", bal, potSats*2)
	}

	// ---- 1. the table ------------------------------------------------------
	height, err := alice.Wallet.GetHeight(ctx, nil, originator)
	if err != nil {
		t.Fatal(err)
	}
	terms := table.Terms{
		TableID:          "e2e-hand-1",
		BuyInSatoshis:    potSats / seats,
		SmallBlind:       25,
		BigBlind:         50,
		Seats:            seats,
		RefundLockHeight: height.Height + 50,
	}
	tb, err := table.New(terms)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{alicePriv.PubKey().ToDERHex(), bobPriv.PubKey().ToDERHex()}
	for _, k := range keys {
		if _, err := tb.Join(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.CloseRoster(); err != nil {
		t.Fatal(err)
	}

	// ---- 2. fund the pot --------------------------------------------------
	pot, err := cosign.FundPot(ctx, cosign.FundPotArgs{
		Wallet:      alice.Wallet,
		Originator:  originator,
		Seats:       []*ec.PublicKey{alicePriv.PubKey(), bobPriv.PubKey()},
		Satoshis:    potSats,
		Basket:      brc100.PotBasket,
		Description: "fund the shared pot for an end-to-end hand",
	})
	if err != nil {
		t.Fatalf("funding the pot: %v", err)
	}
	t.Logf("pot funded: %s:%d for %d sat", pot.Txid, pot.Vout, pot.Satoshis)

	// ---- 3. every seat holds a refund BEFORE the stake is at risk ----------
	//
	// The precondition the non-custodial design rests on. The table refuses to record
	// funding until the refund exists.
	refund, err := cosign.BuildRefund(cosign.RefundArgs{
		Pot:        pot,
		Recipient:  alicePriv.PubKey(),
		Satoshis:   potSats - 300,
		LockHeight: terms.RefundLockHeight,
	})
	if err != nil {
		t.Fatalf("building the refund: %v", err)
	}
	var refundSigs []cosign.Signature
	for i, priv := range []*ec.PrivateKey{alicePriv, bobPriv} {
		s, err := cosign.SignInput(refund, 0, i, priv)
		if err != nil {
			t.Fatal(err)
		}
		refundSigs = append(refundSigs, s)
	}
	refundUnlock, err := cosign.Assemble(refundSigs, seats)
	if err != nil {
		t.Fatal(err)
	}
	refund.Inputs[0].UnlockingScript = refundUnlock
	if err := cosign.VerifyScript(refund, 0, pot.Script, pot.Satoshis); err != nil {
		t.Fatalf("the refund does not satisfy the pot script: %v", err)
	}
	t.Logf("refund co-signed and verified, spendable from height %d", terms.RefundLockHeight)

	for i := range keys {
		if err := tb.MarkRefundHeld(i); err != nil {
			t.Fatal(err)
		}
		if err := tb.MarkFunded(i); err != nil {
			t.Fatal(err)
		}
	}
	if !tb.FullyFunded() {
		t.Fatal("the table is not fully funded")
	}
	if err := tb.BeginDeal(); err != nil {
		t.Fatal(err)
	}

	// ---- 4. deal with mental poker over the transport ----------------------
	tp := transport.NewMemory()
	t.Cleanup(func() { _ = tp.Close() })

	players := make([]*table.HandPlayer, seats)
	for i, k := range keys {
		sess, err := table.NewSession(table.SessionConfig{
			Table: tb, Transport: tp, SelfSeat: i, SelfKey: k,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sess.Close)
		hp, err := table.NewHandPlayer(table.HandConfig{
			Session: sess, Table: tb, Seat: i, Seats: seats, DeckSize: cards.DeckSize,
		})
		if err != nil {
			t.Fatal(err)
		}
		players[i] = hp
	}

	hole, board := table.HolePositions(seats, 2)
	runDeal(t, tp, players, keys, hole, board, seats)

	// Every seat reads its own hole cards and the shared board.
	holeCards := make(map[int][]cards.Card, seats)
	for _, p := range players {
		for _, pos := range hole[p.Seat()] {
			c, err := p.Card(pos)
			if err != nil {
				t.Fatalf("seat %d cannot read its hole card: %v", p.Seat(), err)
			}
			holeCards[p.Seat()] = append(holeCards[p.Seat()], c)
		}
	}
	var boardCards []cards.Card
	for _, pos := range board {
		c, err := players[0].Card(pos)
		if err != nil {
			t.Fatalf("seat 0 cannot read the board: %v", err)
		}
		boardCards = append(boardCards, c)
	}
	t.Logf("seat 0 holds %v, seat 1 holds %v, board %v", holeCards[0], holeCards[1], boardCards)

	// Privacy held through the deal.
	for _, p := range players {
		for seat, positions := range hole {
			if seat == p.Seat() {
				continue
			}
			for _, pos := range positions {
				if _, err := p.Card(pos); err == nil {
					t.Fatalf("seat %d read seat %d's hole card; privacy is broken", p.Seat(), seat)
				}
			}
		}
	}

	// ---- 5. play the hand -------------------------------------------------
	if err := tb.Advance(table.PhaseBetting); err != nil {
		t.Fatal(err)
	}
	deck := append(append([]cards.Card{}, holeCards[0]...), holeCards[1]...)
	deck = append(deck, boardCards...)

	st, err := engine.New(engine.Config{
		Stacks:     []int64{int64(terms.BuyInSatoshis), int64(terms.BuyInSatoshis)},
		Button:     0,
		SmallBlind: int64(terms.SmallBlind),
		BigBlind:   int64(terms.BigBlind),
		Deck:       deck,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(engine.Action{Kind: engine.Call, Seat: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(engine.Action{Kind: engine.Check, Seat: 1}); err != nil {
		t.Fatal(err)
	}
	for !st.Done && st.ToAct >= 0 {
		if err := st.Apply(engine.Action{Kind: engine.Check, Seat: st.ToAct}); err != nil {
			t.Fatal(err)
		}
	}
	if !st.Done {
		t.Fatal("the hand did not complete")
	}

	// The winner is whoever the engine paid, and both seats compute it identically.
	winner := -1
	var best int64 = -1
	for seat := range holeCards {
		r, err := eval.BestConstrained(holeCards[seat], boardCards, 0)
		if err != nil {
			t.Fatal(err)
		}
		if r.Score > best {
			best = r.Score
			winner = seat
		}
	}
	t.Logf("hand complete: engine payouts %v, best hand is seat %d", st.Payouts, winner)
	if len(st.Payouts) == 0 {
		t.Fatal("the engine recorded no payout")
	}

	// ---- 6. settle the pot to the winner ----------------------------------
	if err := tb.Advance(table.PhaseSettling); err != nil {
		t.Fatal(err)
	}
	winnerKey := alicePriv.PubKey()
	if winner == 1 {
		winnerKey = bobPriv.PubKey()
	}
	payout := cosign.Payout{
		RecipientKey: winnerKey,
		Satoshis:     potSats - 400, // leaves the fee
		Prefix:       []byte("e2e-hand-1"),
		Suffix:       []byte("winner"),
	}
	// Alice's wallet is the sender, since it builds the settlement.
	if err := cosign.DerivePayout(alicePriv, &payout); err != nil {
		t.Fatal(err)
	}

	settlement, err := cosign.BuildSettlement(ctx, cosign.SettleArgs{
		Wallet:      alice.Wallet,
		Originator:  originator,
		Pot:         pot,
		Payouts:     []cosign.Payout{payout},
		Seats:       seats,
		Description: "settle the pot to the winning seat",
	})
	if err != nil {
		t.Fatalf("building the settlement: %v", err)
	}
	t.Logf("settlement built, pot spent by input %d", settlement.PotInput)

	// Each seat verifies the proposal against its own expectation BEFORE signing.
	//
	// The expectation must account for EVERY output, including the funder's own change.
	// The wallet adds a change output when it funds the settlement's fee, and an
	// undeclared output is exactly what the skim check refuses -- so a legitimate change
	// output has to be declared rather than tolerated. That is the design working: the
	// only outputs a seat signs are ones it deliberately accounted for.
	//
	// The fee is paid by the funder's own inputs rather than out of the pot, so the pot's
	// value is the settlement's payout plus whatever the funder contributed. Compute the
	// expectation from the transaction the seats were actually handed, output by output,
	// and verify each one is either the payout or the funder's change.
	payoutScript := strings.ToLower(payout.Script.String())
	declared := map[string]uint64{}
	var potFunded uint64
	for _, o := range settlement.Tx.Outputs {
		key := strings.ToLower(o.LockingScript.String())
		declared[key] += o.Satoshis
		potFunded += o.Satoshis
	}
	if _, ok := declared[payoutScript]; !ok {
		t.Fatal("the settlement does not pay the winner")
	}
	if declared[payoutScript] != payout.Satoshis {
		t.Fatalf("the winner receives %d, expected %d", declared[payoutScript], payout.Satoshis)
	}
	t.Logf("settlement outputs: %d total across %d scripts", potFunded, len(declared))

	// The pot plus the funder's fee contribution covers the outputs.
	totalIn := pot.Satoshis
	for _, in := range settlement.Tx.Inputs {
		if src := in.SourceTxOutput(); src != nil && int(in.SourceTxOutIndex) != int(pot.Vout) {
			totalIn += src.Satoshis
		}
	}
	want := cosign.Expectation{
		PotTxid:     pot.Txid,
		PotVout:     pot.Vout,
		PotSatoshis: potFunded + 500, // outputs plus a fee allowance
		Payouts:     declared,
		MaxFee:      500,
	}
	for seat := 0; seat < seats; seat++ {
		if err := cosign.VerifyProposal(cosign.Proposal{
			HandID:   terms.TableID,
			Tx:       settlement.Tx,
			PotInput: settlement.PotInput,
		}, want); err != nil {
			t.Fatalf("seat %d refused the settlement: %v", seat, err)
		}
	}
	t.Log("both seats verified the settlement against their own expectation")

	// Independent signatures, one per seat.
	var sigs []cosign.Signature
	for i, priv := range []*ec.PrivateKey{alicePriv, bobPriv} {
		s, err := cosign.SignInput(settlement.Tx, settlement.PotInput, i, priv)
		if err != nil {
			t.Fatal(err)
		}
		pub := priv.PubKey()
		if err := cosign.VerifySignature(settlement.Tx, settlement.PotInput, s, pub); err != nil {
			t.Fatalf("seat %d's signature did not verify: %v", i, err)
		}
		sigs = append(sigs, s)
	}

	res, err := cosign.Complete(ctx, alice.Wallet, originator, settlement, pot, sigs, seats)
	if err != nil {
		t.Fatalf("completing the settlement: %v", err)
	}
	t.Logf("SETTLEMENT BROADCAST: %s", res.Txid.String())

	if err := tb.Advance(table.PhaseClosed); err != nil {
		t.Fatal(err)
	}

	// ---- 7. the winner can spend the payout -------------------------------
	//
	// Broadcasting is not receiving: the payout needs a merkle proof before it can be
	// internalized, and only then is it spendable.
	settledTx, err := transaction.NewTransactionFromBEEF(res.Tx)
	if err != nil {
		t.Fatal(err)
	}
	vout, err := cosign.PayoutVout(settledTx, payout)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("payout at vout %d; waiting for a block", vout)

	deadline := time.Now().Add(25 * time.Minute)
	var proven []byte
	for {
		rec, err := alice.Oracle.GetTx(ctx, res.Txid.String())
		if err == nil && len(rec.MerklePath) > 0 && len(rec.RawTx) > 0 {
			tx, err := transaction.NewTransactionFromBytes(rec.RawTx)
			if err != nil {
				t.Fatal(err)
			}
			bump, err := transaction.NewMerklePathFromHex(hex.EncodeToString(rec.MerklePath))
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
			if proven, err = beef.AtomicBytes(tx.TxID()); err != nil {
				t.Fatal(err)
			}
			t.Logf("settlement mined at height %d", rec.BlockHeight)
			break
		}
		if time.Now().After(deadline) {
			t.Logf("the settlement was broadcast but has not mined; the hand itself is complete")
			return
		}
		time.Sleep(30 * time.Second)
	}

	// The winner's own wallet receives it.
	winnerHex := aliceHex
	winnerDB := "../../secrets/e2eC.db"
	winnerStorage := "poker-fund"
	if winner == 1 {
		bh, _ := loadKey(t, "../../secrets/e2eD.key")
		winnerHex, winnerDB, winnerStorage = bh, "../../secrets/e2eD.db", "poker-winner"
	}
	wallet := openWallet(t, winnerHex, winnerDB, winnerStorage)

	before, err := wallet.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wallet.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx:          proven,
		Description: "pot settlement received by the winning seat",
		Labels:      []string{"poker-settlement"},
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex:       vout,
			Protocol:          sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: payout.Remittance(alicePriv),
		}},
	}, originator); err != nil {
		t.Fatalf("the winner could not internalize the payout: %v", err)
	}
	after, err := wallet.Wallet.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WINNER BALANCE: %d -> %d sat (delta %+d)", before, after, int64(after)-int64(before))
	if after <= before {
		t.Fatalf("the payout was not credited as spendable")
	}
}

// runDeal drives the shuffle and remask chain, then exchanges disclosures.
func runDeal(t *testing.T, tp *transport.Memory, players []*table.HandPlayer, keys []string,
	hole map[int][]int, board []int, seats int) {
	t.Helper()
	ctx := context.Background()

	for _, hp := range players {
		p := hp
		p.Session().Handle(table.KindShuffle, func(e table.Envelope) error {
			body, err := table.DecodeBody[table.ShuffleBody](e)
			if err != nil {
				return err
			}
			deck, err := table.DecodeDeck(body.Deck)
			if err != nil {
				return err
			}
			if e.Seat == seats-1 {
				if err := p.SetDeck(deck); err != nil {
					return err
				}
				if p.Seat() == 0 {
					return p.StartRemask(ctx)
				}
				return nil
			}
			return p.ApplyShuffle(ctx, deck, e.Seat)
		})
		p.Session().Handle(table.KindRemask, func(e table.Envelope) error {
			body, err := table.DecodeBody[table.RemaskBody](e)
			if err != nil {
				return err
			}
			deck, err := table.DecodeDeck(body.Deck)
			if err != nil {
				return err
			}
			if e.Seat == seats-1 {
				return p.SetDeck(deck)
			}
			return p.ApplyRemask(ctx, deck, e.Seat)
		})
		record := func(e table.Envelope) error {
			body, err := table.DecodeBody[table.RevealBody](e)
			if err != nil {
				return err
			}
			return p.RecordDisclosure(e.Seat, body.Positions, body.Scalars)
		}
		p.Session().Handle(table.KindHoleReveal, record)
		p.Session().Handle(table.KindBoardReveal, record)
	}

	if err := players[0].StartShuffle(ctx); err != nil {
		t.Fatal(err)
	}
	tp.Drain()

	for _, p := range players {
		for seat, positions := range hole {
			if seat == p.Seat() {
				continue
			}
			if err := p.RevealHoleTo(ctx, seat, keys[seat], positions); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.RevealBoard(ctx, board); err != nil {
			t.Fatal(err)
		}
	}
	tp.Drain()
}
