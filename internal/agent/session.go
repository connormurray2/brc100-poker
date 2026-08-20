package agent

import (
	"fmt"
	"sync"

	"github.com/cmurray/brc100-poker/internal/protocol/substrate"
)

// SessionApprover approves signing requests without a prompt, within limits the player set once.
//
// Poker is unattended by nature: a session is many hands, each needing a signature, and a player
// cannot sit at a terminal answering prompts between them. But blanket approval hands a table
// whatever it asks for, and the per-request check is what caught every settlement bug this project
// has had.
//
// So the player consents to a session rather than to each request: a maximum stake per hand, and a
// requirement that they are never paid less than they are owed by the hand's own accounting. The
// wallet still refuses anything outside that. The expectation check in signPot runs first and is
// unaffected -- this only replaces the human at the terminal, not the arithmetic.
type SessionApprover struct {
	mu sync.Mutex
	// maxPotSatoshis is the largest pot this session will sign for. A table asking for more
	// than the player agreed to play for is refused without asking.
	maxPotSatoshis uint64
	// maxFeeSatoshis bounds what any one signature may consume in fees.
	maxFeeSatoshis uint64
	// hands counts what has been approved, for the record the player sees on exit.
	hands int
	// log receives one line per approval, so an unattended session is still auditable.
	log func(string)
}

// NewSessionApprover builds an approver for an unattended session.
func NewSessionApprover(maxPot, maxFee uint64, log func(string)) (*SessionApprover, error) {
	if maxPot == 0 {
		return nil, fmt.Errorf("agent: a session needs a maximum pot; unlimited consent is not consent")
	}
	if maxFee == 0 {
		return nil, fmt.Errorf("agent: a session needs a fee bound")
	}
	return &SessionApprover{maxPotSatoshis: maxPot, maxFeeSatoshis: maxFee, log: log}, nil
}

// Approve decides a request against the session's limits.
func (s *SessionApprover) Approve(req substrate.SigningRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.PotSatoshis > s.maxPotSatoshis {
		return &substrate.Error{
			Code: substrate.CodeDeclined,
			Message: fmt.Sprintf("this session signs for pots up to %d sat; the table asked for %d",
				s.maxPotSatoshis, req.PotSatoshis),
		}
	}
	if req.FeeSatoshis > s.maxFeeSatoshis {
		return &substrate.Error{
			Code: substrate.CodeDeclined,
			Message: fmt.Sprintf("this session allows fees up to %d sat; this one consumes %d",
				s.maxFeeSatoshis, req.FeeSatoshis),
		}
	}
	// Outputs must not exceed the pot. The expectation check already enforces this, but a
	// second cheap check here costs nothing and covers a request that reached the approver by
	// some path that skipped it.
	var out uint64
	for _, o := range req.Outputs {
		out += o.Satoshis
	}
	if out > req.PotSatoshis {
		return &substrate.Error{
			Code:    substrate.CodeDeclined,
			Message: fmt.Sprintf("the outputs total %d sat against a %d sat pot", out, req.PotSatoshis),
		}
	}

	s.hands++
	if s.log != nil {
		mine := uint64(0)
		for _, o := range req.Outputs {
			if o.Description == "you" {
				mine += o.Satoshis
			}
		}
		s.log(fmt.Sprintf("approved %s for hand %s: pot %d sat, to you %d sat, fee %d sat",
			req.Purpose, req.HandID, req.PotSatoshis, mine, req.FeeSatoshis))
	}
	return nil
}

// Approved reports how many requests this session has approved.
func (s *SessionApprover) Approved() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hands
}
