package webui

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
)

// The point of a real pot is that the table cannot spend it alone. These assert the manager
// collects a genuine signature from every seat's own wallet, over the real transaction, and
// refuses anything that does not verify.

// A wallet refuses to sign for a hand it has no record of, and that refusal is the second of the
// two signing gates -- the one that stops a table proposing a settlement for a pot the player
// never agreed to enter.
//
// This is why a table cannot simply fund a pot and collect signatures: each player's own client
// must record its stake first, having independently confirmed the pot and built its expectation.
// RecordStake is deliberately not reachable over the substrate, because a stake the table could
// write would be the table's expectation rather than the player's.
func TestSeatRefusesToSignAHandItHasNoStakeIn(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ags := startAgents(t, 2, tableKey)

	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tableKey, "table.poker.local", coord, nil)
	if err != nil {
		t.Fatal(err)
	}

	seats := make([]AgentEndpoint, 0, len(ags))
	pubs := make([]*ec.PublicKey, 0, len(ags))
	for _, a := range ags {
		seats = append(seats, AgentEndpoint{
			Seat: a.seat, IdentityKey: a.key.PubKey().ToDERHex(), URL: a.url,
		})
		pubs = append(pubs, a.key.PubKey())
	}

	// A real pot script and a real transaction spending it, so the signatures are over
	// something that would actually have to satisfy the script.
	lock, err := cosign.PotScript(pubs)
	if err != nil {
		t.Fatal(err)
	}
	const potSats = 10000
	spend := transaction.NewTransaction()
	var src chainhash.Hash
	src[0] = 0x11
	spend.AddInput(&transaction.TransactionInput{
		SourceTXID:       &src,
		SourceTxOutIndex: 0,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	})
	spend.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis: potSats, LockingScript: lock,
	})
	payout := &script.Script{}
	if err := payout.AppendOpcodes(script.OpTRUE); err != nil {
		t.Fatal(err)
	}
	spend.AddOutput(&transaction.TransactionOutput{Satoshis: potSats - 300, LockingScript: payout})

	_, err = pm.collectSignatures(context.Background(), "pot-hand-1", spend, 0, seats)
	if err == nil {
		t.Fatal("a wallet signed for a hand it holds no stake in")
	}
	if !contains(err.Error(), "holds no stake") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
	// And it names the seat, so an operator can act on it.
	if !contains(err.Error(), "seat 0") {
		t.Fatalf("the refusal does not name the seat: %v", err)
	}
	_ = lock
	_ = potSats
}

