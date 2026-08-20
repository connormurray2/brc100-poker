// Package agent is one player's side of the game.
//
// It runs on a machine the player controls, holds that player's key, and serves BRC-100 calls
// over the substrate. Nothing else in the system ever sees the key: the table proposes, and the
// agent decides.
//
// The agent is what makes the design non-custodial in practice rather than in principle. Every
// signature it produces is gated on two independent checks — the transaction must match the
// player's own record of the hand, and the player must approve it — so a compromised table can
// stall a hand and can never move the player's money.
package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/cmurray/brc100-poker/internal/protocol/cosign"
	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
	"github.com/cmurray/brc100-poker/internal/wallet/brc100"
)

// Stake is the agent's record of what it has committed to a hand.
//
// The agent keeps this itself rather than trusting the table's account of it, because the whole
// point is that the table's claims are checked rather than believed.
type Stake struct {
	HandID string
	// PotTxid, PotVout and PotSatoshis identify the pot this stake went into.
	PotTxid     string
	PotVout     uint32
	PotSatoshis uint64
	// Seat is this player's seat index, which fixes its position in the pot script.
	Seat int
	// Expectation is what this seat will accept as a settlement. Built from the player's
	// own view of the hand.
	Expectation cosign.Expectation
	// RefundHeld records that a signed refund exists. No stake is committed before it does.
	RefundHeld bool
	// RefundTxHex is the signed refund, retained so the player can broadcast it unilaterally
	// if the hand stalls.
	RefundTxHex string
}

// Agent serves one player's wallet.
type Agent struct {
	logger *slog.Logger
	wallet *brc100.Wallet
	priv   *ec.PrivateKey
	// identity is the player's public key, hex-encoded.
	identity   string
	originator string

	server *substrate.Server

	mu sync.RWMutex
	// stakes is the agent's own record, keyed by hand id.
	stakes map[string]*Stake
	// deals holds this seat's mental-poker secrets. They never leave the process: that is
	// what makes the deal dealerless rather than merely private from other players.
	deals *dealStore
}

// Config parameterises an Agent.
type Config struct {
	// PrivateKeyHex is the player's key. It stays in this process.
	PrivateKeyHex string
	// Wallet is the player's BRC-100 wallet.
	Wallet *brc100.Wallet
	// Approver decides whether to sign. Required: without it the agent would sign whatever
	// it is asked to, which is the custodial behaviour this package exists to avoid.
	Approver substrate.Approver
	// RequireTLS refuses to serve substrate calls over plaintext.
	RequireTLS bool
	// Originator is the FQDN-shaped identifier BRC-100 requires.
	Originator string
	// AllowedOrigins are the web origins permitted to call this agent from a browser.
	AllowedOrigins []string

	Logger *slog.Logger
}

