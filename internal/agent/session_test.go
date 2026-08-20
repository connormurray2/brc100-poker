package agent

import (
	"strings"
	"testing"

	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// Unattended play is only acceptable if the limits actually bind. If they do not, this is blanket
// approval with extra steps, and the per-request check is what caught every settlement bug here.

func req(pot, fee uint64, outs ...uint64) substrate.SigningRequest {
	r := substrate.SigningRequest{
		HandID: "h1", Purpose: "pot settlement", PotSatoshis: pot, FeeSatoshis: fee,
	}
	for _, o := range outs {
		r.Outputs = append(r.Outputs, substrate.SigningOutput{Satoshis: o, Description: "you"})
	}
	return r
}

func TestSessionApprovesWithinItsLimits(t *testing.T) {
	var logged []string
	s, err := NewSessionApprover(10000, 2000, func(l string) { logged = append(logged, l) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(req(10000, 1200, 8800)); err != nil {
		t.Fatalf("a request inside the limits was declined: %v", err)
	}
	if s.Approved() != 1 {
		t.Fatalf("approved count is %d, want 1", s.Approved())
	}
	// An unattended session must still be auditable.
	if len(logged) != 1 || !strings.Contains(logged[0], "to you 8800") {
		t.Fatalf("approval was not logged usefully: %v", logged)
	}
}

func TestSessionRefusesAPotLargerThanAgreed(t *testing.T) {
	s, err := NewSessionApprover(10000, 2000, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A table asking the player to risk more than they agreed to play for.
	if err := s.Approve(req(50000, 1200, 48000)); err == nil {
		t.Fatal("signed for a pot five times the agreed limit")
	}
}

func TestSessionRefusesAnExcessiveFee(t *testing.T) {
	s, err := NewSessionApprover(10000, 500, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A fee is the one thing a table can inflate without an obvious beneficiary.
	if err := s.Approve(req(10000, 4000, 6000)); err == nil {
		t.Fatal("signed a settlement consuming eight times the agreed fee")
	}
}

func TestSessionRefusesOutputsExceedingThePot(t *testing.T) {
	s, err := NewSessionApprover(10000, 2000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(req(10000, 100, 9000, 5000)); err == nil {
		t.Fatal("signed a settlement paying out more than the pot holds")
	}
}

// Unlimited consent is not consent, so a session must state a bound.
func TestSessionRequiresLimits(t *testing.T) {
	if _, err := NewSessionApprover(0, 2000, nil); err == nil {
		t.Fatal("a session with no pot limit was allowed")
	}
	if _, err := NewSessionApprover(10000, 0, nil); err == nil {
		t.Fatal("a session with no fee limit was allowed")
	}
}
