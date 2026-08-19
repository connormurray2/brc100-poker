package brc100

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
)

// FaucetURL is the live teratestnet faucet.
//
// Note this is not the host the faucet's own API docs show: that example points at
// faucet.teratestnet.org, which does not resolve.
const FaucetURL = "https://faucet-ttn.bsvblockchain.tech"

// faucetClaimResponse is the BRC-29 payment the faucet returns.
//
// The faucet does not pay a plain address. It pays a key derived from the claiming
// wallet's identity key and hands back the derivation material, which the wallet needs in
// order to re-derive the locking script and prove the coin is spendable by it. Without
// internalizing this material the payment exists on-chain but the wallet cannot spend it.
type faucetClaimResponse struct {
	TxID              string `json:"txid"`
	Amount            uint64 `json:"amount"`
	AtomicBEEF        string `json:"atomicBEEF"`
	OutputIndex       uint32 `json:"outputIndex"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
}

type faucetErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// FaucetClaim is the outcome of a successful claim.
type FaucetClaim struct {
	TxID   string
	Amount uint64
}

// ClaimFromFaucet claims teratestnet coins into this wallet.
//
// Two steps, and both are required:
//
//  1. POST the wallet's identity key to the faucet, which builds, signs and broadcasts a
//     BRC-29 payment to a key derived from it.
//  2. Internalize the returned transaction as a **wallet payment**, supplying the
//     derivation material. A basket insertion would record no derivation material, so the
//     coin would be visible but permanently unspendable.
//
// captchaToken may be empty when a bearer token is supplied, which the faucet accepts in
// place of the captcha.
func (w *Wallet) ClaimFromFaucet(ctx context.Context, originator, captchaToken, bearerToken string) (FaucetClaim, error) {
	identityKey, err := w.IdentityKey(ctx, originator)
	if err != nil {
		return FaucetClaim{}, fmt.Errorf("brc100: resolving identity key for the faucet claim: %w", err)
	}

	res, err := requestFaucetClaim(ctx, identityKey, captchaToken, bearerToken)
	if err != nil {
		return FaucetClaim{}, err
	}

	beef, err := hex.DecodeString(res.AtomicBEEF)
	if err != nil {
		return FaucetClaim{}, fmt.Errorf("brc100: faucet returned a non-hex atomicBEEF: %w", err)
	}
	prefix, err := decodeDerivation(res.DerivationPrefix)
	if err != nil {
		return FaucetClaim{}, fmt.Errorf("brc100: faucet derivationPrefix: %w", err)
	}
	suffix, err := decodeDerivation(res.DerivationSuffix)
	if err != nil {
		return FaucetClaim{}, fmt.Errorf("brc100: faucet derivationSuffix: %w", err)
	}
	sender, err := ec.PublicKeyFromString(res.SenderIdentityKey)
	if err != nil {
		return FaucetClaim{}, fmt.Errorf("brc100: faucet senderIdentityKey: %w", err)
	}

	if _, err := w.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx:          beef,
		Description: "Teratestnet faucet payout",
		Labels:      []string{"faucet"},
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex: res.OutputIndex,
			// "wallet payment", never "basket insertion": only this protocol
			// records the derivation material that makes the coin spendable.
			Protocol: sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  prefix,
				DerivationSuffix:  suffix,
				SenderIdentityKey: sender,
			},
		}},
	}, originator); err != nil {
		return FaucetClaim{}, fmt.Errorf("brc100: internalizing the faucet payment (txid %s): %w", res.TxID, err)
	}

	return FaucetClaim{TxID: res.TxID, Amount: res.Amount}, nil
}

// decodeDerivation accepts either base64 or hex derivation material.
//
// The wire format is not documented, and the reference client passes the values straight
// through to a JSON wallet call where they are base64. Try base64 first, then hex, so a
// change on either side does not silently produce wrong bytes.
func decodeDerivation(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("value is empty")
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("value %q is neither base64 nor hex", s)
	}
	return b, nil
}

func requestFaucetClaim(ctx context.Context, identityKey, captchaToken, bearerToken string) (faucetClaimResponse, error) {
	body, err := json.Marshal(map[string]string{
		"identityKey":  identityKey,
		"captchaToken": captchaToken,
	})
	if err != nil {
		return faucetClaimResponse{}, fmt.Errorf("brc100: encoding the faucet request: %w", err)
	}

	url := FaucetURL + "/api/claim/wallet"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return faucetClaimResponse{}, fmt.Errorf("brc100: building the faucet request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return faucetClaimResponse{}, fmt.Errorf("brc100: calling the faucet: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return faucetClaimResponse{}, fmt.Errorf("brc100: reading the faucet response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var e faucetErrorResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg := fmt.Sprintf("brc100: faucet returned %d: %s", resp.StatusCode, e.Error)
			if e.Code != "" {
				msg += " (" + e.Code + ")"
			}
			// A captcha rejection is the common case and worth naming, since the
			// token has to come from a browser.
			if strings.Contains(strings.ToLower(e.Error), "captcha") {
				msg += "; supply -captcha with a token from the faucet page, or -bearer with an API key"
			}
			return faucetClaimResponse{}, fmt.Errorf("%s", msg)
		}
		return faucetClaimResponse{}, fmt.Errorf("brc100: faucet returned %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var out faucetClaimResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return faucetClaimResponse{}, fmt.Errorf("brc100: decoding the faucet response: %w", err)
	}
	if out.AtomicBEEF == "" {
		return faucetClaimResponse{}, fmt.Errorf("brc100: faucet response carried no atomicBEEF: %s", truncate(string(raw), 400))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