// New builds an agent and registers its substrate handlers.
func New(cfg Config) (*Agent, error) {
	if cfg.Wallet == nil {
		return nil, errors.New("agent: a wallet is required")
	}
	if cfg.Approver == nil {
		return nil, errors.New("agent: an approver is required; without one the agent would sign anything it is asked to")
	}
	if cfg.Originator == "" {
		return nil, errors.New("agent: an originator is required")
	}

	raw, err := hex.DecodeString(strings.TrimSpace(cfg.PrivateKeyHex))
	if err != nil {
		return nil, fmt.Errorf("agent: the player key is not hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("agent: the player key must be 32 bytes, got %d", len(raw))
	}
	// Validate the scalar ourselves: PrivateKeyFromBytes accepts a zero key and reduces an
	// out-of-range one, either of which would give the player an identity that is not the
	// one they configured.
	d := new(big.Int).SetBytes(raw)
	if d.Sign() == 0 {
		return nil, errors.New("agent: the player key is zero, which is not a valid secp256k1 scalar")
	}
	if d.Cmp(ec.S256().N) >= 0 {
		return nil, errors.New("agent: the player key is not less than the curve order")
	}
	priv, _ := ec.PrivateKeyFromBytes(raw)
	if priv == nil {
		return nil, errors.New("agent: the player key is not a valid secp256k1 scalar")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	srv, err := substrate.NewServer(substrate.Config{
		Wallet:         priv,
		Approver:       cfg.Approver,
		RequireTLS:     cfg.RequireTLS,
		AllowedOrigins: cfg.AllowedOrigins,
		Logger:         logger,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: building the substrate server: %w", err)
	}

	a := &Agent{
		logger:     logger,
		wallet:     cfg.Wallet,
		priv:       priv,
		identity:   priv.PubKey().ToDERHex(),
		originator: cfg.Originator,
		server:     srv,
		stakes:     make(map[string]*Stake),
		deals:      newDealStore(),
	}

	if err := a.registerHandlers(); err != nil {
		return nil, err
	}
	return a, nil
}

// Identity returns the player's identity key.
func (a *Agent) Identity() string { return a.identity }

// Server returns the substrate server, for mounting on an HTTP mux.
func (a *Agent) Server() *substrate.Server { return a.server }

// GrantTable authorises a table service to call this agent.
//
// A table receives only what it needs: it can ask for the seat's identity and propose a
// signature, and cannot enumerate the player's outputs or history, or make the wallet spend on
// its own.
func (a *Agent) GrantTable(tableIdentityKey string) error {
	return a.server.Grant(tableIdentityKey, substrate.TableGrants())
}

// RevokeTable withdraws a table's authorisation.
func (a *Agent) RevokeTable(tableIdentityKey string) { a.server.Revoke(tableIdentityKey) }

// RecordStake stores the agent's own record of a hand.
//
// Called by the player's client once it has independently confirmed the pot and built its
// expectation. The agent will not sign for a hand it has no record of.
func (a *Agent) RecordStake(s Stake) error {
	if s.HandID == "" {
		return errors.New("agent: a stake needs a hand id")
	}
	if s.PotTxid == "" {
		return errors.New("agent: a stake needs the pot it went into")
	}
	if !s.RefundHeld {
		return errors.New("agent: refusing to record a stake with no refund held; a stall could trap it")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stakes[s.HandID] = &s
	return nil
}

// Stake returns the agent's record for a hand.
func (a *Agent) Stake(handID string) (Stake, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.stakes[handID]
	if !ok {
		return Stake{}, false
	}
	return *s, true
}

// signPotParams is the signPot method's arguments.
type signPotParams struct {
	// HandID identifies which hand this signature is for, so the agent can find its own
	// record rather than trusting what the caller says about the pot.
	HandID string `json:"handId"`
	// RawTxHex is the transaction to sign.
	RawTxHex string `json:"rawTxHex"`
	// PotInput is the index of the input spending the pot.
	PotInput int `json:"potInput"`
}

// signPotResult is the signature the agent returns.
type signPotResult struct {
	Seat int    `json:"seat"`
	DER  string `json:"der"`
}

func (a *Agent) registerHandlers() error {
	if err := a.server.HandleMethod(substrate.MethodGetPublicKey, a.handleGetPublicKey); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodGetNetwork, a.handleGetNetwork); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodSignPot, a.handleSignPot); err != nil {
		return err
	}
	// Owner-only by grant: a stake the table could write would make the signing gate a rubber
	// stamp, because the expectation it checks against would be the table's.
	if err := a.server.HandleMethod(substrate.MethodRecordStake, a.handleRecordStake); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodInternalizeAction, a.handleInternalize); err != nil {
		return err
	}
	// The deal methods. The table sequences the chain by calling these in turn; the secrets
	// behind them never leave this process.
	if err := a.server.HandleMethod(substrate.MethodDealCommit, a.handleDealCommit); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodDealShuffle, a.handleDealShuffle); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodDealRemask, a.handleDealRemask); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodDealReveal, a.handleDealReveal); err != nil {
		return err
	}
	if err := a.server.HandleMethod(substrate.MethodDealFinal, a.handleDealFinal); err != nil {
		return err
	}
	return a.server.HandleMethod(substrate.MethodDealCard, a.handleDealCard)
}

func (a *Agent) handleGetPublicKey(_ *ec.PublicKey, _ json.RawMessage) (any, error) {
	return map[string]string{"publicKey": a.identity}, nil
}

// handleGetNetwork reports the network as a valid BRC-100 value.
//
// The wallet's own GetNetwork returns internal names and emits the outright-invalid "ttn" on
// teratestnet, so it is translated here rather than leaked to a caller.
func (a *Agent) handleGetNetwork(_ *ec.PublicKey, _ json.RawMessage) (any, error) {
	got, err := a.wallet.Wallet.GetNetwork(context.Background(), nil, a.originator)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	translated, err := brc100.TranslateSDKNetwork(got.Network)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	return map[string]string{"network": translated}, nil
}

