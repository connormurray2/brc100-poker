package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

// servedEnv is an agent behind a real HTTP server, called the way a table would call it.
type servedEnv struct {
	env
	url   string
	table *ec.PrivateKey
}

func newServedEnv(t *testing.T, approver substrate.Approver) servedEnv {
	t.Helper()
	e := newEnv(t, approver)

	tableKey := newKey(t)
	if err := e.agent.GrantTable(tableKey.PubKey().ToDERHex()); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(e.agent.Server())
	t.Cleanup(ts.Close)
	return servedEnv{env: e, url: ts.URL, table: tableKey}
}

// call makes an authenticated substrate call and returns the raw response body alongside the
// decoded envelope, so a test can inspect exactly what went over the wire.
func (s servedEnv) call(t *testing.T, method substrate.Method, params any) (substrate.Response, []byte, int) {
	t.Helper()

	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	req := substrate.Request{
		Method:     method,
		Originator: "table.poker.local",
		Params:     raw,
	}
	if err := substrate.SignRequest(&req, s.table, s.agent.Server().Audience()); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody := readAll(t, resp)
	var out substrate.Response
	if err := json.Unmarshal(rawBody, &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", resp.StatusCode, rawBody)
	}
	// Every response must authenticate, including failures.
	if err := substrate.VerifyResponse(out, s.agent.Server().Audience(), req.Nonce); err != nil {
		t.Fatalf("the response did not authenticate: %v", err)
	}
	return out, rawBody, resp.StatusCode
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Task 4.9: a call over the substrate must return the same answer as calling the wallet in
// process. If they diverge, the substrate is not a transport — it is a second implementation.
func TestGetPublicKeyMatchesTheInProcessWallet(t *testing.T) {
	e := newServedEnv(t, approveAll())

	resp, _, status := e.call(t, substrate.MethodGetPublicKey, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %+v", status, resp.Error)
	}
	var over struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(resp.Result, &over); err != nil {
		t.Fatal(err)
	}

	// The same question asked directly of the wallet.
	inProcess, err := e.agent.wallet.IdentityKey(context.Background(), "agent.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	if over.PublicKey != inProcess {
		t.Fatalf("over the wire %s, in process %s", over.PublicKey, inProcess)
	}
}

func TestGetNetworkMatchesTheTranslatedWalletValue(t *testing.T) {
	e := newServedEnv(t, approveAll())

	resp, _, status := e.call(t, substrate.MethodGetNetwork, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %+v", status, resp.Error)
	}
	var over struct {
		Network string `json:"network"`
	}
	if err := json.Unmarshal(resp.Result, &over); err != nil {
		t.Fatal(err)
	}

	raw, err := e.agent.wallet.Wallet.GetNetwork(context.Background(), nil, "agent.poker.local")
	if err != nil {
		t.Fatal(err)
	}
	want, err := brc100.TranslateSDKNetwork(raw.Network)
	if err != nil {
		t.Fatal(err)
	}
	if over.Network != want {
		t.Fatalf("over the wire %q, translated in process %q", over.Network, want)
	}
	// And it must be a value BRC-100 actually defines, not the library's internal name.
	if over.Network != brc100.BRC100Testnet {
		t.Errorf("network = %q; the wallet's internal name leaked", over.Network)
	}
}

// A signature obtained over the substrate must be the same signature the seat would produce
// locally — same seat index, and it verifies against the player's key.
func TestSignPotOverTheWireMatchesALocalSignature(t *testing.T) {
	e := newServedEnv(t, approveAll())
	win := winnerScript(t, 0xaa)
	e.recordStake(t, "hand-parity", map[string]uint64{win.String(): 4800})
	tx := e.settlement(t, win, 4800)

	resp, _, status := e.call(t, substrate.MethodSignPot, signPotParams{
		HandID:   "hand-parity",
		RawTxHex: tx.Hex(),
		PotInput: 0,
	})
	if status != http.StatusOK {
		t.Fatalf("status %d: %+v", status, resp.Error)
	}
	var got signPotResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Seat != 0 {
		t.Errorf("seat = %d, want 0", got.Seat)
	}

	der, err := hex.DecodeString(got.DER)
	if err != nil {
		t.Fatal(err)
	}
	// The signature from over the wire must satisfy the same verification a locally produced
	// one would.
	if err := cosign.VerifySignature(tx, 0, cosign.Signature{
		Seat: 0, IdentityKey: e.player.PubKey().ToDERHex(), DER: der,
	}, e.player.PubKey()); err != nil {
		t.Fatalf("the wire signature does not verify: %v", err)
	}
}

// Task 4.10: the player's key must never appear in a request, a response, or a log line. This is
// the whole premise of the design, so it is asserted rather than assumed.
func TestPrivateKeyNeverAppearsOnTheWireOrInLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := context.Background()
	player := newKey(t)
	keyHex := hex.EncodeToString(player.Serialize())

	w, err := brc100.New(ctx, brc100.Options{
		Backend:       brc100.BackendSQLite,
		SQLitePath:    filepath.Join(t.TempDir(), "leak.db"),
		StorageName:   "leak-test",
		PrivateKeyHex: keyHex,
		Logger:        logger,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })

	a, err := New(Config{
		PrivateKeyHex: keyHex,
		Wallet:        w,
		Approver:      approveAll(),
		Originator:    "agent.poker.local",
		Logger:        logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	tableKey := newKey(t)
	if err := a.GrantTable(tableKey.PubKey().ToDERHex()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Server())
	t.Cleanup(ts.Close)

	// Exercise every served method, including one that signs.
	other := newKey(t)
	potScript, err := cosign.PotScript([]*ec.PublicKey{player.PubKey(), other.PubKey()})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RecordStake(Stake{
		HandID: "leak-hand", PotTxid: strings.Repeat("11", 32), PotVout: 0,
		PotSatoshis: 5000, Seat: 0, RefundHeld: true,
		Expectation: cosign.Expectation{
			PotTxid:      strings.Repeat("11", 32),
			PotSatoshis:  5000,
			Payouts:      map[string]uint64{potScript.String(): 4800},
			MaxFee:       500,
			PotScriptHex: potScript.String(),
		},
	}); err != nil {
		t.Fatal(err)
	}

	served := servedEnv{
		env:   env{agent: a, player: player},
		url:   ts.URL,
		table: tableKey,
	}

	var wire bytes.Buffer
	for _, m := range []substrate.Method{substrate.MethodGetPublicKey, substrate.MethodGetNetwork} {
		resp, raw, _ := served.call(t, m, nil)
		wire.Write(raw)
		if resp.Error != nil {
			t.Logf("%s returned %v (fine: the point is the bytes)", m, resp.Error)
		}
	}
	// A signing attempt too, since that is the call that touches the key.
	_, raw, _ := served.call(t, substrate.MethodSignPot, signPotParams{
		HandID: "leak-hand", RawTxHex: "00", PotInput: 0,
	})
	wire.Write(raw)

	// The key, in every encoding it could plausibly leak as.
	forms := map[string]string{
		"hex lower": strings.ToLower(keyHex),
		"hex upper": strings.ToUpper(keyHex),
	}
	for name, form := range forms {
		if strings.Contains(wire.String(), form) {
			t.Errorf("the player's private key (%s) appeared on the wire", name)
		}
		if strings.Contains(logs.String(), form) {
			t.Errorf("the player's private key (%s) appeared in a log line", name)
		}
	}

	// The identity PUBLIC key is expected to appear; if it does not, the test is not
	// actually exercising anything.
	if !strings.Contains(wire.String(), player.PubKey().ToDERHex()) {
		t.Fatal("the identity public key never appeared on the wire; the test exercised nothing")
	}
	t.Logf("checked %d bytes of wire traffic and %d bytes of logs", wire.Len(), logs.Len())
}

// An ungranted caller must be refused, and the refusal must still authenticate — otherwise a
// caller cannot tell a real refusal from an injected one.
func TestUngrantedCallerIsRefusedWithASignedResponse(t *testing.T) {
	e := newServedEnv(t, approveAll())
	stranger := newKey(t)

	req := substrate.Request{Method: substrate.MethodGetPublicKey, Originator: "evil.example.com"}
	if err := substrate.SignRequest(&req, stranger, e.agent.Server().Audience()); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var out substrate.Response
	if err := json.Unmarshal(readAll(t, resp), &out); err != nil {
		t.Fatal(err)
	}
	if err := substrate.VerifyResponse(out, e.agent.Server().Audience(), req.Nonce); err != nil {
		t.Fatalf("the refusal did not authenticate: %v", err)
	}
}