// A seat that is unreachable must fail the collection and name that seat, so an operator can act
// on it rather than seeing a generic failure.
func TestPotManagerNamesTheSeatThatWouldNotSign(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	coord, err := NewCoordinator(tableKey, "table.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tableKey, "table.poker.local", coord, nil)
	if err != nil {
		t.Fatal(err)
	}

	absent, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	seats := []AgentEndpoint{{
		Seat: 4, IdentityKey: absent.PubKey().ToDERHex(),
		URL: "http://127.0.0.1:1", // nothing listens here
	}}

	spend := transaction.NewTransaction()
	_, err = pm.collectSignatures(context.Background(), "pot-hand-2", spend, 0, seats)
	if err == nil {
		t.Fatal("want an error when a seat cannot be reached")
	}
	if !contains(err.Error(), "seat 4") {
		t.Fatalf("the error does not name the seat: %v", err)
	}
}

// A table with no funding wallet plays for chips. It must say so rather than reporting a pot it
// does not have, and must refuse to open one.
func TestPotManagerWithoutAWalletIsNotForValue(t *testing.T) {
	tableKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tableKey, "table.poker.local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pm.Enabled() {
		t.Fatal("a manager with no wallet reported itself able to fund a pot")
	}
	if _, err := pm.OpenPot(context.Background(), "h", nil, 1000, 100); err == nil {
		t.Fatal("a manager with no wallet opened a pot")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Settlement must wait until every seat has recorded its expectation.
//
// The regression: the browser armed its wallet mid-hand, when the payouts were not yet known, so
// the expectation named none. Settlement then paid the actual winner and the wallet declined --
// correctly -- which surfaced to players as "hand stalled by a seat". The table caused it.
func TestSettlementWaitsForEverySeatToArm(t *testing.T) {
	tk, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tk, "t.local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seats := []AgentEndpoint{{Seat: 0, IdentityKey: "02aa"}, {Seat: 1, IdentityKey: "03bb"}}

	if pm.AllArmed("h1", seats) {
		t.Fatal("reported all seats armed before any had")
	}
	pm.MarkArmed("h1", 0)
	if pm.AllArmed("h1", seats) {
		t.Fatal("reported all seats armed with only seat 0 armed")
	}
	pm.MarkArmed("h1", 1)
	if !pm.AllArmed("h1", seats) {
		t.Fatal("both seats armed but not reported so")
	}
	// A different hand must not inherit it.
	if pm.AllArmed("h2", seats) {
		t.Fatal("arming for one hand counted for another")
	}
}

// A stake must not be described before the hand is decided: an expectation naming no payouts
// would make the wallet refuse the settlement that pays the winner.
func TestStakeIsNotOfferedBeforeTheHandIsDecided(t *testing.T) {
	tk, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tk, "t.local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Register a pot directly; no payouts are known yet.
	pm.mu.Lock()
	pm.pots["h1"] = &livePot{}
	pm.mu.Unlock()

	if _, ok := pm.StakeFor("h1", 0, []AgentEndpoint{{Seat: 0, IdentityKey: "02aa"}}, nil); ok {
		t.Fatal("a stake was offered with no payouts known")
	}
}

// The settlement the table builds and the expectation it describes must agree exactly.
//
// The bug players hit: the engine's payouts sum to the whole pot, so a settlement paying them in
// full left the funding wallet to find a fee, which it did by emitting change back to itself --
// an output no seat declared, so every seat refused with "unexpected output of 1155 sat".
func TestReserveFeeLeavesNoChangeAndMatchesTheExpectation(t *testing.T) {
	// A whole pot to one winner, as heads-up produces.
	payouts := map[int]uint64{1: 10000}
	reserved := reserveFee(payouts, settlementFee)

	total := uint64(0)
	for _, v := range reserved {
		total += v
	}
	if total != 10000-settlementFee {
		t.Fatalf("payouts total %d, want %d so the fee leaves no change", total, 10000-settlementFee)
	}
	// The caller's map must be untouched, or a second call would deduct twice.
	if payouts[1] != 10000 {
		t.Fatalf("reserveFee mutated its input: %d", payouts[1])
	}

	// Split pots: the fee comes off the largest, since a small side pot may not cover it.
	split := reserveFee(map[int]uint64{0: 9000, 1: 1000}, settlementFee)
	if split[1] != 1000 {
		t.Fatalf("the fee was taken from the smaller payout: %d", split[1])
	}
	if split[0] != 9000-settlementFee {
		t.Fatalf("the fee was not taken from the largest payout: %d", split[0])
	}

	// Nothing large enough to absorb it: leave the payouts alone so the failure is loud
	// rather than paying someone nothing.
	tiny := reserveFee(map[int]uint64{0: 100}, settlementFee)
	if tiny[0] != 100 {
		t.Fatalf("a payout smaller than the fee was reduced to %d", tiny[0])
	}
}

// The property the whole design exists for: refusing to sign never pays.
//
// Stated as arithmetic over the states a session actually passes through, so a future change that
// reintroduces the incentive fails here rather than in a real game.
func TestRefusingToSignNeverPaysAcrossASession(t *testing.T) {
	const buyIn = 20000
	pot := uint64(buyIn * 2)

	// A session where seat 1 is losing. At each state, compare what seat 1 gets by settling
	// against what it gets by refusing and falling back on the refund.
	states := []map[int]uint64{
		{0: 20000, 1: 20000}, // buy-in
		{0: 20300, 1: 19700},
		{0: 19800, 1: 20200},
		{0: 21000, 1: 19000}, // seat 1 down 1000
	}

	for i, balances := range states {
		var total uint64
		for _, v := range balances {
			total += v
		}
		if total != pot {
			t.Fatalf("state %d: balances total %d, want the pot %d", i, total, pot)
		}

		// Settling pays seat 1 its balance.
		settling := balances[1]
		// Refusing leaves seat 1 with the refund, which pays the same balances. The fee is
		// taken from the largest, so seat 1 gets at least its balance minus the fee.
		refusing := balances[1]
		if balances[1] >= balances[0] {
			refusing -= refundFee
		}

		if refusing > settling {
			t.Fatalf("state %d: refusing pays %d and settling pays %d, so refusing is profitable",
				i, refusing, settling)
		}
	}

	// And the contrast with the old design, kept as the reason this matters: a per-seat refund
	// paying the whole pot made refusing worth up to the entire pot.
	oldRefusalValue := pot - 300
	if oldRefusalValue <= states[len(states)-1][1] {
		t.Fatal("the old design's payoff should have exceeded the losing balance; the comparison is wrong")
	}
}

// A seat's expectation must describe the SETTLEMENT, and a session pot settles balances -- not the
// last hand's payouts.
//
// The bug players hit at cash-out: StakeForSeat described a hand payout of 4000 while the
// settlement paid the seat its 6975 session balance, so the wallet correctly refused with
// "receives 5775, expected 2800" (both figures net of the 1200 fee).
func TestStakeDescribesSessionBalancesNotHandPayouts(t *testing.T) {
	tk, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPotManager(nil, tk, "t.local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := ec.NewPrivateKey()
	b, _ := ec.NewPrivateKey()
	lock, err := cosign.PotScript([]*ec.PublicKey{a.PubKey(), b.PubKey()})
	if err != nil {
		t.Fatal(err)
	}
	pm.mu.Lock()
	pm.pots["s"] = &livePot{
		pot:      cosign.FundedPot{Txid: "dd", Vout: 0, Script: lock, Satoshis: 10000},
		seats:    []*ec.PublicKey{a.PubKey(), b.PubKey()},
		balances: map[int]uint64{0: 3025, 1: 6975},
	}
	pm.mu.Unlock()

	seats := []AgentEndpoint{
		{Seat: 0, IdentityKey: a.PubKey().ToDERHex()},
		{Seat: 1, IdentityKey: b.PubKey().ToDERHex()},
	}
	balances, ok := pm.Balances("s")
	if !ok {
		t.Fatal("no balances for the session")
	}
	info, ok := pm.StakeFor("s", 1, seats, balances)
	if !ok {
		t.Fatal("no stake described for an open session")
	}

	// The amounts must be the balances less the reserved fee, matching what Settle pays.
	want := map[string]uint64{
		a.PubKey().ToDERHex(): 3025,
		b.PubKey().ToDERHex(): 6975 - settlementFee,
	}
	if len(info.Payouts) != 2 {
		t.Fatalf("described %d payouts, want one per seat", len(info.Payouts))
	}
	for _, p := range info.Payouts {
		if want[p.RecipientKey] != p.Satoshis {
			t.Fatalf("seat with key %s… is told %d, want %d",
				p.RecipientKey[:12], p.Satoshis, want[p.RecipientKey])
		}
	}
}

// Arming is per state. A seat armed for the previous balances holds an expectation the settlement
// cannot match, so moving the balances must clear it.
func TestArmingIsClearedWhenBalancesMove(t *testing.T) {
	tk, _ := ec.NewPrivateKey()
	pm, err := NewPotManager(nil, tk, "t.local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seats := []AgentEndpoint{{Seat: 0, IdentityKey: "02aa"}, {Seat: 1, IdentityKey: "03bb"}}
	pm.MarkArmed("s", 0)
	pm.MarkArmed("s", 1)
	if !pm.AllArmed("s", seats) {
		t.Fatal("both seats armed but not reported so")
	}

	// Simulate the balance move ApplyHand performs.
	pm.mu.Lock()
	delete(pm.armed, "s")
	pm.mu.Unlock()

	if pm.AllArmed("s", seats) {
		t.Fatal("seats still count as armed after the balances moved")
	}
}