// handleSignPot signs one input of a pot transaction.
//
// Two independent gates, in this order. First the transaction is checked against the agent's
// OWN record of the hand — a settlement that pays the wrong seat, alters an amount, or carries
// an output the player never agreed to is refused before a human is ever asked. Only then is
// the player asked to approve, so the prompt is never used to launder a transaction the agent
// could already tell was wrong.
func (a *Agent) handleSignPot(caller *ec.PublicKey, params json.RawMessage) (any, error) {
	var p signPotParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "signPot params are not valid JSON"}
	}
	if p.HandID == "" || p.RawTxHex == "" {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "signPot needs a hand id and a transaction"}
	}

	stake, ok := a.Stake(p.HandID)
	if !ok {
		// Refusing an unknown hand is what stops a table inventing one.
		return nil, &substrate.Error{
			Code:    substrate.CodeForbidden,
			Message: fmt.Sprintf("this agent holds no stake in hand %q and will not sign for it", p.HandID),
		}
	}

	tx, err := transaction.NewTransactionFromHex(p.RawTxHex)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "the transaction is not parseable"}
	}
	if p.PotInput < 0 || p.PotInput >= len(tx.Inputs) {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "the pot input index is out of range"}
	}

	// Gate one: the transaction must match this seat's own expectation.
	if err := cosign.VerifyProposal(cosign.Proposal{
		HandID:   p.HandID,
		Tx:       tx,
		PotInput: p.PotInput,
	}, stake.Expectation); err != nil {
		a.logger.Warn("refusing a settlement that does not match this seat's record",
			"handId", p.HandID, "caller", short(caller.ToDERHex()), "reason", err.Error())
		return nil, &substrate.Error{Code: substrate.CodeDeclined, Message: err.Error()}
	}

	// The sighash commits to the input's value and script, so they must be attached before
	// signing or the signature would cover the wrong message.
	potScript, err := cosign.PotScriptFromHex(stake.Expectation.PotScriptHex)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	tx.Inputs[p.PotInput].SetSourceTxOutput(&transaction.TransactionOutput{
		Satoshis:      stake.PotSatoshis,
		LockingScript: potScript,
	})

	// Gate two: the player approves this specific request, having seen its material terms.
	req := substrate.SigningRequest{
		HandID:      p.HandID,
		Purpose:     "pot settlement",
		PotOutpoint: fmt.Sprintf("%s:%d", stake.PotTxid, stake.PotVout),
		PotSatoshis: stake.PotSatoshis,
		RawTxHex:    p.RawTxHex,
	}
	var paid uint64
	for _, o := range tx.Outputs {
		paid += o.Satoshis
		desc := "another seat"
		if _, mine := stake.Expectation.Payouts[strings.ToLower(o.LockingScript.String())]; mine {
			desc = "an expected recipient"
		}
		req.Outputs = append(req.Outputs, substrate.SigningOutput{
			Satoshis:      o.Satoshis,
			LockingScript: o.LockingScript.String(),
			Description:   desc,
		})
	}
	if stake.PotSatoshis > paid {
		req.FeeSatoshis = stake.PotSatoshis - paid
	}

	if err := a.server.Approve(req); err != nil {
		var se *substrate.Error
		if errors.As(err, &se) {
			return nil, se
		}
		return nil, &substrate.Error{Code: substrate.CodeDeclined, Message: err.Error()}
	}

	sig, err := cosign.SignInput(tx, p.PotInput, stake.Seat, a.priv)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	a.logger.Info("signed a pot settlement", "handId", p.HandID, "seat", stake.Seat)
	return signPotResult{Seat: stake.Seat, DER: hex.EncodeToString(sig.DER)}, nil
}

// internalizeParams is the internalizeAction method's arguments.
type internalizeParams struct {
	BEEFHex           string `json:"beefHex"`
	OutputIndex       uint32 `json:"outputIndex"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
	Description       string `json:"description"`
}

// handleInternalize records an incoming payment, so the player can spend a payout.
//
// A payment must be internalized as a wallet payment carrying its derivation material: a basket
// insertion records none, so the coin would be visible and permanently unspendable.
func (a *Agent) handleInternalize(_ *ec.PublicKey, params json.RawMessage) (any, error) {
	var p internalizeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "internalize params are not valid JSON"}
	}
	beef, err := hex.DecodeString(p.BEEFHex)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "beefHex is not valid hex"}
	}
	sender, err := ec.PublicKeyFromString(p.SenderIdentityKey)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "senderIdentityKey is not a public key"}
	}
	prefix, err := hex.DecodeString(p.DerivationPrefix)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "derivationPrefix is not valid hex"}
	}
	suffix, err := hex.DecodeString(p.DerivationSuffix)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeBadRequest, Message: "derivationSuffix is not valid hex"}
	}

	desc := p.Description
	if len(desc) < 5 {
		// The toolbox enforces a five-byte minimum; supplying a sane default beats
		// surfacing a validation error from deep inside the library.
		desc = "incoming poker payment"
	}

	res, err := a.wallet.Wallet.InternalizeAction(context.Background(), sdk.InternalizeActionArgs{
		Tx:          beef,
		Description: desc,
		Labels:      []string{"poker"},
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex: p.OutputIndex,
			Protocol:    sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  prefix,
				DerivationSuffix:  suffix,
				SenderIdentityKey: sender,
			},
		}},
	}, a.originator)
	if err != nil {
		return nil, &substrate.Error{Code: substrate.CodeInternal, Message: err.Error()}
	}
	return map[string]bool{"accepted": res.Accepted}, nil
}

func short(key string) string {
	const n = 12
	if len(key) <= n {
		return key
	}
	return key[:n]
}
